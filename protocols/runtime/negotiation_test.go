package runtime

import (
	"bytes"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// TestNegotiatedVersion_Total pins the resolution table. Every input must map
// to a version an emitter writes: an emitter reads this result and stamps it on
// every line, so an unresolved input would mean an unstampable stream.
func TestNegotiatedVersion_Total(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "one-stream-one-negotiated-version", "the-resolution-is-total-and-defaults-to-2")
	cases := []struct {
		raw  string
		want int
	}{
		{"", ProtocolVersion},
		{"   ", ProtocolVersion},
		{"nonsense", ProtocolVersion},
		{"2.0", ProtocolVersion},
		{"0", ProtocolVersion},
		{"-1", ProtocolVersion},
		{"1", ProtocolVersion},
		{"2", ProtocolVersion},
		{" 2 ", ProtocolVersion},
		// An invoker that accepts a version newer than this build knows also
		// accepts every version below it, so clamping down is safe.
		{"3", MaxKnownProtocolVersion},
		{"99", MaxKnownProtocolVersion},
	}
	for _, tc := range cases {
		if got := NegotiatedVersion(tc.raw); got != tc.want {
			t.Errorf("NegotiatedVersion(%q) = %d, want %d", tc.raw, got, tc.want)
		}
		if got := NegotiatedVersion(tc.raw); got < ProtocolVersion || got > MaxKnownProtocolVersion {
			t.Errorf("NegotiatedVersion(%q) = %d, a version no emitter writes", tc.raw, got)
		}
	}
}

// TestNegotiatedVersionFromEnv_DefaultsToV2 pins the default: a subprocess
// invoked without an advertisement, such as a hand-run extension binary, speaks
// ProtocolVersion.
func TestNegotiatedVersionFromEnv_DefaultsToV2(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "one-stream-one-negotiated-version", "the-resolution-is-total-and-defaults-to-2")
	if got := NegotiatedVersionFromEnv(nil); got != ProtocolVersion {
		t.Errorf("nil lookup = %d, want %d", got, ProtocolVersion)
	}
	empty := func(string) string { return "" }
	if got := NegotiatedVersionFromEnv(empty); got != ProtocolVersion {
		t.Errorf("absent variable = %d, want %d", got, ProtocolVersion)
	}
	env := map[string]string{AcceptedVersionEnv: "2"}
	lookup := func(key string) string { return env[key] }
	if got := NegotiatedVersionFromEnv(lookup); got != ProtocolVersion {
		t.Errorf("advertised v2 = %d, want %d", got, ProtocolVersion)
	}
}

// TestAdvertisedVersionEnv_RoundTrips pins that the writing side and the
// reading side agree: whatever a CLI advertises, the SDK resolves to the same
// version. They share this file precisely so they cannot drift apart.
func TestAdvertisedVersionEnv_RoundTrips(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "one-stream-one-negotiated-version", "the-resolution-is-total-and-defaults-to-2")
	for _, version := range []int{ProtocolVersion1, ProtocolVersion, 7, -3} {
		entry := AdvertisedVersionEnv(version)
		prefix := AcceptedVersionEnv + "="
		if len(entry) <= len(prefix) || entry[:len(prefix)] != prefix {
			t.Fatalf("AdvertisedVersionEnv(%d) = %q, want %q prefix", version, entry, prefix)
		}
		value := entry[len(prefix):]
		want := clampProtocolVersion(version)
		if got := NegotiatedVersion(value); got != want {
			t.Errorf("advertise %d → %q → negotiate %d, want %d", version, value, got, want)
		}
	}
}

