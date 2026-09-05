---
name: image-analysis
description: 审核图片分析结果并整理入库。用户在图片分析页面完成提取后，使用本 Skill 在对话中审核提取内容（台词、角色、设定等），去重纠错后写入资料库和正式文件。Review extracted content from image analysis, de-duplicate, and persist to workspace files and lore database.
agent: ide
---

# 图片分析结果审核与入库

用户在**图片分析页面**上传图片完成分析后，提取结果会展示在页面中，同时后端会把原始 JSON 结果保存在项目目录下的 `assets/analysis/batch-{id}/result.md`。

本 Skill 在 **Agent 对话**中被调用，负责审核这些结果并整理入库。

## 前置条件

- 用户已在图片分析页面完成至少一个批次的图片分析
- 后端已将结果保存到项目 `assets/analysis/batch-{id}/result.md`
- 用户来到 Agent 对话，请求审核提取结果

## 工作流程

### 阶段一：读取并展示

1. 确认批次 ID（用户提供，或使用 `list_image_analysis_batches` 查找最近的已完成批次）。
2. 读取 `{{PROJECT_DIR}}/assets/analysis/batch-{id}/result.md` 获取提取结果（路径前缀为 `.denova/projects/<书名>/`，用 `ls` 先确认目录结构再读取）。
3. 在对话中按意图分组展示摘要，标注置信度：
   - 🔴 `type: "other"` 且内容含"模糊/无法识别" → 低置信，建议丢弃
   - 🟡 角色名用描述代替（如"黑发少年"）→ 可能是新角色，需用户命名
   - 🟡 多页间矛盾的信息 → 需人工核对
4. 询问用户：全部接受 / 部分保留 / 调整重新分析？

### 阶段二：写入文件（用户确认后）

5. 按用户确认范围写入：
   - **台词脚本** → `write_file` 写入 `script/dialogue.md`（按页整理，标注页码和角色）
   - **大纲线索** → `write_file` 写入 `setting/outline-from-images.md`（与正式大纲隔离）
   - **进度线索** → `edit_file` 追加到 `setting/progress.md`
   - **灵感碎片** → `write_file` 追加到 `setting/inspiration.md`
   - **角色状态快照** → `edit_file` 更新 `setting/character-states.md`

### 阶段三：整理入库（可选，用户提出时执行）

6. 从提取内容中识别长期稳定设定，去重后 `write_lore_items` 批量入库。
7. 不使用 `lore-init`（那是交互式从零创建），这里已有具体内容，直接写。
8. 如需结构化大纲，可建议调用 `outline` skill 整合碎片线索。

## 与其他 Skill 的关系

| 场景 | 在哪操作 | 用什么 |
|------|----------|--------|
| 上传图片、启动分析、看进度 | **图片分析页面**（UI 面板） | 不需要 skill |
| 审核提取结果、去重纠错 | **Agent 对话** | 本 skill |
| 将审核后的设定写入资料库 | **Agent 对话** | 本 skill → `write_lore_items` |
| 把碎片线索整理为正式大纲 | **Agent 对话** | `outline` skill |
| 从零讨论新建资料库 | **Agent 对话** | `lore-init` skill |

## 注意事项

- 本 skill **不负责上传图片和启动分析**——那是图片分析页面（UI）的事。
- 本 skill 的入口是**已有分析结果文件**，用户在对话中说"帮我审核一下刚才的分析结果"。
- OCR 和模型分析存在误差，必须展示摘要等用户确认后才写入。
- `setting/outline-from-images.md` 是临时线索，不覆盖正式 `setting/outline.md`。
- 对话气泡文字保留汉化组原文，不做改写。
