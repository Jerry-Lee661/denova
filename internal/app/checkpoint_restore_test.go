package app

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"

	"denova/config"
	"denova/internal/book"
	"denova/internal/session"
)

// newCheckpointTestApp 创建一个用于检查点还原测试的 App：绑定 workspace、
// 会话 store/当前会话与工作区版本服务。
func newCheckpointTestApp(workspace string) *App {
	store, err := session.NewStore(workspace + "/sessions")
	if err != nil {
		return nil
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		return nil
	}
	return &App{
		cfg:            &config.Config{VersionTimedEnabled: false},
		workspace:      workspace,
		bookService:    book.NewService(workspace),
		versionService: book.NewVersionService(workspace),
		sessionStore:   store,
		session:        sess,
	}
}

// TestRestoreCheckpointSuccessTruncates 验证还原成功时按检查点 MessageIndex 截断。
func TestRestoreCheckpointSuccessTruncates(t *testing.T) {
	workspace := t.TempDir()
	a := newCheckpointTestApp(workspace)
	if a == nil {
		t.Fatal("failed to build checkpoint test app")
	}

	// 追加若干消息，使 MessageIndex=1 有意义。
	if err := a.session.Append(schema.UserMessage("m0")); err != nil {
		t.Fatal(err)
	}
	if err := a.session.Append(schema.AssistantMessage("a1", nil)); err != nil {
		t.Fatal(err)
	}
	if err := a.session.Append(schema.UserMessage("m2")); err != nil {
		t.Fatal(err)
	}

	// 创建有效版本并记录检查点（保留到当前最后一条消息）。
	version, err := a.CreateVersion(context.Background(), "initial")
	if err != nil || version.Version == nil {
		t.Fatalf("create version: result=%#v err=%v", version, err)
	}
	cp, err := a.CreateCheckpoint(context.Background(), "", "manual")
	if err != nil {
		t.Fatalf("create checkpoint: %v", err)
	}
	wantIndex := cp.MessageIndex
	if wantIndex != 2 {
		t.Fatalf("checkpoint MessageIndex = %d, want 2", cp.MessageIndex)
	}

	// 还原成功 → 截断到检查点 MessageIndex。
	result, err := a.RestoreCheckpoint(context.Background(), "", cp.ID)
	if err != nil {
		t.Fatalf("restore checkpoint: %v", err)
	}
	if result.MessageIndex != wantIndex {
		t.Fatalf("RestoreResult.MessageIndex = %d, want %d", result.MessageIndex, wantIndex)
	}
	if got := a.session.TruncateIndex(); got != wantIndex {
		t.Fatalf("TruncateIndex = %d, want %d", got, wantIndex)
	}
}

// TestRestoreCheckpointFailureDoesNotTruncate 验证版本还原失败时不做逻辑截断。
func TestRestoreCheckpointFailureDoesNotTruncate(t *testing.T) {
	workspace := t.TempDir()
	a := newCheckpointTestApp(workspace)
	if a == nil {
		t.Fatal("failed to build checkpoint test app")
	}

	// 追加若干消息，使 TruncateIndex 初始为 -1（未截断）。
	for _, msg := range []string{"m0", "m2", "m4"} {
		if err := a.session.Append(schema.UserMessage(msg)); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.session.TruncateIndex(); got != -1 {
		t.Fatalf("initial TruncateIndex = %d, want -1", got)
	}

	// 记录一个检查点，其 VersionID 指向不存在的提交 → restoreVersionByID 失败。
	cp, err := a.session.AppendCheckpoint(session.Checkpoint{
		MessageIndex: 1,
		VersionID:    "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Reason:       "manual",
	})
	if err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}

	_, err = a.RestoreCheckpoint(context.Background(), "", cp.ID)
	if err == nil {
		t.Fatalf("restore checkpoint should return error on version restore failure")
	}

	// 关键：还原失败后不做截断。
	if got := a.session.TruncateIndex(); got != -1 {
		t.Fatalf("TruncateIndex = %d after failed restore, want -1 (no truncate)", got)
	}
}
