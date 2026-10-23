package terminal

import (
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

// errWriterStopped 表示写入者已随会话回收而停止。
var errWriterStopped = errors.New("terminal writer stopped")

// writeResult 是一次写入的结果。
type writeResult struct {
	n   int
	err error
}

// writeRequest 是一次待执行的写入请求。
type writeRequest struct {
	data     []byte
	deadline time.Time
	// result 容量为 1：写者投递结果绝不会阻塞，即使请求方已经超时离开。
	result chan writeResult
}

// ptyWriter 是会话内唯一的 PTY 写入者。
//
// 设计要点：
//   - 串行化所有写入，保证终端输入不会交错破坏（单写者约束）。
//   - 队列容量固定为 1，不存在无界输入积压。
//   - 每次写入都有硬超时；超时意味着写入结果未知（数据可能已部分进入
//     PTY），调用方必须当作失败处理且绝不重放。
//   - 写者自身可能阻塞在底层 Write（例如前台进程停止消费输入，PTY 缓冲区
//     写满）。此时唯一可移植的解除手段是 Terminal.Close 释放 PTY 主描述符，
//     因此 stopAndWait 必须在 term.Close 之后调用，且等待必须有界。
type ptyWriter struct {
	term     Terminal
	requests chan writeRequest
	stop     chan struct{}
	done     chan struct{}

	stopOnce sync.Once
}

// newPtyWriter 创建写入者并启动其唯一的写入协程。
func newPtyWriter(term Terminal) *ptyWriter {
	writer := &ptyWriter{
		term:     term,
		requests: make(chan writeRequest, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go writer.run()
	return writer
}

// write 提交一次写入并等待其完成，总耗时不超过 timeout。
//
// 返回错误时写入结果未知：数据可能已部分送达。调用方不得重放该数据，
// 否则会造成命令重复执行。
func (w *ptyWriter) write(data []byte, timeout time.Duration) (int, error) {
	if timeout <= 0 {
		return 0, fmt.Errorf("terminal write timeout must be positive")
	}
	deadline := time.Now().Add(timeout)
	request := writeRequest{
		data:     data,
		deadline: deadline,
		result:   make(chan writeResult, 1),
	}

	// 入队阶段：写者可能仍卡在上一次写入中，队列容量为 1 保证不会积压。
	select {
	case w.requests <- request:
	case <-w.stop:
		return 0, errWriterStopped
	case <-time.After(time.Until(deadline)):
		return 0, fmt.Errorf("terminal write stalled for %v (writer busy)", timeout)
	}

	// 完成阶段：等待写者返回结果。
	select {
	case result := <-request.result:
		return result.n, result.err
	case <-w.stop:
		// 请求可能已经进入底层写入，结果未知，不得重放。
		return 0, errWriterStopped
	case <-time.After(time.Until(deadline)):
		// 底层 Write 仍阻塞（前台进程不消费输入）。结果未知，不得重放。
		return 0, fmt.Errorf("terminal write did not complete within %v", timeout)
	}
}

// run 是写入者的唯一执行协程：串行执行请求并投递结果。
func (w *ptyWriter) run() {
	defer close(w.done)

	// 平台支持时使用文件级写截止时间，让阻塞中的 Write 自行返回；
	// 不支持或设置失败时，完全依赖 Terminal.Close 解除阻塞。
	deadlineSetter, hasDeadline := w.term.(interface {
		SetWriteDeadline(time.Time) error
	})

	for {
		select {
		case request := <-w.requests:
			if hasDeadline {
				// 尽力而为：失败不影响正确性，只是失去提前解阻的能力。
				_ = deadlineSetter.SetWriteDeadline(request.deadline)
			}
			n, err := w.term.Write(request.data)
			if err == nil && n != len(request.data) {
				// 短写意味着输入被截断，结果不可知：按失败处理且不重放。
				err = io.ErrShortWrite
			}
			select {
			case request.result <- writeResult{n: n, err: err}:
			default:
				// 请求方已超时返回：结果丢弃，且绝不重放这次写入。
			}
		case <-w.stop:
			w.failPending()
			return
		}
	}
}

// failPending 唤醒仍在等待结果的请求方，避免它们空等到自身超时。
func (w *ptyWriter) failPending() {
	for {
		select {
		case request := <-w.requests:
			select {
			case request.result <- writeResult{err: errWriterStopped}:
			default:
			}
		default:
			return
		}
	}
}

// stopAndWait 停止写入者并等待其退出。
//
// 必须在 Terminal.Close（释放 PTY 主描述符）之后调用：那是解除阻塞中
// 底层 Write 的唯一可移植手段。等待有界，超时只记录日志。
func (w *ptyWriter) stopAndWait(timeout time.Duration) {
	w.stopOnce.Do(func() {
		close(w.stop)
	})
	select {
	case <-w.done:
	case <-time.After(timeout):
		log.Printf("terminal writer did not exit within %v", timeout)
	}
}
