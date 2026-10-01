package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/engine"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/telemetry"
)

// This file covers the terminal ADAPTER over engine.Run: the telemetry seam and
// the flag vocabulary it reports. The run lifecycle itself is tested in
// internal/engine.

func TestTelemetryFlagPresence(t *testing.T) {
	t.Parallel()
	got := telemetryFlagPresence([]string{
		"build",
		"--impacted",
		"--coverage=false",
		"--output", "jsonl",
		"--no-cache",
		"--projects=tooling/cli",
		"-w",
		"--json",
		"--unrelated=value",
	})
	if !got.Impacted || !got.Coverage || !got.Output || !got.NoCache || !got.Projects || !got.Watch {
		t.Fatalf("flag presence = %+v, want every allowlisted flag present", got)
	}

	if got := telemetryFlagPresence([]string{"build", "--impacted-strict"}); got.Impacted || got.Coverage || got.Output || got.NoCache || got.Projects || got.Watch {
		t.Fatalf("flag presence = %+v, want no allowlisted flags", got)
	}
}

func TestStartPreviousTelemetryDrainRunsAsynchronously(t *testing.T) {
	original := drainTelemetry
	t.Cleanup(func() { drainTelemetry = original })
	started := make(chan struct{})
	release := make(chan struct{})
	drainTelemetry = func(context.Context) {
		close(started)
		<-release
	}

	start := time.Now()
	startPreviousTelemetryDrain(context.Background(), true)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("startPreviousTelemetryDrain blocked for %s", elapsed)
	}
	// A hang detector, not a latency budget: the drain runs on its own
	// goroutine, and a one-second bound let host scheduling decide the
	// verdict.
	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("background telemetry drain was not invoked within 60s")
	}

	// Releasing only after the caller has returned proves the run path does not
	// wait for an in-flight drain before it can report its exit.
	close(release)
}

