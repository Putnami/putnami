package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/logger"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// readinessHangDetector bounds every wait in these tests. It detects a hang; it
// is never a latency assertion.
const readinessHangDetector = 30 * time.Second

// readyLog is the JSON log stream of one application. A record that carries a
// workload readiness marker is also added to rec as "ready", so the marker's
// position among the lifecycle events is observable.
type readyLog struct {
	rec *recorder

	mu      sync.Mutex
	records []map[string]any
}

func (l *readyLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range bytes.Split(bytes.TrimSpace(p), []byte("\n")) {
		var record map[string]any
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		l.records = append(l.records, record)
		if data, ok := runtimeproto.ReadyMarkerFromLogRecord(record); ok && data.Target == runtimeproto.ReadyTargetWorkload {
			l.rec.add("ready")
		}
	}
	return len(p), nil
}

// marked returns every record that carries a readiness marker of any target.
func (l *readyLog) marked() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var marked []map[string]any
	for _, record := range l.records {
		if _, ok := record[runtimeproto.ReadyLogKey]; ok {
			marked = append(marked, record)
		}
	}
	return marked
}

// gatePlugin is a Starter that records its start once it may return: at once
// when release is nil, otherwise when release closes. It closes entered first.
type gatePlugin struct {
	name    string
	rec     *recorder
	entered chan struct{}
	release chan struct{}
	err     error
}

func (p *gatePlugin) Name() string { return p.name }

func (p *gatePlugin) Start(ctx context.Context, _ *Module) error {
	if p.entered != nil {
		close(p.entered)
	}
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.rec.add("start:" + p.name)
	return p.err
}

// newReadinessApp returns an application that logs into a readyLog and whose
// runner records "runner" and returns.
func newReadinessApp(name string) (*Application, *recorder, *readyLog) {
	rec := &recorder{}
	out := &readyLog{rec: rec}
	a := New(name)
	a.log = logger.New(name, logger.LevelDebug, logger.NewJSONSinkWriter(out))
	a.Run(func(context.Context) error {
		rec.add("runner")
		return nil
	})
	return a, rec, out
}

// recordingHook is a module OnStart hook that adds event to rec.
func recordingHook(rec *recorder, event string) func(context.Context) error {
	return func(context.Context) error {
		rec.add(event)
		return nil
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(readinessHangDetector):
		t.Fatalf("%s did not happen", what)
	}
}

func TestStart_ReadyRecordFollowsEveryStarterAndStartHook(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "completed-startup-readiness",
		"the-ready-record-follows-every-starter-and-start-hook")
	a, rec, out := newReadinessApp("ready-order")
	child := NewModule("child")
	a.Use(child)
	a.Use(&gatePlugin{name: "alpha", rec: rec})
	child.Use(&gatePlugin{name: "beta", rec: rec})
	a.Module.OnStart(recordingHook(rec, "onstart:root"))
	child.OnStart(recordingHook(rec, "onstart:child"))

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	events := rec.snapshot()
	ready := slices.Index(events, "ready")
	if ready < 0 || slices.Index(events[ready+1:], "ready") >= 0 {
		t.Fatalf("events = %v, want exactly one workload readiness record", events)
	}
	for _, event := range []string{"start:alpha", "start:beta", "onstart:root", "onstart:child"} {
		if at := slices.Index(events, event); at < 0 || at > ready {
			t.Errorf("events = %v: %s must come before the ready record", events, event)
		}
	}
	if !slices.Equal(events[ready+1:], []string{"runner"}) {
		t.Errorf("events = %v: only the runner follows the ready record", events)
	}

	marked := out.marked()
	if len(marked) != 1 {
		t.Fatalf("%d records carry a readiness marker, want the ready record alone: %v", len(marked), marked)
	}
	record := marked[0]
	if record["message"] != "🤖 ready" {
		t.Errorf("message = %v, want the ready record", record["message"])
	}
	if _, ok := record["durationMs"]; !ok {
		t.Error("the ready record lost its durationMs field")
	}
	data, ok := runtimeproto.ReadyMarkerFromLogRecord(record)
	if !ok || data.Target != runtimeproto.ReadyTargetWorkload || len(data.Endpoints) != 0 {
		t.Errorf("marker = %+v (valid %v), want a workload claim without endpoints", data, ok)
	}
}

func TestStart_ADelayedStarterOrStartHookDelaysTheReadyRecord(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "completed-startup-readiness",
		"a-delayed-starter-or-start-hook-delays-the-ready-record")
	for _, gated := range []string{"starter", "start hook"} {
		t.Run(gated, func(t *testing.T) {
			a, rec, _ := newReadinessApp("ready-delayed")
			entered, release := make(chan struct{}), make(chan struct{})
			a.Use(&gatePlugin{name: "fast", rec: rec})
			if gated == "starter" {
				a.Use(&gatePlugin{name: "slow", rec: rec, entered: entered, release: release})
			} else {
				a.Module.OnStart(func(ctx context.Context) error {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					rec.add("onstart:slow")
					return nil
				})
			}

			started := make(chan error, 1)
			go func() { started <- a.Start(context.Background()) }()
			waitFor(t, entered, "the gated "+gated)
			if events := rec.snapshot(); slices.Contains(events, "ready") {
				t.Fatalf("events = %v: the ready record was written while a %s was still running", events, gated)
			}
			close(release)
			select {
			case err := <-started:
				if err != nil {
					t.Fatalf("start: %v", err)
				}
			case <-time.After(readinessHangDetector):
				t.Fatal("Start did not return once the gate was released")
			}
			t.Cleanup(func() { _ = a.Stop(context.Background()) })

			events := rec.snapshot()
			slow := slices.IndexFunc(events, func(event string) bool { return event == "start:slow" || event == "onstart:slow" })
			ready := slices.Index(events, "ready")
			if slow < 0 || ready < slow {
				t.Errorf("events = %v, want the ready record after the delayed %s returned", events, gated)
			}
		})
	}
}

func TestStart_AFailedStarterOrStartHookWritesNoReadyRecord(t *testing.T) {
	spectest.Proves(t, "go/application-lifecycle", "completed-startup-readiness",
		"a-failed-starter-or-start-hook-writes-no-ready-record")
	boom := errors.New("boom")
	cases := map[string]func(a *Application, rec *recorder){
		"a starter fails": func(a *Application, rec *recorder) {
			a.Use(&gatePlugin{name: "ok", rec: rec})
			a.Use(&gatePlugin{name: "broken", rec: rec, err: boom})
		},
		"a start hook fails": func(a *Application, rec *recorder) {
			a.Use(&gatePlugin{name: "ok", rec: rec})
			child := NewModule("child")
			a.Use(child)
			child.OnStart(func(context.Context) error { return boom })
		},
		"a starter times out": func(a *Application, rec *recorder) {
			a.WithStartTimeout(50 * time.Millisecond)
			a.Use(&gatePlugin{name: "ok", rec: rec})
			a.Use(&gatePlugin{name: "stuck", rec: rec, release: make(chan struct{})})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			a, rec, out := newReadinessApp("ready-failed")
			setup(a, rec)
			if err := a.Start(context.Background()); err == nil {
				t.Fatal("Start succeeded, want the startup failure")
			}
			if events := rec.snapshot(); slices.Contains(events, "ready") || slices.Contains(events, "runner") {
				t.Errorf("events = %v: a failed startup reported readiness or ran its runner", events)
			}
			if marked := out.marked(); len(marked) != 0 {
				t.Errorf("a failed startup wrote %d readiness markers: %v", len(marked), marked)
			}
		})
	}
}
