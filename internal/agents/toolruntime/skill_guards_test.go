package toolruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"denova/internal/agents/skills"
)

func guardTestMiddleware(t *testing.T, guards []skills.SkillGuard, workspace string) *OrchestratorMiddleware {
	t.Helper()
	return &OrchestratorMiddleware{skillGuards: guards, workspace: workspace}
}

func TestRequireOutlineGuardBlocksChapterWrite(t *testing.T) {
	workspace := t.TempDir()
	middleware := guardTestMiddleware(t, []skills.SkillGuard{{
		ID:           "require_outline",
		Check:        skills.SkillGuardCheckRequireFile,
		Path:         "setting/outline.md",
		TargetPrefix: "chapters/",
	}}, workspace)

	decision := middleware.buildToolDecision(context.Background(), testToolContext("write", "c1"), `{"path":"chapters/ch01.md"}`)
	if decision.Action != "blocked" {
		t.Fatalf("chapter write should be blocked without outline: %#v", decision)
	}
	if !strings.Contains(decision.Reason, "require_outline") || !strings.Contains(decision.Reason, "setting/outline.md") {
		t.Fatalf("blocked reason should name the guard and required file, got %q", decision.Reason)
	}
}

func TestRequireOutlineGuardAllowsAfterOutlineExists(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "setting"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "setting", "outline.md"), []byte("# outline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	middleware := guardTestMiddleware(t, []skills.SkillGuard{{
		ID: "require_outline", Check: skills.SkillGuardCheckRequireFile, Path: "setting/outline.md",
	}}, workspace)

	decision := middleware.buildToolDecision(context.Background(), testToolContext("write", "c1"), `{"path":"chapters/ch01.md"}`)
	if decision.Action != "allowed" {
		t.Fatalf("chapter write should pass once the outline exists: %#v", decision)
	}
}

func TestRequireOutlineGuardTargetPrefixLeavesOtherPathsOpen(t *testing.T) {
	workspace := t.TempDir()
	middleware := guardTestMiddleware(t, []skills.SkillGuard{{
		ID: "require_outline", Check: skills.SkillGuardCheckRequireFile,
		Path: "setting/outline.md", TargetPrefix: "chapters/",
	}}, workspace)

	decision := middleware.buildToolDecision(context.Background(), testToolContext("write", "c1"), `{"path":"setting/outline.md"}`)
	if decision.Action != "allowed" {
		t.Fatalf("writing the outline itself must not be blocked by require_outline: %#v", decision)
	}
}

func TestWarnModeGuardNeverBlocks(t *testing.T) {
	workspace := t.TempDir()
	middleware := guardTestMiddleware(t, []skills.SkillGuard{{
		ID: "require_outline", Mode: skills.SkillGuardModeWarn,
		Check: skills.SkillGuardCheckRequireFile, Path: "setting/outline.md",
	}}, workspace)

	decision := middleware.buildToolDecision(context.Background(), testToolContext("write", "c1"), `{"path":"chapters/ch01.md"}`)
	if decision.Action != "allowed" {
		t.Fatalf("warn-mode guard must not block: %#v", decision)
	}
}

func TestScopeBasedGuardIgnoresReadTools(t *testing.T) {
	workspace := t.TempDir()
	middleware := guardTestMiddleware(t, []skills.SkillGuard{{
		ID: "require_outline", Check: skills.SkillGuardCheckRequireFile, Path: "setting/outline.md",
	}}, workspace)

	decision := middleware.buildToolDecision(context.Background(), testToolContext("read", "c2"), `{"path":"chapters/ch01.md"}`)
	if decision.Action != "allowed" {
		t.Fatalf("read-only calls must stay unguarded: %#v", decision)
	}

	blocked := middleware.buildToolDecision(context.Background(), testToolContext("write", "c3"), `{"path":"chapters/ch01.md"}`)
	if blocked.Action != "blocked" {
		t.Fatalf("workspace mutation without the required file should be blocked: %#v", blocked)
	}
}

func TestExplicitToolListRestrictsGuard(t *testing.T) {
	workspace := t.TempDir()
	middleware := guardTestMiddleware(t, []skills.SkillGuard{{
		ID: "require_outline", Check: skills.SkillGuardCheckRequireFile, Path: "setting/outline.md",
		Tools: []string{"edit"},
	}}, workspace)

	write := middleware.buildToolDecision(context.Background(), testToolContext("write", "c1"), `{"path":"chapters/ch01.md"}`)
	if write.Action != "allowed" {
		t.Fatalf("guard limited to edit must not block write: %#v", write)
	}
	edit := middleware.buildToolDecision(context.Background(), testToolContext("edit", "c2"), `{"path":"chapters/ch01.md"}`)
	if edit.Action != "blocked" {
		t.Fatalf("guard limited to edit should block edit: %#v", edit)
	}
}
