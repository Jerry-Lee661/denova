package imageanalysis

import (
	"fmt"
	"strings"
)

// systemInstruction returns the system prompt for the vision model.
func systemInstruction(intents []AnalysisIntent) string {
	var sb strings.Builder
	sb.WriteString("你是一个专业的漫画/小说页面分析助手。你的任务是仔细阅读用户提供的图片（漫画页、小说扫描页、手写大纲照片等），提取结构化的创作内容。\n\n")
	sb.WriteString("## 输出格式\n\n")
	sb.WriteString("你必须以 JSON 格式输出，结构如下：\n")
	sb.WriteString("```json\n{\n  \"items\": [\n    {\"type\": \"character|scene|plot_point|world_building|dialogue|other\", \"content\": \"提取的内容\"}\n  ]\n}\n```\n\n")
	sb.WriteString("## 提取要求\n\n")
	sb.WriteString("根据用户指定的分析意图，重点提取以下内容：\n\n")
	for _, intent := range intents {
		switch intent {
		case IntentOutline:
			sb.WriteString("- **大纲**：识别故事结构、章节划分、主要剧情线、起承转合、伏笔和转折点。提取每页的核心事件摘要。\n")
		case IntentProgress:
			sb.WriteString("- **进度**：识别当前故事进展到哪一步、已完成了哪些情节、下一步可能的发展方向。\n")
		case IntentInspiration:
			sb.WriteString("- **灵感**：提取有创意的设定、独特的表现手法、有趣的对话、值得借鉴的叙事技巧。\n")
		case IntentState:
			sb.WriteString("- **角色状态**：识别角色的外貌变化、情绪状态、当前位置、持有物品、人际关系变化、能力变化。\n")
		case IntentLore:
			sb.WriteString("- **资料库**：提取角色身份设定、世界观规则、地点描述、势力关系、能力体系、关键物品等长期稳定设定。\n")
		}
	}
	sb.WriteString("\n## 台词提取（重要）\n\n")
	sb.WriteString("默认输入为日文漫画的汉化版页面。请逐格 OCR 识别对话气泡和旁白框中的文字，以 type=\"dialogue\" 条目输出：\n")
	sb.WriteString("- content 格式：`[角色名或描述]：\"对话内容\"`，旁白不加角色名\n")
	sb.WriteString("- 尽可能完整还原气泡中的原文，不要总结或改写\n")
	sb.WriteString("- 如果无法识别具体角色名，用描述替代（如\"黑发少年\"、\"戴眼镜的女性\"）\n")
	sb.WriteString("- 多个气泡按阅读顺序排列\n")
	sb.WriteString("- **禁止提取拟声词/拟态词**（如「あっ」「んっ」「ぐじゅ」「ドキドキ」「ザワザワ」等）为 dialogue。拟声词不是台词，不要作为对话输出。\n\n")
	sb.WriteString("## 规则\n\n")
	sb.WriteString("- 用中文输出所有提取内容。\n")
	sb.WriteString("- content 字段使用 Markdown 格式。\n")
	sb.WriteString("- 如果图片内容模糊或无法识别，在 items 中添加 type 为 \"other\" 的条目说明情况。\n")
	sb.WriteString("- 不要编造图片中不存在的内容。\n")
	sb.WriteString("- 只输出 JSON，不要输出其他文字。\n")
	sb.WriteString("- **数据密集型页面**（角色状态卡、统计表、设定参数页等）：必须尽可能完整保留页面上的所有数值、字段名和对应数据，按原格式逐项列出，不要省略或概括。例如身高/体重/三围/技能等级/行为统计等每一项都要独立提取。\n")
	return sb.String()
}

// pageInstruction returns the user prompt for analyzing a single page.
// prevPageSummary provides cross-page context from the previous page's
// extraction (truncated); empty string disables injection.
func pageInstruction(pageIndex int, fileName string, intents []AnalysisIntent, prevPageSummary string) string {
	intentNames := make([]string, 0, len(intents))
	for _, intent := range intents {
		intentNames = append(intentNames, string(intent))
	}
	base := fmt.Sprintf("请分析第 %d 页（文件名：%s）。\n分析意图：%s。",
		pageIndex+1, fileName, strings.Join(intentNames, "、"))
	if prevPageSummary != "" {
		base += fmt.Sprintf("\n\n## 上一页摘要（供参考，不要重复提取已有内容）\n%s", prevPageSummary)
	}
	base += "\n\n请仔细查看图片内容，按系统提示的 JSON 格式输出提取结果。"
	return base
}

// panelEnumerationInstruction is the stage-1 system prompt for panel-aware
// extraction. It asks the model to enumerate all visible panels in reading
// order without deep analysis, producing a lightweight JSON list.
func panelEnumerationInstruction() string {
	return `你是漫画分镜识别助手。请按阅读顺序逐格扫描图片，列出每个分镜。

日式漫画阅读顺序：从右上到左下（先右上格，再向左，然后下一行从右开始）。
美式/条漫阅读顺序：从左上到右下（或从上到下）。
请根据画面布局判断阅读顺序。

输出 JSON：
` + "```json\n{\n  \"panels\": [\n    {\"index\": 1, \"summary\": \"一句话概述本格画面内容\", \"has_dialogue\": true}\n  ]\n}\n```" + `

规则：
- 不要遗漏任何分镜，包括无对话的动作格、特写格、跨页大格
- summary 只描述画面可见内容，不推断剧情
- has_dialogue 为该格是否包含对话气泡或旁白文字
- 只输出 JSON，不要输出其他文字`
}

// panelExtractionInstruction is the stage-2 user prompt for deep extraction
// of a single panel. It reuses the main systemInstruction but focuses the
// model on one panel identified by its summary from stage 1.
func panelExtractionInstruction(pageIndex int, fileName string, intents []AnalysisIntent, panelIndex int, panelSummary string) string {
	intentNames := make([]string, 0, len(intents))
	for _, intent := range intents {
		intentNames = append(intentNames, string(intent))
	}
	return fmt.Sprintf("请分析第 %d 页（文件名：%s）的第 %d 个分镜。\n该分镜概述：%s\n分析意图：%s。\n请针对此分镜内容，按系统提示的 JSON 格式输出提取结果。",
		pageIndex+1, fileName, panelIndex, panelSummary, strings.Join(intentNames, "、"))
}
