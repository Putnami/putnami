// Package profiler collects pprof-compatible trace events during execution
// and outputs them in Chrome trace format (chrome://tracing).
package profiler

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// TraceEvent is a single event in the Chrome Trace Event Format.
// See: https://docs.google.com/document/d/1CvAClvFfyA5R-PhYUmn5OOQtYMH4h6I0nSsKchNAySU
type TraceEvent struct {
	Name      string         `json:"name"`
	Category  string         `json:"cat"`
	Phase     string         `json:"ph"`            // "B" (begin), "E" (end), "X" (complete), "I" (instant)
	Timestamp int64          `json:"ts"`            // microseconds since epoch
	Duration  int64          `json:"dur,omitempty"` // microseconds (for "X" events)
	PID       int            `json:"pid"`
	TID       int            `json:"tid"`
	Args      map[string]any `json:"args,omitempty"`
}

// Profiler collects trace events during CLI execution.
type Profiler struct {
	mu        sync.Mutex
	events    []TraceEvent
	startTime time.Time
	enabled   bool
	tidMap    map[string]int // jobKey → thread ID
	nextTID   int
}

// New creates a new profiler. If enabled is false, all operations are no-ops.
func New(enabled bool) *Profiler {
	return &Profiler{
		enabled:   enabled,
		startTime: time.Now(),
		tidMap:    make(map[string]int),
		nextTID:   1, // TID 0 reserved for the main thread
	}
}

// IsEnabled returns whether profiling is active.
func (p *Profiler) IsEnabled() bool {
	return p.enabled
}

// threadID returns a stable thread ID for a job key.
func (p *Profiler) threadID(jobKey string) int {
	if jobKey == "" {
		return 0
	}
	if tid, ok := p.tidMap[jobKey]; ok {
		return tid
	}
	tid := p.nextTID
	p.nextTID++
	p.tidMap[jobKey] = tid
	return tid
}

// ts returns microseconds since profiling started.
func (p *Profiler) ts(t time.Time) int64 {
	return t.Sub(p.startTime).Microseconds()
}

// JobStart records a job execution start.
func (p *Profiler) JobStart(jobKey string) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	p.events = append(p.events, TraceEvent{
		Name:      jobKey,
		Category:  "job",
		Phase:     "B",
		Timestamp: p.ts(time.Now()),
		PID:       1,
		TID:       p.threadID(jobKey),
	})
}

// JobEnd records a job execution end with status and duration.
func (p *Profiler) JobEnd(jobKey, status string, duration time.Duration) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	p.events = append(p.events, TraceEvent{
		Name:      jobKey,
		Category:  "job",
		Phase:     "E",
		Timestamp: p.ts(time.Now()),
		PID:       1,
		TID:       p.threadID(jobKey),
		Args: map[string]any{
			"status":      status,
			"duration_ms": duration.Milliseconds(),
		},
	})
}

// Instant records a point-in-time event (e.g., cache hit, error).
func (p *Profiler) Instant(jobKey, name, category string, args map[string]any) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	p.events = append(p.events, TraceEvent{
		Name:      name,
		Category:  category,
		Phase:     "i",
		Timestamp: p.ts(time.Now()),
		PID:       1,
		TID:       p.threadID(jobKey),
		Args:      args,
	})
}

// Write outputs the trace events to the given file path in Chrome trace format.
func (p *Profiler) Write(path string) error {
	if !p.enabled {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// Add process metadata
	allEvents := make([]TraceEvent, 0, len(p.events)+len(p.tidMap)+1)
	allEvents = append(allEvents, TraceEvent{
		Name:      "process_name",
		Category:  "__metadata",
		Phase:     "M",
		Timestamp: 0,
		PID:       1,
		TID:       0,
		Args:      map[string]any{"name": "putnami"},
	})

	// Add thread name metadata for each job
	for jobKey, tid := range p.tidMap {
		allEvents = append(allEvents, TraceEvent{
			Name:      "thread_name",
			Category:  "__metadata",
			Phase:     "M",
			Timestamp: 0,
			PID:       1,
			TID:       tid,
			Args:      map[string]any{"name": jobKey},
		})
	}

	allEvents = append(allEvents, p.events...)

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	return encoder.Encode(allEvents)
}
