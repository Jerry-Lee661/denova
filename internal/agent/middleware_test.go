package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	localbk "github.com/cloudwego/eino-ext/adk/backend/local"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"denova/config"
)

// TestHandleUnknownTool 验证 LLM 幻觉调用不存在工具时，处理器返回引导性
// ToolMessage 而不是抛出错误，从而让 Agent 自行修正。
func TestHandleUnknownTool(t *testing.T) {
	result, err := handleUnknownTool(context.Background(), "write_todo", `{"todos":[]}`)
	if err != nil {
		t.Fatalf("处理未知工具不应返回错误: %v", err)
	}
	if !strings.Contains(result, "write_todo") {
		t.Fatalf("结果应包含工具名: %s", result)
	}
	if !strings.Contains(result, "[tool error]") {
		t.Fatalf("结果应携带 [tool error] 前缀以提示模型自我修复: %s", result)
	}
}

func TestInteractiveStoryToolMiddlewareBlocksWriteTools(t *testing.T) {
	middleware := newInteractiveStoryToolMiddleware()
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "write_file"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{"file_path":"/tmp/a"}`)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("write_file should be blocked before endpoint is called")
	}
	if !strings.Contains(result, "游戏模式禁止使用写文件工具") {
		t.Fatalf("unexpected block result: %s", result)
	}
}

func TestInteractiveTurnReceiptRecordsDomainOutcomeSeparatelyFromTransport(t *testing.T) {
	record := ToolExecutionRecord{ToolName: interactiveTurnSubmissionToolName, Status: "success"}
	applyInteractiveTurnReceiptToExecutionRecord(&record, `{"ready":false,"module_status":{"state_changes":"rejected","choices":"accepted"},"diagnostics":[{"code":"invalid_module"}],"retry_modules":["state_changes"]}`)
	if record.Status != "success" || record.DomainStatus != "rejected" || record.DomainDiagnosticCount != 1 || len(record.RetryModules) != 1 || record.RetryModules[0] != "state_changes" {
		t.Fatalf("transport success should retain the rejected domain outcome: %#v", record)
	}

	accepted := ToolExecutionRecord{ToolName: interactiveTurnSubmissionToolName, Status: "success"}
	applyInteractiveTurnReceiptToExecutionRecord(&accepted, `{"ready":true,"module_status":{"state_changes":"accepted","choices":"accepted"}}`)
	if accepted.DomainStatus != "accepted" || accepted.DomainDiagnosticCount != 0 {
		t.Fatalf("ready receipt should be recorded as domain accepted: %#v", accepted)
	}
}

func TestInteractiveStoryToolMiddlewareAllowsReadTools(t *testing.T) {
	middleware := newInteractiveStoryToolMiddleware()
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "read_file"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !called || result != "ok" {
		t.Fatalf("read_file should pass through, called=%v result=%s", called, result)
	}
}

func TestInteractiveDirectorPlanFileMiddlewareBlocksStateTools(t *testing.T) {
	middleware := newInteractiveDirectorPlanFileMiddleware()
	for _, name := range []string{"apply_actor_state_patch"} {
		called := false
		endpoint, err := middleware.WrapInvokableToolCall(
			context.Background(),
			func(context.Context, string, ...tool.Option) (string, error) {
				called = true
				return "ok", nil
			},
			&adk.ToolContext{Name: name},
		)
		if err != nil {
			t.Fatal(err)
		}
		result, err := endpoint(context.Background(), `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if called || !strings.Contains(result, "不能写 Actor State") {
			t.Fatalf("%s should be blocked, called=%v result=%s", name, called, result)
		}
	}
}

