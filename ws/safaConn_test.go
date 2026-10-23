package ws

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dialTestConn(t *testing.T, server *httptest.Server) *SafeConn {
	t.Helper()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, resp, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		t.Fatalf("dial %q: %v", endpoint, err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return NewSafeConn(conn)
}

// newEchoConn starts a peer that echoes every message back.
func newEchoConn(t *testing.T) *SafeConn {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			messageType, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			if writeErr := conn.WriteMessage(messageType, payload); writeErr != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	sc := dialTestConn(t, server)
	t.Cleanup(func() { _ = sc.Close() })
	return sc
}

// newStalledConn starts a peer that completes the handshake and then stops
// reading, so the client eventually blocks inside a write.
func newStalledConn(t *testing.T, writeTimeout time.Duration) *SafeConn {
	t.Helper()
	release := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		<-release
	}))
	// Cleanups run LIFO: close the client, release the handler, stop the server.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	sc := dialTestConn(t, server)
	sc.SetWriteTimeout(writeTimeout)
	t.Cleanup(func() { _ = sc.Close() })
	return sc
}

// startStalledWrite fills the socket buffers in the background and returns a
// channel that reports how the write loop ended.
func startStalledWrite(t *testing.T, sc *SafeConn) <-chan error {
	t.Helper()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		payload := make([]byte, 1<<20)
		close(started)
		var err error
		for i := 0; i < 256; i++ {
			if err = sc.WriteMessage(websocket.BinaryMessage, payload); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	<-started
	return done
}

func TestWriteMessageRoundTrip(t *testing.T) {
	sc := newEchoConn(t)
	if err := sc.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	messageType, payload, err := sc.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if messageType != websocket.TextMessage || string(payload) != "hello" {
		t.Fatalf("echoed %d %q, want text %q", messageType, payload, "hello")
	}
}

// TestCloseDoesNotWaitForBlockedWrite is the regression test for the reported
// stall: WriteMessage held a mutex with no deadline, so Close waited behind it.
func TestCloseDoesNotWaitForBlockedWrite(t *testing.T) {
	sc := newStalledConn(t, 0)

	writeDone := startStalledWrite(t, sc)
	// Give the writer time to fill the socket buffers and block in write(2).
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-writeDone:
		t.Fatalf("write loop finished before Close (%v); backpressure was not reproduced", err)
	default:
	}

	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- sc.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Close took %v while a write was blocked", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind the stalled write")
	}

	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked write reported success after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release the blocked write")
	}
}

func TestWriteDeadlineReleasesBlockedWrite(t *testing.T) {
	sc := newStalledConn(t, 200*time.Millisecond)

	start := time.Now()
	writeDone := startStalledWrite(t, sc)
	var err error
	select {
	case err = <-writeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("write deadline did not fire")
	}
	if err == nil {
		t.Fatal("blocked write reported success although the peer never read")
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("blocked write error = %v, want ErrClosed", err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("write deadline fired after %v", elapsed)
	}
}

func TestConcurrentCloseIsIdempotent(t *testing.T) {
	sc := newEchoConn(t)

	const closures = 8
	var wg sync.WaitGroup
	results := make(chan error, closures)
	for i := 0; i < closures; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- sc.Close()
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent Close returned %v", err)
		}
	}
	if !sc.IsClosed() {
		t.Fatal("connection is not marked closed")
	}
	select {
	case <-sc.Closed():
	default:
		t.Fatal("Closed() channel was not closed")
	}
}

func TestWriteAfterCloseReturnsErrClosed(t *testing.T) {
	sc := newEchoConn(t)
	if err := sc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sc.WriteMessage(websocket.TextMessage, []byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close = %v, want ErrClosed", err)
	}
	if err := sc.WriteJSON(map[string]string{"late": "write"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("json write after close = %v, want ErrClosed", err)
	}
}

// TestGetConnDoesNotBlockDuringStalledWrite guards the accessor used by
// shutdown paths: it must not queue behind a blocked writer.
func TestGetConnDoesNotBlockDuringStalledWrite(t *testing.T) {
	sc := newStalledConn(t, 0)
	writeDone := startStalledWrite(t, sc)
	time.Sleep(300 * time.Millisecond)

	accessorDone := make(chan struct{})
	go func() {
		if conn := sc.GetConn(); conn != nil {
			close(accessorDone)
		}
	}()
	select {
	case <-accessorDone:
	case <-time.After(time.Second):
		t.Fatal("GetConn blocked while a write was stalled")
	}

	// A second writer must not deadlock either: it is either queued and then
	// released by Close, or rejected outright.
	queued := make(chan error, 1)
	go func() {
		queued <- sc.WriteMessage(websocket.TextMessage, []byte("queued"))
	}()
	if err := sc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		t.Fatal("queued writer was not released by Close")
	}
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stalled writer leaked after Close")
	}
}
