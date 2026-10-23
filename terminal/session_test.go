package terminal

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var errFakeClosed = errors.New("fake terminal closed")

// fakeTerminal 是可控的终端替身：可以模拟"写入永久阻塞"（前台进程停止
// 消费输入）以及"关闭后写入被解除阻塞"（真实 PTY 的行为）。
type fakeTerminal struct {
	mu         sync.Mutex
	writes     [][]byte
	writeCalls int
	resizes    int
	closeCalls int
	closed     chan struct{}
	closeOnce  sync.Once

	// blockWrites 为真时 Write 阻塞直到终端被关闭。
	blockWrites bool
	// neverUnblock 为真时 Write 永久阻塞，即使终端已关闭（病态终端）。
	neverUnblock bool
}

func newFakeTerminal(blockWrites bool) *fakeTerminal {
	return &fakeTerminal{closed: make(chan struct{}), blockWrites: blockWrites}
}

func (f *fakeTerminal) Write(p []byte) (int, error) {
	f.mu.Lock()
	f.writeCalls++
	f.writes = append(f.writes, append([]byte(nil), p...))
	neverUnblock := f.neverUnblock
	f.mu.Unlock()

	if neverUnblock {
		select {} // 永久阻塞，模拟无法解除的死锁写入
	}
	if f.blockWrites {
		<-f.closed
		return 0, errFakeClosed
	}
	return len(p), nil
}

func (f *fakeTerminal) Read(p []byte) (int, error) {
	<-f.closed
	return 0, errFakeClosed
}

func (f *fakeTerminal) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes++
	return nil
}

func (f *fakeTerminal) Close() error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeTerminal) Wait() error {
	<-f.closed
	return nil
}

func (f *fakeTerminal) snapshot() (writes [][]byte, writeCalls, resizes, closeCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writes = make([][]byte, 0, len(f.writes))
	for _, w := range f.writes {
		writes = append(writes, w)
	}
	return writes, f.writeCalls, f.resizes, f.closeCalls
}

// countWrites 统计与 payload 完全相同的写入次数，用于验证输入未被重放。
func (f *fakeTerminal) countWrites(payload []byte) int {
	writes, _, _, _ := f.snapshot()
	count := 0
	for _, w := range writes {
		if bytes.Equal(w, payload) {
			count++
		}
	}
	return count
}

// registerSession 把会话放入全局表，并在测试结束后确保回收。
func registerSession(t *testing.T, session *terminalSession) {
	t.Helper()
	sessionsMu.Lock()
	sessions[session.id] = session
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		delete(sessions, session.id)
		sessionsMu.Unlock()
		session.teardown()
	})
}

func sessionExists(id string) bool {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	return sessions[id] != nil
}

// newConnPair 建立一对已握手的 WebSocket 连接，返回（客户端侧, 服务端侧）。
func newConnPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	serverConn := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
	}))
	t.Cleanup(server.Close)

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	var peer *websocket.Conn
	select {
	case peer = <-serverConn:
	case <-time.After(5 * time.Second):
		t.Fatal("server side websocket was not established")
	}
	t.Cleanup(func() { _ = peer.Close() })
	return client, peer
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", message)
}

// TestWriteInputTearsDownSessionWhenTerminalStalls 覆盖核心缺陷：前台进程
// 停止消费输入导致写入阻塞时，输入写入必须有界，并明确回收整个会话。
func TestWriteInputTearsDownSessionWhenTerminalStalls(t *testing.T) {
	fake := newFakeTerminal(true)
	session := newTerminalSession("stall-write", fake)
	registerSession(t, session)

	payload := bytes.Repeat([]byte("x"), 1<<20)
	done := make(chan struct{})
	go func() {
		session.writeInput(payload)
		close(done)
	}()

	waitFor(t, 3*time.Second, func() bool {
		_, calls, _, _ := fake.snapshot()
		return calls >= 1
	}, "the write to reach the terminal")

	start := time.Now()
	select {
	case <-done:
	case <-time.After(inputWriteTimeout + 5*time.Second):
		t.Fatal("writeInput never returned although the terminal is stalled")
	}
	elapsed := time.Since(start)

	if elapsed > inputWriteTimeout+2*time.Second {
		t.Fatalf("writeInput returned after %v, expected the %v write deadline", elapsed, inputWriteTimeout)
	}
	if sessionExists("stall-write") {
		t.Fatal("stalled session was not removed from the session table")
	}
	if _, _, _, closeCalls := fake.snapshot(); closeCalls == 0 {
		t.Fatal("terminal was not closed after the write deadline")
	}
	select {
	case <-session.writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal writer goroutine did not exit")
	}
	if count := fake.countWrites(payload); count != 1 {
		t.Fatalf("input was written %d times, want exactly 1 (no replay)", count)
	}
}

