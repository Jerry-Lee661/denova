package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"

	"denova/internal/workspacechange"
)

var workspaceEditFileToolDescription = strings.TrimSpace(`Apply one or more exact text edits to a single workspace file as one reviewed change.
- file_path must identify one file inside the current workspace.
- Every item in edits is matched against the same original file snapshot, not against the result of an earlier item.
- Keep edits non-overlapping. Use replace_all only when every exact occurrence should change.
- Put dependent changes to the same file in one call. Independent files may use separate edit_file calls in the same assistant response.
- The tool captures and protects the current file snapshot internally when the call starts.

将一个或多个精确文本修改作为一次可审阅变更应用到同一个 workspace 文件。
- file_path 必须指向当前 workspace 内的单个文件。
- edits 中的每一项都基于同一份原始文件快照匹配，不基于前一项修改后的结果。
- 各修改区间不得重叠；只有确实需要替换全部精确匹配时才使用 replace_all。
- 同一文件内相互依赖的修改必须放在一次调用中；不同文件的独立修改可以在同一轮分别调用 edit_file。
- 工具会在调用开始时自行获取并保护当前文件快照。`)

var workspaceWriteFileToolDescription = strings.TrimSpace(`Replace the complete content of one workspace file as a reviewed change.
- Use edit_file for localized changes; use write_file only for a new file or an intentional full rewrite.
- file_path must identify one file inside the current workspace.
- The tool detects whether the file exists and protects its current snapshot internally.

将一个 workspace 文件的完整内容替换为新内容，并记录为可审阅变更。
- 局部修改使用 edit_file；只有新建文件或明确需要整体重写时才使用 write_file。
- file_path 必须指向当前 workspace 内的单个文件。
- 工具会自行判断文件是否存在并保护当前快照。`)

type workspaceChangeService interface {
	Workspace() string
	ReadFile(string) (content string, revision string, err error)
	ApplyEdits(context.Context, workspacechange.ApplyEditsRequest) (workspacechange.ChangeSet, error)
	ReplaceFile(context.Context, workspacechange.ReplaceFileRequest) (workspacechange.ChangeSet, error)
}

type workspaceEditFileInput struct {
	FilePath string                      `json:"file_path" jsonschema:"required,description=Absolute or workspace-relative path of the single file to edit"`
	Edits    []workspaceEditFileTextEdit `json:"edits" jsonschema:"required,description=One or more non-overlapping exact replacements evaluated against the same original file snapshot"`
}

