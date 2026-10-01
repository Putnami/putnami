package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestSessionReportingSharedCorpus(t *testing.T) {
	data, err := os.ReadFile("conformance/session-reporting.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus []struct {
		Name  string          `json:"name"`
		Kind  string          `json:"kind"`
		Valid bool            `json:"valid"`
		Wire  json.RawMessage `json:"wire"`
		// Reporter, when present, names the capability that sends or receives
		// the frame: a frame for an artifact it does not receive is invalid.
		Reporter string `json:"reporter"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	reporters := map[string]bool{}
	for _, c := range corpus {
		t.Run(c.Name, func(t *testing.T) {
			var err error
			var artifact string
			if c.Kind == "chunk" {
				var chunk *SessionReportingChunk
				if chunk, err = ParseSessionReportingChunk(c.Wire); err == nil {
					artifact = chunk.Artifact
				}
			} else {
				var ack *SessionReportingAck
				if ack, err = ParseSessionReportingAck(c.Wire); err == nil {
					artifact = ack.Artifact
				}
			}
			if err == nil && c.Reporter != "" && !slices.Contains(SessionReportingArtifacts(c.Reporter), artifact) {
				err = fmt.Errorf("%s does not receive %s", c.Reporter, artifact)
			}
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v error=%v", c.Valid, err)
			}
		})
		reporters[c.Reporter] = reporters[c.Reporter] || c.Valid
	}
	for _, command := range []string{SessionReporterCommand, LogReporterCommand} {
		if !reporters[command] {
			t.Errorf("the corpus holds no valid frame for %s", command)
		}
	}
}

// TestReporterCapabilityNames pins the discovery names and the artifacts each
// reporter capability receives. The TypeScript twin pins the same literals.
func TestReporterCapabilityNames(t *testing.T) {
	names := [][2]string{
		{SessionReporterCommand, "session-reporter"}, {SessionReporterEnv, "PUTNAMI_SESSION_REPORTER"}, {SessionReporterTokenEnv, "PUTNAMI_SESSION_REPORTER_TOKEN"},
		{LogReporterCommand, "log-reporter"}, {LogReporterEnv, "PUTNAMI_LOG_REPORTER"}, {LogReporterTokenEnv, "PUTNAMI_LOG_REPORTER_TOKEN"},
	}
	for _, name := range names {
		if name[0] != name[1] {
			t.Errorf("reporter name %q, want %q", name[0], name[1])
		}
	}
	for command, want := range map[string][]string{
		SessionReporterCommand: {"session.json", "events.jsonl"},
		LogReporterCommand:     {"events.jsonl"},
		"cache-provider":       nil,
		"":                     nil,
	} {
		if got := SessionReportingArtifacts(command); !slices.Equal(got, want) {
			t.Errorf("SessionReportingArtifacts(%q) = %v, want %v", command, got, want)
		}
	}
	SessionReportingArtifacts(LogReporterCommand)[0] = "session.json"
	if got := SessionReportingArtifacts(LogReporterCommand); !slices.Equal(got, []string{"events.jsonl"}) {
		t.Fatalf("a caller changed the log reporter's artifacts: %v", got)
	}
}

func TestSessionReportingSchemaAndBounds(t *testing.T) {
	data, err := os.ReadFile("schemas/session-reporting.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		ID   string `json:"$id"`
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != SessionReportingSchemaID {
		t.Fatal("schema id drift")
	}
	for name, value := range map[string]any{"chunk": SessionReportingChunk{}, "ack": SessionReportingAck{}} {
		if !reflect.DeepEqual(keys(schema.Defs[name].Properties), jsonFieldNames(t, value)) {
			t.Fatalf("%s fields drift", name)
		}
	}
	for _, n := range []int{1, SessionReportingChunkBytes, SessionReportingChunkBytes + 1} {
		chunk := NewSessionReportingChunk("session", "events.jsonl", 0, 0, bytes.Repeat([]byte("x"), n), false)
		wire, _ := json.Marshal(chunk)
		if n <= SessionReportingChunkBytes && len(wire) > SessionReportingLineBytes {
			t.Fatal("maximum chunk does not fit wire bound")
		}
		_, err := ParseSessionReportingChunk(wire)
		if (err == nil) != (n <= SessionReportingChunkBytes) {
			t.Fatalf("size %d: %v", n, err)
		}
	}
}

func TestSessionReportingAckBindsEveryIdentityField(t *testing.T) {
	chunk := NewSessionReportingChunk("session", "events.jsonl", 12, 3, []byte("abc"), false)
	ack := chunk.Ack()
	if !ack.Matches(chunk) {
		t.Fatal("matching acknowledgement refused")
	}
	changes := []func(*SessionReportingAck){
		func(a *SessionReportingAck) { a.ProtocolVersion++ }, func(a *SessionReportingAck) { a.SessionID = "other" }, func(a *SessionReportingAck) { a.Artifact = "session.json" }, func(a *SessionReportingAck) { a.Offset++ }, func(a *SessionReportingAck) { a.Sequence++ }, func(a *SessionReportingAck) { a.SHA256 = SessionReportingDigest(nil) }, func(a *SessionReportingAck) { a.Final = true },
	}
	for i, change := range changes {
		altered := ack
		change(&altered)
		if altered.Matches(chunk) {
			t.Errorf("identity field %d ignored", i)
		}
	}
	for _, wire := range []string{"null", "{}", `{"protocolVersion":1} {}`, string(bytes.Repeat([]byte("x"), SessionReportingLineBytes+1))} {
		if _, err := ParseSessionReportingChunk([]byte(wire)); err == nil {
			t.Fatal("malformed chunk accepted")
		}
		if _, err := ParseSessionReportingAck([]byte(wire)); err == nil {
			t.Fatal("malformed ack accepted")
		}
	}
}
