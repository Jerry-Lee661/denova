package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"denova/internal/book"
)

// 文件别名索引：把「复制长路径字符串」转换为「短语义主键查询」。
// 设计文档见 docs/agent-file-alias-index.md。别名只解析路径，绝不缓存文件内容；
// 解析结果仍走 resolveAgentToolPath 的 workspace 边界校验，不绕过安全链路。

// fileAliasKind 表示别名指向的目标类型。
type fileAliasKind int

const (
	aliasKindFile fileAliasKind = iota
	aliasKindDir
)

// aliasErrorCode 区分别名错误的三分类，避免模型把「别名无效」误当「路径不存在」反复重试。
type aliasErrorCode int

const (
	// aliasErrUnknown：别名字面量不存在，应引导 list_aliases。
	aliasErrUnknown aliasErrorCode = iota
	// aliasErrChapterMissing：@chN 格式正确但该章节不存在（文件缺失，非路径错误）。
	aliasErrChapterMissing
	// aliasErrUseLoreTool：@lore 不是文件，设定应走 read_lore_items。
	aliasErrUseLoreTool
)

// agentFileAliasError 是文件别名解析的结构化错误，便于上层区分错误分类并生成恢复提示。
type agentFileAliasError struct {
	code    aliasErrorCode
	alias   string
	message string
}

func (e *agentFileAliasError) Error() string { return e.message }

// staticFileAliasEntry 描述静态别名表的一个条目（Denova 结构稳定，无需扫描）。
type staticFileAliasEntry struct {
	path string
	kind fileAliasKind
	desc string
}

// staticFileAliases 静态别名表：Denova 工作区结构固定，纯常量，无 IO、天然一致。
var staticFileAliases = map[string]staticFileAliasEntry{
	"@progress":         {path: "setting/progress.md", kind: aliasKindFile, desc: "写作进度"},
	"@character-states": {path: "setting/character-states.md", kind: aliasKindFile, desc: "角色状态"},
	"@outline":          {path: "setting/outline.md", kind: aliasKindFile, desc: "长期大纲"},
	"@ideas":            {path: "ideas.md", kind: aliasKindFile, desc: "灵感"},
	"@creator":          {path: "CREATOR.md", kind: aliasKindFile, desc: "创作规则（书名/世界观/角色/风格）"},
	"@setting":          {path: "setting", kind: aliasKindDir, desc: "设定目录"},
	"@chapters":         {path: "chapters", kind: aliasKindDir, desc: "章节目录"},
}

// aliasResolution 是别名解析结果。
type aliasResolution struct {
	relative string
	kind     fileAliasKind
}

var chapterAliasPattern = regexp.MustCompile(`(?i)^@ch(\d+)$`)

// resolveFileAlias 解析 @alias 为相对 workspace 的斜杠路径。
//   - 静态表优先；@chN 实时调用 book.Service.ChapterAliases() 按章节序号解析（轻量枚举，不读内容）；
//   - 错误按 aliasErrorCode 三分类返回 agentFileAliasError，供上层生成区分化的恢复提示。
func resolveFileAlias(workspace, alias string) (aliasResolution, error) {
	alias = strings.TrimSpace(alias)
	if !strings.HasPrefix(alias, "@") || len(alias) <= 1 {
		return aliasResolution{}, &agentFileAliasError{
			code:    aliasErrUnknown,
			alias:   alias,
			message: fmt.Sprintf("无效文件别名 %q：使用 list_aliases 查看可用别名。", alias),
		}
	}
	if alias == "@lore" {
		return aliasResolution{}, &agentFileAliasError{
			code:    aliasErrUseLoreTool,
			alias:   alias,
			message: "@lore 不是文件别名；读取设定资料请使用 read_lore_items（先 list_lore_items 定位条目）。",
		}
	}
	if entry, ok := staticFileAliases[alias]; ok {
		return aliasResolution{relative: entry.path, kind: entry.kind}, nil
	}
	if matches := chapterAliasPattern.FindStringSubmatch(alias); len(matches) == 2 {
		order := 0
		for _, r := range matches[1] {
			order = order*10 + int(r-'0')
		}
		aliases, err := book.NewService(workspace).ChapterAliases()
		if err != nil {
			return aliasResolution{}, fmt.Errorf("解析章节别名 %s 失败: %w", alias, err)
		}
		for _, ch := range aliases {
			if ch.Index == order {
				return aliasResolution{relative: ch.Path, kind: aliasKindFile}, nil
			}
		}
		return aliasResolution{}, &agentFileAliasError{
			code:    aliasErrChapterMissing,
			alias:   alias,
			message: fmt.Sprintf("别名 %s 有效，但当前作品没有第 %d 章（可用 list_aliases 查看现有章节）。", alias, order),
		}
	}
	return aliasResolution{}, &agentFileAliasError{
		code:    aliasErrUnknown,
		alias:   alias,
		message: fmt.Sprintf("无效文件别名 %q：使用 list_aliases 查看可用别名（如 @progress、@ch1）。", alias),
	}
}

// resolveFileAliasInput 作为 resolveAgentToolPath 的前置：输入以 @ 开头时解析为相对路径，
// 否则原样返回。这样 read_file/ls/glob/grep 统一获得别名能力。
func resolveFileAliasInput(workspace, input string) (string, error) {
	if !strings.HasPrefix(input, "@") {
		return input, nil
	}
	res, err := resolveFileAlias(workspace, input)
	if err != nil {
		return "", err
	}
	return res.relative, nil
}

