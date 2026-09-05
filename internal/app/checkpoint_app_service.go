package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"denova/internal/book"
	"denova/internal/session"
)

// CheckpointAppService 负责"检查点 + 逻辑截断"能力：
// 每轮对话自动创建检查点（工作区 git 快照 + 会话语义截断点），用户可手动创建、
// 列出与还原。还原检查点 = 将工作区恢复到该检查点对应的 git 版本，并对会话做
// 逻辑截断（保留到该消息索引，之后的消息不再显示/进入模型，磁盘记录保留可回滚）。
type CheckpointAppService struct {
	app *App
}

// ListCheckpoints 返回指定会话的全部检查点（按创建时间升序）。
func (a *App) ListCheckpoints(ctx context.Context, sessionID string) ([]session.Checkpoint, error) {
	sess, err := a.resolveSession(sessionID)
	if err != nil {
		return nil, err
	}
	return sess.ListCheckpoints(), nil
}

// CreateCheckpoint 为指定会话手动创建一次检查点：先对工作区打 git 快照，
// 再记录会话语义截断点（保留到当前最后一条消息）。
func (a *App) CreateCheckpoint(ctx context.Context, sessionID, reason string) (session.Checkpoint, error) {
	sess, err := a.resolveSession(sessionID)
	if err != nil {
		return session.Checkpoint{}, err
	}
	versionID := ""
	if version, vErr := a.runtime().createVersionSnapshot(ctx, book.VersionSourceManual); vErr == nil && version.ID != "" {
		versionID = version.ID
	} else if vErr != nil {
		log.Printf("[checkpoint] 手动检查点 git 快照失败 session=%s err=%v", sessionID, vErr)
	}
	record := session.Checkpoint{
		MessageIndex: sess.MessageCountTotal() - 1,
		VersionID:    versionID,
		Reason:       reason,
	}
	return sess.AppendCheckpoint(record)
}

// RestoreCheckpoint 还原到指定检查点：将工作区恢复到该检查点对应的 git 版本，
// 并对会话做逻辑截断（保留到检查点的 MessageIndex）。
func (a *App) RestoreCheckpoint(ctx context.Context, sessionID, checkpointID string) (session.RestoreResult, error) {
	checkpoints, err := a.ListCheckpoints(ctx, sessionID)
	if err != nil {
		return session.RestoreResult{}, err
	}
	var target *session.Checkpoint
	for i := range checkpoints {
		if checkpoints[i].ID == checkpointID {
			target = &checkpoints[i]
			break
		}
	}
	if target == nil {
		return session.RestoreResult{}, book.ErrVersionNotFound
	}

	versionID := target.VersionID
	if versionID != "" {
		restore, rErr := a.runtime().restoreVersionByID(ctx, versionID)
		if rErr != nil {
			return session.RestoreResult{}, rErr
		}
		if restore.Version != nil {
			versionID = restore.Version.ID
		}
	}

	sess, err := a.resolveSession(sessionID)
	if err != nil {
		return session.RestoreResult{}, err
	}
	if err := sess.AppendTruncate(target.MessageIndex, "checkpoint_restore"); err != nil {
		return session.RestoreResult{}, err
	}
	return session.RestoreResult{
		Checkpoint:   *target,
		VersionID:    versionID,
		MessageIndex: target.MessageIndex,
	}, nil
}

// TruncateSession 对指定会话做逻辑截断：保留到 messageIndex（含）的消息，
// 之后的消息进入隐藏区间（不再显示/进入模型），磁盘记录保留可回滚。
func (a *App) TruncateSession(ctx context.Context, sessionID string, messageIndex int, reason string) (session.TruncateResult, error) {
	sess, err := a.resolveSession(sessionID)
	if err != nil {
		return session.TruncateResult{}, err
	}
	if err := sess.AppendTruncate(messageIndex, reason); err != nil {
		return session.TruncateResult{}, err
	}
	return session.TruncateResult{MessageIndex: sess.TruncateIndex()}, nil
}

// createAutoCheckpoint 在后台 goroutine 中为会话自动创建检查点（reason=auto），
// 用于每轮对话成功后自动快照工作区与会话截断点。panic 安全。
// 节流：距上一个自动检查点不足 CheckpointAutoIntervalMinutes 时跳过，
// 避免高频对话产生大量 git 版本（0 表示不节流）。
func (a *App) createAutoCheckpoint(ctx context.Context, sessionID string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("[checkpoint] auto checkpoint panic recovered session=%s err=%v", sessionID, recovered)
		}
	}()
	if interval := a.checkpointAutoInterval(); interval > 0 {
		if sess, err := a.resolveSession(sessionID); err == nil {
			checkpoints := sess.ListCheckpoints()
			for i := len(checkpoints) - 1; i >= 0; i-- {
				cp := checkpoints[i]
				if cp.Reason != "auto" {
					continue
				}
				if since := time.Since(cp.CreatedAt); since < interval {
					log.Printf("[checkpoint] auto checkpoint throttled session=%s last_auto=%s since=%s interval=%s", sessionID, cp.CreatedAt.Format(time.RFC3339), since.Round(time.Second), interval)
					return
				}
				break
			}
		}
	}
	if _, err := a.CreateCheckpoint(ctx, sessionID, "auto"); err != nil {
		log.Printf("[checkpoint] auto checkpoint failed session=%s err=%v", sessionID, err)
	}
}

// checkpointAutoInterval 返回自动检查点最小创建间隔；<=0 表示不节流。
func (a *App) checkpointAutoInterval() time.Duration {
	a.mu.RLock()
	minutes := a.cfg.CheckpointAutoIntervalMinutes
	a.mu.RUnlock()
	if minutes <= 0 {
		return 0
	}
	return time.Duration(minutes) * time.Minute
}

// resolveSession 解析指定会话：空 ID 返回当前活动会话，否则从 store 加载。
func (a *App) resolveSession(sessionID string) (*session.Session, error) {
	a.mu.RLock()
	store := a.sessionStore
	current := a.session
	a.mu.RUnlock()
	if store == nil {
		return nil, ErrNoWorkspace
	}
	if strings.TrimSpace(sessionID) == "" {
		if current == nil {
			return nil, ErrNoWorkspace
		}
		return current, nil
	}
	if isAgentSessionID(sessionID) {
		return nil, fmt.Errorf("不能通过创作会话读取固定 Agent 会话: %s", sessionID)
	}
	return store.Get(sessionID)
}

// createVersionSnapshot 对工作区打一次 git 快照（手动来源），返回新版本条目。
func (s *WorkspaceRuntimeManager) createVersionSnapshot(ctx context.Context, source string) (book.VersionEntry, error) {
	result, err := s.CreateVersion(ctx, "")
	if err != nil {
		return book.VersionEntry{}, err
	}
	if result.Version == nil {
		return book.VersionEntry{}, book.ErrVersionNotFound
	}
	return *result.Version, nil
}

// restoreVersionByID 将工作区恢复到目标版本（整本书范围）。
func (s *WorkspaceRuntimeManager) restoreVersionByID(ctx context.Context, id string) (book.VersionRestoreResult, error) {
	return s.RestoreVersion(ctx, id)
}
