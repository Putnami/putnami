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
	for _, method := range []string{MethodHello, MethodCollaborationCall, MethodVerificationReport} {
		if !slices.Contains(methods, method) {
			t.Errorf("Methods() = %v, missing %s", methods, method)
		}
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
