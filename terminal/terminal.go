package terminal

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"

	pkg_flags "github.com/komari-monitor/komari-agent/cmd/flags"
)

var flags = pkg_flags.GlobalConfig

// 与 Server 端会话保留窗口对齐，网络恢复后同一会话可重新附着。
const disconnectedRetention = 5 * time.Minute

// closeGracePeriod 是等待终端进程退出的上限，超时后强制结束。
const closeGracePeriod = 5 * time.Second

// inputWriteTimeout 是单次终端输入写入的硬上限。
//
// 前台进程停止消费输入时（例如 sleep、卡死的交互程序），PTY 输入缓冲区
// 写满后 write(2) 会一直阻塞。写入超时说明会话已经无法可靠地继续使用，
// 必须明确终止会话并回收进程与资源，而不是无限期等待。
const inputWriteTimeout = 3 * time.Second

// gracefulShutdownBudget 约束整个优雅退出阶段（Ctrl-C / Ctrl-D / exit）的
// 总时长；gracefulWriteTimeout 约束其中单次控制字符写入。
//
// 优雅退出只是"尽力而为"：一旦 PTY 不可写（例如前台进程停止消费输入），
// 立即放弃并转入强制关闭路径（进程组终止 + PTY 释放）。
const (
	gracefulShutdownBudget = 1500 * time.Millisecond
	gracefulWriteTimeout   = 250 * time.Millisecond
)

// Terminal 接口定义平台特定的终端操作
type Terminal interface {
	Close() error
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	Wait() error
}

// terminalImpl 封装终端和平台特定逻辑
type terminalImpl struct {
	shell      string
	workingDir string
	term       Terminal
}

type terminalSession struct {
	id     string
	term   Terminal
	writer *ptyWriter

	mu      sync.Mutex
	writeMu sync.Mutex
	conn    *websocket.Conn
	timer   *time.Timer
	removed bool

	teardownOnce sync.Once
}

// newTerminalSession 创建一个会话并启动其唯一的 PTY 写入者。
func newTerminalSession(id string, term Terminal) *terminalSession {
	return &terminalSession{
		id:     id,
		term:   term,
		writer: newPtyWriter(term),
	}
}

var (
	sessionsMu sync.Mutex
	sessions   = make(map[string]*terminalSession)
)

// StartTerminal keeps a PTY alive briefly after its WebSocket drops. A new
// connection with the same request ID reattaches the existing shell.
func StartTerminal(conn *websocket.Conn, requestID string) bool {
	if flags.DisableWebSsh {
		notifyTerminate(conn)
		_ = conn.Close()
		return false
	}
	if requestID == "" {
		notifyTerminate(conn)
		_ = conn.Close()
		return false
	}

	session, firstConnection, err := acquireSession(requestID, conn)
	if err != nil {
		notifyTerminate(conn)
		_ = conn.Close()
		return false
	}
	if firstConnection {
		go session.readOutput()
	}

	session.handleInput(conn)
	_ = conn.Close()
	session.mu.Lock()
	retry := !session.removed
	session.mu.Unlock()
	return retry
}

func acquireSession(requestID string, conn *websocket.Conn) (*terminalSession, bool, error) {
	sessionsMu.Lock()
	var orphan *terminalSession
	session := sessions[requestID]
	firstConnection := false
	if session == nil {
		sessionsMu.Unlock()

		impl, err := newTerminalImpl()
		if err != nil {
			return nil, false, err
		}
		candidate := newTerminalSession(requestID, impl.term)

		sessionsMu.Lock()
		if sessions[requestID] != nil {
			// 竞态：另一个连接已经创建了同 ID 的会话，候选会话立即回收。
			session = sessions[requestID]
			orphan = candidate
		} else {
			sessions[requestID] = candidate
			session = candidate
			firstConnection = true
		}
	}

	session.mu.Lock()
	if session.timer != nil {
		session.timer.Stop()
		session.timer = nil
	}
	oldConn := session.conn
	session.conn = conn
	session.mu.Unlock()
	sessionsMu.Unlock()

	if orphan != nil {
		go orphan.teardown()
	}

	if oldConn != nil && oldConn != conn {
		_ = oldConn.Close()
	}
	return session, firstConnection, nil
}

func (s *terminalSession) handleInput(conn *websocket.Conn) {
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			s.detach(conn)
			return
		}

		if messageType == websocket.BinaryMessage {
			s.writeInput(data)
			continue
		}

		cmd, ok := parseControl(data)
		if !ok {
			s.writeInput(data)
			continue
		}
		switch cmd.Type {
		case controlTypeResize:
			if cmd.Cols > 0 && cmd.Rows > 0 {
				_ = s.term.Resize(cmd.Cols, cmd.Rows)
			}
		case "input":
			if cmd.Input != "" {
				s.writeInput([]byte(cmd.Input))
			}
		case controlTypeTerminate, "close":
			// 服务端要求结束会话：这是终态，不重连。
			closeSession(s.id, s)
			return
		case controlTypeAbort:
			// 浏览器一侧掉线：PTY 保留，等服务端重连后重新附着。
			s.detach(conn)
			return
		case controlTypeHeartbeat:
			if cmd.ID != "" {
				_ = s.writeJSON(conn, controlMessage{Type: controlTypeHeartbeat, ID: cmd.ID})
			}
		}
		// 未知控制消息按前向兼容忽略，绝不写进 PTY。
	}
}