type workspaceEditFileTextEdit struct {
	ID         string `json:"id,omitempty" jsonschema:"description=Optional stable identifier used to associate review comments with this edit"`
	OldString  string `json:"old_string" jsonschema:"required,description=Exact non-empty text to replace in the original file snapshot"`
	NewString  string `json:"new_string" jsonschema:"description=Replacement text; an empty string deletes the matched text"`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"description=Replace every exact occurrence of old_string; defaults to false"`
}

type workspaceWriteFileInput struct {
	FilePath string `json:"file_path" jsonschema:"required,description=Absolute or workspace-relative path of the file to replace"`
	Content  string `json:"content" jsonschema:"description=Complete new file content"`
}

func newWorkspaceEditFileTool(changes workspaceChangeService) (tool.BaseTool, error) {
	if changes == nil {
		return nil, fmt.Errorf("workspace change service is nil")
	}
	workspace, err := canonicalChangeWorkspace(changes)
	if err != nil {
		return nil, err
	}
	return utils.InferTool("edit_file", workspaceEditFileToolDescription, func(ctx context.Context, input workspaceEditFileInput) (string, error) {
		path, err := resolveWriteFilePath(workspace, input.FilePath)
		if err != nil {
			return "", err
		}
		// 写参数体积准入：限制单次 edits 数量与新增总量，避免模型在超长上下文里
		// 生成超长结构化输出（本地模型会因 KV+解码缓冲内存不足而 OOM）。
		if msg, ok := editFileInjectionAdmission(input); ok {
			return msg, nil
		}
		baseRevision, err := currentWorkspaceBaseRevision(changes, path)
		if err != nil {
			return "", err
		}
		edits := make([]workspacechange.TextEdit, 0, len(input.Edits))
		for _, edit := range input.Edits {
			edits = append(edits, workspacechange.TextEdit{
				ID:         edit.ID,
				OldString:  edit.OldString,
				NewString:  edit.NewString,
				ReplaceAll: edit.ReplaceAll,
			})
		}
		changeSet, err := changes.ApplyEdits(ctx, workspacechange.ApplyEditsRequest{
			Path:         path,
			BaseRevision: baseRevision,
			Edits:        edits,
			Metadata:     workspaceChangeMetadata(ctx),
		})
		if err != nil {
			return "", err
		}
		return marshalWorkspaceChangeToolReceipt(workspace, changeSet)
	})
}

func newWorkspaceWriteFileTool(changes workspaceChangeService) (tool.BaseTool, error) {
	if changes == nil {
		return nil, fmt.Errorf("workspace change service is nil")
	}
	workspace, err := canonicalChangeWorkspace(changes)
	if err != nil {
		return nil, err
	}
	return utils.InferTool("write_file", workspaceWriteFileToolDescription, func(ctx context.Context, input workspaceWriteFileInput) (string, error) {
		path, err := resolveWriteFilePath(workspace, input.FilePath)
		if err != nil {
			return "", err
		}
		baseRevision, err := currentWorkspaceBaseRevisionOrMissing(changes, path)
		if err != nil {
			return "", err
		}
		changeSet, err := changes.ReplaceFile(ctx, workspacechange.ReplaceFileRequest{
			Path:         path,
			Content:      input.Content,
			BaseRevision: baseRevision,
			Metadata:     workspaceChangeMetadata(ctx),
		})
		if err != nil {
			return "", err
		}
		return marshalWorkspaceChangeToolReceipt(workspace, changeSet)
	})
}

func canonicalChangeWorkspace(changes workspaceChangeService) (string, error) {
	workspace := strings.TrimSpace(changes.Workspace())
	if workspace == "" {
		return "", fmt.Errorf("workspace change service has no workspace identity")
	}
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("workspace change service path is not absolute: %s", workspace)
	}
	return filepath.Clean(workspace), nil
}

// maxEditFileEdits 限制单次 edit_file 的 edits 数量；maxEditTotalNewStringBytes 限制
// 新增内容总字节。避免模型在超长上下文里生成超长结构化输出（本地模型 OOM 的直接触发点）。
const (
	maxEditFileEdits           = 6
	maxEditTotalNewStringBytes = 8192
)

// editFileInjectionAdmission 检查 edit_file 参数体积，超限返回引导消息（不放行），
// 未超限返回空串。写操作不做模糊匹配，只做体积准入（安全：宁可不做也不写错）。
func editFileInjectionAdmission(input workspaceEditFileInput) (string, bool) {
	if len(input.Edits) > maxEditFileEdits {
		return fmt.Sprintf(`[tool error]
type: write_parameter_too_large
tool: edit_file
retryable: false
workspace_mutated: false
中文：单次 edit_file 最多 %d 处修改，本次 %d 处。请拆小：一次只改需要的片段（≤%d 处），或大范围重写改用 write_file 全量重写，避免生成超大工具输出导致模型 OOM。
English: edit_file accepts at most %d edits per call, got %d. Split into smaller calls (≤%d edits), or use write_file for a full rewrite; oversized tool output can OOM local models.`,
			maxEditFileEdits, len(input.Edits), maxEditFileEdits,
			maxEditFileEdits, len(input.Edits), maxEditFileEdits), true
	}
	total := 0
	for _, e := range input.Edits {
		total += len(e.NewString)
	}
	if total > maxEditTotalNewStringBytes {
		return fmt.Sprintf(`[tool error]
type: write_parameter_too_large
tool: edit_file
retryable: false
workspace_mutated: false
中文：本次 edit_file 新增内容共 %d 字节，超过 %d 上限。请拆小：一次只改局部片段，或大范围重写改用 write_file 全量重写，避免生成超大工具输出导致模型 OOM。
English: edit_file new content totals %d bytes, exceeding the %d limit. Split into smaller local edits, or use write_file for a full rewrite; oversized tool output can OOM local models.`,
			total, maxEditTotalNewStringBytes,
			total, maxEditTotalNewStringBytes), true
	}
	return "", false
}

// resolveWriteFilePath 解析写/编辑工具的文件路径：支持 @ 别名（@chN 等），
// 不支持的输入原样返回。写操作只做别名解析，不做模糊匹配（避免写错文件）。
func resolveWriteFilePath(workspace, input string) (string, error) {
	return resolveFileAliasInput(workspace, input)
}

func currentWorkspaceBaseRevision(changes workspaceChangeService, path string) (string, error) {
	_, revision, err := changes.ReadFile(path)
	if err != nil {
		return "", err
	}
	revision = strings.TrimSpace(revision)
	if revision != "" {
		return revision, nil
	}
	return "", &workspacechange.Error{
		Code:    workspacechange.ErrorCodeConflict,
		Message: "workspace change service returned an empty current revision",
		Details: map[string]any{"path": path, "workspace_mutated": false},
	}
}

func currentWorkspaceBaseRevisionOrMissing(changes workspaceChangeService, path string) (string, error) {
	revision, err := currentWorkspaceBaseRevision(changes, path)
	if err == nil {
		return revision, nil
	}
	var changeErr *workspacechange.Error
	if errors.As(err, &changeErr) && changeErr.Code == workspacechange.ErrorCodeNotFound {
		return "missing", nil
	}
	return "", err
}

func workspaceChangeMetadata(ctx context.Context) workspacechange.ChangeMetadata {
	callID := strings.TrimSpace(compose.GetToolCallID(ctx))
	runID := ""
	sessionID := ""
	reviewThreadID := ""
	if observer := RunObserverFromContext(ctx); observer != nil {
		runID = strings.TrimSpace(observer.RunID())
		sessionID = strings.TrimSpace(observer.SessionID())
		reviewThreadID = strings.TrimSpace(observer.ReviewThreadID())
	}
	groupID := runID
	if groupID == "" {
		groupID = callID
	}
	return workspacechange.ChangeMetadata{
		Origin:         workspacechange.OriginAgent,
		ChangeGroupID:  groupID,
		RunID:          runID,
		SessionID:      sessionID,
		ReviewThreadID: reviewThreadID,
		ToolCallID:     callID,
	}
}

func marshalWorkspaceChangeToolReceipt(workspace string, changeSet workspacechange.ChangeSet) (string, error) {
	receipt := workspaceChangeToolReceipt{
		Schema:         workspaceChangeToolResultSchema,
		Status:         workspaceChangeReceiptStatus(changeSet),
		Workspace:      workspace,
		ChangeGroupID:  changeSet.GroupID,
		ReviewThreadID: changeSet.ReviewThreadID,
		ChangeSetID:    changeSet.ID,
		Path:           changeSet.Path,
		BaseRevision:   changeSet.BaseRevision,
		Revision:       changeSet.Revision,
		ReviewStatus:   changeSet.ReviewStatus,
		ApplyState:     changeSet.ApplyState,
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("serialize workspace change receipt: %w", err)
	}
	return string(data), nil
}

func workspaceChangeReceiptStatus(changeSet workspacechange.ChangeSet) string {
	if strings.TrimSpace(changeSet.ApplyState) == "" || changeSet.ApplyState == workspacechange.ApplyStateApplied {
		return "applied"
	}
	return changeSet.ApplyState
}

type workspaceChangeToolErrorReceipt struct {
	Schema           string         `json:"schema"`
	Status           string         `json:"status"`
	Tool             string         `json:"tool"`
	Code             string         `json:"code"`
	Message          string         `json:"message"`
	Details          map[string]any `json:"details,omitempty"`
	Retryable        bool           `json:"retryable"`
	WorkspaceMutated bool           `json:"workspace_mutated"`
}

func formatWorkspaceChangeToolError(toolName string, err error) (string, bool) {
	var changeErr *workspacechange.Error
	if !errors.As(err, &changeErr) || changeErr == nil {
		return "", false
	}
	receipt := workspaceChangeToolErrorReceipt{
		Schema:           "workspace_change.tool_error.v1",
		Status:           "rejected",
		Tool:             normalizeToolName(toolName),
		Code:             changeErr.Code,
		Message:          workspaceChangeToolPublicErrorMessage(changeErr),
		Details:          workspaceChangeToolPublicErrorDetails(changeErr.Details),
		Retryable:        workspaceChangeErrorRetryable(changeErr),
		WorkspaceMutated: workspaceChangeErrorMutated(changeErr),
	}
	data, marshalErr := json.Marshal(receipt)
	if marshalErr != nil {
		return "", false
	}
	return "[tool error]\n" + string(data), true
}

func workspaceChangeToolPublicErrorMessage(changeErr *workspacechange.Error) string {
	if changeErr != nil && changeErr.Code == workspacechange.ErrorCodeRevisionConflict {
		return "File was modified since last read. Re-read the file to get the latest content and revision, then retry with write_file or edit_file. Do NOT fall back to shell commands like execute — they are slower, serialized, and bypass revision protection. / 文件自上次读取后被修改。请重新 read_file 获取最新内容和 revision，再用 write_file 或 edit_file 重试。不要退而使用 execute 等 shell 命令——它们更慢、串行执行且绕过 revision 保护。"
	}
	if changeErr == nil {
		return ""
	}
	return changeErr.Message
}

func workspaceChangeToolPublicErrorDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	public := make(map[string]any, len(details))
	for key, value := range details {
		if strings.Contains(strings.ToLower(key), "revision") {
			continue
		}
		public[key] = value
	}
	if len(public) == 0 {
		return nil
	}
	return public
}

func workspaceChangeErrorMutated(changeErr *workspacechange.Error) bool {
	if changeErr == nil || changeErr.Details == nil {
		return false
	}
	mutated, _ := changeErr.Details["workspace_mutated"].(bool)
	return mutated
}

func workspaceChangeErrorRetryable(changeErr *workspacechange.Error) bool {
	if changeErr == nil {
		return false
	}
	// "replacement does not change the file" 不是瞬态错误，重试不会改变结果
	if changeErr.Code == workspacechange.ErrorCodeInvalidEdit &&
		strings.Contains(changeErr.Message, "replacement does not change") {
		return false
	}
	switch changeErr.Code {
	case workspacechange.ErrorCodeInvalidEdit,
		workspacechange.ErrorCodeRevisionConflict,
		workspacechange.ErrorCodeNotFound,
		workspacechange.ErrorCodeConflict,
		workspacechange.ErrorCodeDurabilityPending:
		return true
	default:
		return false
	}
}
