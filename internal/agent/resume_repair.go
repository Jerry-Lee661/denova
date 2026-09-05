package agent

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// repairResumeStructure 修复中断恢复时的消息结构问题（Plan.md Phase 5）：
//
//  1. 悬空工具调用：assistant 消息带有 ToolCalls 但没有对应的 tool 结果
//     （运行在工具执行前被中断）。修复：移除未配对的 ToolCalls，保留正文。
//  2. 角色交替：连续同角色消息合并（如中断后用户发"继续"导致两个连续 user 消息）。
//
// 该修复只作用于送入模型的投影，不修改持久化历史。
func repairResumeStructure(messages []*schema.Message) []*schema.Message {
	if len(messages) <= 1 {
		return messages
	}
	messages = removeDanglingToolCalls(messages)
	messages = mergeConsecutiveSameRole(messages)
	return messages
}

// removeDanglingToolCalls 移除没有对应 tool 结果的 assistant ToolCalls。
// 收集所有 tool 结果的 ToolCallID，然后对每个 assistant 消息，
// 移除其 ToolCalls 中 ID 不在结果集合中的调用（悬空调用）。
func removeDanglingToolCalls(messages []*schema.Message) []*schema.Message {
	resultIDs := make(map[string]struct{})
	for _, msg := range messages {
		if msg == nil || msg.Role != schema.Tool {
			continue
		}
		if id := strings.TrimSpace(msg.ToolCallID); id != "" {
			resultIDs[id] = struct{}{}
		}
	}

	changed := false
	result := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil || msg.Role != schema.Assistant || len(msg.ToolCalls) == 0 {
			result = append(result, msg)
			continue
		}
		kept := make([]schema.ToolCall, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			if id := strings.TrimSpace(call.ID); id != "" {
				if _, ok := resultIDs[id]; ok {
					kept = append(kept, call)
					continue
				}
			}
			// ID 为空或没有对应结果 → 悬空，移除
		}
		if len(kept) == len(msg.ToolCalls) {
			result = append(result, msg)
			continue
		}
		changed = true
		next := *msg
		next.ToolCalls = kept
		result = append(result, &next)
	}
	if !changed {
		return messages
	}
	return result
}

// mergeConsecutiveSameRole 合并连续同角色消息，保证角色交替合法。
// user 消息：拼接 content（换行分隔）。
// assistant 消息：拼接 content，合并 ToolCalls。
// tool 消息：不合并（tool 消息紧跟 assistant，正常不会连续出现）。
// 压缩摘要消息（isContextCompactionMessage）是系统生成的 user 角色消息，
// 不参与合并，避免与后续真实用户消息粘连。
func mergeConsecutiveSameRole(messages []*schema.Message) []*schema.Message {
	if len(messages) <= 1 {
		return messages
	}
	result := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			result = append(result, msg)
			continue
		}
		if len(result) > 0 {
			last := result[len(result)-1]
			if last != nil && last.Role == msg.Role && last.Role != schema.Tool && !isContextCompactionMessage(last) && !isContextCompactionMessage(msg) {
				merged := *last
				if last.Content != "" && msg.Content != "" {
					merged.Content = last.Content + "\n" + msg.Content
				} else {
					merged.Content = last.Content + msg.Content
				}
				if len(msg.ToolCalls) > 0 {
					merged.ToolCalls = append(merged.ToolCalls, msg.ToolCalls...)
				}
				result[len(result)-1] = &merged
				continue
			}
		}
		result = append(result, msg)
	}
	return result
}
