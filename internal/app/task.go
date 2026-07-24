package app

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"denova/internal/agent"
	"denova/internal/observability"
)

// TaskStatus 表示后台任务的执行状态。
type TaskStatus string

const (
	TaskRunning TaskStatus = "running"
	TaskDone    TaskStatus = "done"
	TaskAborted TaskStatus = "aborted"
	TaskError   TaskStatus = "error"
)

var taskSeq atomic.Uint64

// Task 表示一个后台运行的 Agent 任务，独立于 HTTP 连接生命周期。
// 事件缓冲到内存，SSE 客户端作为订阅者消费事件。
// 高频事件（thinking、tool_args_delta）会批量合并，减少通道发送次数和锁竞争。
type Task struct {
	id        string
	startedAt time.Time
	mu        sync.Mutex
	status    TaskStatus
	finished  bool
	events    []agent.Event
	subs      []*subscriber
	cancel    context.CancelFunc

	// 批量节流：高频事件合并后按类型分间隔发送
	batchMu      sync.Mutex
	batchPending []agent.Event // thinking / tool_args_delta / chunk
	batchTimer   *time.Timer
}

// subscriber 包装一个订阅者 channel，并跟踪背压丢事件情况。
// 当连续发送失败超过阈值时主动断开该订阅者，让 SSE 消费端感知断流并触发
// 重连回放（snapshot 含完整历史），而非静默持续丢弃导致前端状态长期落后。
type subscriber struct {
	ch chan agent.Event
	// consecutiveDrops 记录连续发送失败的次数；任意一次成功发送即归零。
	// 使用原子操作，因为 sendEvent/sendBatch 可能从 run goroutine 与
	// time.AfterFunc 回调 goroutine 并发调用。
	consecutiveDrops atomic.Int64
}

const (
	taskSubscriberBuffer     = 1024                  // channel 缓冲大小（原 256）
	taskBatchInterval        = 30 * time.Millisecond // 批量事件统一合并间隔（顺序优先）
	taskBatchSize            = 200                   // 达到此数量立即 flush
	taskSubscribeReplaySlack = 256                   // 回放期间给实时事件预留的额外缓冲
	// taskSubscriberMaxConsecutiveDrops 是主动断开慢订阅者的背压阈值。
	// 批量事件约每 30ms 发送一次，连续 50 次失败约等于 1.5s 持续无法消费，
	// 足以区分“短暂卡顿（会自愈）”与“真正掉线/标签页冻结（应断开重连）”。
	taskSubscriberMaxConsecutiveDrops = 50
)

// NewTask 创建并启动后台任务。run 函数在独立 goroutine 中执行。
func NewTask(run func(ctx context.Context, task *Task, emit func(agent.Event))) *Task {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		id:        strconv.FormatUint(taskSeq.Add(1), 10),
		startedAt: time.Now(),
		status:    TaskRunning,
		cancel:    cancel,
	}
	observability.Info("agent-task", "task_start", slog.String("task_id", t.id))
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				observability.Error("agent-task", "task_panic_recovered", slog.String("task_id", t.id), slog.Any("error", recovered))
				t.emit(agent.Event{Type: "error", Data: map[string]string{"message": "Agent 后台任务异常中断"}})
			}
			t.flushBatch()
			t.finish()
		}()
		run(ctx, t, t.emit)
	}()
	return t
}

// emit 缓冲事件并广播给所有订阅者。
// 高频事件统一进入单一队列批量发送，保证跨类型（thinking/chunk/tool_args_delta）顺序稳定。
func (t *Task) emit(ev agent.Event) {
	// 关键事件立即发送
	if !shouldBatchEvent(ev.Type) {
		t.sendEvent(ev)
		return
	}

	t.batchMu.Lock()
	t.batchPending = append(t.batchPending, ev)
	if len(t.batchPending) >= taskBatchSize {
		batch := t.batchPending
		t.batchPending = nil
		if t.batchTimer != nil {
			t.batchTimer.Stop()
			t.batchTimer = nil
		}
		t.batchMu.Unlock()
		t.sendBatch(batch)
		return
	}
	if t.batchTimer == nil {
		t.batchTimer = time.AfterFunc(taskBatchInterval, func() {
			t.batchMu.Lock()
			batch := t.batchPending
			t.batchPending = nil
			t.batchTimer = nil
			t.batchMu.Unlock()
			if len(batch) > 0 {
				t.sendBatch(batch)
			}
		})
	}
	t.batchMu.Unlock()
}

