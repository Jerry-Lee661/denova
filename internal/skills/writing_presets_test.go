package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinWritingPresetInstructionsCoverScopeInference(t *testing.T) {
	for _, name := range []string{"novel-lite", "novel-standard", "novel-heavy"} {
		content := readBuiltinWritingPreset(t, name)
		for _, required := range []string{
			"agent: ide",
			"不要假设任务一定是下一章",
			"没有 `writing_scope` 字段",
		} {
			if !strings.Contains(content, required) {
				t.Fatalf("%s missing required instruction %q", name, required)
			}
		}
	}
}

func TestBuiltinWritingPresetInstructionsCoverMultiChapterPlanning(t *testing.T) {
	for _, name := range []string{"novel-standard", "novel-heavy"} {
		content := readBuiltinWritingPreset(t, name)
		for _, required := range []string{
			"整体计划",
			"分章计划",
		} {
			if !strings.Contains(content, required) {
				t.Fatalf("%s missing multi-chapter planning instruction %q", name, required)
			}
		}
	}
}

func TestBuiltinWritingPresetInstructionsCoverRequiredTools(t *testing.T) {
	for _, name := range []string{"novel-lite", "novel-standard", "novel-heavy"} {
		content := readBuiltinWritingPreset(t, name)
		for _, required := range []string{
			"read_file",
			"write_file",
			"edit_file",
			"[tool error]",
			"不得宣称已完成",
		} {
			if !strings.Contains(content, required) {
				t.Fatalf("%s missing required tool instruction %q", name, required)
			}
		}
	}
}

func TestBuiltinWritingPresetInstructionsCoverTaskDelegation(t *testing.T) {
	for _, name := range []string{"novel-standard", "novel-heavy"} {
		content := readBuiltinWritingPreset(t, name)
		for _, required := range []string{
			"task",
			"description",
			"reviewer",
		} {
			if !strings.Contains(content, required) {
				t.Fatalf("%s missing task delegation instruction %q", name, required)
			}
		}
	}
}

func TestBuiltinChapterIllustrationSkillIsIDEOnly(t *testing.T) {
	content := readBuiltinWritingPreset(t, "chapter-illustration")
	for _, required := range []string{
		"name: chapter-illustration",
		"agent: ide",
		"generate_image",
		"不要自动编辑章节正文",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("chapter-illustration missing required instruction %q", required)
		}
	}
}

func TestBuiltinLoreSkillCoversToolUsage(t *testing.T) {
	content := readBuiltinWritingPreset(t, "lore")
	for _, required := range []string{
		"name: lore",
		"agent: ide,config_manager,interactive_story",
		"list_lore_items",
		"read_lore_items",
		"write_lore_items",
		"`list_lore_items` 全量索引",
		"`read_lore_items` 批量读取正文",
		"`write_lore_items` 创建或更新条目",
		`"delete_ids": []`,
		`"items":[],"delete_ids":["old-hero-draft"]`,
		"`delete_ids` 必须是数组",
		"不要传字符串 `\"[]\"`",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("lore skill missing required instruction %q", required)
		}
	}
}

func readBuiltinWritingPreset(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "skills", name, SkillFileName))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	// 复用生产侧 frontmatter 解析获取依赖列表
	frontmatter, _, fmErr := parseFrontmatter(content)
	if fmErr != nil {
		t.Fatalf("skill %s frontmatter parse failed: %v", name, fmErr)
	}
	deps := parseDepends(frontmatter)
	if len(deps) == 0 {
		return content
	}
	// 与生产 resolveDepends 一致：依赖在前，自身在后，分隔符一致
	parts := make([]string, 0, len(deps)+1)
	for _, dep := range deps {
		depData, depErr := os.ReadFile(filepath.Join("..", "..", "skills", dep, SkillFileName))
		if depErr != nil {
			// 与生产一致：依赖读不到时跳过，不中断
			continue
		}
		parts = append(parts, string(depData))
	}
	parts = append(parts, content)
	return strings.Join(parts, "\n\n---\n\n")
}
