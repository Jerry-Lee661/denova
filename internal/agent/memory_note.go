package agent

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"

	"denova/config"
	"denova/internal/session"
)

// MemoryNoteWriter persists structured background notes written while a
// conversation progresses (Plan.md M7). Compaction reads them to seed the next
// checkpoint instead of re-summarizing raw turns.
type MemoryNoteWriter interface {
	AppendMemoryNote(note session.MemoryNote) error
}

// MemoryNoteGenerator produces one bounded structured note from the incremental
// source plus any existing notes. It is model-backed and stubbed in tests.
type MemoryNoteGenerator func(ctx context.Context, cfg *config.Config, agentKind string, existingNotes []session.MemoryNote, source []*schema.Message) (session.MemoryNote, error)

var generateMemoryNote MemoryNoteGenerator = generateMemoryNoteFromModel

// SetMemoryNoteGeneratorForTest swaps the note generator so tests can capture or
// stub it. Callers must restore the previous generator.
func SetMemoryNoteGeneratorForTest(fn MemoryNoteGenerator) {
	generateMemoryNote = fn
}

// MemoryNoteSystemInstruction is the system prompt for the background note
// writer. It keeps the note focused on goals/progress/conclusions with bounded
// length so compaction can reuse it directly.
func MemoryNoteSystemInstruction() string {
	return strings.TrimSpace(`
你是 Denova 的后台记忆笔记写入器，负责在对话推进过程中持续写一份结构化笔记（Plan.md M7）。

你的任务：
- 基于新增的有效上下文与已有的笔记，增量更新一份结构化笔记。
- 笔记只保留会对后续剧情、写作任务或用户意图产生长期影响的信息。
- 不要删除旧笔记中的长期影响信息，除非新增上下文明确说明该信息已经失效、解决或被推翻。
- 如果出现矛盾，不要自行修正；保留矛盾并标记为“待确认矛盾”。
- 游戏 Turn 输入包含 source turn_id 时，事件和因果结论必须保留相应 turn_id；无法确定来源时明确标记来源缺失，不得自造 ID。

输入可能包含：
1. existing_notes：此前后台写入器产出的笔记，可能为空。
2. new_context：上次写入后新增的原始有效对话链或互动回合链。

处理目标：
- 将 existing_notes 与 new_context 合并，输出一份更新后的结构化笔记。
- 如果 existing_notes 为空，则从 new_context 初始化；否则增量更新，不要重复记录同一事件。

笔记结构：
【目标】
- 仍会影响后续行为的目标、偏好、任务边界与已确认决策

【进展】
- 已完成事项及其最终结果与遗留影响
- 未完成事项、伏笔、承诺、债务、秘密、危险、倒计时

【关键结论】
- 角色关系/状态变化、世界/阵营变化、物品资源变动、能力变化、线索与未解谜团
- 因果与来源（← 原因与 source turn_id）；矛盾和不确定性在这里标明

约束：
- 不要写成小说文风；要写成清晰、紧凑、可供后续模型继续阅读的结构化要点。
- 排除 thinking/reasoning 内容、传输噪音、展示用日志、重复工具卡片和无结果的实现过程。
- 禁止编造事实；不确定时明确标记“不确定”。
- 目标长度由输入字符数控制，默认是输入字符数的 5%-20%；信息密度高时使用上半区。
`)
}

// formatMemoryNote renders one note for display and for the next writer pass.
func formatMemoryNote(note session.MemoryNote) string {
	var sb strings.Builder
	title := strings.TrimSpace(note.Title)
	if title == "" {
		title = "本轮笔记"
	}
	sb.WriteString(fmt.Sprintf("=== %s ===\n", title))
	if strings.TrimSpace(note.Goals) != "" {
		sb.WriteString("【目标】\n")
		sb.WriteString(strings.TrimSpace(note.Goals))
		sb.WriteString("\n\n")
	}
	if strings.TrimSpace(note.Progress) != "" {
		sb.WriteString("【进展】\n")
		sb.WriteString(strings.TrimSpace(note.Progress))
		sb.WriteString("\n\n")
	}
	if strings.TrimSpace(note.Conclusions) != "" {
		sb.WriteString("【关键结论】\n")
		sb.WriteString(strings.TrimSpace(note.Conclusions))
		sb.WriteString("\n\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// buildMemoryNoteInput composes the model input: existing notes followed by the
// incremental source, with a bounded character count for length control.
func buildMemoryNoteInput(existingNotes []session.MemoryNote, source []*schema.Message) (string, int) {
	var sb strings.Builder
	for i, note := range existingNotes {
		block := formatMemoryNote(note)
		if block == "" {
			continue
		}
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(block)
	}
	existingChars := countRunes(sb.String())
	newBlocks := make([]string, 0, len(source))
	for i, msg := range source {
		if msg == nil {
			continue
		}
		newBlocks = append(newBlocks, formatCompactionMessage(i+1, msg))
	}
	newContext := strings.Join(newBlocks, "")
	total := existingChars + countRunes(newContext)
	input := sb.String()
	if newContext != "" {
		if input != "" {
			input += "\n\n"
		}
		input += "<new_context>\n" + newContext + "\n</new_context>"
	}
	return input, total
}

// generateMemoryNoteFromModel runs the background note writer against the model.
// The note follows the agent whose context is being noted so a stale profile does
// not route notes through an old model.
func generateMemoryNoteFromModel(ctx context.Context, cfg *config.Config, agentKind string, existingNotes []session.MemoryNote, source []*schema.Message) (session.MemoryNote, error) {
	modelCfg := chatModelConfigForAgent(cfg, agentKind)
	cm, err := openai.NewChatModel(ctx, &modelCfg)
	if err != nil {
		return session.MemoryNote{}, fmt.Errorf("创建后台笔记模型失败: %w", err)
	}
	systemPrompt := protectedSystemInstruction(cfg, config.AgentKindContextCompaction, MemoryNoteSystemInstruction())
	input, _ := buildMemoryNoteInput(existingNotes, source)
	if strings.TrimSpace(input) == "" {
		// No incremental content to note; return an empty note so callers can skip.
		return session.MemoryNote{AgentKind: agentKind}, nil
	}
	msg, err := streamNoteAttempt(ctx, cm, systemPrompt, input)
	if err != nil {
		return session.MemoryNote{}, fmt.Errorf("后台笔记生成失败: %w", err)
	}
	return parseMemoryNoteContent(agentKind, strings.TrimSpace(msg.Content)), nil
}

// parseMemoryNoteContent extracts the structured fields from a raw note body.
func parseMemoryNoteContent(agentKind, content string) session.MemoryNote {
	note := session.MemoryNote{AgentKind: agentKind}
	body := strings.TrimSpace(content)
	if body == "" {
		return note
	}
	note.Title = "本轮笔记"
	note.Goals = extractSection(body, "目标")
	note.Progress = extractSection(body, "进展")
	note.Conclusions = extractSection(body, "关键结论")
	return note
}

// extractSection pulls the text under one section header from a structured note.
func extractSection(body, title string) string {
	var sb strings.Builder
	collecting := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "【") && strings.Contains(trimmed, title) {
			collecting = true
			continue
		}
		if collecting {
			if strings.HasPrefix(trimmed, "【") {
				break
			}
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func streamNoteAttempt(ctx context.Context, cm *openai.ChatModel, systemPrompt, userText string) (*schema.Message, error) {
	input := []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(userText),
	}
	stream, err := cm.Stream(ctx, input)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var chunks []*schema.Message
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if msg == nil {
			continue
		}
		chunks = append(chunks, msg)
	}
	return schema.ConcatMessages(chunks)
}
