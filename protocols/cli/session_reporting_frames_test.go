package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// TestSessionReportingFrameVersionFollowsSize pins ADR 0131's rule: a frame
// of more than 64 KiB is version 3 and carries at most 4 MiB, every other
// frame is version 1, and each version keeps its own line bound.
func TestSessionReportingFrameVersionFollowsSize(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "a-large-frame-is-negotiated", "a-version-3-frame-passes-its-corpus")
	for _, tc := range []struct {
		n       int
		version int
		valid   bool
	}{
		{1, SessionReportingVersion, true},
		{SessionReportingChunkBytes, SessionReportingVersion, true},
		{SessionReportingChunkBytes + 1, SessionReportingLargeFrameVersion, true},
		{SessionReportingMaxChunkBytes, SessionReportingLargeFrameVersion, true},
		{SessionReportingMaxChunkBytes + 1, SessionReportingLargeFrameVersion, false},
	} {
		chunk := NewSessionReportingChunk("session", "session.json", 0, 0, bytes.Repeat([]byte("x"), tc.n), false)
		if chunk.ProtocolVersion != tc.version {
			t.Fatalf("%d bytes: version %d, want %d", tc.n, chunk.ProtocolVersion, tc.version)
		}
		wire := mustJSON(t, chunk)
		if len(wire) > SessionReportingMaxLineBytes && tc.valid {
			t.Fatalf("%d bytes: the line does not fit the version 3 line bound", tc.n)
		}
		parsed, err := ParseSessionReportingChunk(wire)
		if (err == nil) != tc.valid {
			t.Fatalf("%d bytes: %v, want valid=%v", tc.n, err, tc.valid)
		}
		if !tc.valid {
			continue
		}
		ack := parsed.Ack()
		if ack.ProtocolVersion != tc.version || !ack.Matches(chunk) {
			t.Fatalf("%d bytes: the acknowledgement does not echo version %d", tc.n, tc.version)
		}
		if _, err := ParseSessionReportingAck(mustJSON(t, ack)); err != nil {
			t.Fatalf("%d bytes: acknowledgement refused: %v", tc.n, err)
		}
	}
	// The wrong version for the size is refused both ways.
	small := NewSessionReportingChunk("session", "session.json", 0, 0, []byte("abc"), false)
	small.ProtocolVersion = SessionReportingLargeFrameVersion
	if _, err := ParseSessionReportingChunk(mustJSON(t, small)); err == nil {
		t.Fatal("a version 3 frame of 64 KiB or less was accepted")
	}
	// A version 1 line keeps the 96 KiB bound, padding included.
	padded := strings.Replace(string(mustJSON(t, NewSessionReportingChunk("session", "session.json", 0, 0, []byte("abc"), false))), "{", "{"+strings.Repeat(" ", SessionReportingLineBytes), 1)
	if _, err := ParseSessionReportingChunk([]byte(padded)); err == nil {
		t.Fatal("a version 1 line past its bound was accepted")
	}
	// The plan bound still holds for a version 3 frame.
	late := NewSessionReportingChunk("session", "plan.json", SessionReportingPlanBytes-SessionReportingChunkBytes, 1, bytes.Repeat([]byte("x"), SessionReportingChunkBytes+1), false)
	if _, err := ParseSessionReportingChunk(mustJSON(t, late)); err == nil {
		t.Fatal("a version 3 plan.json frame past the plan bound was accepted")
	}
}

// TestSessionReportingAckChunkBytes pins the acknowledgement's optional cap:
// absent, or between 64 KiB and 4 MiB, on an acceptance or a refusal.
func TestSessionReportingAckChunkBytes(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "a-large-frame-is-negotiated", "a-version-3-frame-passes-its-corpus")
	chunk := NewSessionReportingChunk("session", "events.jsonl", 0, 0, []byte("abc"), false)
	for cap, valid := range map[int]bool{
		SessionReportingChunkBytes: true, SessionReportingMaxChunkBytes: true,
		SessionReportingChunkBytes - 1: false, SessionReportingMaxChunkBytes + 1: false,
	} {
		ack := chunk.Ack()
		ack.ChunkBytes = cap
		if _, err := ParseSessionReportingAck(mustJSON(t, ack)); (err == nil) != valid {
			t.Errorf("chunkBytes %d: %v, want valid=%v", cap, err, valid)
		}
	}
	if _, err := ParseSessionReportingAck([]byte(strings.Replace(string(mustJSON(t, chunk.Ack())), `"ok":true`, `"ok":true,"chunkBytes":0`, 1))); err == nil {
		t.Error("a zero chunkBytes member was accepted")
	}
	if encoded := string(mustJSON(t, chunk.Ack())); strings.Contains(encoded, "chunkBytes") {
		t.Errorf("an acknowledgement without a cap encodes it: %s", encoded)
	}
	refusal := chunk.Ack()
	refusal.OK, refusal.Code, refusal.ChunkBytes = false, SessionReportingCodeFrameUnsupported, SessionReportingChunkBytes
	if _, err := ParseSessionReportingAck(mustJSON(t, refusal)); err != nil {
		t.Errorf("a frame_unsupported refusal with its cap was refused: %v", err)
	}
}

// TestSessionReportingFramingNames pins the literals the TypeScript twin and
// the reporters share.
func TestSessionReportingFramingNames(t *testing.T) {
	spectest.Proves(t, "cli/machine-contract", "a-large-frame-is-negotiated", "a-version-3-frame-passes-its-corpus")
	if SessionReportingFramingEnv != "PUTNAMI_SESSION_REPORTING_FRAMING" || SessionReportingCodeFrameUnsupported != "frame_unsupported" ||
		SessionReportingLargeFrameVersion != 3 || SessionReportingMaxChunkBytes != 4194304 || SessionReportingMaxLineBytes != 6291456 {
		t.Fatal("a framing literal drifted")
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	data, err := os.ReadFile("schemas/session-reporting.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	var cap struct {
		Minimum int `json:"minimum"`
		Maximum int `json:"maximum"`
	}
	if err := json.Unmarshal(schema.Defs["ack"].Properties["chunkBytes"], &cap); err != nil || cap.Minimum != SessionReportingChunkBytes || cap.Maximum != SessionReportingMaxChunkBytes {
		t.Fatalf("schema chunkBytes bounds %+v drift", cap)
	}
}
