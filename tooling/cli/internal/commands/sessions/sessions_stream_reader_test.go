package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// recordedStreamFixture holds every shape a recorded events.jsonl can take: a
// record, a blank line, a malformed line, a CRLF record, and torn trailing
// bytes no LF ends.
const recordedStreamFixture = "{\"type\":\"job:end\",\"jobKey\":\"a\"}\n\n{not-json}\n{\"type\":\"job:end\",\"jobKey\":\"b\"}\r\n{\"type\":\"job:end\",\"jobKey\":\"torn\"}"

func writeRecordedStream(t *testing.T, content string) (*workspace_state.SessionStore, string) {
	t.Helper()
	store := workspace_state.NewSessionStore(t.TempDir())
	id := "20260917-120000-abc123"
	dir := filepath.Join(store.Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return store, id
}

// TestSessionEventReadersKeepTheirSemanticsThroughTheStreamReader proves the
// sessions reader, now a subscriber of the recorded stream from offset 0,
// returns exactly what the whole-file reader it replaced returned.
func TestSessionEventReadersKeepTheirSemanticsThroughTheStreamReader(t *testing.T) {
	store, id := writeRecordedStream(t, recordedStreamFixture)

	var lenient []workspace_state.SessionEvent
	for _, line := range strings.Split(recordedStreamFixture, "\n") {
		line = strings.TrimSpace(line)
		var event workspace_state.SessionEvent
		if line != "" && json.Unmarshal([]byte(line), &event) == nil {
			lenient = append(lenient, event)
		}
	}
	got, err := readSessionEvents(store, id)
	if err != nil || !reflect.DeepEqual(got, lenient) || len(got) != 3 {
		t.Fatalf("lenient reader = %+v, %v; want the whole-file reader's %+v", got, err, lenient)
	}
}

// TestSessionsInspectPrintsSubscriberEvidence surfaces subscribers.json in the
// human inspect view, and reports an unreadable document instead of hiding it.
func TestSessionsInspectPrintsSubscriberEvidence(t *testing.T) {
	root := t.TempDir()
	store := workspace_state.NewSessionStore(root)
	id := "20260917-120000-abc123"
	dir := filepath.Join(store.Root(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(&workspace_state.SessionMetadata{ID: id, Commands: []string{"build"}})
	_ = os.WriteFile(filepath.Join(dir, "session.json"), meta, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{}\n{}\n"), 0o644)

	out, err := sharedtest.CaptureStdout(t, func() error { return SessionsInspect(root, []string{id}, "") })
	if err != nil || strings.Contains(out, "Subscribers") {
		t.Fatalf("a session without live subscribers printed evidence: %v\n%s", err, out)
	}

	evidence := protocolcli.SessionSubscribersFile{
		ProtocolVersion: protocolcli.SessionSubscribersVersion, SessionID: id,
		Stream: protocolcli.SessionStreamPosition{Offset: 6, Records: 2},
		Subscribers: []protocolcli.SessionSubscriberEvidence{protocolcli.NewSessionSubscriberEvidence(
			protocolcli.SessionReporterCommand, protocolcli.SessionStreamPosition{Offset: 6, Records: 2}, protocolcli.SessionStreamPosition{Offset: 3, Records: 1}, false)},
	}
	data, _ := json.Marshal(evidence)
	_ = os.WriteFile(filepath.Join(dir, protocolcli.SessionSubscribersFileName), data, 0o644)
	out, err = sharedtest.CaptureStdout(t, func() error { return SessionsInspect(root, []string{id}, "") })
	want := fmt.Sprintf("    %-24s %-9s acknowledged 1 records, 1 lost\n", "session-reporter", "partial")
	if err != nil || !strings.Contains(out, "  Subscribers (2 records):\n"+want) {
		t.Fatalf("inspect output lacks the reporter's partial evidence: %v\n%s", err, out)
	}

	_ = os.WriteFile(filepath.Join(dir, protocolcli.SessionSubscribersFileName), bytes.Repeat([]byte("{"), 3), 0o644)
	out, err = sharedtest.CaptureStdout(t, func() error { return SessionsInspect(root, []string{id}, "") })
	if err != nil || !strings.Contains(out, "subscribers.json is unreadable") {
		t.Fatalf("an unreadable evidence document was hidden: %v\n%s", err, out)
	}
}