// writeInput 将终端输入交给会话唯一的 PTY 写入者，并施加硬超时。
//
// 写入失败或超时时说明前台进程已经停止消费输入，PTY 写入处于阻塞状态，
// 会话不再可靠：立即整体回收（终止进程组、释放 PTY、回收写入者）。
// 结果未知的输入绝不重放，避免命令被重复执行。
func (s *terminalSession) writeInput(data []byte) {
	if len(data) == 0 {
		return
	}
	if _, err := s.writer.write(data, inputWriteTimeout); err != nil {
		closeSession(s.id, s)
	}
}

func (s *terminalSession) readOutput() {
	buf := make([]byte, 4096)
	for {
		n, err := s.term.Read(buf)
		if err != nil {
			// PTY 结束（用户 exit、进程被杀等）：这是正常终止，明确告诉
			// 服务端不要等重连，而不是留下一根静默断掉的连接。
			s.mu.Lock()
			conn := s.conn
			s.mu.Unlock()
			if conn != nil {
				_ = s.writeJSON(conn, controlMessage{Type: controlTypeTerminate})
			}
			closeSession(s.id, s)
			return
		}

		s.mu.Lock()
		conn := s.conn
		s.mu.Unlock()
		if conn == nil {
			continue
		}
		if err := s.writeMessage(conn, websocket.BinaryMessage, buf[:n]); err != nil {
			s.detach(conn)
			continue
		}
	}
}

func (s *terminalSession) writeMessage(conn *websocket.Conn, messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return conn.WriteMessage(messageType, data)
}

func (s *terminalSession) writeJSON(conn *websocket.Conn, v interface{}) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return conn.WriteJSON(v)
}

// detach 保留 PTY，等待服务端用同一个 request_id 重连回来。
// 超过保留窗口仍未重连则清理。
func (s *terminalSession) detach(conn *websocket.Conn) {
	s.mu.Lock()
	if s.removed || s.conn != conn {
		s.mu.Unlock()
		return
	}
	s.conn = nil
	if s.timer == nil {
		s.timer = time.AfterFunc(disconnectedRetention, func() {
			closeSession(s.id, s)
		})
	}
	s.mu.Unlock()
	_ = conn.Close()
}

func closeSession(requestID string, expected *terminalSession) {
	sessionsMu.Lock()
	session := sessions[requestID]
	if session == nil || session != expected {
		sessionsMu.Unlock()
		return
	}
	delete(sessions, requestID)
	sessionsMu.Unlock()

	session.mu.Lock()
	if session.removed {
		session.mu.Unlock()
		return
	}
	session.removed = true
	if session.timer != nil {
		session.timer.Stop()
		session.timer = nil
	}
	conn := session.conn
	session.conn = nil
	session.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	session.teardown()
}

// teardown 是会话的最终回收路径，顺序固定：
//
//  1. 有界的优雅退出（Ctrl-C / Ctrl-D / exit），总时长受预算约束；
//  2. 强制关闭终端：终止进程组、必要时 SIGKILL，并主动释放 PTY 主描述符
//     —— 释放 PTY 同时解除写入者可能阻塞中的底层 Write；
//  3. 停止并等待写入者退出，保证不遗留后台写入协程。
//
// 三个步骤都有时间上限，因此即使 PTY 完全写不动，关闭也能在有限时间内完成。
func (s *terminalSession) teardown() {
	s.teardownOnce.Do(func() {
		gracefulShutdown(s)
		_ = s.term.Close()
		s.writer.stopAndWait(closeGracePeriod)
	})
}

// gracefulShutdown 尝试让 shell 自行退出（Ctrl-C ×3、Ctrl-D、exit）。
//
// 每次写入和整个阶段都有时间上限：PTY 不可写（例如前台进程停止消费输入，
// 写入一直阻塞）时立即放弃，转入 teardown 的强制关闭路径。
func gracefulShutdown(s *terminalSession) {
	deadline := time.Now().Add(gracefulShutdownBudget)

	writeControl := func(data []byte) bool {
		timeout := time.Until(deadline)
		if timeout <= 0 {
			return false
		}
		if timeout > gracefulWriteTimeout {
			timeout = gracefulWriteTimeout
		}
		_, err := s.writer.write(data, timeout)
		return err == nil
	}

	sleepBounded := func(d time.Duration) bool {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		if d > remaining {
			d = remaining
		}
		time.Sleep(d)
		return true
	}

	for i := 0; i < 3; i++ {
		if !writeControl([]byte{3}) {
			return
		}
		if !sleepBounded(50 * time.Millisecond) {
			return
		}
	}
	if !sleepBounded(200 * time.Millisecond) {
		return
	}
	_ = writeControl([]byte{4})
	if !sleepBounded(100 * time.Millisecond) {
		return
	}
	_ = writeControl([]byte("exit\n"))
	sleepBounded(100 * time.Millisecond)
}
