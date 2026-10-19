package hostrpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReadFrameSplitsOnNewlines(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader("{\"a\":1}\n{\"b\":2}\n"), 16)
	for _, want := range []string{`{"a":1}`, `{"b":2}`} {
		frame, err := ReadFrame(reader, 64)
		if err != nil || string(frame) != want {
			t.Fatalf("ReadFrame = %q, %v, want %q", frame, err, want)
		}
	}
	if _, err := ReadFrame(reader, 64); !errors.Is(err, io.EOF) {
		t.Fatalf("after the last frame ReadFrame = %v, want io.EOF", err)
	}
}

func TestReadFrameHoldsAFrameLongerThanItsBuffer(t *testing.T) {
	long := strings.Repeat("x", 100)
	reader := bufio.NewReaderSize(strings.NewReader(long+"\n"), 16)
	frame, err := ReadFrame(reader, 100)
	if err != nil || string(frame) != long {
		t.Fatalf("ReadFrame = %d bytes, %v, want the 100-byte frame", len(frame), err)
	}
}

func TestReadFrameRefusesAFrameOverTheLimit(t *testing.T) {
	for _, input := range []string{
		strings.Repeat("x", 101) + "\n",
		strings.Repeat("x", 1000),
	} {
		reader := bufio.NewReaderSize(strings.NewReader(input), 16)
		if _, err := ReadFrame(reader, 100); !errors.Is(err, ErrFrameTooLarge) {
			t.Errorf("ReadFrame of %d bytes = %v, want ErrFrameTooLarge", len(input), err)
		}
	}
}

