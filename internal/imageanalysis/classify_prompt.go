package imageanalysis

import "strings"

// bookClassificationInstruction returns the system prompt for determining a
// document's overall nature from a sample of its first pages.
func bookClassificationInstruction() string {
	return `你是一个文档类型识别助手。用户会提供同一本书的前几页图片，请综合判断这本书属于哪种类型。

类型定义：
- **manga**（漫画）：页面由多个分镜格子组成，有对话气泡、拟声词、连续的动作分格。
- **novel**（小说）：页面以连续的文字段落为主（横排或竖排），可能有少量插图，但没有分镜格子。
- **illustration**（插图集/画集）：页面以整幅插图为主，文字极少或仅有标题/说明。
- **mixed**（混合）：上述类型在书页间明显交替出现，无法归为单一类型。

判断依据：
- 是否有分镜格子和对话气泡 → manga
- 是否大段连续文字 → novel
- 是否整页画面、文字稀少 → illustration
- 类型在页间交替 → mixed

只输出 JSON，不要输出其他文字：
` + "```json\n{\"type\": \"manga|novel|illustration|mixed\", \"confidence\": \"high|medium|low\", \"reason\": \"简短理由\"}\n```"
}

// systemInstructionForNovel returns the system prompt for extracting content
// from novel pages (continuous text, vertical or horizontal). Unlike the manga
// prompt, it does not assume speech bubbles or panel grids.
func systemInstructionForNovel(intents []AnalysisIntent) string {
	var sb strings.Builder
	sb.WriteString("你是一个专业的小说页面分析助手。你的任务是仔细阅读用户提供的小说页面图片（可能是竖排或横排文字），提取结构化的创作内容。\n\n")
	sb.WriteString("## 输出格式\n\n")
	sb.WriteString("你必须以 JSON 格式输出，结构如下：\n")
	sb.WriteString("```json\n{\n  \"items\": [\n    {\"type\": \"character|scene|plot_point|world_building|dialogue|other\", \"content\": \"提取的内容\"}\n  ]\n}\n```\n\n")
	sb.WriteString("## 阅读方向\n\n")
	sb.WriteString("- 日文竖排（縦書き）：从右向左、从上到下阅读。\n")
	sb.WriteString("- 横排：从左向右、从上到下阅读。\n")
	sb.WriteString("- 请根据页面排版判断阅读方向，按正确顺序还原文本。\n\n")
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
	sb.WriteString("\n## 正文与对话提取（重要）\n\n")
	sb.WriteString("请完整还原页面中的正文段落和角色对话，以 type=\"dialogue\" 条目输出对话：\n")
	sb.WriteString("- content 格式：`[角色名或描述]：\"对话内容\"`，旁白不加角色名\n")
	sb.WriteString("- 正文叙述段落以 type=\"scene\" 或 type=\"plot_point\" 输出\n")
	sb.WriteString("- 尽可能完整还原原文，不要总结或改写\n")
	sb.WriteString("- 如果页面包含插图，用 type=\"other\" 描述插图画面内容\n\n")
	sb.WriteString("## 规则\n\n")
	sb.WriteString("- 用中文输出所有提取内容。\n")
	sb.WriteString("- content 字段使用 Markdown 格式。\n")
	sb.WriteString("- 如果图片内容模糊或无法识别，在 items 中添加 type 为 \"other\" 的条目说明情况。\n")
	sb.WriteString("- 不要编造图片中不存在的内容。\n")
	sb.WriteString("- 只输出 JSON，不要输出其他文字。\n")
	return sb.String()
}

// systemInstructionForIllustration returns the system prompt for extracting
// content from illustration/artbook pages (full-page artwork, minimal text).
func systemInstructionForIllustration(intents []AnalysisIntent) string {
	var sb strings.Builder
	sb.WriteString("你是一个专业的插图/画集分析助手。你的任务是仔细观察用户提供的整页插图，提取结构化的创作内容。\n\n")
	sb.WriteString("## 输出格式\n\n")
	sb.WriteString("你必须以 JSON 格式输出，结构如下：\n")
	sb.WriteString("```json\n{\n  \"items\": [\n    {\"type\": \"character|scene|plot_point|world_building|dialogue|other\", \"content\": \"提取的内容\"}\n  ]\n}\n```\n\n")
	sb.WriteString("## 提取要求\n\n")
	sb.WriteString("整页描述画面内容，重点关注：\n")
	sb.WriteString("- **角色**（type=\"character\"）：外貌、服饰、姿态、表情、持有物品。\n")
	sb.WriteString("- **场景**（type=\"scene\"）：环境、背景、光影、氛围、色彩基调。\n")
	sb.WriteString("- **构图与技法**（type=\"other\"）：视角、构图、值得借鉴的表现手法。\n")
	sb.WriteString("- 如果图中有文字（标题、说明、签名），以 type=\"dialogue\" 完整还原。\n\n")
	sb.WriteString("## 规则\n\n")
	sb.WriteString("- 用中文输出所有提取内容。\n")
	sb.WriteString("- content 字段使用 Markdown 格式。\n")
	sb.WriteString("- 如果图片内容模糊或无法识别，在 items 中添加 type 为 \"other\" 的条目说明情况。\n")
	sb.WriteString("- 不要编造图片中不存在的内容。\n")
	sb.WriteString("- 只输出 JSON，不要输出其他文字。\n")
	return sb.String()
}
