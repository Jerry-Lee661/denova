package session

import (
	"fmt"
	"strings"
	"time"
)

// AppendContextCompaction persists a compaction epoch. It intentionally does
// not append to messages, so user-visible history stays uncompressed.
func (s *Session) AppendContextCompaction(record ContextCompaction) (ContextCompaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	record.Type = historyTypeCompaction
	if strings.TrimSpace(record.ID) == "" {
		record.ID = newContextCompactionID()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	if record.Epoch <= 0 {
		record.Epoch = s.nextCompactionEpochLocked(record.AgentKind)
	}
	if record.SourceEndIndex <= 0 || record.SourceEndIndex > len(s.messages) {
		record.SourceEndIndex = len(s.messages)
	}
	if record.SourceStartIndex < s.clearAfterIndex {
		record.SourceStartIndex = s.clearAfterIndex
	}
	if record.SourceStartIndex > record.SourceEndIndex {
		record.SourceStartIndex = record.SourceEndIndex
	}
	if record.SourceMessageCount <= 0 {
		record.SourceMessageCount = record.SourceEndIndex - record.SourceStartIndex
	}
	s.records = append(s.records, historyRecord{kind: historyTypeCompaction, compaction: &record, createdAt: record.CreatedAt})
	s.UpdatedAt = record.CreatedAt
	return record, s.persistLocked()
}

// RemoveLatestContextCompaction soft-disables the latest active compaction for
// an agent. Raw messages remain untouched so context can reconnect to history.
func (s *Session) RemoveLatestContextCompaction(agentKind, reason string) (ContextCompactionRemoval, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	compaction, ok := s.latestActiveContextCompactionLocked(agentKind)
	if !ok {
		return ContextCompactionRemoval{}, false, nil
	}
	now := time.Now().UTC()
	record := ContextCompactionRemoval{
		Type:             historyTypeCompactionRemoved,
		ID:               newContextCompactionRemovalID(),
		AgentKind:        compaction.AgentKind,
		CompactionID:     compaction.ID,
		SourceStartIndex: compaction.SourceStartIndex,
		SourceEndIndex:   compaction.SourceEndIndex,
		Reason:           strings.TrimSpace(reason),
		CreatedAt:        now,
	}
	if strings.TrimSpace(record.AgentKind) == "" {
		record.AgentKind = strings.TrimSpace(agentKind)
	}
	s.records = append(s.records, historyRecord{kind: historyTypeCompactionRemoved, compactionRemoval: &record, createdAt: record.CreatedAt})
	s.UpdatedAt = record.CreatedAt
	return record, true, s.persistLocked()
}

// LatestContextCompaction returns the newest compaction epoch after the latest
// clear marker for the given agent kind.
func (s *Session) LatestContextCompaction(agentKind string) (ContextCompaction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.latestActiveContextCompactionLocked(agentKind)
}

// AppendExternalizedResult persists a tool-result externalization decision so
// resume can replay the exact "externalized" decision and the next request can
// micro-compact it on cold cache. The full body stays on disk; only the bounded
// location reference is recorded here.
func (s *Session) AppendExternalizedResult(record ExternalizedResultEntry) (ExternalizedResultEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(record.IdempotencyKey) == "" {
		return ExternalizedResultEntry{}, fmt.Errorf("externalized result requires an idempotency key")
	}
	if strings.TrimSpace(record.Location) == "" {
		return ExternalizedResultEntry{}, fmt.Errorf("externalized result requires a location")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	s.records = append(s.records, historyRecord{kind: historyTypeExternalize, externalize: &record, createdAt: record.CreatedAt})
	s.UpdatedAt = record.CreatedAt
	return record, s.persistLocked()
}

// ExternalizedResults returns every persisted externalization decision in
// chronological order. Callers use Location to read the full body back from
// disk and MicroCompactResult to decide hot/cold projection.
func (s *Session) ExternalizedResults() []ExternalizedResultEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	var results []ExternalizedResultEntry
	for _, record := range s.records {
		if record.kind == historyTypeExternalize && record.externalize != nil {
			results = append(results, *record.externalize)
		}
	}
	return results
}

// LatestContextCompactionRemoval returns the newest removal marker after the
// latest clear marker for the given agent kind.
func (s *Session) LatestContextCompactionRemoval(agentKind string) (ContextCompactionRemoval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.records) - 1; i >= 0; i-- {
		record := s.records[i]
		if record.kind != historyTypeCompactionRemoved || record.compactionRemoval == nil {
			continue
		}
		removal := *record.compactionRemoval
		if removal.SourceEndIndex <= s.clearAfterIndex {
			continue
		}
		if strings.TrimSpace(agentKind) != "" && strings.TrimSpace(removal.AgentKind) != "" && removal.AgentKind != agentKind {
			continue
		}
		return removal, true
	}
	return ContextCompactionRemoval{}, false
}

func (s *Session) NextContextCompactionEpoch(agentKind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextCompactionEpochLocked(agentKind)
}

