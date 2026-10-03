package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
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
			switch c.Kind {
			case "chunk":
				var chunk *SessionReportingChunk
				if chunk, err = ParseSessionReportingChunk(c.Wire); err == nil {
					artifact = chunk.Artifact
				}
			case "ack":
				var ack *SessionReportingAck
				if ack, err = ParseSessionReportingAck(c.Wire); err == nil {
					artifact = ack.Artifact
				}
			case "handshake":
				_, err = ParseSessionReportingHandshake(c.Wire)
			case "handshake-result":
				_, err = ParseSessionReportingHandshakeResult(c.Wire)
			default:
				t.Fatalf("unknown corpus kind %q", c.Kind)
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
	for name, value := range map[string]any{
		"chunk": SessionReportingChunk{}, "ack": SessionReportingAck{},
		"handshake": SessionReportingHandshake{}, "handshakeResult": SessionReportingHandshakeResult{},
	} {
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

// TestSessionReportingHandshakeHandsTheCredentialOnlyInAuthenticate pins the
// v2 handshake: initialize carries no credential and a v1 parser rejects it,
// authenticate carries the credential within its bound, every format verb
// redacts it, and a result answers only the line of its own operation.
func TestSessionReportingHandshakeHandsTheCredentialOnlyInAuthenticate(t *testing.T) {
	const credential = "hosted-run-credential-7c1e"
	initialize := NewSessionReportingInitialize()
	wire, err := json.Marshal(initialize)
	if err != nil || string(wire) != `{"protocolVersion":2,"op":"initialize"}` {
		t.Fatalf("initialize wire = %s, %v", wire, err)
	}
	if _, err := ParseSessionReportingChunk(wire); err == nil {
		t.Fatal("a v1 chunk parser accepted initialize")
	}
	authenticate := NewSessionReportingAuthenticate(credential)
	wire, err = json.Marshal(authenticate)
	if err != nil || string(wire) != `{"protocolVersion":2,"op":"authenticate","runCredential":"`+credential+`"}` {
		t.Fatalf("authenticate wire = %s, %v", wire, err)
	}
	parsed, err := ParseSessionReportingHandshake(wire)
	if err != nil || parsed.RunCredential != credential {
		t.Fatalf("authenticate round trip = %+v, %v", parsed, err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
		if got := fmt.Sprintf(verb, authenticate); strings.Contains(got, credential) || !strings.Contains(got, "<redacted>") {
			t.Errorf("%s renders %q", verb, got)
		}
	}
	for size, valid := range map[int]bool{SessionReportingMaxCredentialBytes: true, SessionReportingMaxCredentialBytes + 1: false} {
		line, _ := json.Marshal(NewSessionReportingAuthenticate(strings.Repeat("x", size)))
		if len(line) > SessionReportingLineBytes {
			t.Fatalf("a %d-byte credential does not fit the line bound", size)
		}
		if _, err := ParseSessionReportingHandshake(line); (err == nil) != valid {
			t.Errorf("a %d-byte credential: error %v, want valid=%v", size, err, valid)
		}
	}
	// The bound counts UTF-8 bytes: 8193 two-byte characters are within the
	// schema's maxLength, which counts characters, and over the parser's bound.
	wide, _ := json.Marshal(NewSessionReportingAuthenticate(strings.Repeat("é", 8193)))
	if _, err := ParseSessionReportingHandshake(wide); err == nil {
		t.Error("a credential of 16386 UTF-8 bytes was accepted")
	}
	if !initialize.Accept().Answers(initialize) || initialize.Accept().Answers(authenticate) || !authenticate.Refuse("unauthorized").Answers(authenticate) {
		t.Fatal("a result answers another operation, or not its own")
	}
	if result, err := ParseSessionReportingHandshakeResult(mustJSON(t, authenticate.Refuse("unauthorized"))); err != nil || result.OK || result.Code != "unauthorized" {
		t.Fatalf("refusal round trip = %+v, %v", result, err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
