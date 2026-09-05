package session

import (
	"strings"
	"time"
)

// AppendRuntimeState persists the agent's runtime context that affects the next
// turn (which agent kind was active, working mode, active story/branch, last
// read file, and bounded settings). It is append-only so resume can replay the
// exact "work site" (Plan.md M11). The latest record after the clear marker wins.
func (s *Session) AppendRuntimeState(record RuntimeState) (RuntimeState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	record.Type = historyTypeRuntimeState
	if strings.TrimSpace(record.ID) == "" {
		record.ID = newRuntimeStateID()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	s.records = append(s.records, historyRecord{kind: historyTypeRuntimeState, runtimeState: &record, createdAt: record.CreatedAt})
	s.UpdatedAt = record.CreatedAt
	return record, s.persistLocked()
}

// LatestRuntimeState returns the newest runtime-state record after the latest
// clear marker. It carries the agent kind, mode, story/branch, last read file,
// and any bounded settings that should be replayed on resume so the model
// inherits the same work site it left behind.
func (s *Session) LatestRuntimeState() (RuntimeState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.records) - 1; i >= 0; i-- {
		record := s.records[i]
		if record.kind != historyTypeRuntimeState || record.runtimeState == nil {
			continue
		}
		rs := *record.runtimeState
		if strings.TrimSpace(rs.AgentKind) == "" && strings.TrimSpace(rs.Mode) == "" {
			continue
		}
		return rs, true
	}
	return RuntimeState{}, false
}
