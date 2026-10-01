package runtime

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The v2 corpus lives in its own directory instead of joining fixtures/valid.
// fixtures/{valid,invalid} is a CROSS-LANGUAGE corpus: the TypeScript mirror
// (typescript/framework/runtime/test/jobs/events.test.ts) globs those two
// directories and asserts its v1 validator accepts every valid file. Dropping a
// v2 stream in there would fail that suite, which is exactly the additive
// invariant v2 must preserve — v1 fixtures untouched, v1 readers
// unaffected.

// TestConformance_ProtocolVersion2 pins the v2 version constant the same way
// TestConformance_ProtocolVersion pins v1: a bump is a migration, not an edit.
func TestConformance_ProtocolVersion2(t *testing.T) {
	if ProtocolVersion2 != 2 {
		t.Fatalf("ProtocolVersion2 = %d, want 2 — bumping requires a migration story", ProtocolVersion2)
	}
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — v2 is additive, v1 stays emitted", ProtocolVersion)
	}
	if !IsKnownProtocolVersion(ProtocolVersion) || !IsKnownProtocolVersion(ProtocolVersion2) {
		t.Fatal("both protocol versions must be accepted")
	}
	if IsKnownProtocolVersion(0) || IsKnownProtocolVersion(3) {
		t.Fatal("only versions 1 and 2 are known")
	}
}

func TestConformance_V2ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/v2/valid/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid v2 fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			events, err := DecodeAll(f)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if diags := ValidateEventStream(events); diag.HasErrors(diags) {
				t.Errorf("valid v2 fixture %s produced errors: %v", path, diags)
			}
			for i, e := range events {
				if e.V != ProtocolVersion2 {
					t.Errorf("event[%d].v = %d, want %d", i, e.V, ProtocolVersion2)
				}
			}
		})
	}
}

func TestConformance_V2InvalidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/v2/invalid/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid v2 fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			events, err := DecodeAll(f)
			if err != nil {
				return // a parse error is also a valid rejection
			}
			if diags := ValidateEventStream(events); !diag.HasErrors(diags) {
				t.Errorf("invalid v2 fixture %s should produce errors but none found", path)
			}
		})
	}
}

// TestConformance_V2InvalidFixtureCoverage pins that every v2 reject branch is
// exercised by a fixture: relaxing one of the readiness rules turns a red
// fixture green and fails here, the same contract protocols/job's
// TestInvalidFixtureCoverage enforces.
func TestConformance_V2InvalidFixtureCoverage(t *testing.T) {
	files, err := filepath.Glob("fixtures/v2/invalid/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	triggered := map[string]bool{}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		events, err := DecodeAll(f)
		f.Close()
		if err != nil {
			continue
		}
		for _, d := range ValidateEventStream(events) {
			triggered[d.Code] = true
		}
	}

	for _, code := range []string{
		"invalid-version",
		"invalid-event-type",
		"invalid-enum",
		"invalid-value",
		"required-field",
		"duplicate-endpoint",
		"non-canonical-order",
		"mixed-protocol-version",
	} {
		if !triggered[code] {
			t.Errorf("no invalid v2 fixture triggers %q — that reject branch is unguarded", code)
		}
	}
}

// TestConformance_V2ServeRestartReadiness pins the serve contract a readiness
// emitter answers to: readiness is per serve ITERATION, not per process. A watch-mode
// serve that restarts its workload re-announces readiness, and the stream stays
// valid with more than one ready event — unlike `result`, which is capped at
// one. A validator that started rejecting the second ready would silently break
// restart detection for the watcher.
func TestConformance_V2ServeRestartReadiness(t *testing.T) {
	f, err := os.Open("fixtures/v2/valid/serve-restart.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	events, err := DecodeAll(f)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if diags := ValidateEventStream(events); diag.HasErrors(diags) {
		t.Fatalf("a restarting serve stream must validate: %v", diags)
	}

	var ready []*ReadyData
	for _, e := range events {
		if e.Type != EventReady {
			continue
		}
		data, err := ReadyPayload(e)
		if err != nil {
			t.Fatalf("ready payload: %v", err)
		}
		ready = append(ready, data)
	}
	if len(ready) != 2 {
		t.Fatalf("serve-restart fixture carries %d ready events, want 2 (initial serve + restart)", len(ready))
	}
	for i, data := range ready {
		if data.Target != ReadyTargetServer {
			t.Errorf("ready[%d].target = %q, want %q", i, data.Target, ReadyTargetServer)
		}
		if len(data.Endpoints) == 0 {
			t.Errorf("ready[%d] carries no endpoint", i)
		}
	}
}

// TestConformance_V1FixturesUnchangedByV2 is the additivity guard: the v1
// corpus must still validate exactly as before, with every event at v1.
func TestConformance_V1FixturesUnchangedByV2(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid v1 fixtures found")
	}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		events, err := DecodeAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		for i, e := range events {
			if e.V != ProtocolVersion {
				t.Errorf("%s event[%d].v = %d, want %d — the v1 corpus must stay v1",
					path, i, e.V, ProtocolVersion)
			}
		}
	}
}
