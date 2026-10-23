//go:build windows

package terminal

import (
	"sync"
	"testing"
	"time"

	"github.com/UserExistsError/conpty"
)

// TestWindowsTerminalCloseReleasesConPTY mirrors the Unix PTY test on the
// Windows implementation: Close must release the ConPTY and accept repeated or
// concurrent calls without panicking or racing.
func TestWindowsTerminalCloseReleasesConPTY(t *testing.T) {
	tty, err := conpty.Start("cmd.exe")
	if err != nil {
		t.Skipf("conpty unavailable: %v", err)
	}
	term := &windowsTerminal{tty: tty}

	readDone := make(chan struct{})
	go func() {
		buffer := make([]byte, 256)
		_, _ = term.Read(buffer)
		close(readDone)
	}()
	time.Sleep(100 * time.Millisecond)

	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-readDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not unblock the ConPTY reader")
	}

	if err := term.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if closeErr := term.Close(); closeErr != nil {
				t.Errorf("concurrent Close: %v", closeErr)
			}
		}()
	}
	wg.Wait()

	if _, err := term.Write([]byte("echo test\r\n")); err == nil {
		t.Fatal("write to a closed ConPTY succeeded")
	}
}

// TestWindowsTerminalWaitIsBounded checks that Wait does not hang forever on a
// shell that has already been closed.
func TestWindowsTerminalWaitIsBounded(t *testing.T) {
	tty, err := conpty.Start("cmd.exe")
	if err != nil {
		t.Skipf("conpty unavailable: %v", err)
	}
	term := &windowsTerminal{tty: tty}
	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- term.Wait() }()
	select {
	case <-done:
	case <-time.After(closeGracePeriod + 3*time.Second):
		t.Fatal("Wait did not return after the ConPTY was closed")
	}
}
