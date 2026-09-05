package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAgentChapter 在 workspace 下创建章节文件。
func writeAgentChapter(t *testing.T, workspace, rel string) {
	t.Helper()
	abs := filepath.Join(workspace, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("创建章节目录失败: %v", err)
	}
	if err := os.WriteFile(abs, []byte("正文内容\n"), 0o644); err != nil {
		t.Fatalf("写入章节失败: %v", err)
	}
}

func TestResolveFileAliasStatic(t *testing.T) {
	cases := []struct {
		alias    string
		expected string
		kind     fileAliasKind
	}{
		{"@progress", "setting/progress.md", aliasKindFile},
		{"@character-states", "setting/character-states.md", aliasKindFile},
		{"@outline", "setting/outline.md", aliasKindFile},
		{"@ideas", "ideas.md", aliasKindFile},
		{"@creator", "CREATOR.md", aliasKindFile},
		{"@setting", "setting", aliasKindDir},
		{"@chapters", "chapters", aliasKindDir},
	}
	// 静态别名不依赖 workspace，传空串也应解析成功
	for _, c := range cases {
		res, err := resolveFileAlias("", c.alias)
		if err != nil {
			t.Fatalf("resolveFileAlias(%q) 返回错误: %v", c.alias, err)
		}
		if res.relative != c.expected || res.kind != c.kind {
			t.Fatalf("resolveFileAlias(%q) = %+v, 期望 relative=%q kind=%v", c.alias, res, c.expected, c.kind)
		}
	}
}

func TestResolveFileAliasChapter(t *testing.T) {
	workspace := t.TempDir()
	writeAgentChapter(t, workspace, "chapters/ch00001-正文.md")
	writeAgentChapter(t, workspace, "chapters/v00001-第一卷/ch00002-第二章.md")

	res, err := resolveFileAlias(workspace, "@ch1")
	if err != nil {
		t.Fatalf("@ch1 解析失败: %v", err)
	}
	if res.relative != "chapters/ch00001-正文.md" || res.kind != aliasKindFile {
		t.Fatalf("@ch1 = %+v, 期望 chapters/ch00001-正文.md/file", res)
	}

	res, err = resolveFileAlias(workspace, "@ch2")
	if err != nil {
		t.Fatalf("@ch2 解析失败: %v", err)
	}
	if res.relative != "chapters/v00001-第一卷/ch00002-第二章.md" {
		t.Fatalf("@ch2 = %q, 期望卷内章节路径", res.relative)
	}
}

func TestResolveFileAliasErrorTaxonomy(t *testing.T) {
	workspace := t.TempDir()
	writeAgentChapter(t, workspace, "chapters/ch00001-正文.md")

	if _, err := resolveFileAlias(workspace, "@ch99"); !isAliasErrorCode(err, aliasErrChapterMissing) {
		t.Fatalf("@ch99 应返回 aliasErrChapterMissing，得到: %v", err)
	}
	if _, err := resolveFileAlias(workspace, "@nope"); !isAliasErrorCode(err, aliasErrUnknown) {
		t.Fatalf("@nope 应返回 aliasErrUnknown，得到: %v", err)
	}
	if _, err := resolveFileAlias(workspace, "@"); !isAliasErrorCode(err, aliasErrUnknown) {
		t.Fatalf("@ 应返回 aliasErrUnknown，得到: %v", err)
	}
	if _, err := resolveFileAlias(workspace, "@lore"); !isAliasErrorCode(err, aliasErrUseLoreTool) {
		t.Fatalf("@lore 应返回 aliasErrUseLoreTool，得到: %v", err)
	}
}

func TestResolveFileAliasInputPassthrough(t *testing.T) {
	for _, input := range []string{"chapters/ch00001.md", "C:/abs/path.md", "setting/"} {
		got, err := resolveFileAliasInput("", input)
		if err != nil || got != input {
			t.Fatalf("resolveFileAliasInput(%q) = %q, %v; 期望原样返回", input, got, err)
		}
	}
	got, err := resolveFileAliasInput("", "@progress")
	if err != nil || got != "setting/progress.md" {
		t.Fatalf("resolveFileAliasInput(@progress) = %q, %v", got, err)
	}
}

