//go:build !windows

package terminal

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// newTerminalImpl 创建一个新的终端实例。
// 它会尝试根据用户配置文件查找默认 shell，如果失败则回退到常见 shell。
// 优先以交互模式启动 shell，如果不支持则回退到非交互模式。
func newTerminalImpl() (*terminalImpl, error) {
	shell := ""
	// 从 /etc/passwd 获取用户默认 shell
	userHomeDir, err := os.UserHomeDir() // 获取当前用户的主目录
	if err == nil {
		passwdContent, err := os.ReadFile("/etc/passwd")
		if err == nil {
			for _, line := range strings.Split(string(passwdContent), "\n") {
				if strings.Contains(line, userHomeDir) {
					parts := strings.Split(line, ":")
					if len(parts) >= 7 && parts[6] != "" {
						shell = parts[6]
						//log.Printf("Found shell from /etc/passwd: %s for user home: %s\n", shell, userHomeDir)
						break
					}
				}
			}
		} else {
			log.Printf("Error reading /etc/passwd: %v\n", err)
		}
	} else {
		log.Printf("Error getting user home directory: %v\n", err)
	}

	// 验证从 /etc/passwd 获取的 shell 是否可用
	if shell != "" {
		if _, err := exec.LookPath(shell); err != nil {
			log.Printf("Shell '%s' from /etc/passwd not found in PATH, falling back.\n", shell)
			shell = "" // 默认 shell 不可用，清空以进入回退逻辑
		}
	}

	// 回退到默认 shell 列表
	defaultShells := []string{"zsh", "bash", "sh"}
	if shell == "" {
		log.Println("Shell not found or invalid, trying default shells.")
		for _, s := range defaultShells {
			if _, err := exec.LookPath(s); err == nil {
				shell = s
				log.Printf("Using default shell: %s\n", shell)
				break
			}
		}
	}

	if shell == "" {
		return nil, fmt.Errorf("no supported shell found among %v", defaultShells)
	}

	cmd := buildMotdShellCommand(shell)
	cmd.Env = append(os.Environ(), // 继承系统环境变量
		"TERM=xterm-256color", // 设置终端类型，提高兼容性
		"LANG=C.UTF-8",        // 设置语言环境为 UTF-8
		"LC_ALL=C.UTF-8",      // 强制所有本地化变量为 UTF-8
	)

	tty, err := pty.Start(cmd)
	if err != nil {
		// 回退到原始启动逻辑（直接启动 shell，再无参数）
		cmd = exec.Command(shell)
		cmd.Env = append(os.Environ(),
			"TERM=xterm-256color",
			"LANG=C.UTF-8",
			"LC_ALL=C.UTF-8",
		)
		tty, err = pty.Start(cmd)
		if err != nil {
			return nil, fmt.Errorf("failed to start pty with argv0 prelude and plain shell: %v", err)
		}
	}

	// 设置初始终端大小
	pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: 80})

	// 保存原始 fd 并确保它处于非阻塞模式：os.File.Fd() 会把 fd 切回阻塞
	// 模式，那样 write(2) 会阻塞在内核里且无法被关闭打断。
	fd := int(tty.Fd())
	_ = unix.SetNonblock(fd, true)

	return &terminalImpl{
		shell: shell,
		term: &unixTerminal{
			tty:      tty,
			cmd:      cmd,
			fd:       fd,
			waitDone: make(chan struct{}),
		},
	}, nil
}

const motdShellPrelude = "for f in /etc/update-motd.d/*; do [ -e \"$f\" ] && [ -x \"$f\" ] && \"$f\"; done; [ -r /etc/motd ] && cat /etc/motd; exec \"$1\""

func buildMotdShellCommand(shell string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", motdShellPrelude, "komari-motd", shell)
}

// unixTerminal 实现了 Unix 系统下的终端接口。
type unixTerminal struct {
	tty *os.File  // 伪终端设备文件
	cmd *exec.Cmd // 启动的 shell 进程命令
	fd  int       // PTY 主描述符（原始 fd，避免 Fd() 把 fd 切回阻塞模式）

	closedFlag atomic.Bool   // PTY 主描述符是否已释放
	writeLimit atomic.Int64  // 写截止时间（Unix 纳秒；0 表示无限制）

	ttyOnce sync.Once
	ttyErr  error

	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error

	closeOnce sync.Once
	closeErr  error
}

// Close 关闭终端、主动释放 PTY 主描述符，并尝试终止 shell 进程组。
// 重复或并发调用是安全的：只有第一次调用会执行关闭流程。
func (t *unixTerminal) Close() error {
	t.closeOnce.Do(func() {
		t.closeErr = t.shutdown()
	})
	return t.closeErr
}

func (t *unixTerminal) shutdown() error {
	if t.cmd == nil || t.cmd.Process == nil {
		_ = t.closeTTY()
		return fmt.Errorf("terminal process is already nil or not started")
	}

	// 获取进程组 ID (PGID)。如果获取失败，则使用进程 PID 作为回退。
	// 向进程组发送信号可以确保 shell 启动的子进程也能接收到信号。
	pgid, err := syscall.Getpgid(t.cmd.Process.Pid)
	if err != nil {
		log.Printf("Failed to get process group ID for PID %d: %v. Using PID as PGID.\n", t.cmd.Process.Pid, err)
		pgid = t.cmd.Process.Pid
	}

	// 发送 SIGTERM 信号，请求进程组优雅退出
	log.Printf("Sending SIGTERM to process group %d...\n", pgid)
	_ = syscall.Kill(-pgid, syscall.SIGTERM) // -pgid 表示发送给进程组

	// 主动释放 PTY 主描述符。关闭后内核会向会话发送 SIGHUP，
	// 同时解除阻塞在 Read/Write 上的调用方（例如输出泵 goroutine），
	// 避免资源回收依赖进程退出的时序。
	if err := t.closeTTY(); err != nil {
		log.Printf("Failed to close pty master: %v\n", err)
	}

	return t.waitForExit(pgid)
}

