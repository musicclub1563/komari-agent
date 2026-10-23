package ws

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultWriteTimeout bounds how long a single frame write may block when the
// peer stops reading. Without it a stalled peer can pin a writer forever.
const DefaultWriteTimeout = 30 * time.Second

// ErrClosed is returned by writes that are attempted on (or interrupted by) a
// connection that has been closed.
var ErrClosed = errors.New("websocket connection is closed")

// SafeConn serializes writes on a gorilla websocket connection while keeping
// Close non-blocking.
//
// gorilla.Conn allows one concurrent writer; the writeSlots channel enforces
// that. Close never takes the write slot: it only closes the underlying socket,
// which is documented as safe to call concurrently with all other methods and
// unblocks any in-flight or queued write.
type SafeConn struct {
	conn *websocket.Conn

	writeSlots chan struct{}
	writeMu    sync.RWMutex
	writeLimit time.Duration

	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}
}

func NewSafeConn(conn *websocket.Conn) *SafeConn {
	return &SafeConn{
		conn:       conn,
		writeSlots: make(chan struct{}, 1),
		writeLimit: DefaultWriteTimeout,
		closed:     make(chan struct{}),
	}
}

// SetWriteTimeout adjusts the per-write deadline. A non-positive duration
// disables the deadline (useful for tests that need an unbounded write).
func (sc *SafeConn) SetWriteTimeout(timeout time.Duration) {
	sc.writeMu.Lock()
	sc.writeLimit = timeout
	sc.writeMu.Unlock()
}

func (sc *SafeConn) writeTimeout() time.Duration {
	sc.writeMu.RLock()
	defer sc.writeMu.RUnlock()
	return sc.writeLimit
}

func (sc *SafeConn) acquireWrite() error {
	select {
	case sc.writeSlots <- struct{}{}:
		// Close may have completed while this writer was queued; releasing the
		// slot here keeps the channel usable for the closed-state fast path.
		select {
		case <-sc.closed:
			<-sc.writeSlots
			return ErrClosed
		default:
			return nil
		}
	case <-sc.closed:
		return ErrClosed
	}
}

func (sc *SafeConn) releaseWrite() {
	<-sc.writeSlots
}

func (sc *SafeConn) WriteMessage(messageType int, data []byte) error {
	if err := sc.acquireWrite(); err != nil {
		return err
	}
	defer sc.releaseWrite()

	if timeout := sc.writeTimeout(); timeout > 0 {
		if err := sc.conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
	}
	err := sc.conn.WriteMessage(messageType, data)
	var timeoutError interface{ Timeout() bool }
	if err != nil && errors.As(err, &timeoutError) && timeoutError.Timeout() {
		// A timed out write leaves the connection in an unusable state; surface
		// it as closed so callers recycle the connection instead of retrying.
		return ErrClosed
	}
	return err
}

func (sc *SafeConn) WriteJSON(v interface{}) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return sc.WriteMessage(websocket.TextMessage, payload)
}

// Close releases the connection without waiting for an in-flight write. It is
// safe to call concurrently and repeatedly; every call returns the result of
// the first one.
func (sc *SafeConn) Close() error {
	sc.closeOnce.Do(func() {
		close(sc.closed)
		sc.closeErr = sc.conn.Close()
	})
	return sc.closeErr
}

// Closed reports a channel that is closed once the connection is shut down.
func (sc *SafeConn) Closed() <-chan struct{} {
	return sc.closed
}

// IsClosed reports whether Close has been called.
func (sc *SafeConn) IsClosed() bool {
	select {
	case <-sc.closed:
		return true
	default:
		return false
	}
}

func (sc *SafeConn) ReadMessage() (int, []byte, error) {
	return sc.conn.ReadMessage()
}

func (sc *SafeConn) ReadJSON(v interface{}) error {
	return sc.conn.ReadJSON(v)
}

func (sc *SafeConn) SetReadDeadline(t time.Time) error {
	return sc.conn.SetReadDeadline(t)
}

// GetConn exposes the underlying connection for read-side helpers. It never
// blocks, so callers on a shutdown path cannot be stalled by a slow writer.
func (sc *SafeConn) GetConn() *websocket.Conn {
	return sc.conn
}