// TestCloseSessionUnblocksStalledInput 覆盖显式关闭：关闭动作本身不能被
// 阻塞的输入写入拖住。
func TestCloseSessionUnblocksStalledInput(t *testing.T) {
	fake := newFakeTerminal(true)
	session := newTerminalSession("stall-close", fake)
	registerSession(t, session)

	payload := bytes.Repeat([]byte("y"), 1<<20)
	writeDone := make(chan struct{})
	go func() {
		session.writer.write(payload, inputWriteTimeout)
		close(writeDone)
	}()

	waitFor(t, 3*time.Second, func() bool {
		_, calls, _, _ := fake.snapshot()
		return calls >= 1
	}, "the write to reach the terminal")

	start := time.Now()
	closeSession("stall-close", session)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("closeSession took %v while a write was stalled", elapsed)
	}
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the stalled write was not released by the close")
	}
	select {
	case <-session.writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal writer goroutine did not exit")
	}
	if sessionExists("stall-close") {
		t.Fatal("closed session is still registered")
	}
}

// TestTeardownIsBoundedWhenTerminalCannotWrite 覆盖优雅退出失效的场景：
// 优雅退出与强制回收都必须在有限时间内完成。
func TestTeardownIsBoundedWhenTerminalCannotWrite(t *testing.T) {
	fake := newFakeTerminal(true)
	session := newTerminalSession("bounded-teardown", fake)

	start := time.Now()
	session.teardown()
	elapsed := time.Since(start)

	if elapsed > gracefulShutdownBudget+2*time.Second {
		t.Fatalf("teardown took %v, expected the graceful budget to bound it", elapsed)
	}
	if _, _, _, closeCalls := fake.snapshot(); closeCalls == 0 {
		t.Fatal("terminal was not closed")
	}
	select {
	case <-session.writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal writer goroutine did not exit")
	}
}

// TestTeardownReturnsEvenWhenWriteNeverUnblocks 覆盖病态终端：即使底层写入
// 永远无法返回，回收流程也必须有界（真实 PTY 关闭后不会走到这个分支）。
func TestTeardownReturnsEvenWhenWriteNeverUnblocks(t *testing.T) {
	fake := newFakeTerminal(true)
	fake.neverUnblock = true
	session := newTerminalSession("never-unblock", fake)

	go func() {
		_, _ = session.writer.write([]byte("payload"), inputWriteTimeout)
	}()

	waitFor(t, 3*time.Second, func() bool {
		_, calls, _, _ := fake.snapshot()
		return calls >= 1
	}, "the write to reach the terminal")

	start := time.Now()
	session.teardown()
	elapsed := time.Since(start)

	if elapsed > closeGracePeriod+3*time.Second {
		t.Fatalf("teardown took %v, expected it to stay bounded", elapsed)
	}
}

// TestCloseSessionIsIdempotentUnderConcurrency 覆盖重复与并发关闭。
func TestCloseSessionIsIdempotentUnderConcurrency(t *testing.T) {
	fake := newFakeTerminal(false)
	session := newTerminalSession("concurrent-close", fake)
	registerSession(t, session)

	const callers = 8
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			closeSession("concurrent-close", session)
		}()
	}
	wg.Wait()
	// 关闭后再次关闭同样必须是空操作。
	closeSession("concurrent-close", session)

	writes, _, _, closeCalls := fake.snapshot()
	if closeCalls != 1 {
		t.Fatalf("terminal closed %d times, want exactly 1", closeCalls)
	}
	// 优雅退出序列只应执行一次：3 × Ctrl-C + Ctrl-D + exit。
	if len(writes) != 5 {
		t.Fatalf("graceful shutdown wrote %d times, want 5: %q", len(writes), writes)
	}
	ctrlC := 0
	for _, w := range writes {
		if bytes.Equal(w, []byte{3}) {
			ctrlC++
		}
	}
	if ctrlC != 3 {
		t.Fatalf("wrote %d Ctrl-C sequences, want 3", ctrlC)
	}
	if sessionExists("concurrent-close") {
		t.Fatal("closed session is still registered")
	}
	select {
	case <-session.writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal writer goroutine did not exit")
	}
}