func TestStartPreviousTelemetryDrainSkipsCancelledRun(t *testing.T) {
	original := drainTelemetry
	t.Cleanup(func() { drainTelemetry = original })
	called := make(chan struct{}, 1)
	drainTelemetry = func(context.Context) { called <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	startPreviousTelemetryDrain(ctx, true)

	select {
	case <-called:
		t.Fatal("telemetry drain ran after the owning run was canceled")
	case <-time.After(100 * time.Millisecond):
	}
}

// consentingTelemetryHome points the telemetry client at a temp home whose
// persisted config both enables collection and records that the first-run notice
// was already shown, which is the only state in which a non-interactive run may
// record. It returns the buffer path.
func consentingTelemetryHome(t *testing.T) string {
	t.Helper()
	home := hometest.Temp(t)
	cfg := `{"enabled":true,"noticeShownAt":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(home, ".putnami-telemetry.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write telemetry config: %v", err)
	}
	return filepath.Join(home, ".putnami-telemetry-buffer.jsonl")
}

func bufferedEventNames(t *testing.T, bufferPath string) []string {
	t.Helper()
	data, err := os.ReadFile(bufferPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read telemetry buffer: %v", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode telemetry event %q: %v", line, err)
		}
		names = append(names, event.Name)
	}
	return names
}

// TestSessionObserver_RecordsSessionStart is the positive half of the ADR 0001
// §4 seam: the terminal adapter's observer records exactly the session:start it
// recorded before A3a, from the counts the engine hands it.
func TestSessionObserver_RecordsSessionStart(t *testing.T) {
	bufferPath := consentingTelemetryHome(t)

	tc := telemetry.NewClient()
	tc.PrepareRun(false)
	observer := newSessionObserver(tc, []string{"build", "--impacted"}, false)
	if observer == nil {
		t.Fatal("terminal adapter must supply an observer")
	}

	// The observer also kicks off the previous run's drain, exactly where the
	// terminal path did before A3a. Waiting for it here is not decoration: it is
	// the happens-before edge that lets Cleanup restore the hook safely.
	original := drainTelemetry
	t.Cleanup(func() { drainTelemetry = original })
	drained := make(chan struct{})
	drainTelemetry = func(context.Context) { close(drained) }

	observer.RunPlanned(context.Background(), engine.RunPlan{Commands: []string{"build"}, Projects: 2, Jobs: 3})
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("observing a planned run must start the previous run's telemetry drain")
	}
	if err := tc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	names := bufferedEventNames(t, bufferPath)
	if len(names) != 1 || names[0] != "session:start" {
		t.Fatalf("buffered events = %v, want exactly one session:start", names)
	}
}

// TestNilObserver_RecordsNothing is the negative half, and the one that keeps
// the "never send before notice" rule honest: an adapter that supplies no observer writes nothing at all, even
// with consent granted and the notice shown. Every non-terminal adapter
// (MCP, watch, lifecycle) passes nil, so this is the state they run in.
func TestNilObserver_RecordsNothing(t *testing.T) {
	bufferPath := consentingTelemetryHome(t)

	// A client that CAN collect: the buffer stays empty only because nothing
	// observed the run, not because consent was missing.
	tc := telemetry.NewClient()
	tc.PrepareRun(false)
	if !tc.CanCollect() {
		t.Fatal("fixture must be able to collect, otherwise the assertion is vacuous")
	}

	// Without a client the adapter injects the interface's nil value, not a typed
	// nil the engine would happily call.
	if observer := newSessionObserver(nil, nil, false); observer != nil {
		t.Fatalf("newSessionObserver(nil) = %v, want a nil Observer so the engine's no-op path applies", observer)
	}

	// A full engine run with no observer: the engine owns the lifecycle but no
	// telemetry of its own, so nothing reaches the buffer. This is exactly the
	// state MCP, watch, and lifecycle adapters run in.
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() {
			_, _ = engine.New().Run(context.Background(), engine.Request{
				WorkspaceRoot: t.TempDir(),
				Config:        &wsproto.Config{},
				Commands:      []string{"build"},
			}, nil)
		})
	})
	if err := tc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if names := bufferedEventNames(t, bufferPath); len(names) != 0 {
		t.Fatalf("buffered events = %v, want none", names)
	}
}

// TestApplyPublishDryRun_SingleCommandOptsIntoExecution pins the one
// deliberate CommandParams synthesis on the terminal path: exactly
// `publish --dry-run` executes with a synthesized dry-run param — and keys its
// own cache/marker space with it, so RunMarkerParams stays untouched (a
// dry-run publish must never satisfy the marker a real publish would write).
func TestApplyPublishDryRun_SingleCommandOptsIntoExecution(t *testing.T) {
	t.Parallel()
	parsed := &ParsedArgs{Commands: []string{"publish"}}
	parsed.Global.DryRun = true
	req := engine.Request{CommandParams: map[string]any{"docker": true}}

	applyPublishDryRun(&req, parsed)

	if !req.ExecutesUnderDryRun {
		t.Fatal("ExecutesUnderDryRun = false, want the publish dry-run to execute")
	}
	if req.CommandParams["dry-run"] != true {
		t.Fatalf("CommandParams[dry-run] = %v, want true", req.CommandParams["dry-run"])
	}
	if req.CommandParams["docker"] != true {
		t.Fatal("existing params must be preserved by the synthesis")
	}
	if req.RunMarkerParams != nil {
		t.Fatal("RunMarkerParams must stay nil: the synthesized param belongs IN the marker key here (re-derived per adapter)")
	}
}

func TestApplyPublishDryRun_OtherShapesKeepThePreview(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		commands []string
		dryRun   bool
	}{
		{"publish without dry-run", []string{"publish"}, false},
		{"mixed command list", []string{"package", "publish"}, true},
		{"other command", []string{"build"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed := &ParsedArgs{Commands: c.commands}
			parsed.Global.DryRun = c.dryRun
			req := engine.Request{}

			applyPublishDryRun(&req, parsed)

			if req.ExecutesUnderDryRun {
				t.Fatal("ExecutesUnderDryRun = true, want the plan-only preview preserved")
			}
			if _, synthesized := req.CommandParams["dry-run"]; synthesized {
				t.Fatal("dry-run param synthesized outside the exact publish form")
			}
		})
	}
}
