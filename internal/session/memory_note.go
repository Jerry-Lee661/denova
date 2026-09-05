package session

import (
	"strings"
	"time"
)

// MemoryNote is a bounded structured background note written by a background
// sub-agent while a conversation progresses (Plan.md M7). It captures goals,
// progress, and key conclusions so the next compaction can reuse them directly
// instead of re-summarizing raw turns. The full body stays bounded; only the
// record is persisted append-only so resume can replay it.
type MemoryNote struct {
	Type           string    `json:"type"`
	ID             string    `json:"id,omitempty"`
	AgentKind      string    `json:"agent_kind,omitempty"`
	Title          string    `json:"title,omitempty"`
	Goals          string    `json:"goals,omitempty"`
	Progress       string    `json:"progress,omitempty"`
	Conclusions    string    `json:"conclusions,omitempty"`
	SourceEndIndex int       `json:"source_end_index,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
}

// AppendMemoryNote persists one background memory note. It intentionally does
// not append to messages or change the boundary; the note is a durable side
// channel that compaction reads to seed its checkpoint.
func (s *Session) AppendMemoryNote(note MemoryNote) (MemoryNote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	note.Type = historyTypeMemoryNote
	if strings.TrimSpace(note.ID) == "" {
		note.ID = newMemoryNoteID()
	}
	if strings.TrimSpace(note.AgentKind) == "" {
		note.AgentKind = "ide"
	}
	if strings.TrimSpace(note.Title) == "" {
		note.Title = "本轮笔记"
	}
	if note.CreatedAt.IsZero() {
		note.CreatedAt = now
	}
	// Record the message boundary so a later clear marker can drop stale notes.
	note.SourceEndIndex = len(s.messages)
	s.records = append(s.records, historyRecord{kind: historyTypeMemoryNote, memoryNote: &note, createdAt: note.CreatedAt})
	s.UpdatedAt = note.CreatedAt
	return note, s.persistLocked()
}

// LatestMemoryNotes returns up to maxNotes newest memory notes after the latest
// clear marker for the given agent kind, newest first. A zero maxNotes falls
// back to a bounded default so a long conversation cannot feed an unbounded
// prompt into compaction.
func (s *Session) LatestMemoryNotes(agentKind string, maxNotes int) []MemoryNote {
	s.mu.Lock()
	defer s.mu.Unlock()

	if maxNotes <= 0 {
		maxNotes = DefaultMemoryNotesLimit
	}
	var notes []MemoryNote
	for i := len(s.records) - 1; i >= 0 && len(notes) < maxNotes; i-- {
		record := s.records[i]
		if record.kind != historyTypeMemoryNote || record.memoryNote == nil {
			continue
		}
		note := *record.memoryNote
		// A note is stale only when its source ended strictly before the latest
		// clear marker. Use `<` (not `<=`) so a note written exactly at the clear
		// boundary — e.g. the first note of a fresh session with SourceEndIndex 0,
		// or one appended right after Clear() — is still kept.
		if note.SourceEndIndex < s.clearAfterIndex {
			continue
		}
		if strings.TrimSpace(agentKind) != "" && strings.TrimSpace(note.AgentKind) != "" && note.AgentKind != agentKind {
			continue
		}
		notes = append(notes, note)
	}
	return notes
}
