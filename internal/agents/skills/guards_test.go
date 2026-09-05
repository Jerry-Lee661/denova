package skills

import (
	"strings"
	"testing"
)

func TestNormalizeSkillGuardsDefaultsAndAcceptsRequireFile(t *testing.T) {
	normalized, err := normalizeSkillGuards([]SkillGuard{{
		ID:           "require_outline",
		Check:        SkillGuardCheckRequireFile,
		Path:         "setting/outline.md",
		TargetPrefix: "chapters/",
	}})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(normalized) != 1 {
		t.Fatalf("expected one guard, got %d", len(normalized))
	}
	guard := normalized[0]
	if guard.Mode != SkillGuardModeBlock {
		t.Fatalf("default mode should be block, got %q", guard.Mode)
	}
	if guard.Path != "setting/outline.md" || guard.TargetPrefix != "chapters/" {
		t.Fatalf("unexpected normalization: %#v", guard)
	}
}

func TestNormalizeSkillGuardsRejectsInvalidEntries(t *testing.T) {
	cases := []struct {
		name   string
		guards []SkillGuard
		want   string
	}{
		{"missing id", []SkillGuard{{Check: SkillGuardCheckRequireFile, Path: "a.md"}}, "id is required"},
		{"duplicate id", []SkillGuard{
			{ID: "g", Check: SkillGuardCheckRequireFile, Path: "a.md"},
			{ID: "g", Check: SkillGuardCheckRequireFile, Path: "b.md"},
		}, "duplicate id"},
		{"unknown mode", []SkillGuard{{ID: "g", Mode: "audit", Check: SkillGuardCheckRequireFile, Path: "a.md"}}, "mode"},
		{"unknown check", []SkillGuard{{ID: "g", Check: "require_db", Path: "a.md"}}, "unsupported check"},
		{"missing path", []SkillGuard{{ID: "g", Check: SkillGuardCheckRequireFile}}, "path is required"},
		{"escaping path", []SkillGuard{{ID: "g", Check: SkillGuardCheckRequireFile, Path: "../outside.md"}}, "workspace-relative"},
		{"absolute path", []SkillGuard{{ID: "g", Check: SkillGuardCheckRequireFile, Path: "C:\\tmp\\a.md"}}, "workspace-relative"},
		{"escaping target prefix", []SkillGuard{{ID: "g", Check: SkillGuardCheckRequireFile, Path: "a.md", TargetPrefix: "../out/"}}, "workspace-relative"},
		{"empty tool entry", []SkillGuard{{ID: "g", Check: SkillGuardCheckRequireFile, Path: "a.md", Tools: []string{" "}}}, "non-empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeSkillGuards(tc.guards)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestParseRecordAcceptsGuardsFrontmatter(t *testing.T) {
	data := "---\nname: demo\ndescription: Demo skill.\ncategory: writing\nguards:\n  - id: require_outline\n    mode: warn\n    check: require_file\n    path: setting/outline.md\n    target_prefix: chapters/\n---\n\n# demo\n"
	record, err := parseRecord(t.Context(), Directory{}, "skills/demo/SKILL.md", data)
	if err != nil {
		t.Fatalf("parseRecord: %v", err)
	}
	guards := record.skill.FrontMatter.Guards
	if len(guards) != 1 || guards[0].ID != "require_outline" || guards[0].Mode != SkillGuardModeWarn {
		t.Fatalf("unexpected guards: %#v", guards)
	}
}

func TestParseRecordRejectsInvalidGuards(t *testing.T) {
	data := "---\nname: demo\ndescription: Demo skill.\nguards:\n  - id: bad\n    check: require_file\n    path: ../escape.md\n---\n\n# demo\n"
	if _, err := parseRecord(t.Context(), Directory{}, "skills/demo/SKILL.md", data); err == nil || !strings.Contains(err.Error(), "skill demo") {
		t.Fatalf("expected skill-scoped error, got %v", err)
	}
}