// flushBatch 发送所有积压的批量事件（任务结束时调用）。
func (t *Task) flushBatch() {
	t.batchMu.Lock()
	if t.batchTimer != nil {
		t.batchTimer.Stop()
		t.batchTimer = nil
	}
	batch := t.batchPending
	t.batchPending = nil
	t.batchMu.Unlock()
	if len(batch) > 0 {
		t.sendBatch(batch)
	}
}

// shouldBatchEvent 判断事件类型是否应该合并。
func shouldBatchEvent(eventType string) bool {
	switch eventType {
	case "thinking", "tool_args_delta", "chunk":
		return true
	default:
		return false
	}
}

// sendEvent 立即发送单个事件给所有订阅者。
func (t *Task) sendEvent(ev agent.Event) {
	t.mu.Lock()
	// 任务已结束，channel 已关闭或即将关闭，不再发送
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.events = append(t.events, ev)
	if ev.Type == "error" {
		t.status = TaskError
	}
	if ev.Type == "aborted" {
		t.status = TaskAborted
	}
	subs := make([]*subscriber, len(t.subs))
	copy(subs, t.subs)
	eventCount := len(t.events)
	subCount := len(t.subs)
	t.mu.Unlock()
	if shouldLogEvent(ev.Type, eventCount) {
		observability.Info("agent-task", "task_event", slog.String("task_id", t.id), slog.String("event_type", ev.Type), slog.Int("events", eventCount), slog.Int("subscribers", subCount))
	}
	var slow []*subscriber
	for _, sub := range subs {
		select {
		case sub.ch <- ev:
			sub.consecutiveDrops.Store(0)
		default:
			observability.Warn("agent-task", "task_event_dropped", slog.String("task_id", t.id), slog.String("event_type", ev.Type), slog.String("reason", "subscriber_slow"))
			if sub.consecutiveDrops.Add(1) >= taskSubscriberMaxConsecutiveDrops {
				slow = append(slow, sub)
			}
		}
	}
	t.disconnectSlowSubscribers(slow)
}

// sendBatch 批量发送合并事件。使用单个合并事件包装，大幅减少通道发送次数。
func (t *Task) sendBatch(batch []agent.Event) {
	if len(batch) == 0 {
		return
	}
	t.mu.Lock()
	// 任务已结束，channel 已关闭或即将关闭，不再发送
	if t.finished {
		t.mu.Unlock()
		return
	}
	// 将批量事件追加到历史，但不逐个新增事件计数
	t.events = append(t.events, batch...)
	subs := make([]*subscriber, len(t.subs))
	copy(subs, t.subs)
	subCount := len(t.subs)
	t.mu.Unlock()
	if subCount == 0 {
		return
	}
	// 单个合并事件发送给订阅者
	merged := agent.Event{
		Type: "batch",
		Data: map[string]interface{}{
			"events": batch,
			"count":  len(batch),
			"kinds":  batchKinds(batch),
		},
	}
	var slow []*subscriber
	for _, sub := range subs {
		select {
		case sub.ch <- merged:
			sub.consecutiveDrops.Store(0)
		default:
			for _, ev := range batch {
				// 合并发送失败时，尽力逐条发送批量中的每个事件
				if !shouldBatchEvent(ev.Type) {
					continue
				}
				select {
				case sub.ch <- ev:
				default:
				}
			}
			observability.Warn("agent-task", "task_batch_dropped", slog.String("task_id", t.id), slog.Int("dropped", len(batch)), slog.String("reason", "subscriber_slow"))
			if sub.consecutiveDrops.Add(1) >= taskSubscriberMaxConsecutiveDrops {
				slow = append(slow, sub)
			}
		}
	}
	t.disconnectSlowSubscribers(slow)
}

