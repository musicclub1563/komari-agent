//go:build windows

package terminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/UserExistsError/conpty"
)

func newTerminalImpl() (*terminalImpl, error) {
	// 查找 shell
	shell, err := exec.LookPath("powershell.exe")
	if err != nil || shell == "" {
		shell = "cmd.exe"
	}
	if shell == "" {
		return nil, fmt.Errorf("no supported shell found")
	}

	// 获取工作目录
	workingDir := "."
	if executable, err := os.Executable(); err == nil {
		workingDir = filepath.Dir(executable)
	}

	// 启动 ConPTY
	tty, err := conpty.Start(shell, conpty.ConPtyWorkDir(workingDir))
	if err != nil {
		return nil, fmt.Errorf("failed to start conpty: %v", err)
	}

	// 设置初始终端大小
	tty.Resize(80, 24)

	return &terminalImpl{
		shell:      shell,
		workingDir: workingDir,
		term: &windowsTerminal{
			tty: tty,
		},
	}, nil
}

type windowsTerminal struct {
	tty *conpty.ConPty

	mu     sync.Mutex
	closed bool
}

// Close 关闭 ConPTY 并释放其管道句柄。重复或并发调用是安全的，
// 只有第一次调用会真正关闭底层资源。
func (t *windowsTerminal) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if t.tty == nil {
		return nil
	}
	return t.tty.Close()
}

func (t *windowsTerminal) Read(p []byte) (int, error) {
	return t.tty.Read(p)
}

func (t *windowsTerminal) Write(p []byte) (int, error) {
	return t.tty.Write(p)
}

func (t *windowsTerminal) Resize(cols, rows int) error {
	return t.tty.Resize(cols, rows)
}

func (t *windowsTerminal) Wait() error {
	if t.tty == nil {
		return fmt.Errorf("tty is nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeGracePeriod)
	defer cancel()
	_, err := t.tty.Wait(ctx)
	return err
}