// TestEmitter_StreamVersionIsUniform is the mixed-version guard at the source:
// a v2 emitter stamps v2 on EVERY line, not only on the readiness one, because
// ValidateEventStream rejects a stream that carries more than one version.
func TestEmitter_StreamVersionIsUniform(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "one-stream-one-negotiated-version", "every-line-carries-the-stream-version")
	var buf bytes.Buffer
	e := NewEmitterForVersion(&buf, ProtocolVersion)
	if e.Version() != ProtocolVersion {
		t.Fatalf("emitter version = %d, want %d", e.Version(), ProtocolVersion)
	}
	if err := e.Meta("@putnami/go", "serve"); err != nil {
		t.Fatal(err)
	}
	if err := e.Log(LevelInfo, "starting"); err != nil {
		t.Fatal(err)
	}
	if err := e.Ready(ReadyData{
		Target:    ReadyTargetServer,
		Endpoints: []ReadyEndpoint{{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000}},
	}); err != nil {
		t.Fatal(err)
	}
	// SummaryData goes through emitWithExtra, the second stamping path.
	if err := e.SummaryData("done", map[string]any{"tests": 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.Result(ResultOK, nil, nil); err != nil {
		t.Fatal(err)
	}

	events, err := DecodeAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("decoded %d events, want 5", len(events))
	}
	for i, evt := range events {
		if evt.V != ProtocolVersion {
			t.Errorf("event[%d] (%s) v = %d, want %d", i, evt.Type, evt.V, ProtocolVersion)
		}
	}
	if diags := ValidateEventStream(events); diag.HasErrors(diags) {
		t.Errorf("a negotiated v2 stream must validate: %v", diags)
	}
}

// TestEmitter_DefaultStreamIsV2 pins the default constructor: a build that
// links this package without negotiating writes ProtocolVersion on every line,
// readiness included.
func TestEmitter_DefaultStreamIsV2(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "one-stream-one-negotiated-version", "every-line-carries-the-stream-version")
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	if e.Version() != ProtocolVersion {
		t.Fatalf("default emitter version = %d, want %d", e.Version(), ProtocolVersion)
	}
	if err := e.Log(LevelInfo, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := e.Ready(ReadyData{Target: ReadyTargetWorkload}); err != nil {
		t.Fatal(err)
	}
	if err := e.SummaryData("done", map[string]any{"tests": 1}); err != nil {
		t.Fatal(err)
	}
	events, err := DecodeAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for i, evt := range events {
		if evt.V != ProtocolVersion {
			t.Errorf("event[%d] v = %d, want %d", i, evt.V, ProtocolVersion)
		}
	}
}

// TestNewEmitterForVersion_NeverWritesVersion1 pins that no emitter writes a
// version 1 stream, so no emitter can write a ready line a version 1 reader
// rejects: asked for version 1, the emitter writes ProtocolVersion and its ready
// line validates.
func TestNewEmitterForVersion_NeverWritesVersion1(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "version-2-adds-only-ready", "a-version-1-stream-never-carries-ready")
	var buf bytes.Buffer
	e := NewEmitterForVersion(&buf, ProtocolVersion1)
	if e.Version() != ProtocolVersion {
		t.Fatalf("NewEmitterForVersion(w, %d).Version() = %d, want %d", ProtocolVersion1, e.Version(), ProtocolVersion)
	}
	if err := e.Ready(ReadyData{
		Target:    ReadyTargetServer,
		Endpoints: []ReadyEndpoint{{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000}},
	}); err != nil {
		t.Fatal(err)
	}
	evt, diags := ParseAndValidateEvent(bytes.TrimSpace(buf.Bytes()))
	if diag.HasErrors(diags) || evt.V != ProtocolVersion {
		t.Fatalf("ready line %q: v = %v, diagnostics %v", buf.String(), evt, diags)
	}
}

// TestNewEmitterForVersion_UnknownVersionFallsBack pins that a version no
// emitter writes does not produce an unknown-version stream: it falls back to
// ProtocolVersion.
func TestNewEmitterForVersion_UnknownVersionFallsBack(t *testing.T) {
	spectest.Proves(t, "runtime/event-stream", "one-stream-one-negotiated-version", "the-resolution-is-total-and-defaults-to-2")
	for _, version := range []int{0, -1, ProtocolVersion1, 99} {
		var buf bytes.Buffer
		e := NewEmitterForVersion(&buf, version)
		if e.Version() != ProtocolVersion {
			t.Errorf("NewEmitterForVersion(w, %d).Version() = %d, want %d", version, e.Version(), ProtocolVersion)
		}
	}
}