func TestInteractiveDirectorPlanMiddlewareAllowsStructuredSubmitAndBlocksFiles(t *testing.T) {
	middleware := newInteractiveDirectorPlanFileMiddleware()
	for _, tc := range []struct {
		name    string
		allowed bool
	}{
		{name: submitDirectorPlanUpdateToolName, allowed: true},
		{name: "read_file", allowed: false},
		{name: "write_file", allowed: false},
	} {
		called := false
		endpoint, err := middleware.WrapInvokableToolCall(
			context.Background(),
			func(context.Context, string, ...tool.Option) (string, error) {
				called = true
				return "ok", nil
			},
			&adk.ToolContext{Name: tc.name},
		)
		if err != nil {
			t.Fatal(err)
		}
		result, err := endpoint(context.Background(), `{}`)
		if err != nil {
			t.Fatal(err)
		}
		if tc.allowed && (!called || result != "ok") {
			t.Fatalf("%s should pass through, called=%v result=%s", tc.name, called, result)
		}
		if !tc.allowed && (called || !strings.Contains(result, submitDirectorPlanUpdateToolName)) {
			t.Fatalf("%s should be blocked in favor of structured submit, called=%v result=%s", tc.name, called, result)
		}
	}
}

func TestInteractiveDirectorPlanFileMiddlewareBlocksUnauthorizedTools(t *testing.T) {
	middleware := newInteractiveDirectorPlanFileMiddleware()
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "execute_shell"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{"cmd":"ls"}`)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("unauthorized director tool should be blocked before endpoint is called")
	}
	if !strings.Contains(result, "拒绝工具: execute_shell") {
		t.Fatalf("unexpected block result: %s", result)
	}
}

func TestToolOrchestratorBlocksInteractiveWriteTools(t *testing.T) {
	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindInteractiveStory}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "write_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{"path":"chapters/ch01.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("interactive write tool should be blocked before endpoint is called")
	}
	if !strings.Contains(result, "游戏模式禁止使用写文件工具") {
		t.Fatalf("unexpected block result: %s", result)
	}
}

func TestToolOrchestratorBlocksInteractiveSubAgentWriteTools(t *testing.T) {
	middleware := &toolOrchestratorMiddleware{agentKind: "researcher", policyKind: AgentKindInteractiveStory}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "write_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{"path":"chapters/ch01.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("interactive subagent write tool should be blocked before endpoint is called")
	}
	if !strings.Contains(result, "游戏模式禁止使用写文件工具") {
		t.Fatalf("unexpected block result: %s", result)
	}
}

func TestToolOrchestratorAllowsIDEWriteAndFiltersResult(t *testing.T) {
	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE}
	content := strings.Repeat("正文", 100)
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			return content, nil
		},
		&adk.ToolContext{Name: "write_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{"path":"chapters/ch01.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "schema: tool_result.v1") ||
		!strings.Contains(result, "mutates_workspace: true") ||
		!strings.Contains(result, "target: chapters/ch01.md") {
		t.Fatalf("result should include filtered metadata: %s", result)
	}
	if !strings.Contains(result, content) {
		t.Fatalf("result below the high default limit should stay complete")
	}
}

func TestToolOrchestratorTruncatesResultWhenLimitConfigured(t *testing.T) {
	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE, toolResultMaxBytes: 128}
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			return strings.Repeat("正文", 200), nil
		},
		&adk.ToolContext{Name: "write_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{"path":"chapters/ch01.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "[tool result truncated]") ||
		!strings.Contains(result, "truncated: true") {
		t.Fatalf("configured limit should truncate result: %s", result)
	}
}

func TestToolOrchestratorBlocksMalformedJSONArguments(t *testing.T) {
	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "write_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	args := "{\"file_path\":\"chapters/ch01.md\",\"content\":\"过了一遍。\\\\n\\\\n韩十四。武监司。三十\n\t^\n\\"
	result, err := endpoint(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("malformed JSON arguments should be blocked before endpoint is called")
	}
	if !strings.Contains(result, "参数不是完整 JSON 对象") ||
		!strings.Contains(result, "Tool arguments must be a complete JSON object") {
		t.Fatalf("unexpected malformed-arguments result: %s", result)
	}
	if strings.Contains(result, "重新发起同一个工具调用") {
		t.Fatalf("malformed-arguments result should not force a same-tool retry: %s", result)
	}
}

func TestToolOrchestratorReturnsContentFilterContextForIncompleteWriteArguments(t *testing.T) {
	workspace := t.TempDir()
	ledger, err := newRunLedger(workspace, RunLedgerPolicy{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	observer := newRunObserver(ledger, "root-span")
	observer.RecordLLMOutcome(LLMOutcome{
		FinishReason:      "content_filter",
		RequestedTools:    []string{"write_file"},
		ProviderRequestID: "provider-1",
	})
	ctx := ContextWithRunObserver(context.Background(), observer)
	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "write_file", CallID: "call-content-filter"},
	)
	if err != nil {
		t.Fatal(err)
	}
	args := `{"file_path":"chapters/ch01.md","content":"正文被过滤中断`
	result, err := endpoint(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("content-filter interrupted arguments should be blocked before endpoint is called")
	}
	for _, want := range []string{
		"reason: model_output_interrupted_by_content_filter",
		"retryable: false",
		"workspace_mutated: false",
		"args_complete: false",
		"model_finish_reason: content_filter",
		"target: chapters/ch01.md",
		"文件未写入",
		"do not retry the same write tool",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("content-filter context missing %q:\n%s", want, result)
		}
	}
	if strings.Contains(result, "重新发起同一个工具调用") {
		t.Fatalf("content-filter context should not force a same-tool retry: %s", result)
	}
	records := readRunLedgerRecords(t, ledger.Path())
	var decision map[string]any
	var toolAttrs map[string]any
	for _, record := range records {
		data, _ := record["data"].(map[string]any)
		switch record["type"] {
		case "tool_decision":
			decision, _ = data["decision"].(map[string]any)
		case "tool_call":
			toolAttrs, _ = data["attrs"].(map[string]any)
		}
	}
	if decision == nil || toolAttrs == nil {
		t.Fatalf("expected tool decision and trace span records: %#v", records)
	}
	if decision["model_finish_reason"] != "content_filter" || decision["args_complete"] != false {
		t.Fatalf("decision should record incomplete content-filter args: %#v", decision)
	}
	if got, _ := decision["args_bytes"].(float64); int(got) != len(args) {
		t.Fatalf("decision args_bytes = %v, want %d", decision["args_bytes"], len(args))
	}
	if toolAttrs["model_finish_reason"] != "content_filter" || toolAttrs["args_complete"] != false {
		t.Fatalf("tool span should record incomplete content-filter args: %#v", toolAttrs)
	}
}

