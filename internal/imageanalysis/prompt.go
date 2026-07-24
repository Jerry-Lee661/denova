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
	sb.WriteString("\n## 规则\n\n")
	sb.WriteString("- 用中文输出所有提取内容。\n")
	sb.WriteString("- content 字段使用 Markdown 格式。\n")
	sb.WriteString("- 如果图片内容模糊或无法识别，在 items 中添加 type 为 \"other\" 的条目说明情况。\n")
	sb.WriteString("- 不要编造图片中不存在的内容。\n")
	sb.WriteString("- 只输出 JSON，不要输出其他文字。\n")
	return sb.String()
}

// pageInstruction returns the user prompt for analyzing a single page.
func pageInstruction(pageIndex int, fileName string, intents []AnalysisIntent) string {
	intentNames := make([]string, 0, len(intents))
	for _, intent := range intents {
		intentNames = append(intentNames, string(intent))
	}
	return fmt.Sprintf("请分析第 %d 页（文件名：%s）。\n分析意图：%s。\n请仔细查看图片内容，按系统提示的 JSON 格式输出提取结果。",
		pageIndex+1, fileName, strings.Join(intentNames, "、"))
}