func TestResolveAgentToolPathWithAlias(t *testing.T) {
	workspace := t.TempDir()
	writeAgentChapter(t, workspace, "chapters/ch00001-正文.md")

	absolute, relative, err := resolveAgentToolPath(workspace, "@progress")
	if err != nil {
		t.Fatalf("resolveAgentToolPath(@progress) 失败: %v", err)
	}
	if relative != "setting/progress.md" {
		t.Fatalf("relative = %q, 期望 setting/progress.md", relative)
	}
	if !strings.HasPrefix(absolute, workspace) {
		t.Fatalf("absolute %q 未落在 workspace %q 内", absolute, workspace)
	}

	_, _, err = resolveAgentToolPath(workspace, "@ch1")
	if err != nil {
		t.Fatalf("resolveAgentToolPath(@ch1) 失败: %v", err)
	}

	if _, _, err = resolveAgentToolPath(workspace, "@nope"); err == nil {
		t.Fatal("resolveAgentToolPath(@nope) 应返回错误")
	}
}

func TestListFileAliasesIncludesStaticAndChapters(t *testing.T) {
	workspace := t.TempDir()
	writeAgentChapter(t, workspace, "chapters/ch00001-正文.md")

	out, err := listFileAliases(workspace)
	if err != nil {
		t.Fatalf("listFileAliases 失败: %v", err)
	}
	for _, want := range []string{"@progress", "@chapters", "@ch1", "chapters/ch00001-正文.md", "@lore"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listFileAliases 输出缺少 %q:\n%s", want, out)
		}
	}
}

func TestAliasRecoveryHintDistinguishesCodes(t *testing.T) {
	unknown := aliasRecoveryHint("ws", "read_file", &agentFileAliasError{code: aliasErrUnknown, alias: "@x", message: "无效"})
	if !strings.Contains(unknown, "invalid_file_alias") || !strings.Contains(unknown, "list_aliases") {
		t.Fatalf("unknown hint 错误:\n%s", unknown)
	}
	missing := aliasRecoveryHint("ws", "read_file", &agentFileAliasError{code: aliasErrChapterMissing, alias: "@ch9", message: "缺失"})
	if !strings.Contains(missing, "alias_target_missing") || !strings.Contains(missing, "not a path error") {
		t.Fatalf("missing hint 错误:\n%s", missing)
	}
	lore := aliasRecoveryHint("ws", "read_file", &agentFileAliasError{code: aliasErrUseLoreTool, alias: "@lore", message: "read_lore_items"})
	if !strings.Contains(lore, "alias_requires_specialized_tool") {
		t.Fatalf("lore hint 错误:\n%s", lore)
	}
}

func isAliasErrorCode(err error, code aliasErrorCode) bool {
	var aliasErr *agentFileAliasError
	if !errors.As(err, &aliasErr) {
		return false
	}
	return aliasErr.code == code
}

func TestListFileAliasesMarksMissingFiles(t *testing.T) {
	workspace := t.TempDir()
	out, err := listFileAliases(workspace)
	if err != nil {
		t.Fatalf("listFileAliases 失败: %v", err)
	}
	// 空 workspace 中未创建的静态文件应标注出来。
	if !strings.Contains(out, "未创建") {
		t.Fatalf("空 workspace 的静态文件应标注未创建:\n%s", out)
	}

	// 创建 progress.md 后，对应行不应再标注未创建。
	if err := os.MkdirAll(filepath.Join(workspace, "setting"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "setting", "progress.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out2, err := listFileAliases(workspace)
	if err != nil {
		t.Fatalf("listFileAliases 失败: %v", err)
	}
	for _, line := range strings.Split(out2, "\n") {
		if strings.Contains(line, "@progress") && strings.Contains(line, "未创建") {
			t.Fatalf("@progress 已创建但仍标注未创建:\n%s", out2)
		}
	}
}
