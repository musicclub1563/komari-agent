//go:build !windows

package terminal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// startTestTerminal launches a controlled child inside a real PTY. Tests never
// touch the user's shell or an existing session.
func startTestTerminal(t *testing.T, script string) *unixTerminal {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	// 与 newTerminalImpl 保持一致：保存原始 fd 并保持非阻塞模式。
	fd := int(tty.Fd())
	_ = unix.SetNonblock(fd, true)
	term := &unixTerminal{tty: tty, cmd: cmd, fd: fd, waitDone: make(chan struct{})}
	t.Cleanup(func() {
		// Close is idempotent, so this is a no-op when the test already closed.
		_ = term.Close()
	})
	return term
}

// TestUnixTerminalCloseReleasesPTYAndReapsProcess is the regression test for
// the leaked PTY master descriptor: Close must release the descriptor itself
// instead of relying on garbage collection.
func TestUnixTerminalCloseReleasesPTYAndReapsProcess(t *testing.T) {
	term := startTestTerminal(t, "sleep 30")
	// 使用保存的原始 fd：os.File.Fd() 会把 fd 切回阻塞模式。
	fd := term.fd

	start := time.Now()
	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > closeGracePeriod {
		t.Fatalf("Close took %v, expected the graceful window or less", elapsed)
	}

	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != syscall.EBADF {
		t.Fatalf("pty master fd %d is still open after Close (err=%v)", fd, err)
	}
	if term.cmd.ProcessState == nil {
		t.Fatal("child process was not reaped by Close")
	}
	if _, err := term.tty.Write([]byte("x")); err == nil {
		t.Fatal("write to the closed pty master succeeded")
	}
}

// TestUnixTerminalCloseUnblocksReader covers a shutdown while the output pump
// sits in Read.
func TestUnixTerminalCloseUnblocksReader(t *testing.T) {
	term := startTestTerminal(t, "sleep 30")

	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 128)
		_, err := term.Read(buffer)
		readDone <- err
	}()
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("blocked reader reported success after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock the reader")
	}
	if elapsed := time.Since(start); elapsed > closeGracePeriod {
		t.Fatalf("Close took %v", elapsed)
	}
}

// TestUnixTerminalCloseIsIdempotent checks repeated and concurrent shutdown.
func TestUnixTerminalCloseIsIdempotent(t *testing.T) {
	term := startTestTerminal(t, "sleep 30")

	const callers = 4
	var wg sync.WaitGroup
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- term.Close()
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Close returned %v", err)
		}
	}
	if err := term.Close(); err != nil {
		t.Fatalf("second Close returned %v", err)
	}
	if err := term.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("Wait after Close returned %v", err)
		}
	}
}

// TestUnixTerminalCloseAfterProcessExit covers the already-exited path.
func TestUnixTerminalCloseAfterProcessExit(t *testing.T) {
	term := startTestTerminal(t, "exit 3")
	if err := term.Wait(); err == nil {
		t.Fatal("expected a non-zero exit status from the test shell")
	}
	if err := term.Close(); err != nil {
		t.Fatalf("Close after exit: %v", err)
	}
	if err := term.Close(); err != nil {
		t.Fatalf("second Close after exit: %v", err)
	}
}

// TestHelperStubbornTerminalChild is not a test: it is re-executed as a child
// process that ignores termination signals, so the SIGKILL path is exercised
// deterministically instead of relying on shell trap semantics.
func TestHelperStubbornTerminalChild(t *testing.T) {
	if os.Getenv("KOMARI_TEST_STUBBORN_CHILD") != "1" {
		t.Skip("helper process for TestUnixTerminalCloseForceKillsStubbornProcess")
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	// Tell the parent that the handlers are installed before it sends signals.
	fmt.Println("KOMARI_READY")
	for {
		select {
		case <-signals:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// waitForPTYOutput reads until the marker shows up or the deadline expires.
func waitForPTYOutput(t *testing.T, tty *os.File, marker string, timeout time.Duration) {
	t.Helper()
	type chunk struct {
		data []byte
		err  error
	}
	chunks := make(chan chunk, 16)
	go func() {
		buffer := make([]byte, 512)
		for {
			n, err := tty.Read(buffer)
			if n > 0 {
				chunks <- chunk{data: append([]byte(nil), buffer[:n]...)}
			}
			if err != nil {
				chunks <- chunk{err: err}
				return
			}
		}
	}()

	deadline := time.After(timeout)
	var seen strings.Builder
	for {
		select {
		case c := <-chunks:
			if c.err != nil {
				t.Fatalf("pty closed before marker %q appeared (saw %q): %v", marker, seen.String(), c.err)
			}
			seen.Write(c.data)
			if strings.Contains(seen.String(), marker) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q on the pty (saw %q)", marker, seen.String())
		}
	}
}

// TestUnixTerminalCloseForceKillsStubbornProcess covers the timeout path: a
// child that ignores SIGTERM and SIGHUP must still be killed and reaped.
func TestUnixTerminalCloseForceKillsStubbornProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the full closeGracePeriod to elapse")
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperStubbornTerminalChild")
	cmd.Env = append(os.Environ(), "KOMARI_TEST_STUBBORN_CHILD=1")
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	term := &unixTerminal{tty: tty, cmd: cmd, waitDone: make(chan struct{})}
	t.Cleanup(func() { _ = term.Close() })
	pid := cmd.Process.Pid
	waitForPTYOutput(t, tty, "KOMARI_READY", 5*time.Second)

	start := time.Now()
	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed < closeGracePeriod {
		t.Fatalf("Close returned after %v; the SIGKILL path was not exercised", elapsed)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("process %d survived Close (err=%v)", pid, err)
	}
	if term.cmd.ProcessState == nil {
		t.Fatal("stubborn child was not reaped")
	}
}

// TestUnixTerminalWriteAfterCloseFails documents the descriptor state used by
// the session close path.
func TestUnixTerminalWriteAfterCloseFails(t *testing.T) {
	term := startTestTerminal(t, "sleep 30")
	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := term.Write([]byte("echo test\n")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close = %v, want a closed-file error", err)
	}
	if err := term.Resize(80, 24); err == nil {
		t.Fatal("resize after close succeeded")
	}
}