// A frame one byte over a limit that is a multiple of the reader's buffer is
// refused once that byte arrives, while the peer keeps the stream open and
// sends nothing more.
func TestReadFrameRefusesAFrameOverTheLimitWithoutWaitingForMore(t *testing.T) {
	t.Parallel()
	peer, sender := io.Pipe()
	t.Cleanup(func() { _ = sender.Close() })
	go func() { _, _ = io.WriteString(sender, strings.Repeat("x", 33)) }()
	reader := bufio.NewReaderSize(peer, 16)
	refused := make(chan error, 1)
	go func() {
		_, err := ReadFrame(reader, 32)
		refused <- err
	}()
	select {
	case err := <-refused:
		if !errors.Is(err, ErrFrameTooLarge) {
			t.Fatalf("ReadFrame = %v, want ErrFrameTooLarge", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ReadFrame waits for more bytes after the frame passed its limit")
	}
}

func TestReadFrameReturnsAnEmptyFrame(t *testing.T) {
	t.Parallel()
	frame, err := ReadFrame(bufio.NewReaderSize(strings.NewReader("\n{}\n"), 16), 8)
	if err != nil || len(frame) != 0 {
		t.Fatalf("ReadFrame = %q, %v, want the empty frame", frame, err)
	}
}

func TestReadFrameRefusesAStreamThatEndsInsideAFrame(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(`{"a":`))
	if _, err := ReadFrame(reader, 64); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestEncodeFrameEndsWithOneNewline(t *testing.T) {
	frame, err := EncodeFrame(Response{JSONRPC: JSONRPCVersion, ID: json.RawMessage(`1`), Result: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"; string(frame) != want {
		t.Fatalf("EncodeFrame = %q, want %q", frame, want)
	}
}

func TestEncodeFrameRefusesAValueOverTheLimit(t *testing.T) {
	if _, err := EncodeFrame(strings.Repeat("x", MaxFrameBytes)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("EncodeFrame = %v, want ErrFrameTooLarge", err)
	}
}

func TestResponseCarriesANullIDWhenTheRequestHadNone(t *testing.T) {
	encoded, err := json.Marshal(Response{JSONRPC: JSONRPCVersion, Error: &Error{Code: CodeParseError, Message: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"m"}}`; string(encoded) != want {
		t.Fatalf("encoded %s, want %s", encoded, want)
	}
}

func TestDecodeStrictRefusesUnknownMembersAndTrailingData(t *testing.T) {
	var params CollaborationCallParams
	if err := DecodeStrict([]byte(`{"contract":"tasks","operation":"find"}`), &params); err != nil {
		t.Fatalf("a valid document was refused: %v", err)
	}
	for _, document := range []string{
		`{"contract":"tasks","operaton":"find"}`,
		`{"contract":"tasks"} {}`,
		`{"contract":"tasks"}}`,
	} {
		if err := DecodeStrict([]byte(document), &params); err == nil {
			t.Errorf("DecodeStrict accepted %s", document)
		}
	}
}

func TestMethodsAreSortedAndComplete(t *testing.T) {
	methods := Methods()
	if !slices.IsSorted(methods) {
		t.Errorf("Methods() = %v, not sorted", methods)
	}
	want := []string{"collaboration.call", "host.hello", "verification.report", "version.lines", "workspace.owners"}
	if !slices.Equal(methods, want) {
		t.Errorf("Methods() = %v, want %v", methods, want)
	}
}

func TestValidWorkspacePathIsAnIOFSPathBelowTheRoot(t *testing.T) {
	for path, want := range map[string]bool{
		"a.go":     true,
		"dir/a.go": true,
		"":         false,
		".":        false,
		"..":       false,
		"../a":     false,
		"/a":       false,
		"a/":       false,
		"a//b":     false,
		"a/./b":    false,
	} {
		if got := ValidWorkspacePath(path); got != want {
			t.Errorf("ValidWorkspacePath(%q) = %t, want %t", path, got, want)
		}
	}
}

func TestAPathWithoutOwnersTravelsAsAnEmptyArray(t *testing.T) {
	encoded, err := json.Marshal(WorkspaceOwnersResult{Owners: []PathOwners{{Path: "a.go", Projects: []OwningProject{}}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"owners":[{"path":"a.go","projects":[]}]}`; string(encoded) != want {
		t.Fatalf("encoded %s, want %s", encoded, want)
	}
}

func TestAVersionLineTravelsWithItsWireNames(t *testing.T) {
	encoded, err := json.Marshal(VersionLinesResult{
		SourceRevision: "abc",
		Lines: []VersionLine{
			{Line: "", TagPattern: "v{version}", Version: "1.0.0", Tagged: true, Tag: "v1.0.0", Commits: []LineCommit{}},
			{Line: "tooling", TagPattern: "tooling/v{version}", Version: "0.2.0-20261019000000-abc",
				Release: &LineRelease{Version: "0.2.0", Tag: "tooling/v0.2.0"},
				Commits: []LineCommit{{SHA: "abc", Subject: "feat!: x", Body: "y"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sourceRevision":"abc","lines":[` +
		`{"line":"","tagPattern":"v{version}","version":"1.0.0","tagged":true,"tag":"v1.0.0","commits":[]},` +
		`{"line":"tooling","tagPattern":"tooling/v{version}","version":"0.2.0-20261019000000-abc","tagged":false,` +
		`"release":{"version":"0.2.0","tag":"tooling/v0.2.0"},"commits":[{"sha":"abc","subject":"feat!: x","body":"y"}]}]}`
	if string(encoded) != want {
		t.Fatalf("encoded %s\nwant    %s", encoded, want)
	}
	recorded, err := json.Marshal(VersionLinesResult{SourceRevision: "abc", Lines: []VersionLine{},
		RecordedGraph: &RecordedGraph{ObservedAt: "2026-10-19T00:00:00Z", RefreshError: "offline"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"sourceRevision":"abc","recordedGraph":{"observedAt":"2026-10-19T00:00:00Z","refreshError":"offline"},"lines":[]}`; string(recorded) != want {
		t.Fatalf("encoded %s\nwant    %s", recorded, want)
	}
}

func TestVerificationReportTravelsAsBase64(t *testing.T) {
	encoded, err := json.Marshal(VerificationReportResult{Report: []byte("<report/>")})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"report":"PHJlcG9ydC8+"}`; string(encoded) != want {
		t.Fatalf("encoded %s, want %s", encoded, want)
	}
}
