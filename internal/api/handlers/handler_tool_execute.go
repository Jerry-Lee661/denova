package handlers

import (
	"context"
	"fmt"
	"strings"

	"denova/config"
	agenttoolruntime "denova/internal/agents/toolruntime"
	agent "github.com/alfredxw/denova/agent"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// HandleToolExecute POST /api/tools/execute — rebuild a single tool by name
// and run it with the caller-supplied arguments. Backs the frontend tool-card
// retry action for transient tool failures (e.g. timeouts). Tool errors are
// reported in the "error" field with a 200 status so the UI can render the
// failure in place; only transport-level failures use HTTP error codes.
func (h *Handlers) HandleToolExecute(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	var req struct {
		ToolName string `json:"tool_name"`
		Args     string `json:"args"`
	}
	if err := c.BindJSON(&req); err != nil || strings.TrimSpace(req.ToolName) == "" {
		writeErrorKey(c, consts.StatusBadRequest, "api.common.invalidRequest")
		return
	}

	cfg := h.app.ConfigSnapshot()
	definition, err := buildRetryableToolByName(ctx, &cfg, strings.TrimSpace(req.ToolName))
	if err != nil {
		writeError(c, consts.StatusNotFound, err.Error())
		return
	}

	result, runErr := definition.Tool.Run(ctx, req.Args)
	if runErr != nil {
		writeJSON(c, consts.StatusOK, map[string]any{"error": runErr.Error()})
		return
	}
	writeJSON(c, consts.StatusOK, map[string]any{"result": result.ModelContent})
}

// buildRetryableToolByName rebuilds the standalone workspace/web/lore tools
// for the writing Agent's effective tool settings and returns the definition
// matching toolName. Session-bound toolsets (skills, ask, todo, interactive)
// are intentionally excluded: retrying them outside their owning session
// would bypass the guards and state their run established.
func buildRetryableToolByName(ctx context.Context, cfg *config.Config, toolName string) (agent.ToolDefinition, error) {
	catalog := agenttoolruntime.NewCatalogWithContext(ctx, cfg)
	settings := config.ResolveAgentTools(cfg, config.AgentKindIDE)

	var candidates []agent.ToolDefinition
	workspaceTools, err := catalog.Workspace(settings)
	if err != nil {
		return agent.ToolDefinition{}, fmt.Errorf("rebuild workspace tools: %w", err)
	}
	candidates = append(candidates, workspaceTools...)
	webTools, err := catalog.WebAccess(settings)
	if err != nil {
		return agent.ToolDefinition{}, fmt.Errorf("rebuild web tools: %w", err)
	}
	candidates = append(candidates, webTools...)
	loreTools, err := catalog.Lore(false)(settings)
	if err != nil {
		return agent.ToolDefinition{}, fmt.Errorf("rebuild lore tools: %w", err)
	}
	candidates = append(candidates, loreTools...)

	for _, definition := range candidates {
		info, err := definition.Tool.Info(ctx)
		if err != nil || info == nil {
			continue
		}
		if info.Name == toolName {
			return definition, nil
		}
	}
	return agent.ToolDefinition{}, fmt.Errorf("tool %q is not available for standalone execution", toolName)
}
