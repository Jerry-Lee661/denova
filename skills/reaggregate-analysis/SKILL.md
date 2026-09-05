---
name: reaggregate-analysis
description: When the user inputs /reaggregate-analysis, re-aggregate existing image analysis batches with the latest fixed code to regenerate fact_stream.md and result.md.
agent: ide
---

# Re-aggregate Image Analysis Results

Regenerate the `fact_stream.md` and `result.md` of existing batches with `go run ./cmd/reaggregate`.

## Rules

1. **Do not load other skills.**
2. **Do not read state.json, result.md, or any analysis result files.**
3. **Do not use file tools such as glob, ls, grep, or read_file.**
4. **Run exactly one command with the `execute` tool.**

## Execution

After confirming the project path from the user, run directly (do not search or list files first):

```bash
cd <repo root> && go run ./cmd/reaggregate "<project path>" --all
```

- Repo root: the root of the Denova repository checkout
- Project path: `.denova/projects/<book name>` (note: not `assets/`, not the repo root; the path must keep the `.denova/projects/` prefix)
- Single batch: replace `--all` with the batchID

Report the command outcome when it finishes.
