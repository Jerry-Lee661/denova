package session

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

// appendN 追加 n 条 user 消息，内容带序号便于断言。
func appendN(t *testing.T, sess *Session, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := sess.Append(schema.UserMessage("msg")); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTruncateHidesRangeAndKeepsNewMessagesVisible 验证截断后新增消息（索引 >= hiddenEnd）
// 在 History 与 GetEffectiveMessages 中继续可见。
func TestTruncateHidesRangeAndKeepsNewMessagesVisible(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, sess, 4) // 索引 0..3

	// 截断到索引 1（含）：隐藏区间 [2, 4)。
	if err := sess.AppendTruncate(1, "retry"); err != nil {
		t.Fatal(err)
	}
	if got := sess.TruncateIndex(); got != 1 {
		t.Fatalf("TruncateIndex = %d, want 1", got)
	}

	// 截断后新增消息（索引 4、5）必须可见。
	appendN(t, sess, 2)

	history := sess.History()
	// 可见消息：索引 0、1、4、5 → 4 条 message 条目。
	visible := 0
	for _, entry := range history {
		if entry.Type == historyTypeMessage {
			visible++
		}
	}
	if visible != 4 {
		t.Fatalf("截断后新增消息应可见，History 消息条目 = %d, want 4: %#v", visible, history)
	}

	effective := sess.GetEffectiveMessages()
	if len(effective) != 4 {
		t.Fatalf("有效上下文应包含截断前保留 + 截断后新增 = 4 条，实际: %d", len(effective))
	}
}

// TestTruncateWithClearMarker 验证 clear 分界与隐藏区间叠加：
// 隐藏区间内的 clear 不输出，区间后的 clear 正常输出。
func TestTruncateWithClearMarker(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, sess, 4) // 索引 0..3

	// 截断到索引 1：隐藏区间 [2, 4)。
	if err := sess.AppendTruncate(1, "retry"); err != nil {
		t.Fatal(err)
	}
	// 隐藏区间内追加 clear（此时 messageCount=4，clear 落在隐藏段之后记录流中，
	// 但其语义属于被截断段）：clear 记录不递增 messageCount，
	// 后续新增消息 messageCount=4 >= hiddenEnd=4 → inHidden 复位。
	if err := sess.AppendClearMarker(); err != nil {
		t.Fatal(err)
	}
	appendN(t, sess, 1) // 索引 4

	history := sess.History()
	clearCount := 0
	for _, entry := range history {
		if entry.Type == historyTypeClear {
			clearCount++
		}
	}
	// clear 记录位于隐藏区间消息之后（messageCount 已越过 hiddenEnd），应输出 1 条分界。
	if clearCount != 1 {
		t.Fatalf("隐藏区间后的 clear 分界应输出 1 条，实际: %d: %#v", clearCount, history)
	}
}

// TestTruncateClearInsideHiddenRange 验证 clear 分界落在隐藏区间内时不输出：
// 先 clear（分界在索引 2 处），再截断到索引 1 使 clear 所在段进入隐藏区间。
func TestTruncateClearInsideHiddenRange(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, sess, 2) // 索引 0..1
	if err := sess.AppendClearMarker(); err != nil {
		t.Fatal(err)
	}
	appendN(t, sess, 2) // 索引 2..3

	// 截断到索引 1：隐藏区间 [2, 4)，clear 分界（messageCount=2 处）落入隐藏区间。
	if err := sess.AppendTruncate(1, "retry"); err != nil {
		t.Fatal(err)
	}

	history := sess.History()
	for _, entry := range history {
		if entry.Type == historyTypeClear {
			t.Fatalf("隐藏区间内的 clear 分界不应输出: %#v", history)
		}
	}
}

// TestTruncateWithCompactionBoundary 验证截断 + 压缩边界共存：
// 有效投影 = 边界之后 ∩ 非隐藏区间。
func TestTruncateWithCompactionBoundary(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, sess, 8) // 索引 0..7

	// 截断到索引 3：隐藏区间 [4, 8)。
	if err := sess.AppendTruncate(3, "retry"); err != nil {
		t.Fatal(err)
	}
	// 压缩边界推进到 2：投影起点 max(boundary=2, clear=0) = 2。
	sess.SetBoundary(2)

	effective := sess.GetEffectiveMessages()
	// 索引 0、1 < 边界 2 → 跳过；索引 2、3 >= 边界且不在隐藏区间 → 可见；
	// 索引 4..7 在隐藏区间 [4,8) → 跳过。
	if len(effective) != 2 {
		t.Fatalf("截断+压缩边界共存时应返回 2 条有效消息，实际: %d", len(effective))
	}
}

// TestTruncatePersistsAcrossReload 验证截断状态经 journal 顺序重放后保持：
// 重载后隐藏区间终点 = 截断时刻的消息数，之后新增消息可见。
func TestTruncatePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, sess, 4) // 索引 0..3
	if err := sess.AppendTruncate(1, "retry"); err != nil {
		t.Fatal(err)
	}
	appendN(t, sess, 2) // 索引 4、5（截断后新增）

	reloadedStore, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := reloadedStore.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.TruncateIndex(); got != 1 {
		t.Fatalf("重载后 TruncateIndex = %d, want 1", got)
	}

	history := reloaded.History()
	visible := 0
	for _, entry := range history {
		if entry.Type == historyTypeMessage {
			visible++
		}
	}
	if visible != 4 {
		t.Fatalf("重载后截断保持：可见消息 = %d, want 4: %#v", visible, history)
	}

	effective := reloaded.GetEffectiveMessages()
	if len(effective) != 4 {
		t.Fatalf("重载后有效上下文 = %d, want 4", len(effective))
	}
}

// TestTruncateHidesDisplayEvents 验证隐藏区间内的展示记录（thinking/工具卡片）一并跳过，
// 区间后的展示记录正常输出。
func TestTruncateHidesDisplayEvents(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("q0")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendDisplayEvent(DisplayEvent{Role: "thinking", Content: "隐藏段思考"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("a1", nil)); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("q2")); err != nil {
		t.Fatal(err)
	}

	// 截断到索引 0：隐藏区间 [1, 3)，thinking 展示记录落在隐藏段内。
	if err := sess.AppendTruncate(0, "retry"); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendDisplayEvent(DisplayEvent{Role: "thinking", Content: "新区间思考"}); err != nil {
		t.Fatal(err)
	}

	history := sess.History()
	thinkings := 0
	for _, entry := range history {
		if entry.Role == "thinking" {
			thinkings++
			if entry.Content == "隐藏段思考" {
				t.Fatalf("隐藏区间内的展示记录不应输出: %#v", entry)
			}
		}
	}
	if thinkings != 1 {
		t.Fatalf("区间后的展示记录应输出 1 条，实际: %d: %#v", thinkings, history)
	}
}
