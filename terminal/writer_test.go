package terminal

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

// TestPtyWriterQueueIsBounded 固定"不使用无界输入队列"的要求。
func TestPtyWriterQueueIsBounded(t *testing.T) {
	fake := newFakeTerminal(false)
	writer := newPtyWriter(fake)
	defer writer.stopAndWait(time.Second)

	if capacity := cap(writer.requests); capacity != 1 {
		t.Fatalf("writer queue capacity = %d, want 1 (no unbounded backlog)", capacity)
	}
}

// TestPtyWriterTimeoutDoesNotReplay 固定"结果未知的输入不得重放"。
func TestPtyWriterTimeoutDoesNotReplay(t *testing.T) {
	fake := newFakeTerminal(true)
	writer := newPtyWriter(fake)

	payload := []byte("dangerous command\n")
	start := time.Now()
	n, err := writer.write(payload, 300*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("write to a stalled terminal reported success")
	}
	if n != 0 {
		t.Fatalf("write reported %d bytes on a stalled terminal", n)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("write returned after %v, expected the deadline to bound it", elapsed)
	}
	if calls := fake.countWrites(payload); calls != 1 {
		t.Fatalf("terminal saw the payload %d times, want exactly 1 (no replay)", calls)
	}

	// 释放终端后写入者必须能正常收尾。
	_ = fake.Close()
	writer.stopAndWait(2 * time.Second)
	select {
	case <-writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("writer goroutine did not exit")
	}
}

// TestPtyWriterStopReleasesWaiters 固定"停止写入者必须解除等待方"。
func TestPtyWriterStopReleasesWaiters(t *testing.T) {
	fake := newFakeTerminal(true)
	writer := newPtyWriter(fake)

	waiterDone := make(chan error, 1)
	go func() {
		_, err := writer.write([]byte("blocked"), 10*time.Second)
		waiterDone <- err
	}()

	// 等待写入真正卡在终端上。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, calls, _, _ := fake.snapshot(); calls >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_ = fake.Close()
	writer.stopAndWait(2 * time.Second)

	select {
	case err := <-waiterDone:
		if err == nil {
			t.Fatal("the waiter was released without an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stopping the writer did not release the waiting write")
	}
}

// TestPtyWriterSerializesWrites 固定单写者约束：写入不得交错。
func TestPtyWriterSerializesWrites(t *testing.T) {
	fake := newFakeTerminal(false)
	writer := newPtyWriter(fake)
	defer writer.stopAndWait(time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, _ = writer.write(bytes.Repeat([]byte{byte('a' + index)}, 64), 2*time.Second)
		}(i)
	}
	wg.Wait()

	writes, _, _, _ := fake.snapshot()
	if len(writes) != 8 {
		t.Fatalf("terminal saw %d writes, want 8", len(writes))
	}
	for _, write := range writes {
		if len(write) != 64 {
			t.Fatalf("interleaved write detected: %d bytes", len(write))
		}
		for _, b := range write {
			if b != write[0] {
				t.Fatal("interleaved write detected: mixed payloads")
			}
		}
	}
}
