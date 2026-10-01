package runtime

import (
	"bytes"
	"errors"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestNegotiatedVersion_Total pins the resolution table. Every input must map
// to a version this package knows: an emitter reads this result and stamps it
// on every line, so an unresolved input would mean an unstampable stream.
func TestNegotiatedVersion_Total(t *testing.T) {
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
		{"2", ProtocolVersion2},
		{" 2 ", ProtocolVersion2},
		// An invoker that accepts a version newer than this build knows also
		// accepts every version below it, so clamping down is safe.
		{"3", MaxKnownProtocolVersion},
		{"99", MaxKnownProtocolVersion},
	}
	for _, tc := range cases {
		if got := NegotiatedVersion(tc.raw); got != tc.want {
			t.Errorf("NegotiatedVersion(%q) = %d, want %d", tc.raw, got, tc.want)
		}
		if !IsKnownProtocolVersion(NegotiatedVersion(tc.raw)) {
			t.Errorf("NegotiatedVersion(%q) produced an unknown version", tc.raw)
		}
	}
}

// TestNegotiatedVersionFromEnv_DefaultsToV1 is the no-leak guard: a subprocess
// invoked without an advertisement — a 0.2.x CLI, a hand-run extension binary —
// speaks v1.
func TestNegotiatedVersionFromEnv_DefaultsToV1(t *testing.T) {
	if got := NegotiatedVersionFromEnv(nil); got != ProtocolVersion {
		t.Errorf("nil lookup = %d, want %d", got, ProtocolVersion)
	}
	empty := func(string) string { return "" }
	if got := NegotiatedVersionFromEnv(empty); got != ProtocolVersion {
		t.Errorf("absent variable = %d, want %d", got, ProtocolVersion)
	}
	env := map[string]string{AcceptedVersionEnv: "2"}
	lookup := func(key string) string { return env[key] }
	if got := NegotiatedVersionFromEnv(lookup); got != ProtocolVersion2 {
		t.Errorf("advertised v2 = %d, want %d", got, ProtocolVersion2)
	}
}

// TestAdvertisedVersionEnv_RoundTrips pins that the writing side and the
// reading side agree: whatever a CLI advertises, the SDK resolves to the same
// version. They share this file precisely so they cannot drift apart.
func TestAdvertisedVersionEnv_RoundTrips(t *testing.T) {
	for _, version := range []int{ProtocolVersion, ProtocolVersion2, 7, -3} {
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
	var buf bytes.Buffer
	e := NewEmitterForVersion(&buf, ProtocolVersion2)
	if e.Version() != ProtocolVersion2 || !e.SupportsReady() {
		t.Fatalf("emitter version = %d, supportsReady = %v", e.Version(), e.SupportsReady())
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
		if evt.V != ProtocolVersion2 {
			t.Errorf("event[%d] (%s) v = %d, want %d", i, evt.Type, evt.V, ProtocolVersion2)
		}
	}
	if diags := ValidateEventStream(events); diag.HasErrors(diags) {
		t.Errorf("a negotiated v2 stream must validate: %v", diags)
	}
}

// TestEmitter_DefaultStreamStaysV1 is the leak guard on the emitter: the
// default constructor is unchanged, so a build that links this package without
// negotiating emits the same bytes it always did.
func TestEmitter_DefaultStreamStaysV1(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	if e.Version() != ProtocolVersion || e.SupportsReady() {
		t.Fatalf("default emitter version = %d, supportsReady = %v", e.Version(), e.SupportsReady())
	}
	if err := e.Log(LevelInfo, "hello"); err != nil {
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

// TestEmitter_ReadyRejectedOnV1Stream pins the hard failure. Downgrading to a
// `v: 1` ready line (unknown type) or slipping a `v: 2` line into a v1 stream
// (mixed-protocol-version) are both wire violations, so the only correct
// behavior is to refuse.
func TestEmitter_ReadyRejectedOnV1Stream(t *testing.T) {
	var buf bytes.Buffer
	err := NewEmitter(&buf).Ready(ReadyData{
		Target:    ReadyTargetServer,
		Endpoints: []ReadyEndpoint{{Scheme: ReadySchemeHTTP, Host: "localhost", Port: 3000}},
	})
	if !errors.Is(err, ErrReadyUnsupportedVersion) {
		t.Fatalf("Ready on a v1 stream = %v, want ErrReadyUnsupportedVersion", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused Ready must write nothing, wrote %q", buf.String())
	}
}

// TestNewEmitterForVersion_UnknownVersionFailsClosed pins that an unknown
// version does not produce an unknown-version stream: it falls back to v1, the
// vocabulary every consumer understands.
func TestNewEmitterForVersion_UnknownVersionFailsClosed(t *testing.T) {
	for _, version := range []int{0, -1, 99} {
		var buf bytes.Buffer
		e := NewEmitterForVersion(&buf, version)
		if e.Version() != ProtocolVersion {
			t.Errorf("NewEmitterForVersion(w, %d).Version() = %d, want %d", version, e.Version(), ProtocolVersion)
		}
	}
}
