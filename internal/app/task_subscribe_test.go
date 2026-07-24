package app

import (
	"testing"
	"time"

	"denova/internal/agent"
)

func TestTaskSubscribeScalesChannelBufferWithReplaySnapshot(t *testing.T) {
	task := &Task{
		status: TaskRunning,
		events: make([]agent.Event, 3000),
	}

	snapshot, ch := task.Subscribe()
	if len(snapshot) != 3000 {
		t.Fatalf("expected replay snapshot length 3000, got %d", len(snapshot))
	}
	if got, wantMin := cap(ch), len(snapshot)+taskSubscribeReplaySlack; got < wantMin {
		t.Fatalf("subscriber channel capacity too small: got %d want >= %d", got, wantMin)
	}

	task.Unsubscribe(ch)
}

func TestTaskSubscribeUsesDefaultBufferForSmallReplay(t *testing.T) {
	task := &Task{
		status: TaskRunning,
		events: make([]agent.Event, 8),
	}

	_, ch := task.Subscribe()
	if got := cap(ch); got != taskSubscriberBuffer {
		t.Fatalf("unexpected subscriber channel capacity for small replay: got %d want %d", got, taskSubscriberBuffer)
	}

	task.Unsubscribe(ch)
}

func TestTaskBatchPreservesCrossTypeEmissionOrder(t *testing.T) {
	task := &Task{status: TaskRunning}
	_, ch := task.Subscribe()

	task.emit(agent.Event{Type: "thinking", Data: map[string]any{"content": "先想"}})
	task.emit(agent.Event{Type: "chunk", Data: map[string]any{"content": "再写"}})
	task.emit(agent.Event{Type: "tool_args_delta", Data: map[string]any{"delta": "{}"}})
	task.flushBatch()

	select {
	case merged := <-ch:
		if merged.Type != "batch" {
			t.Fatalf("expected merged batch event, got %q", merged.Type)
		}
		data, ok := merged.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("unexpected merged data type: %#v", merged.Data)
		}
		events, ok := data["events"].([]agent.Event)
		if !ok {
			t.Fatalf("expected typed agent events payload, got %#v", data["events"])
		}
		if len(events) != 3 {
			t.Fatalf("expected 3 events in batch, got %d", len(events))
		}
		if events[0].Type != "thinking" || events[1].Type != "chunk" || events[2].Type != "tool_args_delta" {
			t.Fatalf("unexpected event order: %#v", []string{events[0].Type, events[1].Type, events[2].Type})
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for merged batch")
	}

	task.Unsubscribe(ch)
}

// TestTaskDisconnectsSlowSubscriber 验证持续无法消费事件的订阅者会被主动断开：
// 当连续丢事件次数达到阈值时，其 channel 被关闭，使 SSE 消费端感知断流并重连。
func TestTaskDisconnectsSlowSubscriber(t *testing.T) {
	task := &Task{status: TaskRunning}
	// 订阅者 channel 缓冲为 taskSubscriberBuffer；需先填满缓冲，再连续丢
	// taskSubscriberMaxConsecutiveDrops 次才会触发断开。关键事件（非
	// thinking/chunk/tool_args_delta）走 sendEvent 单条发送路径。
	total := taskSubscriberBuffer + taskSubscriberMaxConsecutiveDrops + 5
	for i := 0; i < total; i++ {
		task.sendEvent(agent.Event{Type: "status", Data: map[string]any{"i": i}})
	}

	task.mu.Lock()
	subCount := len(task.subs)
	task.mu.Unlock()
	if subCount != 0 {
		t.Fatalf("slow subscriber should have been disconnected, still %d subscriber(s)", subCount)
	}
}

// TestTaskKeepsHealthySubscriber 验证正常消费的订阅者不会被误断开：
// 只要订阅者持续读取事件，连续丢事件计数会被归零，不会触发断开。
func TestTaskKeepsHealthySubscriber(t *testing.T) {
	task := &Task{status: TaskRunning}
	_, ch := task.Subscribe()

	for i := 0; i < taskSubscriberMaxConsecutiveDrops*2; i++ {
		task.sendEvent(agent.Event{Type: "status", Data: map[string]any{"i": i}})
		// 立即消费，模拟健康订阅者。
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event on healthy subscriber")
		}
	}

	task.mu.Lock()
	subCount := len(task.subs)
	task.mu.Unlock()
	if subCount != 1 {
		t.Fatalf("healthy subscriber should remain connected, got %d subscriber(s)", subCount)
	}

	task.Unsubscribe(ch)
}
