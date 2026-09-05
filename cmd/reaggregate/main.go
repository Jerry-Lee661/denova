package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"denova/internal/imageanalysis"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "用法:\n")
		fmt.Fprintf(os.Stderr, "  单批次: go run ./cmd/reaggregate <workspace路径> <batchID>\n")
		fmt.Fprintf(os.Stderr, "  全批次: go run ./cmd/reaggregate <workspace路径> --all\n")
		os.Exit(1)
	}
	workspace := filepath.Clean(os.Args[1])
	arg := os.Args[2]

	svc := imageanalysis.NewService(nil, workspace)
	store := svc.Store()

	if arg == "--all" {
		processAll(workspace, svc, store)
		return
	}

	processOne(arg, svc, store)
}

func processOne(batchID string, svc *imageanalysis.Service, store *imageanalysis.Store) {
	state, err := store.Load(batchID)
	if err != nil {
		log.Fatalf("加载批次状态失败 [%s]: %v", batchID, err)
	}
	result := svc.Aggregate(state)
	saveResult(store, batchID, result)
	fmt.Printf("完成: batch=%s pages=%d success=%d failed=%d\n",
		batchID, result.TotalPages, result.SuccessCount, result.FailedCount)
}

func processAll(workspace string, svc *imageanalysis.Service, store *imageanalysis.Store) {
	base := filepath.Join(workspace, "assets", "analysis")
	entries, err := os.ReadDir(base)
	if err != nil {
		log.Fatalf("读取分析目录失败 [%s]: %v", base, err)
	}

	var batchIDs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "batch-") {
			continue
		}
		// Check state.json exists.
		if _, err := os.Stat(filepath.Join(base, name, "state.json")); err != nil {
			continue
		}
		batchIDs = append(batchIDs, strings.TrimPrefix(name, "batch-"))
	}

	if len(batchIDs) == 0 {
		fmt.Println("未找到任何包含 state.json 的批次。")
		return
	}

	sort.Strings(batchIDs)
	fmt.Printf("找到 %d 个批次，开始重新聚合...\n\n", len(batchIDs))

	okCount, failCount := 0, 0
	for _, id := range batchIDs {
		state, err := store.Load(id)
		if err != nil {
			fmt.Printf("  [失败] batch=%s: %v\n", id, err)
			failCount++
			continue
		}
		result := svc.Aggregate(state)
		saveResult(store, id, result)
		fmt.Printf("  [完成] batch=%s pages=%d success=%d failed=%d\n",
			id, result.TotalPages, result.SuccessCount, result.FailedCount)
		okCount++
	}

	fmt.Printf("\n处理完毕: 成功 %d，失败 %d，共 %d 批次\n", okCount, failCount, len(batchIDs))
}

func saveResult(store *imageanalysis.Store, batchID string, result *imageanalysis.BatchResult) {
	markdown := buildResultMarkdown(result)
	if err := store.SaveResult(batchID, markdown); err != nil {
		log.Printf("保存 result.md 失败 [%s]: %v", batchID, err)
	}
	if err := store.SaveFactStream(batchID, result.FactStream); err != nil {
		log.Printf("保存 fact_stream.md 失败 [%s]: %v", batchID, err)
	}
}

func buildResultMarkdown(r *imageanalysis.BatchResult) string {
	labels := map[imageanalysis.AnalysisIntent]string{
		"outline":     "大纲提取",
		"progress":    "进度分析",
		"inspiration": "灵感收集",
		"state":       "角色状态",
		"lore":        "资料库设定",
	}
	s := fmt.Sprintf("# 图片分析结果\n\n批次 ID：%s\n状态：%s\n总页数：%d，成功：%d，失败：%d\n",
		r.BatchID, r.Status, r.TotalPages, r.SuccessCount, r.FailedCount)

	for _, intent := range []imageanalysis.AnalysisIntent{
		"outline", "progress", "inspiration", "state", "lore",
	} {
		content, ok := r.Sections[intent]
		if !ok || content == "" {
			continue
		}
		s += fmt.Sprintf("\n## %s\n\n*来源：批次 %s，共 %d 页，成功 %d 页*\n\n%s\n",
			labels[intent], r.BatchID, r.TotalPages, r.SuccessCount, content)
	}
	return s
}
