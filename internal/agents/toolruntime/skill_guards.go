package toolruntime

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	agenttool "denova/internal/agents/tool"
	"denova/internal/agents/skills"
)

// applySkillGuards enforces the workflow guards declared by the Skills that
// are visible to this Agent. Guards run after the policy, access-mode, and
// capability boundaries: they never widen an already-blocked call, they only
// narrow calls that would otherwise execute.
func (m *OrchestratorMiddleware) applySkillGuards(decision *agenttool.Decision) {
	if m == nil || len(m.skillGuards) == 0 {
		return
	}
	for _, guard := range m.skillGuards {
		if !skillGuardAppliesToTool(guard, decision) {
			continue
		}
		if skillGuardSatisfied(m.workspace, guard) {
			continue
		}
		message := skillGuardBlockedMessage(guard, decision.ToolName, decision.Target)
		if guard.Mode == skills.SkillGuardModeWarn {
			log.Printf("[skill-guard] guard %q would block %s to %q: %s", guard.ID, decision.ToolName, decision.Target, message)
			continue
		}
		decision.Action = "blocked"
		decision.Reason = message
		return
	}
}

// skillGuardAppliesToTool reports whether the guard watches this tool call.
// An explicit Tools list wins; otherwise the guard covers every tool whose
// mutation scope is the workspace, so read-only and session-scoped calls stay
// unguarded.
func skillGuardAppliesToTool(guard skills.SkillGuard, decision *agenttool.Decision) bool {
	if len(guard.Tools) > 0 {
		matched := false
		for _, tool := range guard.Tools {
			if tool == decision.ToolName {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	} else if decision.MutationScope != agenttool.ToolMutationWorkspace {
		return false
	}
	if guard.TargetPrefix != "" {
		target := strings.TrimSpace(filepath.ToSlash(decision.Target))
		if !strings.HasPrefix(target, guard.TargetPrefix) {
			return false
		}
	}
	return true
}

// skillGuardSatisfied evaluates the guard's precondition against the
// workspace. An unusable workspace fails open: guards are workflow
// guarantees, not safety boundaries, and a missing workspace is a
// configuration problem that must not silently brick every tool call.
func skillGuardSatisfied(workspace string, guard skills.SkillGuard) bool {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		log.Printf("[skill-guard] guard %q skipped because the Agent has no workspace", guard.ID)
		return true
	}
	_, err := os.Stat(filepath.Join(filepath.FromSlash(workspace), filepath.FromSlash(guard.Path)))
	return err == nil
}

func skillGuardBlockedMessage(guard skills.SkillGuard, toolName, target string) string {
	return fmt.Sprintf(
		"[skill guard %s] blocked %s to %q: this workflow requires %q to exist first. Create or restore that file before writing (the matching planning Skill can help), or ask the user to adjust the workflow.",
		guard.ID, toolName, target, guard.Path,
	)
}
