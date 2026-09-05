package skills

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

const (
	// SkillGuardModeBlock stops the matching tool call with an explanatory
	// message so the model can satisfy the precondition first.
	SkillGuardModeBlock = "block"
	// SkillGuardModeWarn logs the violation and lets the call proceed.
	SkillGuardModeWarn = "warn"

	// SkillGuardCheckRequireFile requires a workspace-relative file to exist
	// before matching tool calls run.
	SkillGuardCheckRequireFile = "require_file"
)

// normalizeSkillGuards validates and defaults a SKILL.md `guards` frontmatter
// list. Unknown modes, checks, or unsafe paths fail skill loading loudly
// instead of silently weakening a declared workflow guarantee.
func normalizeSkillGuards(guards []SkillGuard) ([]SkillGuard, error) {
	if len(guards) == 0 {
		return nil, nil
	}
	normalized := make([]SkillGuard, 0, len(guards))
	seen := make(map[string]bool, len(guards))
	for index, guard := range guards {
		guard.ID = strings.TrimSpace(guard.ID)
		if guard.ID == "" {
			return nil, fmt.Errorf("guards[%d]: id is required", index)
		}
		if len(guard.ID) > 64 {
			return nil, fmt.Errorf("guards[%d] %q: id must be at most 64 characters", index, guard.ID)
		}
		if seen[guard.ID] {
			return nil, fmt.Errorf("guards[%d]: duplicate id %q", index, guard.ID)
		}
		seen[guard.ID] = true
		if guard.Mode == "" {
			guard.Mode = SkillGuardModeBlock
		}
		if guard.Mode != SkillGuardModeBlock && guard.Mode != SkillGuardModeWarn {
			return nil, fmt.Errorf("guards[%d] %q: mode must be %q or %q", index, guard.ID, SkillGuardModeBlock, SkillGuardModeWarn)
		}
		guard.Check = strings.TrimSpace(guard.Check)
		if guard.Check != SkillGuardCheckRequireFile {
			return nil, fmt.Errorf("guards[%d] %q: unsupported check %q (supported: %q)", index, guard.ID, guard.Check, SkillGuardCheckRequireFile)
		}
		rel, err := normalizeGuardPath(guard.Path)
		if err != nil {
			return nil, fmt.Errorf("guards[%d] %q: %w", index, guard.ID, err)
		}
		guard.Path = rel
		for _, tool := range guard.Tools {
			if strings.TrimSpace(tool) == "" {
				return nil, fmt.Errorf("guards[%d] %q: tools entries must be non-empty", index, guard.ID)
			}
		}
		if prefix := strings.TrimSpace(guard.TargetPrefix); prefix != "" {
			clean := path.Clean(filepath.ToSlash(prefix))
			if err := requireWorkspaceRelative(clean); err != nil {
				return nil, fmt.Errorf("guards[%d] %q: target_prefix %q must be workspace-relative", index, guard.ID, guard.TargetPrefix)
			}
			// Keep a trailing slash so "chapters/" cannot also match sibling
			// directories like "chapters_extra/".
			if strings.HasSuffix(filepath.ToSlash(prefix), "/") && !strings.HasSuffix(clean, "/") {
				clean += "/"
			}
			guard.TargetPrefix = clean
		}
		normalized = append(normalized, guard)
	}
	return normalized, nil
}

func normalizeGuardPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("path is required")
	}
	clean := path.Clean(filepath.ToSlash(value))
	if err := requireWorkspaceRelative(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// requireWorkspaceRelative rejects absolute and parent-escaping paths,
// including Windows drive forms like "C:/tmp", because guard paths must stay
// portable across the platforms Denova workspaces move between.
func requireWorkspaceRelative(clean string) error {
	if path.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("path %q must be workspace-relative", clean)
	}
	if len(clean) >= 2 && clean[1] == ':' {
		return fmt.Errorf("path %q must be workspace-relative", clean)
	}
	return nil
}