var listAliasesToolDescription = `List stable file aliases you can pass directly to read_file (file_path), ls/glob/grep (path) instead of copying long paths.
- Aliases are short, stable keys resolved by the backend; prefer them over hand-copying long mixed-CJK paths.
- @chN refers to the N-th chapter (order from the chapter filename), e.g. @ch1, @ch12.
- Use this tool when a path fails repeatedly or before reading known files (progress, outline, characters).

列出可直接传给 read_file（file_path）、ls/glob/grep（path）的稳定文件别名，避免复制长路径。
- 别名是后端解析的短稳定主键；优先使用别名，而不是手抄长混合中英文路径。
- @chN 表示第 N 章（按章节文件名序号），如 @ch1、@ch12。
- 当路径反复失败，或要读取已知文件（进度、大纲、角色状态）前，先调用本工具。`

// newListAliasesTool 创建只读的 list_aliases 工具，让模型第一次就能发现可用主键。
func newListAliasesTool(workspaces ...string) (tool.BaseTool, error) {
	workspace := ""
	if len(workspaces) > 0 {
		workspace = strings.TrimSpace(workspaces[0])
	}
	return utils.InferTool("list_aliases", listAliasesToolDescription, func(_ context.Context, _ struct{}) (string, error) {
		return listFileAliases(workspace)
	})
}

// listFileAliases 生成人类可读的别名清单（静态表 + 当前章节）。
func listFileAliases(workspace string) (string, error) {
	var b strings.Builder
	b.WriteString("文件别名（read_file 的 file_path、ls/glob/grep 的 path 可直接传 @ 别名）：\n")
	b.WriteString("File aliases (read_file file_path and ls/glob/grep path accept @aliases directly):\n\n")

	b.WriteString("稳定文件 / Stable:\n")
	for _, name := range sortedStaticAliasNames() {
		entry := staticFileAliases[name]
		kind := "file"
		if entry.kind == aliasKindDir {
			kind = "dir"
		}
		// 标注尚未创建的文件，避免模型以为别名失效或反复尝试读取不存在的文件。
		status := ""
		if entry.kind == aliasKindFile {
			if _, statErr := os.Stat(filepath.Join(workspace, filepath.FromSlash(entry.path))); statErr != nil {
				status = "（未创建 / not created yet）"
			}
		}
		fmt.Fprintf(&b, "- %s → %s (%s, %s)%s\n", name, entry.path, kind, entry.desc, status)
	}

	b.WriteString("\n章节别名 / Chapters (@chN by chapter order):\n")
	aliases, err := book.NewService(workspace).ChapterAliases()
	if err != nil {
		return "", fmt.Errorf("列出章节别名失败: %w", err)
	}
	if len(aliases) == 0 {
		b.WriteString("- （当前没有章节）/ (no chapters yet)\n")
	} else {
		for _, ch := range aliases {
			volume := ""
			if ch.Volume != "" {
				volume = " [" + ch.Volume + "]"
			}
			fmt.Fprintf(&b, "- @ch%d → %s (%s)%s\n", ch.Index, ch.Path, ch.Title, volume)
		}
	}

	b.WriteString("\n说明：@lore 不是文件；设定资料请使用 read_lore_items。\n")
	b.WriteString("Note: @lore is not a file; use read_lore_items for lore entries.\n")
	return b.String(), nil
}

// sortedStaticAliasNames 返回静态别名键的确定性排序，保证 list_aliases 输出稳定。
func sortedStaticAliasNames() []string {
	names := make([]string, 0, len(staticFileAliases))
	for name := range staticFileAliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// aliasRecoveryHint 为别名解析错误生成区分化的恢复提示，阻止模型把「别名无效 / 目标缺失」
// 误当「路径不存在」无限重试。由 filePathRecoveryHint 在 read_file/ls/glob 出错时调用。
func aliasRecoveryHint(workspace, toolName string, aliasErr *agentFileAliasError) string {
	wsField := ""
	if strings.TrimSpace(workspace) != "" {
		wsField = "workspace: " + workspace + "\n"
	}
	switch aliasErr.code {
	case aliasErrUnknown:
		return fmt.Sprintf(`[tool error]
type: invalid_file_alias
tool: %s
retryable: false
workspace_mutated: false
%s
中文：%s 请先调用 list_aliases 获取可用别名再重试；不要猜测或手写路径。
English: %s Call list_aliases first to see valid aliases, then retry; do not guess paths.`,
			toolName, wsField, aliasErr.message, aliasErr.message)
	case aliasErrChapterMissing:
		return fmt.Sprintf(`[tool error]
type: alias_target_missing
tool: %s
retryable: false
workspace_mutated: false
%s
中文：%s 这是文件/章节缺失，不是路径错误：停止重试，直接告知用户缺少该章节或文件。
English: %s This is a missing file/chapter, not a path error: stop retrying and tell the user what is missing.`,
			toolName, wsField, aliasErr.message, aliasErr.message)
	default: // aliasErrUseLoreTool
		return fmt.Sprintf(`[tool error]
type: alias_requires_specialized_tool
tool: %s
retryable: false
workspace_mutated: false
%s
中文：%s
English: %s`,
			toolName, wsField, aliasErr.message, aliasErr.message)
	}
}
