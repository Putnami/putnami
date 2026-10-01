package output

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

func TestNewTextRenderer(t *testing.T) {
	var out, errOut bytes.Buffer
	cfg := TextRendererConfig{Verbose: true}
	r := NewTextRenderer(&out, &errOut, cfg)

	if r == nil {
		t.Fatal("NewTextRenderer returned nil")
	}
	if !r.cfg.Verbose {
		t.Error("expected Verbose=true")
	}
}

func TestTextRendererStartQuiet(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{Quiet: true})

	r.Start(nil)
	if errOut.Len() != 0 {
		t.Errorf("quiet mode should produce no output, got %q", errOut.String())
	}
}

func TestTextRendererStartMultiJob(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{})

	planned := []*jobs.ScheduledJob{
		makeLiveTestJob("app", "build"),
		makeLiveTestJob("lib", "build"),
	}
	r.Start(planned)

	if !r.multiJob {
		t.Error("expected multiJob=true when >1 jobs planned")
	}
}

func TestTextRendererFinish(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{})
	r.Start(nil)

	results := map[string]*jobs.JobResult{
		"a": {Status: "success"},
		"b": {Status: "success"},
		"c": {CacheHit: true, Status: "success"},
		"d": {Coalesced: true, Status: "success"},
	}

	r.Finish(results, jobs.SessionOutcome{})

	output := errOut.String()
	if !bytes.Contains([]byte(output), []byte("2 succeeded")) {
		t.Errorf("expected '2 succeeded' in output, got %q", output)
	}
	if !bytes.Contains([]byte(output), []byte("1 cached")) {
		t.Errorf("expected '1 cached' in output, got %q", output)
	}
	if !bytes.Contains([]byte(output), []byte("1 coalesced")) {
		t.Errorf("expected '1 coalesced' in output, got %q", output)
	}
}

func TestTextRendererFinishWithFailures(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{})
	r.Start([]*jobs.ScheduledJob{
		makeLiveTestJob("app", "build"),
		makeLiveTestJob("lib", "build"),
	})

	results := map[string]*jobs.JobResult{
		"a": {Status: "success"},
		"b": {Status: "failed", Error: &jobs.JobError{Message: "compile error"}},
	}

	r.Finish(results, jobs.SessionOutcome{})

	output := errOut.String()
	if !bytes.Contains([]byte(output), []byte("1/2 failed")) {
		t.Errorf("expected '1/2 failed' in output, got %q", output)
	}
}

func TestTextRendererFinishVisibleSummary(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{})
	r.Start(nil)

	payload := "{\n  \"appName\": \"api\"\n}"
	summary := jobs.RawJobEvent{
		Type: jobs.EventTypeSummary,
		Data: map[string]any{
			"message":    "Dry-run config schema payload:\n" + payload,
			"visibility": "always",
		},
	}
	r.Finish(map[string]*jobs.JobResult{"api:publish-config": {
		Status: "success",
		Events: []jobs.RawJobEvent{summary},
	}}, jobs.SessionOutcome{})

	output := errOut.String()
	if !bytes.Contains([]byte(output), []byte(payload)) {
		t.Errorf("expected exact payload in output, got %q", output)
	}
}

func TestTextRendererVerboseUsesLiveStyleRows(t *testing.T) {
	initial := ColorsEnabled()
	defer SetColorsEnabled(initial)
	SetColorsEnabled(false)

	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{Verbose: true})
	job := makeRendererTestJob()

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobEvent(job, jobs.RawJobEvent{
		Type: jobs.EventTypePhase,
		Data: map[string]any{"name": "compile", "action": "start"},
	})
	r.JobEvent(job, jobs.RawJobEvent{
		Type: jobs.EventTypeLog,
		Data: map[string]any{
			"level":   "info",
			"message": "compiled 3 files",
			"context": map[string]any{"logger": "build"},
		},
	})
	r.JobComplete(job, &jobs.JobResult{
		Status:   "success",
		Duration: 240 * time.Millisecond,
		Events: []jobs.RawJobEvent{{
			Type: jobs.EventTypeSummary,
			Data: map[string]any{"message": "3 files built"},
		}},
	})

	output := errOut.String()
	for _, want := range []string{
		"● my-app",
		"build starting",
		"[compile] start",
		"INF [build] compiled 3 files",
		"✓ my-app",
		"build done",
		"3 files built",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected %q in verbose output:\n%s", want, output)
		}
	}
	if strings.Contains(output, "build(my-app)") {
		t.Fatalf("verbose output should not use old command(project) prefix:\n%s", output)
	}
}

// TestTextRenderer_ConcurrentJobReports drives JobStart/JobEvent/JobComplete
// from many goroutines into one shared renderer, mirroring how the scheduler
// invokes the renderer from its parallel worker goroutines. The renderer's
// mutex guards writes into the shared errOut buffer; run under `-race` to catch
// any unsynchronized access. Verbose mode is used so all three lifecycle
// methods take their writing (locked) paths.
func TestTextRenderer_ConcurrentJobReports(t *testing.T) {
	initial := ColorsEnabled()
	defer SetColorsEnabled(initial)
	SetColorsEnabled(false)

	var out bytes.Buffer
	var errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{Verbose: true})

	const goroutines = 16
	const eventsPerGoroutine = 25

	planned := make([]*jobs.ScheduledJob, 0, goroutines)
	for i := 0; i < goroutines; i++ {
		planned = append(planned, makeTestJob(fmt.Sprintf("project-%d", i), "build"))
	}
	r.Start(planned)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			job := planned[id]
			r.JobStart(job)
			for j := 0; j < eventsPerGoroutine; j++ {
				r.JobEvent(job, jobs.RawJobEvent{
					Type: jobs.EventTypePhase,
					Data: map[string]any{"name": fmt.Sprintf("phase-%d", j), "action": "start"},
				})
			}
			r.JobComplete(job, &jobs.JobResult{
				Status:   "success",
				Duration: time.Duration(id) * time.Millisecond,
			})
		}(i)
	}
	wg.Wait()

	// Every job emits a "done" completion line; the count proves all writes
	// landed in the shared buffer without being lost to a data race.
	if got := strings.Count(errOut.String(), "build done"); got != goroutines {
		t.Errorf("expected %d completion lines, got %d", goroutines, got)
	}
}