// AppendContextFold persists a context-fold record. It intentionally does not
// append to messages or change the boundary; the fold only affects the model
// projection. The raw transcript stays intact so it can be re-projected later.
func (s *Session) AppendContextFold(record ContextFold) (ContextFold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	record.Type = historyTypeFold
	if strings.TrimSpace(record.ID) == "" {
		record.ID = newContextFoldID()
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	}
	if record.SourceEndIndex <= 0 || record.SourceEndIndex > len(s.messages) {
		record.SourceEndIndex = len(s.messages)
	}
	if record.SourceStartIndex < s.clearAfterIndex {
		record.SourceStartIndex = s.clearAfterIndex
	}
	if record.SourceStartIndex > record.SourceEndIndex {
		record.SourceStartIndex = record.SourceEndIndex
	}
	if record.SourceMessageCount <= 0 {
		record.SourceMessageCount = record.SourceEndIndex - record.SourceStartIndex
	}
	s.records = append(s.records, historyRecord{kind: historyTypeFold, fold: &record, createdAt: record.CreatedAt})
	s.UpdatedAt = record.CreatedAt
	return record, s.persistLocked()
}

// RemoveLatestContextFold soft-disables the latest active fold for an agent.
func (s *Session) RemoveLatestContextFold(agentKind, reason string) (ContextFoldRemoved, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fold, ok := s.latestActiveFoldLocked(agentKind)
	if !ok {
		return ContextFoldRemoved{}, false, nil
	}
	now := time.Now().UTC()
	record := ContextFoldRemoved{
		Type:             historyTypeFoldRemoved,
		ID:               newContextFoldRemovedID(),
		AgentKind:        strings.TrimSpace(agentKind),
		FoldID:           fold.ID,
		SourceStartIndex: fold.SourceStartIndex,
		SourceEndIndex:   fold.SourceEndIndex,
		Reason:           strings.TrimSpace(reason),
		CreatedAt:        now,
	}
	s.records = append(s.records, historyRecord{kind: historyTypeFoldRemoved, foldRemoved: &record, createdAt: record.CreatedAt})
	s.UpdatedAt = record.CreatedAt
	return record, true, s.persistLocked()
}

// LatestContextFold returns the newest active fold after the latest clear marker
// for the given agent kind.
func (s *Session) LatestContextFold(agentKind string) (ContextFold, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latestActiveFoldLocked(agentKind)
}

// LatestContextFoldRemoved returns the newest removal marker after the latest
// clear marker for the given agent kind.
func (s *Session) LatestContextFoldRemoved(agentKind string) (ContextFoldRemoved, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.records) - 1; i >= 0; i-- {
		record := s.records[i]
		if record.kind != historyTypeFoldRemoved || record.foldRemoved == nil {
			continue
		}
		removal := *record.foldRemoved
		if removal.SourceEndIndex <= s.clearAfterIndex {
			continue
		}
		if strings.TrimSpace(agentKind) != "" && strings.TrimSpace(removal.AgentKind) != "" && removal.AgentKind != agentKind {
			continue
		}
		return removal, true
	}
	return ContextFoldRemoved{}, false
}

func (s *Session) latestActiveFoldLocked(agentKind string) (ContextFold, bool) {
	for i := len(s.records) - 1; i >= 0; i-- {
		record := s.records[i]
		if record.kind == historyTypeFoldRemoved && record.foldRemoved != nil {
			removal := *record.foldRemoved
			if removal.SourceEndIndex <= s.clearAfterIndex {
				continue
			}
			if strings.TrimSpace(agentKind) == "" || strings.TrimSpace(removal.AgentKind) == "" || removal.AgentKind == agentKind {
				return ContextFold{}, false
			}
			continue
		}
		if record.kind != historyTypeFold || record.fold == nil {
			continue
		}
		fold := *record.fold
		if fold.SourceEndIndex <= s.clearAfterIndex {
			continue
		}
		if strings.TrimSpace(agentKind) != "" && strings.TrimSpace(fold.AgentKind) != "" && fold.AgentKind != agentKind {
			continue
		}
		return fold, true
	}
	return ContextFold{}, false
}

func (s *Session) latestActiveContextCompactionLocked(agentKind string) (ContextCompaction, bool) {
	for i := len(s.records) - 1; i >= 0; i-- {
		record := s.records[i]
		if record.kind == historyTypeCompactionRemoved && record.compactionRemoval != nil {
			removal := *record.compactionRemoval
			if removal.SourceEndIndex <= s.clearAfterIndex {
				continue
			}
			if strings.TrimSpace(agentKind) == "" || strings.TrimSpace(removal.AgentKind) == "" || removal.AgentKind == agentKind {
				return ContextCompaction{}, false
			}
			continue
		}
		if record.kind != historyTypeCompaction || record.compaction == nil {
			continue
		}
		compaction := *record.compaction
		if compaction.SourceEndIndex <= s.clearAfterIndex {
			continue
		}
		if strings.TrimSpace(agentKind) != "" && strings.TrimSpace(compaction.AgentKind) != "" && compaction.AgentKind != agentKind {
			continue
		}
		return compaction, true
	}
	return ContextCompaction{}, false
}

func (s *Session) nextCompactionEpochLocked(agentKind string) int {
	epoch := 0
	for _, record := range s.records {
		if record.kind != historyTypeCompaction || record.compaction == nil {
			continue
		}
		if strings.TrimSpace(agentKind) != "" && strings.TrimSpace(record.compaction.AgentKind) != "" && record.compaction.AgentKind != agentKind {
			continue
		}
		if record.compaction.Epoch > epoch {
			epoch = record.compaction.Epoch
		}
	}
	return epoch + 1
}