func TestToolPathFromArgsExtractsPartialFilePath(t *testing.T) {
	args := `{"file_path":"chapters/ch01.md","content":"正文还没闭合`
	if got := toolPathFromArgs(args); got != "chapters/ch01.md" {
		t.Fatalf("partial file_path = %q, want chapters/ch01.md", got)
	}
}

func TestToolOrchestratorAllowsEscapedSpecialCharactersInJSONArguments(t *testing.T) {
	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "write_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{"file_path":"chapters/ch01.md","content":"过了一遍。\\n\\n韩十四。武监司。三十\n\t^\n\""}`)
	if err != nil {
		t.Fatal(err)
	}
	if !called || !strings.Contains(result, "ok") {
		t.Fatalf("escaped special characters should pass through, called=%v result=%s", called, result)
	}
}

func TestToolOrchestratorBlocksMalformedJSONArgumentsForStream(t *testing.T) {
	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE}
	called := false
	endpoint, err := middleware.WrapStreamableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (*schema.StreamReader[string], error) {
			called = true
			return singleChunkReader("ok"), nil
		},
		&adk.ToolContext{Name: "write_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := endpoint(context.Background(), "{\"file_path\":\"chapters/ch01.md\",\"content\":\"过了一遍\n\t^\n")
	if err != nil {
		t.Fatal(err)
	}
	result, recvErr := reader.Recv()
	if recvErr != nil {
		t.Fatal(recvErr)
	}
	if _, eofErr := reader.Recv(); eofErr != io.EOF {
		t.Fatalf("expected stream EOF after block message, got %v", eofErr)
	}
	if called {
		t.Fatal("malformed JSON stream arguments should be blocked before endpoint is called")
	}
	if !strings.Contains(result, "参数不是完整 JSON 对象") {
		t.Fatalf("unexpected malformed-arguments stream result: %s", result)
	}
}

func TestToolOrchestratorBlocksDisabledCapability(t *testing.T) {
	middleware := &toolOrchestratorMiddleware{
		agentKind:           AgentKindIDE,
		enforceToolSettings: true,
		toolSettings:        config.ResolvedAgentToolSettings{FileRead: true},
	}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "write_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(context.Background(), `{"file_path":"chapters/ch01.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("disabled file_write capability should block before endpoint is called")
	}
	if !strings.Contains(result, "file_write") || !strings.Contains(result, "disabled for this Agent") {
		t.Fatalf("unexpected disabled capability result: %s", result)
	}
}

