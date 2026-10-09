package tools

import (
	"context"
	"fmt"
	"strings"

	agent "github.com/alfredxw/denova/agent"

	"denova/config"
	"denova/internal/book/searchindex"
	"denova/internal/embedding"
)

const searchSnippetRunes = 600

type searchInput struct {
	Query string `json:"query" jsonschema:"description=Natural-language query or keywords describing what to find, in the author's language. Use when the exact wording is unknown, for example to recall where a scene, fact, or passage lives. Use grep instead for a known exact string."`
	Limit int    `json:"limit,omitempty" jsonschema:"description=Maximum number of ranked fragments, default 8, maximum 20."`
}

// searchToolDescriptor must stay in lockstep with the book_search capability
// definition in config/agent_registry.go; descriptor drift fails Agent
// construction through the manifest validator.
func searchToolDescriptor() agent.ToolDescriptor {
	return agent.ToolDescriptor{
		Source:             ToolSourceSearch,
		Capability:         config.AgentToolBookSearch,
		Execution:          agent.ToolExecutionParallelRead,
		MutationScope:      agent.ToolMutationNone,
		PostCheck:          agent.ToolPostCheckNone,
		Recovery:           agent.ToolRecoveryReadOnly,
		ResultRecoveryKind: agent.ToolResultRecoveryRerun,
		ResultProjection:   agent.ToolResultBoundedModelContext,
		ResultRetention:    agent.ToolResultDeferred,
		Steering:           agent.SteeringFinishCurrent,
		MaxResultBytes:     defaultToolResultMaxBytes,
		Presentation:       agent.UniformToolPresentation(agent.ToolPresentationSearch),
	}
}

// newSearchTools builds the hybrid workspace search tool. The embedding
// endpoint is optional: without it the index and ranking stay keyword-only,
// and a failing endpoint degrades to keyword results instead of failing the
// tool call.
func newSearchTools(workspace, projectStoreRoot string, embeddingConfig config.EmbeddingConfig) ([]agent.ToolDefinition, error) {
	workspace = strings.TrimSpace(workspace)
	var embedder searchindex.Embedder
	if embeddingConfig.Enabled() {
		client, err := embedding.New(embeddingConfig)
		if err != nil {
			return nil, fmt.Errorf("create embedding client: %w", err)
		}
		embedder = client
	}
	channel := "keyword"
	if embedder != nil {
		channel = "keyword + semantic"
	}
	tool, err := agent.InferTool("search", "Search the current book workspace (chapters, outline, and setting files) by meaning and keywords, and return the most relevant text fragments with their workspace-relative paths. Use it when the exact wording is unknown, for example to recall which chapter a scene, fact, or passage appears in, or to gather passages by topic. Prefer grep for a known exact string. Results are ranked fragments rather than full files; follow up with read to open a source before relying on or editing it.", func(ctx context.Context, input searchInput) (string, error) {
		if workspace == "" {
			return "", fmt.Errorf("cannot search because the current workspace is unavailable")
		}
		if strings.TrimSpace(input.Query) == "" {
			return "", fmt.Errorf("query is required")
		}
		indexDir := searchindex.DefaultIndexDir(workspace, projectStoreRoot)
		results, err := searchindex.Search(ctx, searchindex.Options{
			Workspace: workspace, IndexDir: indexDir, Embedder: embedder,
		}, input.Query, input.Limit)
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			return "No workspace content matched the query. Try different wording or keywords, or use grep for exact strings.", nil
		}
		return formatSearchResults(results, channel), nil
	})
	if err != nil {
		return nil, err
	}
	definition, err := defineTool(tool, searchToolDescriptor())
	if err != nil {
		return nil, err
	}
	return []agent.ToolDefinition{definition}, nil
}

func formatSearchResults(results []searchindex.Result, channel string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Workspace Search Results (%d, ranked by %s relevance)\n\n", len(results), channel)
	builder.WriteString("Source: current book workspace snapshot. Each fragment shows its file path and chunk start offset; open a source with read for full context. Scores rank fragments and are not evidence of correctness.\n")
	for index, result := range results {
		title := strings.TrimSpace(result.Title)
		if title == "" {
			title = result.Path
		}
		fmt.Fprintf(&builder, "\n## %d. %s\n\npath: %s\nchunk_start: %d\n\n%s\n", index+1, title, result.Path, result.Start, truncateSearchSnippet(result.Text))
	}
	return strings.TrimSpace(builder.String())
}

func truncateSearchSnippet(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= searchSnippetRunes {
		return string(runes)
	}
	return string(runes[:searchSnippetRunes]) + "..."
}