// closeTTY 只关闭一次 PTY 主描述符，重复调用返回首次结果。
func (t *unixTerminal) closeTTY() error {
	t.ttyOnce.Do(func() {
		// 先标记已释放：正在等待写缓冲空间的写入者据此立即退出。
		t.closedFlag.Store(true)
		if t.tty == nil {
			return
		}
		t.ttyErr = t.tty.Close()
	})
	return t.ttyErr
}

func (t *unixTerminal) waitForExit(pgid int) error {
	exitErr := make(chan error, 1)
	go func() {
		exitErr <- t.Wait()
	}()

	select {
	case err := <-exitErr:
		// 进程已退出
		return classifyExitError(err)
	case <-time.After(closeGracePeriod):
		// 超时后发送 SIGKILL 强制终止，并等待回收，避免僵尸进程
		log.Printf("Process group %d did not exit within %s, sending SIGKILL\n", pgid, closeGracePeriod)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		err := <-exitErr
		if classified := classifyExitError(err); classified != nil {
			log.Printf("Failed to kill process group %d after SIGKILL: %v\n", pgid, err)
			return fmt.Errorf("failed to kill process group %d: %v", pgid, err)
		}
		return nil
	}
}

// classifyExitError 将已退出（含被信号杀死）进程的 ExitError 视为成功关闭。
func classifyExitError(err error) error {
	if err == nil {
		return nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.Exited() {
			return nil
		}
		// 被 SIGKILL 等信号终止时 Exited() 为 false，但进程同样已被回收。
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return nil
		}
	}
	return fmt.Errorf("process group did not exit gracefully: %v", err)
}

// Read 从伪终端读取数据。
func (t *unixTerminal) Read(p []byte) (int, error) {
	if t.tty == nil {
		return 0, fmt.Errorf("tty is nil")
	}
	return t.tty.Read(p)
}

// Write 向伪终端写入数据。
//
// 实现要点（均为实测结论）：
//   - /dev/ptmx 的 write(2) 在 slave 输入缓冲区写满时返回"短写"而不是
//     EAGAIN。os.File.Write 遇到短写会循环重试，于是调用方永远返回不了
//     （自旋），也无法被 Close 打断。
//   - 因此这里改用单次原始 write(2)：短写表示缓冲区已满，按截止时间
//     有界地等待并重试，超时则返回部分写入量与错误，绝不无限阻塞。
//   - fd 必须保持 O_NONBLOCK：os.File.Fd() 会把 fd 切回阻塞模式，导致
//     write(2) 直接阻塞在内核里且无法被关闭打断。
func (t *unixTerminal) Write(p []byte) (int, error) {
	if t.tty == nil {
		return 0, fmt.Errorf("tty is nil")
	}
	if t.closedFlag.Load() {
		return 0, os.ErrClosed
	}
	deadline, hasDeadline := t.writeDeadline()

	written := 0
	for len(p) > 0 {
		n, err := unix.Write(t.fd, p)
		if n > 0 {
			written += n
			p = p[n:]
		}
		if len(p) == 0 {
			return written, nil
		}
		if err != nil && err != unix.EINTR && err != unix.EAGAIN {
			return written, err
		}
		if !hasDeadline {
			// 没有截止时间时不做等待，直接交回短写。
			return written, io.ErrShortWrite
		}
		if err := t.waitWriteSlot(deadline); err != nil {
			return written, err
		}
	}
	return written, nil
}

// waitWriteSlot 在写截止时间之前等待 PTY 输入缓冲区腾出空间。
// 会话被关闭时立即返回，避免写入者无谓地空等。
func (t *unixTerminal) waitWriteSlot(deadline time.Time) error {
	for {
		if t.closedFlag.Load() {
			return os.ErrClosed
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return os.ErrDeadlineExceeded
		}
		if remaining > 5*time.Millisecond {
			remaining = 5 * time.Millisecond
		}
		time.Sleep(remaining)
	}
}

// writeDeadline 返回当前写截止时间。
func (t *unixTerminal) writeDeadline() (time.Time, bool) {
	value := t.writeLimit.Load()
	if value == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, value), true
}

// SetWriteDeadline 设置写截止时间。
//
// 注意：/dev/ptmx 上 os.File.SetWriteDeadline 不生效（实测写入不会因
// 截止时间到达而返回），因此截止时间由本类型自行保存并在 Write 中执行。
func (t *unixTerminal) SetWriteDeadline(when time.Time) error {
	if when.IsZero() {
		t.writeLimit.Store(0)
		return nil
	}
	t.writeLimit.Store(when.UnixNano())
	return nil
}

// Resize 调整伪终端的大小。
func (t *unixTerminal) Resize(cols, rows int) error {
	if t.tty == nil {
		return fmt.Errorf("tty is nil")
	}
	return pty.Setsize(t.tty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// Wait 等待 shell 进程退出。底层 cmd.Wait 只会执行一次，
// 重复或并发调用共享同一结果，避免重复 Wait 导致的错误与竞态。
func (t *unixTerminal) Wait() error {
	if t.cmd == nil {
		return fmt.Errorf("command is nil")
	}
	if t.waitDone == nil {
		return fmt.Errorf("terminal is not initialised")
	}
	t.waitOnce.Do(func() {
		go func() {
			t.waitErr = t.cmd.Wait()
			close(t.waitDone)
		}()
	})
	<-t.waitDone
	return t.waitErr
}