func TestToolOrchestratorTruncatesStreamResultWhenLimitConfigured(t *testing.T) {
	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE, toolResultMaxBytes: 64}
	endpoint, err := middleware.WrapStreamableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (*schema.StreamReader[string], error) {
			return singleChunkReader(strings.Repeat("流式正文", 100)), nil
		},
		&adk.ToolContext{Name: "read_file", CallID: "call-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := endpoint(context.Background(), `{"path":"chapters/ch01.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	result, recvErr := reader.Recv()
	if recvErr != nil {
		t.Fatal(recvErr)
	}
	if _, eofErr := reader.Recv(); eofErr != io.EOF {
		t.Fatalf("expected stream EOF after filtered result, got %v", eofErr)
	}
	if !strings.Contains(result, "[tool result truncated]") ||
		!strings.Contains(result, "truncated: true") {
		t.Fatalf("configured stream limit should truncate result: %s", result)
	}
}

func TestToolInjectionAdmissionBlocksLargeRead(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 大文件：整章量级（几千行），默认 limit 读取应被准入控制拦截。
	big := strings.Repeat("这是一行很长很长的中文正文内容，用来验证整章全量读取会不会被准入控制拦截。\n", 3000)
	if err := os.WriteFile(filepath.Join(workspace, "chapters", "ch00001-正文.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger, err := newRunLedger(workspace, RunLedgerPolicy{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	observer := newRunObserver(ledger, "root-span")
	observer.RecordBudgetSnapshot(60000, 65536) // projected 已接近工具安全窗口
	ctx := ContextWithRunObserver(context.Background(), observer)

	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE, workspace: workspace}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "read_file", CallID: "call-read-big"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(ctx, `{"file_path":"chapters/ch00001-正文.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("large read should be blocked before execution")
	}
	if !strings.Contains(result, "tool_injection_blocked") || !strings.Contains(result, "offset+limit") {
		t.Fatalf("expected injection-blocked guidance, got: %s", result)
	}
}

func TestToolInjectionAdmissionAllowsSmallRead(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "chapters", "ch01.md"), []byte("短正文内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger, err := newRunLedger(workspace, RunLedgerPolicy{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	observer := newRunObserver(ledger, "root-span")
	observer.RecordBudgetSnapshot(60000, 65536)
	ctx := ContextWithRunObserver(context.Background(), observer)

	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE, workspace: workspace}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "read_file", CallID: "call-read-small"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(ctx, `{"file_path":"chapters/ch01.md","limit":20}`)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatalf("small read should execute, got blocked: %s", result)
	}
}

func TestEstimateReadFileInjectionTokens(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "big.md"), []byte(strings.Repeat("内容\n", 5000)), 0o644); err != nil {
		t.Fatal(err)
	}
	tokens, err := estimateReadFileInjectionTokens(workspace, `{"file_path":"big.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if tokens <= 0 {
		t.Fatalf("expected positive estimate, got %d", tokens)
	}
	if tokens > 200000 {
		t.Fatalf("estimate unexpectedly large: %d", tokens)
	}
}

func TestNewFilesystemMiddlewareRespectsToolSettings(t *testing.T) {
	backend, err := localbk.NewBackend(context.Background(), &localbk.Config{})
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := newFilesystemMiddleware(context.Background(), backend, backend, config.ResolvedAgentToolSettings{
		FileRead:     true,
		FileWrite:    false,
		ShellExecute: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if middleware == nil {
		t.Fatal("filesystem middleware should be registered when read tools are enabled")
	}
	_, runCtx, err := middleware.BeforeAgent(context.Background(), &adk.ChatModelAgentContext{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, item := range runCtx.Tools {
		info, err := item.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		names[info.Name] = true
	}
	for _, name := range []string{"ls", "read_file", "glob", "grep"} {
		if !names[name] {
			t.Fatalf("read tool %s should be registered, names=%v", name, names)
		}
	}
	for _, name := range []string{"write_file", "edit_file", "execute"} {
		if !names[name] {
			t.Fatalf("tool %s should keep a stable schema and be blocked by orchestrator, names=%v", name, names)
		}
	}
}

func TestToolInjectionAdmissionAccountsForCumulativeInjection(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("这是一行很长很长的中文正文内容，用来验证批内累计注入会不会被准入拦截。\n", 3000)
	if err := os.WriteFile(filepath.Join(workspace, "chapters", "ch00001-正文.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger, err := newRunLedger(workspace, RunLedgerPolicy{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	observer := newRunObserver(ledger, "root-span")
	// projected 较低（30000）：单次读大文件 30000+33333=63333<65536 会放行；
	// 但同一批已注入 30000 后，30000+30000+33333=93333>65536 应拒绝——这正是
	// "跨多次工具调用的累计注入上限"要拦的场景。
	observer.RecordBudgetSnapshot(30000, 65536)
	observer.AddInjectedTokens(30000)
	ctx := ContextWithRunObserver(context.Background(), observer)

	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE, workspace: workspace}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "read_file", CallID: "call-read-cumulative"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(ctx, `{"file_path":"chapters/ch00001-正文.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("cumulative-injected read should be blocked before execution")
	}
	if !strings.Contains(result, "tool_injection_blocked") {
		t.Fatalf("expected injection-blocked, got: %s", result)
	}
}

func TestToolInjectionAdmissionAllowsWithinCumulativeBudget(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "chapters", "ch01.md"), []byte("短正文"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger, err := newRunLedger(workspace, RunLedgerPolicy{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	observer := newRunObserver(ledger, "root-span")
	observer.RecordBudgetSnapshot(10000, 65536)
	observer.AddInjectedTokens(20000) // 已注入 2w，但小读取估算很小，仍应放行
	ctx := ContextWithRunObserver(context.Background(), observer)

	middleware := &toolOrchestratorMiddleware{agentKind: AgentKindIDE, workspace: workspace}
	called := false
	endpoint, err := middleware.WrapInvokableToolCall(
		context.Background(),
		func(context.Context, string, ...tool.Option) (string, error) {
			called = true
			return "ok", nil
		},
		&adk.ToolContext{Name: "read_file", CallID: "call-read-ok"},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := endpoint(ctx, `{"file_path":"chapters/ch01.md","limit":10}`)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatalf("small read within cumulative budget should execute, got: %s", result)
	}
}

func TestBudgetSnapshotResetClearsCumulativeInjection(t *testing.T) {
	observer := newRunObserver(nil, "root-span")
	observer.RecordBudgetSnapshot(1000, 65536)
	observer.AddInjectedTokens(5000)
	if got := observer.InjectedTokensSinceSnapshot(); got != 5000 {
		t.Fatalf("cumulative = %d, want 5000", got)
	}
	// 新一轮模型请求（快照重置）后，累计清零——上一批结果已进入下一轮 projected。
	observer.RecordBudgetSnapshot(6000, 65536)
	if got := observer.InjectedTokensSinceSnapshot(); got != 0 {
		t.Fatalf("cumulative after snapshot reset = %d, want 0", got)
	}
}

// TestApplyToolResultBatchBudgetAccumulatesBytes 验证一条消息内并行工具结果总和
// 超过聚合预算时累计字节正确；未设上限或无 observer 时不强制外置。
func TestApplyToolResultBatchBudgetAccumulatesBytes(t *testing.T) {
	dir := t.TempDir()
	store, err := NewResultStore(dir, "sess")
	if err != nil {
		t.Fatal(err)
	}
	const batchLimit = 200 * 1024
	mw := &toolOrchestratorMiddleware{
		agentKind:                 AgentKindIDE,
		resultStore:               store,
		toolResultMaxBytes:        defaultToolResultMaxBytes,
		toolResultBatchLimitBytes: batchLimit,
	}
	ctx := context.Background()

	// 无 observer 时直接返回过滤结果（不累计字节）。
	// 使用 20KB（低于 read_file 外置阈值 32KB），隔离聚合预算行为。
	filtered := mw.applyToolResultPolicy(ctx, "read_file", `{"file_path":"d"}`, strings.Repeat("q", 20*1024))
	if filtered.Externalized {
		t.Fatal("no observer should not externalize for budget")
	}

	obs := newRunObserver(nil, "root-span")
	ctx = ContextWithRunObserver(ctx, obs)

	// 前两次各 20KB，累计仍在预算内（<= limit），未外置（< 32KB read_file 阈值）。
	for i := 0; i < 2; i++ {
		f := mw.applyToolResultPolicy(ctx, "read_file", `{"file_path":"d"}`, strings.Repeat("q", 20*1024))
		if f.Externalized {
			t.Fatalf("call %d: within budget result should not externalize", i)
		}
	}
	cumAfterTwo := obs.InjectedBytesSinceSnapshot()
	if cumAfterTwo <= 20*1024 || cumAfterTwo >= batchLimit {
		t.Fatalf("cumulative after two calls = %d, want (20KB, 200KB)", cumAfterTwo)
	}

	// 第三次 20KB，累计仍在预算内；且 < 32KB 阈值所以仍不外置。
	f := mw.applyToolResultPolicy(ctx, "read_file", `{"file_path":"d"}`, strings.Repeat("q", 20*1024))
	if f.Externalized {
		t.Fatal("small over-budget result stays inline")
	}
	if got := obs.InjectedBytesSinceSnapshot(); got <= cumAfterTwo {
		t.Fatalf("cumulative should grow after third call: %d -> %d", cumAfterTwo, got)
	}
}

// TestApplyToolResultBatchBudgetExternalizesOverLimit 验证超预算且超过单结果阈值的结果被外置降级。
func TestApplyToolResultBatchBudgetExternalizesOverLimit(t *testing.T) {
	dir := t.TempDir()
	store, err := NewResultStore(dir, "sess")
	if err != nil {
		t.Fatal(err)
	}
	// 预算 15KB < 单结果 20KB（低于 read_file 外置阈值 32KB），
	// 单结果路径不外置，但累计超预算 → 聚合预算路径外置降级。
	const batchLimit = 15 * 1024
	mw := &toolOrchestratorMiddleware{
		agentKind:                 AgentKindIDE,
		resultStore:               store,
		toolResultMaxBytes:        defaultToolResultMaxBytes,
		toolResultBatchLimitBytes: batchLimit,
	}
	obs := newRunObserver(nil, "root-span")
	ctx := ContextWithRunObserver(context.Background(), obs)

	// 20KB > 15KB 预算，但 < 32KB read_file 阈值 → 聚合预算路径外置。
	f := mw.applyToolResultPolicy(ctx, "read_file", `{"file_path":"d"}`, strings.Repeat("q", 20*1024))
	if !f.Externalized {
		t.Fatal("over-budget result should externalize via batch budget path")
	}
}
