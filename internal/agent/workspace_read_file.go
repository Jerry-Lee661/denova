package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

const workspaceReadFileResultSchema = "workspace_file.read.v2"

// Keep one selected window bounded even when a file contains a single very
// large line.
const workspaceReadFileMaxSelectedBytes = 1024 * 1024

var workspaceReadFileToolDescription = fmt.Sprintf(`Read a text file and return a bounded, line-numbered selection.
- file_path accepts a stable @alias (e.g. @progress, @ch1; use list_aliases to see all), an absolute path, or a path relative to the workspace root (e.g. chapters/ch00001.md). Relative paths are resolved against the workspace root, never the process directory.
- By default this tool reads up to %d lines from line 1. Use offset and limit to continue reading later sections.
- For summary, keyword search, or locating a specific passage, prefer grep (returns only matching lines) over reading whole chapters; use read_file for close-reading a bounded section.
- Re-reading the same window you already fetched in this run returns a short dedup note instead of the full text again.
- The first result line is JSON pagination metadata.
- The selected text after the metadata is returned in cat -n format.

读取文本文件，返回有界的带行号选段。
- file_path 支持稳定的 @ 别名（如 @progress、@ch1；用 list_aliases 查看全部），也支持绝对路径或相对当前作品根目录的相对路径（如 chapters/ch00001.md；相对路径以作品根为基准，不会访问作品外文件）。
- 默认从第 1 行开始最多读取 %d 行；需要继续读取后续部分时使用 offset 和 limit。
- 摘要、查关键词或定位某段时，优先用 grep（只返回命中行）而非整章读取；read_file 用于精读有界选段。
- 本次运行内重复读取同一窗口时，返回简短去重提示而非再次返回全文。
- 返回结果第一行是 JSON 分页元数据。
- 元数据后的选段使用 cat -n 行号格式。`, agentFileReadDefaultLimitLines, agentFileReadDefaultLimitLines)

type workspaceReadFileInput struct {
	FilePath string `json:"file_path" jsonschema:"required,description=Absolute path of the text file to read"`
	Offset   int    `json:"offset,omitempty" jsonschema:"description=One-based first line to return; defaults to 1"`
	Limit    int    `json:"limit,omitempty" jsonschema:"description=Maximum selected lines to return; defaults to 2000"`
}

