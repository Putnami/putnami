package profiler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProfilerDisabled(t *testing.T) {
	p := New(false)
	if p.IsEnabled() {
		t.Fatal("expected profiler to be disabled")
	}

	// All operations should be no-ops
	p.JobStart("job1")
	p.JobEnd("job1", "success", time.Second)
	p.Instant("job1", "cache-hit", "cache", nil)

	// Write should be no-op
	if err := p.Write("/dev/null"); err != nil {
		t.Fatalf("disabled profiler write should be no-op: %v", err)
	}
}

func TestProfilerEnabled(t *testing.T) {
	p := New(true)
	if !p.IsEnabled() {
		t.Fatal("expected profiler to be enabled")
	}

	p.JobStart("proj:build~transpile")
	time.Sleep(time.Millisecond)
	p.JobEnd("proj:build~transpile", "success", 100*time.Millisecond)

	p.JobStart("proj:build~types")
	p.Instant("proj:build~types", "cache-hit", "cache", map[string]any{"hash": "abc123"})
	p.JobEnd("proj:build~types", "success", 0)

	// Write to temp file
	dir := t.TempDir()
	outPath := filepath.Join(dir, "trace.json")
	if err := p.Write(outPath); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read and validate
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var events []TraceEvent
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Should have metadata + actual events
	if len(events) < 3 {
		t.Fatalf("expected at least 3 events, got %d", len(events))
	}

	// Check process metadata
	found := false
	for _, ev := range events {
		if ev.Name == "process_name" && ev.Phase == "M" {
			found = true
			if ev.Args["name"] != "putnami" {
				t.Errorf("expected process name 'putnami', got %v", ev.Args["name"])
			}
		}
	}
	if !found {
		t.Error("missing process_name metadata event")
	}

	// Check thread names exist for both jobs
	threadNames := 0
	for _, ev := range events {
		if ev.Name == "thread_name" && ev.Phase == "M" {
			threadNames++
		}
	}
	if threadNames < 2 {
		t.Errorf("expected at least 2 thread_name events, got %d", threadNames)
	}

}

func TestProfilerThreadIDStability(t *testing.T) {
	p := New(true)

	// Same job key should get same TID
	tid1 := p.threadID("proj:build")
	tid2 := p.threadID("proj:build")
	if tid1 != tid2 {
		t.Errorf("same key should get same TID: %d != %d", tid1, tid2)
	}

	// Different job key should get different TID
	tid3 := p.threadID("proj:test")
	if tid1 == tid3 {
		t.Errorf("different keys should get different TIDs: %d == %d", tid1, tid3)
	}

	// Empty key returns 0 (main thread)
	if p.threadID("") != 0 {
		t.Error("empty key should return TID 0")
	}
}
