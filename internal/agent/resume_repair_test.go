package agent

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestRemoveDanglingToolCallsRemovesUnpairedCalls(t *testing.T) {
	messages := []*schema.Message{
		schema.UserMessage("读一下文件"),
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "call-1", Function: schema.FunctionCall{Name: "read_file"}},
			{ID: "call-2", Function: schema.FunctionCall{Name: "write_file"}},
		}),
		schema.ToolMessage("文件内容", "call-1", schema.WithToolName("read_file")),
	}
	got := removeDanglingToolCalls(messages)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(got))
	}
	assistant := got[1]
	if assistant.Role != schema.Assistant {
		t.Fatalf("expected assistant, got %s", assistant.Role)
	}
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("expected 1 retained tool call, got %d", len(assistant.ToolCalls))
	}
	if assistant.ToolCalls[0].ID != "call-1" {
		t.Fatalf("expected call-1 retained, got %s", assistant.ToolCalls[0].ID)
	}
}

func TestRemoveDanglingToolCallsRemovesAllWhenNoResults(t *testing.T) {
	messages := []*schema.Message{
		schema.UserMessage("读一下文件"),
		schema.AssistantMessage("我来读取", []schema.ToolCall{
			{ID: "call-1", Function: schema.FunctionCall{Name: "read_file"}},
		}),
	}
	got := removeDanglingToolCalls(messages)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	assistant := got[1]
	if len(assistant.ToolCalls) != 0 {
		t.Fatalf("expected 0 tool calls, got %d", len(assistant.ToolCalls))
	}
	if assistant.Content != "我来读取" {
		t.Fatalf("expected content preserved, got %q", assistant.Content)
	}
}

func TestRemoveDanglingToolCallsKeepsPairedCalls(t *testing.T) {
	messages := []*schema.Message{
		schema.AssistantMessage("", []schema.ToolCall{
			{ID: "call-1", Function: schema.FunctionCall{Name: "read_file"}},
		}),
		schema.ToolMessage("内容", "call-1", schema.WithToolName("read_file")),
	}
	got := removeDanglingToolCalls(messages)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if len(got[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call retained, got %d", len(got[0].ToolCalls))
	}
}

func TestMergeConsecutiveSameRoleMergesUserMessages(t *testing.T) {
	messages := []*schema.Message{
		schema.UserMessage("第一条"),
		schema.UserMessage("第二条"),
	}
	got := mergeConsecutiveSameRole(messages)
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].Role != schema.User {
		t.Fatalf("expected user, got %s", got[0].Role)
	}
	if got[0].Content != "第一条\n第二条" {
		t.Fatalf("expected merged content, got %q", got[0].Content)
	}
}

func TestMergeConsecutiveSameRoleMergesAssistantToolCalls(t *testing.T) {
	messages := []*schema.Message{
		schema.AssistantMessage("开始", []schema.ToolCall{
			{ID: "call-1", Function: schema.FunctionCall{Name: "read_file"}},
		}),
		schema.AssistantMessage("继续", []schema.ToolCall{
			{ID: "call-2", Function: schema.FunctionCall{Name: "write_file"}},
		}),
	}
	got := mergeConsecutiveSameRole(messages)
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].Role != schema.Assistant {
		t.Fatalf("expected assistant, got %s", got[0].Role)
	}
	if len(got[0].ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(got[0].ToolCalls))
	}
	if got[0].Content != "开始\n继续" {
		t.Fatalf("expected merged content, got %q", got[0].Content)
	}
}

func TestMergeConsecutiveSameRoleKeepsAlternating(t *testing.T) {
	messages := []*schema.Message{
		schema.UserMessage("用户"),
		schema.AssistantMessage("助手", nil),
		schema.UserMessage("用户2"),
	}
	got := mergeConsecutiveSameRole(messages)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(got))
	}
}

func TestRepairResumeStructureFullScenario(t *testing.T) {
	// 模拟中断恢复：assistant 带悬空调用，用户发"继续"
	messages := []*schema.Message{
		schema.UserMessage("原始请求"),
		schema.AssistantMessage("我来处理", []schema.ToolCall{
			{ID: "call-1", Function: schema.FunctionCall{Name: "read_file"}},
		}),
		// 中断：call-1 没有结果，用户发"继续"
		schema.UserMessage("继续"),
	}
	got := repairResumeStructure(messages)
	// 期望：悬空调用移除，user/assistant/user 交替合法
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(got))
	}
	if got[0].Role != schema.User {
		t.Fatalf("expected user first, got %s", got[0].Role)
	}
	if got[1].Role != schema.Assistant {
		t.Fatalf("expected assistant second, got %s", got[1].Role)
	}
	if len(got[1].ToolCalls) != 0 {
		t.Fatalf("expected 0 tool calls, got %d", len(got[1].ToolCalls))
	}
	if got[2].Role != schema.User {
		t.Fatalf("expected user third, got %s", got[2].Role)
	}
}

func TestRepairResumeStructureMergesConsecutiveUserAfterInterruption(t *testing.T) {
	// 中断时 assistant 未持久化（空内容），用户发"继续"导致连续 user
	messages := []*schema.Message{
		schema.UserMessage("原始请求"),
		schema.UserMessage("继续"),
	}
	got := repairResumeStructure(messages)
	if len(got) != 1 {
		t.Fatalf("expected 1 message, got %d", len(got))
	}
	if got[0].Role != schema.User {
		t.Fatalf("expected user, got %s", got[0].Role)
	}
	if got[0].Content != "原始请求\n继续" {
		t.Fatalf("expected merged content, got %q", got[0].Content)
	}
}