type workspaceReadFileMetadata struct {
	Schema   string `json:"schema"`
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

// workspaceFileSelectionReader lets the production backend keep reads rooted
// inside the active workspace while selecting only the requested window.
type workspaceFileSelectionReader interface {
	ReadFileSelection(context.Context, *filesystem.ReadRequest) (string, error)
}

func newWorkspaceReadFileTool(backend filesystem.Backend, workspaces ...string) (tool.BaseTool, error) {
	if backend == nil {
		return nil, fmt.Errorf("filesystem backend is nil")
	}
	workspace := ""
	if len(workspaces) > 0 {
		workspace = strings.TrimSpace(workspaces[0])
	}
	return utils.InferTool("read_file", workspaceReadFileToolDescription, func(ctx context.Context, input workspaceReadFileInput) (string, error) {
		filePath, _, err := resolveWorkspaceReadPath(workspace, input.FilePath)
		if err != nil {
			return "", err
		}
		offset, limit := normalizeWorkspaceReadWindow(input.Offset, input.Limit)

		// 同 run 去重：同一文件的同一窗口若已在本 run 内返回给模型且文件未变化
		// （size+modTime 一致），返回简短占位而非再次注入全文——模型已见过这段内容，
		// 重复注入只会白白吃掉上下文窗口（本地模型 346KB 章节被重读 148 次的根因）。
		var fileInfo os.FileInfo
		if info, statErr := os.Stat(filePath); statErr == nil {
			fileInfo = info
			if obs := RunObserverFromContext(ctx); obs != nil && obs.HasReadWindow(filePath, offset, limit, info.Size(), info.ModTime()) {
				return readDedupPlaceholder(filePath, offset, limit), nil
			}
		}

		content, err := readWorkspaceFileSelection(ctx, backend, &filesystem.ReadRequest{
			FilePath: filePath,
			Offset:   offset,
			Limit:    limit,
		})
		if err != nil {
			return "", err
		}
		// 记录已返回窗口，供同 run 内后续重读判重（用读取前的文件状态）。
		if fileInfo != nil {
			if obs := RunObserverFromContext(ctx); obs != nil {
				obs.RecordReadWindow(filePath, offset, limit, fileInfo.Size(), fileInfo.ModTime())
			}
		}
		metadata, err := json.Marshal(workspaceReadFileMetadata{
			Schema:   workspaceReadFileResultSchema,
			FilePath: filePath,
			Offset:   offset,
			Limit:    limit,
		})
		if err != nil {
			return "", fmt.Errorf("serialize read_file metadata: %w", err)
		}
		return string(metadata) + "\n" + formatWorkspaceLineNumbers(content, offset), nil
	})
}

func readWorkspaceFileSelection(ctx context.Context, backend filesystem.Backend, req *filesystem.ReadRequest) (string, error) {
	if reader, ok := backend.(workspaceFileSelectionReader); ok {
		return reader.ReadFileSelection(ctx, req)
	}
	selected, err := backend.Read(ctx, req)
	if err != nil {
		return "", err
	}
	if selected == nil {
		return "", fmt.Errorf("no content found at path: %s", req.FilePath)
	}
	if len(selected.Content) > workspaceReadFileMaxSelectedBytes {
		return "", fmt.Errorf(
			"selected read_file window exceeds %d bytes; use a narrower offset/limit or split the long line",
			workspaceReadFileMaxSelectedBytes,
		)
	}
	return selected.Content, nil
}

// readDedupPlaceholder 生成同 run 内重复读取同一窗口时返回的占位。
// 格式与正常 read_file 结果一致（首行 JSON 元数据），让模型识别为 read_file 结果；
// 正文是简短双语提示，引导模型直接引用已有内容或改用 grep，避免整章重读。
func readDedupPlaceholder(filePath string, offset, limit int) string {
	metadata, _ := json.Marshal(workspaceReadFileMetadata{
		Schema:   workspaceReadFileResultSchema,
		FilePath: filePath,
		Offset:   offset,
		Limit:    limit,
	})
	end := offset + limit - 1
	body := fmt.Sprintf(
		"[dedup] 该文件第 %d~%d 行的选中段已在上下文中，请直接引用，或改用 offset/limit 读取后续部分。\n"+
			"若任务是摘要/查关键词/定位某段，优先用 grep（只返回命中行），不要整章重读。\n"+
			"[dedup] The selected portion of this file (lines %d~%d) is already in context; reference it directly or use offset/limit to read the next section.\n"+
			"For summary / keyword search / locating a passage, prefer grep (returns only matching lines) over re-reading the whole chapter.",
		offset, end, offset, end,
	)
	return string(metadata) + "\n" + body
}

func (b *agentFilesystemBackend) ReadFileSelection(ctx context.Context, req *filesystem.ReadRequest) (string, error) {
	if req == nil {
		return "", fmt.Errorf("read request is nil")
	}
	if b == nil || b.Backend == nil {
		return "", fmt.Errorf("filesystem backend is nil")
	}
	filePath, rel, err := resolveWorkspaceReadPath(b.workspace, req.FilePath)
	if err != nil {
		return "", err
	}
	offset, limit := normalizeWorkspaceReadWindow(req.Offset, req.Limit)

	// Full-file reads (offset=1, large limit) can skip re-reading disk when
	// the file hasn't changed since the last read.
	if cached, ok := b.cache.get(filePath, offset, offset+limit); ok {
		return applyFileWindow(cached, offset, limit)
	}

	// 无 active workspace 时 rel 为空，直接使用绝对路径打开
	openPath := rel
	if openPath == "" {
		openPath = filePath
	}
	file, err := openWorkspaceFile(b.workspace, openPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s", filePath)
		}
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	// read_file 目标是目录时，Windows 直接读会报 "Incorrect function"，毫无引导。
	// 改为明确提示「用 ls 列出」，让模型不再困惑或乱猜路径。
	if info, statErr := file.Stat(); statErr == nil && info.IsDir() {
		file.Close()
		return "", fmt.Errorf(
			"%s is a directory, not a file: use ls to list its contents（%s 是目录而非文件，请用 ls 列出其内容）",
			filePath, filePath,
		)
	}
	defer file.Close()

	content, err := selectWorkspaceFileWindow(ctx, file, offset, limit)
	if err != nil {
		return "", err
	}
	// Cache only full-file reads under the per-entry size threshold.
	if offset == 1 && limit >= agentFileReadDefaultLimitLines && len(content) <= workspaceReadFileMaxSelectedBytes/2 {
		// 记录实际读取到的行数，而非请求的 limit（文件可能不足 limit 行）
		actualLines := strings.Count(content, "\n") + 1
		b.cache.set(filePath, content, offset, actualLines)
	}
	return content, nil
}

// applyFileWindow slices cached full-file content by offset and limit.
func applyFileWindow(full string, offset, limit int) (string, error) {
	lines := strings.Split(full, "\n")
	start := offset - 1
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		return "", nil
	}
	end := start + limit
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n"), nil
}

