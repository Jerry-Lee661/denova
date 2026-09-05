package session

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestLinearChainActiveBacktracks verifies that in a default linear chain,
// ActiveChain returns all messages from root to tip and GetEffectiveMessages
// respects the clear boundary.
func TestLinearChainActiveBacktracks(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("m0")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("m1", nil)); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("m2")); err != nil {
		t.Fatal(err)
	}

	chain := sess.ActiveChain()
	if len(chain) != 3 {
		t.Fatalf("线性链应包含全部 3 条消息，实际: %d", len(chain))
	}
	// 根到末端顺序：索引 0,1,2。
	if chain[0] != 0 || chain[1] != 1 || chain[2] != 2 {
		t.Fatalf("线性链顺序应为 [0 1 2]，实际: %v", chain)
	}

	effective := sess.GetEffectiveMessages()
	if len(effective) != 3 {
		t.Fatalf("无 clear 标记时应返回全部 3 条有效消息，实际: %d", len(effective))
	}
}

// TestBoundaryProjection verifies that SetBoundary limits the effective context
// to messages after the boundary.
func TestBoundaryProjection(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := sess.Append(schema.UserMessage("msg")); err != nil {
			t.Fatal(err)
		}
	}

	sess.SetBoundary(3)
	effective := sess.GetEffectiveMessages()
	if len(effective) != 2 {
		t.Fatalf("边界为 3 时应返回 2 条有效消息，实际: %d", len(effective))
	}
	if sess.BoundaryIndex() != 3 {
		t.Fatalf("边界应为 3，实际: %d", sess.BoundaryIndex())
	}
}

// TestForkBranchActiveChain verifies that forking creates a new active branch
// and ActiveChain follows only the active branch.
func TestForkBranchActiveChain(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	// 追加 3 条消息：索引 0,1,2。
	for i := 0; i < 3; i++ {
		if err := sess.Append(schema.UserMessage("base")); err != nil {
			t.Fatal(err)
		}
	}

	// 从索引 1 分叉出分支 "b"，追加一条消息（索引 3）。
	if err := sess.Fork("b", "1"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("forked", nil)); err != nil {
		t.Fatal(err)
	}

	chain := sess.ActiveChain()
	// 活动分支应为 [0,1,3]：从根到分叉点 1 再到 forked 消息 3。
	if len(chain) != 3 {
		t.Fatalf("活动分支应包含 3 条消息 [0,1,3]，实际: %d (%v)", len(chain), chain)
	}
	if chain[0] != 0 || chain[1] != 1 || chain[2] != 3 {
		t.Fatalf("活动分支应为 [0 1 3]，实际: %v", chain)
	}

	// 活动分支末端为索引 3，GetEffectiveMessages 应返回 3 条（链上全部）。
	effective := sess.GetEffectiveMessages()
	if len(effective) != 3 {
		t.Fatalf("活动分支末端为 3，边界为 0 时应返回 3 条，实际: %d", len(effective))
	}
}

// TestChainLinkPersistence verifies that prevIdx/branchID survive a reload.
func TestChainLinkPersistence(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := sess.Append(schema.UserMessage("m")); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Fork("b", "1"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("forked", nil)); err != nil {
		t.Fatal(err)
	}

	reloaded, err := store.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	chain := reloaded.ActiveChain()
	if len(chain) != 3 || chain[0] != 0 || chain[1] != 1 || chain[2] != 3 {
		t.Fatalf("重载后活动分支应为 [0 1 3]，实际: %v", chain)
	}
}
