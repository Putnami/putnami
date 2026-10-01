package logger

import "sync"

// MemorySink stores log entries in memory, useful for testing.
type MemorySink struct {
	Entries []LogEntry
	mu      sync.Mutex
}

// NewMemorySink creates a new in-memory sink.
func NewMemorySink() *MemorySink {
	return &MemorySink{}
}

func (s *MemorySink) Write(entry LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Entries = append(s.Entries, entry)
}

// Flush is a no-op for MemorySink since entries are stored in memory.
func (s *MemorySink) Flush() error { return nil }

// Close is a no-op for MemorySink.
func (s *MemorySink) Close() error { return nil }

// Clear removes all stored entries.
func (s *MemorySink) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Entries = nil
}

// Len returns the number of stored entries.
func (s *MemorySink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Entries)
}

// Last returns the most recent entry, or nil if empty.
func (s *MemorySink) Last() *LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Entries) == 0 {
		return nil
	}
	e := s.Entries[len(s.Entries)-1]
	return &e
}