// openWorkspaceFile opens a workspace-relative file, trying normalized path
// variants when the original path is not found. This tolerates common model
// path construction errors such as inserting spaces around dashes (e.g.
// "卷一 - 名称" instead of "卷一-名称"), percent-encoding CJK filenames
// (e.g. "正文" → "%E6%AD%A3%E6%96%87"), or dropping CJK characters when
// copying a filename across calls (e.g. "ch00001-正文.md" → "ch00001-.md").
func openWorkspaceFile(workspace, rel string) (*os.File, error) {
	candidates := workspaceFilePathCandidates(rel)
	if fb := readFileUniquePrefixFallback(workspace, rel); fb != "" {
		seen := false
		for _, existing := range candidates {
			if existing == fb {
				seen = true
				break
			}
		}
		if !seen {
			candidates = append(candidates, fb)
		}
	}
	var lastErr error
	for _, candidate := range candidates {
		var f *os.File
		var err error
		if workspace != "" {
			root, rootErr := os.OpenRoot(workspace)
			if rootErr != nil {
				return nil, rootErr
			}
			f, err = root.Open(filepath.FromSlash(candidate))
			// os.Root.Open 返回的 *os.File 独立于 root，关闭 root 不影响已打开的文件。
			root.Close()
		} else {
			// workspace 为空时 candidate 已是绝对路径
			f, err = os.Open(filepath.FromSlash(candidate))
		}
		if err == nil {
			return f, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// readFileUniquePrefixFallback 在原始路径找不到文件时，尝试在目标父目录中做
// "文件名前缀唯一匹配"。兜住 35B 等本地模型在跨调用复制含 CJK 的文件名时丢失
// 字符的情况（如 ch00001-正文.md 被传成 ch00001-.md 或 ch00001-正文.txt）。
// 规则：取传入文件名的去扩展名主干，去掉尾部 `-_ .` 后作为前缀 key；在父目录中
// 找"去扩展名后以 key 开头"的文件；仅当恰好命中一个时才回退，避免读错文件。
// 返回回退后的相对路径；无唯一匹配时返回空字符串。
func readFileUniquePrefixFallback(workspace, rel string) string {
	slashRel := filepath.ToSlash(rel)
	dir := filepath.Dir(slashRel)
	base := filepath.Base(slashRel)
	if dir == "." {
		dir = ""
	}
	stem := base
	if ext := filepath.Ext(base); ext != "" {
		stem = strings.TrimSuffix(base, ext)
	}
	key := strings.TrimRight(stem, "-_ .")
	if key == "" || len([]rune(key)) < 2 {
		return ""
	}
	searchDir := filepath.FromSlash(dir)
	if workspace != "" {
		searchDir = filepath.Join(workspace, filepath.FromSlash(dir))
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return ""
	}
	var match string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if ext := filepath.Ext(name); ext != "" {
			name = strings.TrimSuffix(name, ext)
		}
		if strings.HasPrefix(name, key) {
			if match != "" {
				return "" // 不唯一，安全拒绝
			}
			match = entry.Name()
		}
	}
	if match == "" {
		return ""
	}
	if dir == "" {
		return match
	}
	return filepath.ToSlash(filepath.Join(dir, match))
}

// workspaceFilePathCandidates 生成打开文件时的候选路径，按序尝试：
// 1) 原始路径；2) 折叠破折号空格（历史容错）；3) URL 解码（模型偶尔把中文文件名
// 编码成 %XX，例如 “正文” → %E6%AD%A3%E6%96%87）；4) 字面量 \uXXXX 转义还原
// （模型双重转义的中文文件名，如 \u514b\u6731 → 克珠）。各候选再套折叠/解码组合。
// 仅作为“找不到文件后的容错”，不改变入参语义，也不做“纠错式改写”。
func workspaceFilePathCandidates(rel string) []string {
	candidates := []string{rel}
	appendIfNew := func(p string) {
		for _, existing := range candidates {
			if existing == p {
				return
			}
		}
		candidates = append(candidates, p)
	}
	if alt := normalizeWorkspaceFilePath(rel); alt != rel {
		appendIfNew(alt)
	}
	if decoded, err := url.PathUnescape(rel); err == nil && decoded != rel {
		appendIfNew(decoded)
		if alt := normalizeWorkspaceFilePath(decoded); alt != decoded {
			appendIfNew(alt)
		}
	}
	if unescaped := decodeUnicodeEscapeSequences(rel); unescaped != rel {
		appendIfNew(unescaped)
		if alt := normalizeWorkspaceFilePath(unescaped); alt != unescaped {
			appendIfNew(alt)
		}
		if decoded, err := url.PathUnescape(unescaped); err == nil && decoded != unescaped {
			appendIfNew(decoded)
			if alt := normalizeWorkspaceFilePath(decoded); alt != decoded {
				appendIfNew(alt)
			}
		}
	}
	return candidates
}

// decodeUnicodeEscapeSequences 把路径中“字面量”的 \uXXXX 转义序列还原为实际字符。
// 本地模型在构造工具参数 JSON 时偶尔会双重转义（JSON 里的 \\u 到字符串层变成字面
// \u），使解析层收到的路径里是 \u514b\u6731 这样的文本而不是中文“克珠”。该函数按
// 4 位十六进制扫描 \uXXXX 并还原（含代理对 \uD83D\uDE00 → 😀）；控制字符（< 0x20）
// 与无法配对的孤立代理半区保持原文，避免误造非法路径。仅作候选容错，不改入参语义。
func decodeUnicodeEscapeSequences(s string) string {
	if !strings.Contains(s, `\u`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if i+6 <= len(s) && s[i] == '\\' && s[i+1] == 'u' {
			if v, err := strconv.ParseUint(s[i+2:i+6], 16, 16); err == nil {
				r := rune(v)
				// 高代理半区后紧跟 \uXXXX 低代理半区 → 合成完整码点
				if r >= 0xD800 && r <= 0xDBFF && i+12 <= len(s) && s[i+6] == '\\' && s[i+7] == 'u' {
					if v2, err2 := strconv.ParseUint(s[i+8:i+12], 16, 16); err2 == nil {
						r2 := rune(v2)
						if combined := utf16.DecodeRune(r, r2); combined != utf8.RuneError {
							b.WriteRune(combined)
							i += 12
							continue
						}
					}
				}
				// 控制字符与孤立代理半区不还原，保留原文
				if r >= 0x20 && !(r >= 0xD800 && r <= 0xDFFF) {
					b.WriteRune(r)
					i += 6
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// normalizeWorkspaceFilePath returns a path with common model construction
// errors corrected: spaces around dashes are collapsed (e.g. "卷 - 名" → "卷-名").
func normalizeWorkspaceFilePath(rel string) string {
	normalized := strings.ReplaceAll(rel, " - ", "-")
	normalized = strings.ReplaceAll(normalized, " -", "-")
	normalized = strings.ReplaceAll(normalized, "- ", "-")
	return normalized
}

func selectWorkspaceFileWindow(ctx context.Context, source io.Reader, offset, limit int) (string, error) {
	offset, limit = normalizeWorkspaceReadWindow(offset, limit)
	reader := bufio.NewReaderSize(&contextFileReader{ctx: ctx, reader: source}, 64*1024)
	var selected strings.Builder
	lineNumber := 1
	selectedLines := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		selecting := lineNumber >= offset && selectedLines < limit
		if selecting && len(fragment) > 0 {
			if selected.Len()+len(fragment) > workspaceReadFileMaxSelectedBytes {
				return "", fmt.Errorf(
					"selected read_file window exceeds %d bytes; use a narrower offset/limit or split the long line",
					workspaceReadFileMaxSelectedBytes,
				)
			}
			selected.Write(fragment)
		}
		lineEnded := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if lineEnded || (errors.Is(err, io.EOF) && len(fragment) > 0) {
			if selecting {
				selectedLines++
			}
			lineNumber++
			if selectedLines >= limit {
				break
			}
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != io.EOF {
				return "", fmt.Errorf("error reading file: %w", err)
			}
			break
		}
	}
	return selected.String(), nil
}

// resolveAgentToolPath 是路径类工具（read_file/ls/glob/grep）统一的入参校验与规范化。
//   - 支持 @ 文件别名（@progress、@chN 等，见 file_alias.go）：别名在入口处解析为相对路径，
//     模型无需复制长路径；
//   - 支持绝对路径，也支持相对当前作品根目录的相对路径（相对路径统一锚定 workspace 根，
//     不会访问进程工作目录或作品外文件；这能显著降低本地模型复制长绝对路径时的拼写漂移）；
//   - 有 active workspace 时必须落在 workspace 内，拒绝越界路径；
//   - 只做字符串层面规范化（Trim + filepath.Clean，含反斜杠/正斜杠统一），
//     保留空格、连字符、中文等原始字符，不做任何“纠错式改写”，避免路径漂移。
//
// 返回规范化后的绝对路径，以及（有 workspace 时）相对 workspace 的斜杠路径。
func resolveAgentToolPath(workspace, input string) (absolute, relative string, err error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", fmt.Errorf("path is required")
	}
	if resolved, aliasErr := resolveFileAliasInput(workspace, input); aliasErr != nil {
		return "", "", aliasErr
	} else if resolved != input {
		input = resolved
	}
	workspace = strings.TrimSpace(workspace)
	if workspace != "" {
		var absErr error
		workspace, absErr = filepath.Abs(workspace)
		if absErr != nil {
			return "", "", absErr
		}
		workspace = filepath.Clean(workspace)
		// 容错：本地模型常把作品根目录名本身当作 path 首段传入（如 ls サマーハレーション、
		// read_file サマーハレーション/xxx），而作品根目录名并不是根下的一个子目录——
		// 这样会双重嵌套（workspace/<根目录名>/...）必然失败。检测到相对路径首段等于
		// workspace 根目录名时剥掉该首段再锚定：ls サマーハレーション → 列根目录、
		// read_file サマーハレーション/chapters/x.md → chapters/x.md。
		// 该场景在 Denova 作品结构下几乎不可能是真实子目录，剥除是安全的。
		if !filepath.IsAbs(input) {
			if base := filepath.Base(workspace); base != "" {
				slashInput := filepath.ToSlash(input)
				if slashInput == base || strings.HasPrefix(slashInput, base+"/") {
					input = strings.TrimPrefix(slashInput, base)
					input = strings.TrimPrefix(input, "/")
				}
			}
			input = filepath.Join(workspace, filepath.FromSlash(input))
		}
	} else if !filepath.IsAbs(input) {
		return "", "", fmt.Errorf("path must be absolute: %s", input)
	}
	absolute = filepath.Clean(input)
	if workspace == "" {
		return absolute, "", nil
	}
	relative, err = filepath.Rel(workspace, absolute)
	if err != nil {
		return "", "", err
	}
	// "." 表示路径即 workspace 根本身（ls . 列出作品根合法）；真正越界的是 .. 前缀。
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path is outside the active workspace: %s", absolute)
	}
	return absolute, filepath.ToSlash(relative), nil
}

// resolveWorkspaceReadPath 保留 read_file 语义的兼容入口，内部复用统一的
// resolveAgentToolPath，保证 read_file 与 ls/glob/grep 走同一套路径校验。
func resolveWorkspaceReadPath(workspace, input string) (absolute, relative string, err error) {
	return resolveAgentToolPath(workspace, input)
}

type contextFileReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextFileReader) Read(buffer []byte) (int, error) {
	if r.ctx != nil {
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		default:
		}
	}
	return r.reader.Read(buffer)
}

func normalizeWorkspaceReadWindow(offset, limit int) (int, int) {
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 {
		limit = agentFileReadDefaultLimitLines
	}
	return offset, limit
}

func formatWorkspaceLineNumbers(content string, startLine int) string {
	lines := strings.Split(content, "\n")
	var result strings.Builder
	for index, line := range lines {
		if index < len(lines)-1 {
			fmt.Fprintf(&result, "%6d\t%s\n", startLine+index, line)
		} else {
			fmt.Fprintf(&result, "%6d\t%s", startLine+index, line)
		}
	}
	return result.String()
}