// disconnectSlowSubscribers 主动断开持续无法消费事件的订阅者。
// 关闭其 channel 使 SSE 消费端的 range 循环退出，从而触发客户端重连；
// 重连时 Subscribe 会回放完整 snapshot，补齐被丢弃的高频事件。
func (t *Task) disconnectSlowSubscribers(slow []*subscriber) {
	if len(slow) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	slowSet := make(map[*subscriber]bool, len(slow))
	for _, sub := range slow {
		slowSet[sub] = true
	}
	remaining := t.subs[:0]
	for _, sub := range t.subs {
		if slowSet[sub] {
			close(sub.ch)
			observability.Warn("agent-task", "task_subscriber_disconnected", slog.String("task_id", t.id), slog.Int("threshold", taskSubscriberMaxConsecutiveDrops), slog.String("reason", "subscriber_slow"))
			continue
		}
		remaining = append(remaining, sub)
	}
	t.subs = remaining
}

// batchKinds 返回批量中不同事件类型的计数摘要。
func batchKinds(batch []agent.Event) map[string]int {
	kinds := make(map[string]int, 2)
	for _, ev := range batch {
		kinds[ev.Type]++
	}
	return kinds
}

// finish 标记任务完成，关闭所有订阅者 channel。
func (t *Task) finish() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status == TaskRunning {
		t.status = TaskDone
	}
	t.finished = true
	for _, sub := range t.subs {
		close(sub.ch)
	}
	t.subs = nil
	observability.Info("agent-task", "task_finish", slog.String("task_id", t.id), slog.String("status", string(t.status)), slog.Int("events", len(t.events)), slog.Duration("duration", time.Since(t.startedAt).Round(time.Millisecond)))
}

// Subscribe 返回已有事件的快照和一个用于接收后续事件的 channel。
// 如果任务已结束，channel 立即关闭。
func (t *Task) Subscribe() ([]agent.Event, <-chan agent.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()

	snapshot := make([]agent.Event, len(t.events))
	copy(snapshot, t.events)

	if t.status != TaskRunning {
		ch := make(chan agent.Event)
		close(ch)
		observability.Info("agent-task", "task_subscribe", slog.String("task_id", t.id), slog.String("status", string(t.status)), slog.Int("replay", len(snapshot)), slog.Bool("live", false))
		return snapshot, ch
	}

	bufferSize := taskSubscriberBuffer
	if replayBuffer := len(snapshot) + taskSubscribeReplaySlack; replayBuffer > bufferSize {
		bufferSize = replayBuffer
	}
	ch := make(chan agent.Event, bufferSize)
	t.subs = append(t.subs, &subscriber{ch: ch})
	observability.Info("agent-task", "task_subscribe", slog.String("task_id", t.id), slog.String("status", string(t.status)), slog.Int("replay", len(snapshot)), slog.Int("subscribers", len(t.subs)), slog.Bool("live", true))
	return snapshot, ch
}

// Unsubscribe 移除订阅者。
func (t *Task) Unsubscribe(ch <-chan agent.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, sub := range t.subs {
		if sub.ch == ch {
			t.subs = append(t.subs[:i], t.subs[i+1:]...)
			observability.Info("agent-task", "task_unsubscribe", slog.String("task_id", t.id), slog.Int("subscribers", len(t.subs)))
			return
		}
	}
}

// Abort 取消任务执行。
func (t *Task) Abort() {
	t.mu.Lock()
	t.status = TaskAborted
	t.mu.Unlock()
	observability.Warn("agent-task", "task_abort", slog.String("task_id", t.id))
	t.cancel()
}

// Status 返回当前状态。
func (t *Task) Status() TaskStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

// Finished reports whether the task goroutine has fully exited.
func (t *Task) Finished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished
}

// ID 返回任务编号，用于关联后端日志。
func (t *Task) ID() string {
	return t.id
}

func shouldLogEvent(eventType string, eventCount int) bool {
	switch eventType {
	case "chunk", "thinking", "tool_args_delta":
		return eventCount == 1 || eventCount%100 == 0
	default:
		return true
	}
}