// TestHandleInputDeliversInputResizeAndClose 覆盖正常输入、窗口调整与正常退出。
func TestHandleInputDeliversInputResizeAndClose(t *testing.T) {
	fake := newFakeTerminal(false)
	session := newTerminalSession("normal-flow", fake)
	registerSession(t, session)

	client, peer := newConnPair(t)
	session.mu.Lock()
	session.conn = client
	session.mu.Unlock()

	inputDone := make(chan struct{})
	go func() {
		session.handleInput(client)
		close(inputDone)
	}()

	if err := peer.WriteMessage(websocket.BinaryMessage, []byte("ls -la\n")); err != nil {
		t.Fatalf("write binary input: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		writes, _, _, _ := fake.snapshot()
		return len(writes) >= 1 && bytes.Equal(writes[0], []byte("ls -la\n"))
	}, "binary input to reach the terminal")

	if err := peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"input","input":"echo hi\n"}`)); err != nil {
		t.Fatalf("write text input: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		writes, _, _, _ := fake.snapshot()
		for _, w := range writes {
			if bytes.Equal(w, []byte("echo hi\n")) {
				return true
			}
		}
		return false
	}, "text input to reach the terminal")

	if err := peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		_, _, resizes, _ := fake.snapshot()
		return resizes >= 1
	}, "resize to reach the terminal")

	if err := peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"close"}`)); err != nil {
		t.Fatalf("write close: %v", err)
	}
	select {
	case <-inputDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handleInput did not return after the close command")
	}
	if sessionExists("normal-flow") {
		t.Fatal("closed session is still registered")
	}
	if _, _, _, closeCalls := fake.snapshot(); closeCalls == 0 {
		t.Fatal("terminal was not closed")
	}
	select {
	case <-session.writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal writer goroutine did not exit")
	}
}

// TestHandleInputRecoversWhenTerminalStalls 覆盖端到端背压：写入阻塞时，
// 明确关闭仍然必须生效，并且输入不得被重放。
func TestHandleInputRecoversWhenTerminalStalls(t *testing.T) {
	fake := newFakeTerminal(true)
	session := newTerminalSession("stall-e2e", fake)
	registerSession(t, session)

	client, peer := newConnPair(t)
	session.mu.Lock()
	session.conn = client
	session.mu.Unlock()

	inputDone := make(chan struct{})
	go func() {
		session.handleInput(client)
		close(inputDone)
	}()

	payload := bytes.Repeat([]byte("z"), 1<<20)
	if err := peer.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write stalled input: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		_, calls, _, _ := fake.snapshot()
		return calls >= 1
	}, "the write to reach the terminal")

	// 关闭指令在输入阻塞期间排队；写入超时后必须触发整体回收。
	if err := peer.WriteMessage(websocket.TextMessage, []byte(`{"type":"close"}`)); err != nil {
		t.Fatalf("write close: %v", err)
	}

	start := time.Now()
	select {
	case <-inputDone:
	case <-time.After(inputWriteTimeout + 6*time.Second):
		t.Fatal("handleInput did not return although the terminal is stalled")
	}
	if elapsed := time.Since(start); elapsed > inputWriteTimeout+5*time.Second {
		t.Fatalf("recovery took %v, expected it to be bounded by the write deadline", elapsed)
	}
	if sessionExists("stall-e2e") {
		t.Fatal("stalled session was not removed from the session table")
	}
	if _, _, _, closeCalls := fake.snapshot(); closeCalls == 0 {
		t.Fatal("terminal was not closed")
	}
	select {
	case <-session.writer.done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal writer goroutine did not exit")
	}
	if count := fake.countWrites(payload); count != 1 {
		t.Fatalf("input was written %d times, want exactly 1 (no replay)", count)
	}
}

// TestDetachKeepsSessionAndOldConnectionCannotKillNewOne 覆盖断线保留与
// 重新附着：旧连接退出不得误伤新会话。
func TestDetachKeepsSessionAndOldConnectionCannotKillNewOne(t *testing.T) {
	fake := newFakeTerminal(false)
	session := newTerminalSession("reattach", fake)
	registerSession(t, session)

	oldClient, oldPeer := newConnPair(t)
	newClient, _ := newConnPair(t)

	session.mu.Lock()
	session.conn = oldClient
	session.mu.Unlock()

	session.detach(oldClient)
	session.mu.Lock()
	detached := session.conn == nil
	timerArmed := session.timer != nil
	session.mu.Unlock()
	if !detached {
		t.Fatal("detach did not clear the connection")
	}
	if !timerArmed {
		t.Fatal("detach did not arm the retention timer")
	}
	if !sessionExists("reattach") {
		t.Fatal("detached session must be kept for reattachment")
	}

	reattached, firstConnection, err := acquireSession("reattach", newClient)
	if err != nil {
		t.Fatalf("acquireSession: %v", err)
	}
	if reattached != session {
		t.Fatal("reattach created a different session")
	}
	if firstConnection {
		t.Fatal("reattach must not start a new terminal")
	}
	session.mu.Lock()
	timerStopped := session.timer == nil
	currentConn := session.conn
	session.mu.Unlock()
	if !timerStopped {
		t.Fatal("reattach did not cancel the retention timer")
	}
	if currentConn != newClient {
		t.Fatal("reattach did not install the new connection")
	}

	// 旧连接的读错误不得拆掉新会话。
	_ = oldPeer.Close()
	waitFor(t, 3*time.Second, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.conn == newClient
	}, "the new connection to stay installed")

	if !sessionExists("reattach") {
		t.Fatal("the old connection tore down the reattached session")
	}
	if _, _, _, closeCalls := fake.snapshot(); closeCalls != 0 {
		t.Fatal("the reattached session was closed by the old connection")
	}
}
