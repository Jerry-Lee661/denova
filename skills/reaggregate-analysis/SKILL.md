---
name: reaggregate-analysis
description: 当用户输入 /reaggregate-analysis 时，用最新修复代码重新聚合已有图片分析批次，重新生成 fact_stream.md 和 result.md。Re-aggregate existing image analysis batches to regenerate fact_stream.md and result.md with latest fixes.
agent: ide
---

# 重新聚合图片分析结果

用 `go run ./cmd/reaggregate` 重新生成已有批次的 `fact_stream.md` 和 `result.md`。

## 规则

1. **不要加载其他 skill。**
2. **不要读取 state.json、result.md 或任何分析结果文件。**
3. **不要使用 glob、ls、grep、read_file 等文件工具。**
4. **只用 `execute` 工具运行一条命令。**

## 执行

确认用户要处理的项目路径后，直接运行（不要先搜索或列出文件）：

```bash
cd <项目根目录> && go run ./cmd/reaggregate "<项目路径>" --all
```

- 项目根目录：当前 Denova 项目的根目录
- 项目路径：`.denova/projects/<书名>`（注意：不是 `assets/`，不是项目根目录，必须包含 `.denova/projects/` 前缀）
- 单批次：把 `--all` 替换为 batchID

执行完毕后报告结果即可。
