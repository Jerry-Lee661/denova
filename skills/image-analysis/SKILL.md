---
name: image-analysis
description: 从上传的漫画/小说页面图片中提取结构化创作内容（大纲、进度、灵感、角色状态、资料库设定）。当用户要求分析图片、从图片提取设定、识别角色或场景时使用。Extract structured creative content from uploaded manga/novel page images.
agent: ide
---

# 图片分析

当用户上传了漫画页、小说扫描页、手写大纲照片或参考素材图片，并希望从中提取创作内容时使用本 Skill。

## 前置条件

图片分析由后端批量管线完成（`POST /api/image-analysis/batch`），分析结果保存在 `assets/analysis/batch-{id}/result.md`。本 Skill 指导你如何消费这些已完成的分析结果。

## 工作流程

1. 确认用户已完成图片上传和批量分析（检查 `assets/analysis/` 目录下是否有对应批次结果）。
2. 使用 `read_file` 读取 `assets/analysis/batch-{id}/result.md` 获取聚合分析结果。
3. 根据用户意图，将提取的内容写入对应文件：
   - **大纲** → 使用 `write_file` 或 `edit_file` 更新 `setting/outline.md`
   - **进度** → 使用 `edit_file` 更新 `setting/progress.md`
   - **灵感** → 使用 `write_file` 追加到 `setting/inspiration.md`
   - **角色状态** → 使用 `edit_file` 更新 `setting/character-states.md`
   - **资料库** → 使用 `write_lore_items` 批量写入资料库条目
4. 写入前先向用户展示提取结果摘要，获得确认后再执行写入。
5. 写入完成后，使用 `read_file` 读回验证关键片段已落盘。

## 写入规则

- 遵循 `writing-common` 的状态文件边界规则：大纲只记录长期结构，进度只记录当前进展，资料库只记录稳定设定。
- 从图片提取的内容可能包含不确定性，写入时应标注来源为"图片分析"。
- 资料库条目的 `brief_description` 以"类型 名称。"开头，`content` 使用中文 Markdown。
- 不要编造图片中不存在的内容；如果分析结果中有"其他"类型条目说明图片模糊，应告知用户。

## 注意事项

- 分析结果中的页码顺序已按文件名自然排序处理，不需要重新排序。
- 如果批次状态为 `partial`（部分失败），告知用户哪些页面分析失败，建议重试。
- 不要一次性将所有提取内容全部写入；先展示摘要，让用户选择要保留的部分。
- 图片分析是辅助参考，最终写入内容需要用户确认。
