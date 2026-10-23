//go:build !windows

package terminal

import (
	"bytes"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// setPtyRawMode 把 PTY 从 canonical 模式切换为 raw 模式。
//
// canonical 模式下终端行规程会丢弃超出缓冲区的输入（写入不会阻塞），
// raw 模式下缓冲区写满后 master 的 write(2) 才会短写——这正是
// "前台进程停止消费输入"时的真实背压场景（例如全屏程序，或自行
// 关闭 canonical 模式的程序）。
//
// 注意：这里必须使用 unixTerminal 保存的原始 fd；os.File.Fd() 会把 fd
// 切回阻塞模式，那样写入会直接阻塞在内核里。
func setPtyRawMode(t *testing.T, term *unixTerminal) {
	t.Helper()
	termios, err := unix.IoctlGetTermios(term.fd, unix.TCGETS)
	if err != nil {
		t.Skipf("cannot read termios: %v", err)
	}
	termios.Lflag &^= syscall.ICANON | syscall.ECHO
	termios.Iflag &^= syscall.IXON | syscall.ICRNL
	if err := unix.IoctlSetTermios(term.fd, unix.TCSETS, termios); err != nil {
		t.Skipf("cannot set termios: %v", err)
	}
}

// newRealPtySession 启动一个真实 PTY 与受控子进程，并包装成会话。
// 子进程脚本由测试决定，绝不触碰真实节点或现有终端会话。
func newRealPtySession(t *testing.T, id, script string, raw bool) (*terminalSession, *unixTerminal) {
	t.Helper()
	term := startTestTerminal(t, script)
	if raw {
		setPtyRawMode(t, term)
	}
	session := newTerminalSession(id, term)
	registerSession(t, session)
	return session, term
}

// assertPtyReleased 验证 PTY 主描述符确实被主动关闭。
func assertPtyReleased(t *testing.T, term *unixTerminal) {
	t.Helper()
	if _, err := term.tty.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("pty master is still usable after teardown (err=%v)", err)
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(term.fd, &stat); err != syscall.EBADF {
		t.Fatalf("pty master fd %d is still open after teardown (err=%v)", term.fd, err)
	}
}

// TestRealPtyWriteStallsWhenChildStopsReading 复现核心缺陷：子进程停止消费
// 终端输入（raw 模式）时，PTY 写入会真实阻塞。
func TestRealPtyWriteStallsWhenChildStopsReading(t *testing.T) {
	session, term := newRealPtySession(t, "real-stall", "sleep 300", true)

	payload := bytes.Repeat([]byte("a"), 1<<20)
	writeDone := make(chan error, 1)
	go func() {
		_, err := session.writer.write(payload, inputWriteTimeout)
		writeDone <- err
	}()

	// 写入必须真的卡住（而不是立刻失败或成功）。
	select {
	case err := <-writeDone:
		t.Fatalf("write to a non-consuming child returned early: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	start := time.Now()
	closeSession("real-stall", session)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("closeSession took %v although the pty write was stalled", elapsed)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("the stalled write reported success after the session was closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing the session did not release the stalled pty write")
	}
	select {
	case <-session.writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal writer goroutine did not exit")
	}
	if term.cmd.ProcessState == nil {
		t.Fatal("child process was not reaped")
	}
	assertPtyReleased(t, term)
}

// TestRealPtyWriteDeadlineTearsDownSession 覆盖写入超时后明确回收会话。
func TestRealPtyWriteDeadlineTearsDownSession(t *testing.T) {
	session, term := newRealPtySession(t, "real-deadline", "sleep 300", true)

	payload := bytes.Repeat([]byte("b"), 1<<20)
	done := make(chan struct{})
	go func() {
		session.writeInput(payload)
		close(done)
	}()

	start := time.Now()
	select {
	case <-done:
	case <-time.After(inputWriteTimeout + 6*time.Second):
		t.Fatal("writeInput never returned although the pty write was stalled")
	}
	elapsed := time.Since(start)

	if elapsed > inputWriteTimeout+4*time.Second {
		t.Fatalf("writeInput returned after %v, expected the %v write deadline", elapsed, inputWriteTimeout)
	}
	if sessionExists("real-deadline") {
		t.Fatal("the wedged session was not removed from the session table")
	}
	if term.cmd.ProcessState == nil {
		t.Fatal("child process was not reaped")
	}
	assertPtyReleased(t, term)
	select {
	case <-session.writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal writer goroutine did not exit")
	}
}

// TestRealPtyNormalSessionStillWorks 回归：正常输入被子进程消费，正常关闭。
func TestRealPtyNormalSessionStillWorks(t *testing.T) {
	session, term := newRealPtySession(t, "real-normal", "cat", false)

	if _, err := session.writer.write([]byte("hello pty\n"), 2*time.Second); err != nil {
		t.Fatalf("write to a consuming child failed: %v", err)
	}

	buffer := make([]byte, 256)
	deadline := time.Now().Add(3 * time.Second)
	got := ""
	for time.Now().Before(deadline) && !bytes.Contains([]byte(got), []byte("hello pty")) {
		n, err := term.Read(buffer)
		if n > 0 {
			got += string(buffer[:n])
		}
		if err != nil {
			break
		}
	}
	if !bytes.Contains([]byte(got), []byte("hello pty")) {
		t.Fatalf("child did not echo the input, got %q", got)
	}

	start := time.Now()
	closeSession("real-normal", session)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("closeSession took %v for a healthy session", elapsed)
	}
	if term.cmd.ProcessState == nil {
		t.Fatal("child process was not reaped")
	}
	assertPtyReleased(t, term)
}

// TestRealPtyReadOutputStopsOnClose 覆盖输出协程在关闭后必须退出。
func TestRealPtyReadOutputStopsOnClose(t *testing.T) {
	session, term := newRealPtySession(t, "real-readoutput", "sleep 300", false)

	readOutputDone := make(chan struct{})
	go func() {
		session.readOutput()
		close(readOutputDone)
	}()
	// 让输出协程真正进入 Read。
	time.Sleep(200 * time.Millisecond)

	closeSession("real-readoutput", session)
	select {
	case <-readOutputDone:
	case <-time.After(3 * time.Second):
		t.Fatal("readOutput goroutine did not exit after the session was closed")
	}
	if term.cmd.ProcessState == nil {
		t.Fatal("child process was not reaped")
	}
	assertPtyReleased(t, term)
}

// TestRealPtyGracefulShutdownStopsShell 覆盖优雅退出仍然有效。
func TestRealPtyGracefulShutdownStopsShell(t *testing.T) {
	session, term := newRealPtySession(t, "real-graceful", "sh", false)

	start := time.Now()
	session.teardown()
	elapsed := time.Since(start)

	if elapsed > closeGracePeriod+2*time.Second {
		t.Fatalf("teardown took %v, expected it to stay bounded", elapsed)
	}
	if term.cmd.ProcessState == nil {
		t.Fatal("child process was not reaped")
	}
	assertPtyReleased(t, term)
}

// TestRealPtyConcurrentCloseIsSafe 覆盖并发关闭下的真实 PTY 行为。
func TestRealPtyConcurrentCloseIsSafe(t *testing.T) {
	session, term := newRealPtySession(t, "real-concurrent", "sleep 300", true)

	payload := bytes.Repeat([]byte("c"), 1<<20)
	go func() {
		_, _ = session.writer.write(payload, inputWriteTimeout)
	}()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			closeSession("real-concurrent", session)
		}()
	}
	wg.Wait()

	if term.cmd.ProcessState == nil {
		t.Fatal("child process was not reaped")
	}
	assertPtyReleased(t, term)
	select {
	case <-session.writer.done:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal writer goroutine did not exit")
	}
}
