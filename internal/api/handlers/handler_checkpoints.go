package handlers

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"denova/internal/session"
)

type checkpointIDRequest struct {
	ID string `json:"id"`
}

// HandleCheckpointsList GET /api/checkpoints?session_id=... — 返回指定会话的全部检查点。
func (h *Handlers) HandleCheckpointsList(ctx context.Context, c *app.RequestContext) {
	if !h.app.HasWorkspace() {
		writeJSON(c, consts.StatusOK, []session.Checkpoint{})
		return
	}
	id := strings.TrimSpace(c.Query("session_id"))
	checkpoints, err := h.app.ListCheckpoints(ctx, id)
	if err != nil {
		writeErrorKey(c, consts.StatusNotFound, "api.checkpoints.notFound")
		return
	}
	writeJSON(c, consts.StatusOK, checkpoints)
}

// HandleCheckpointCreate POST /api/checkpoints — 手动创建一次检查点。
func (h *Handlers) HandleCheckpointCreate(ctx context.Context, c *app.RequestContext) {
	if !h.app.HasWorkspace() {
		writeErrorKey(c, consts.StatusConflict, "api.checkpoints.invalidSessionID")
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		Reason    string `json:"reason"`
	}
	if len(c.Request.Body()) > 0 {
		if err := c.BindJSON(&req); err != nil {
			writeErrorKey(c, consts.StatusBadRequest, "api.checkpoints.createFailed", "detail", err.Error())
			return
		}
	}
	checkpoint, err := h.app.CreateCheckpoint(ctx, req.SessionID, req.Reason)
	if err != nil {
		writeErrorKey(c, consts.StatusInternalServerError, "api.checkpoints.createFailed", "detail", err.Error())
		return
	}
	writeJSON(c, consts.StatusOK, checkpoint)
}

// HandleCheckpointRestore POST /api/checkpoints/restore — 还原到指定检查点。
func (h *Handlers) HandleCheckpointRestore(ctx context.Context, c *app.RequestContext) {
	if !h.app.HasWorkspace() {
		writeErrorKey(c, consts.StatusConflict, "api.checkpoints.invalidSessionID")
		return
	}
	var req checkpointIDRequest
	if len(c.Request.Body()) > 0 {
		if err := c.BindJSON(&req); err != nil {
			writeErrorKey(c, consts.StatusBadRequest, "api.checkpoints.restoreFailed", "detail", err.Error())
			return
		}
	}
	if strings.TrimSpace(req.ID) == "" {
		writeErrorKey(c, consts.StatusBadRequest, "api.checkpoints.invalidSessionID")
		return
	}
	result, err := h.app.RestoreCheckpoint(ctx, "", req.ID)
	if err != nil {
		writeErrorKey(c, consts.StatusNotFound, "api.checkpoints.restoreFailed", "detail", err.Error())
		return
	}
	writeJSON(c, consts.StatusOK, result)
}

// HandleSessionTruncate POST /api/session/truncate — 对会话做逻辑截断。
func (h *Handlers) HandleSessionTruncate(ctx context.Context, c *app.RequestContext) {
	if !h.app.HasWorkspace() {
		writeErrorKey(c, consts.StatusConflict, "api.checkpoints.invalidSessionID")
		return
	}
	var req struct {
		SessionID    string `json:"session_id"`
		MessageIndex int    `json:"message_index"`
		Reason       string `json:"reason"`
	}
	if len(c.Request.Body()) > 0 {
		if err := c.BindJSON(&req); err != nil {
			writeErrorKey(c, consts.StatusBadRequest, "api.checkpoints.truncateFailed", "detail", err.Error())
			return
		}
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeErrorKey(c, consts.StatusBadRequest, "api.checkpoints.invalidSessionID")
		return
	}
	result, err := h.app.TruncateSession(ctx, req.SessionID, req.MessageIndex, req.Reason)
	if err != nil {
		writeErrorKey(c, consts.StatusInternalServerError, "api.checkpoints.truncateFailed", "detail", err.Error())
		return
	}
	writeJSON(c, consts.StatusOK, result)
}
