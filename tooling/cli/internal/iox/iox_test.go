package iox_test

import (
	"bytes"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/iox"
)

func TestFprintf(t *testing.T) {
	var buf bytes.Buffer
	iox.Fprintf(&buf, "hello %s", "world")
	if got := buf.String(); got != "hello world" {
		t.Errorf("Fprintf: got %q, want %q", got, "hello world")
	}
}

func TestFprintln(t *testing.T) {
	var buf bytes.Buffer
	iox.Fprintln(&buf, "line")
	if got := buf.String(); got != "line\n" {
		t.Errorf("Fprintln: got %q, want %q", got, "line\n")
	}
}

func TestFprint(t *testing.T) {
	var buf bytes.Buffer
	iox.Fprint(&buf, "raw")
	if got := buf.String(); got != "raw" {
		t.Errorf("Fprint: got %q, want %q", got, "raw")
	}
}

func TestWrite(t *testing.T) {
	var buf bytes.Buffer
	iox.Write(&buf, []byte("bytes"))
	if got := buf.String(); got != "bytes" {
		t.Errorf("Write: got %q, want %q", got, "bytes")
	}
}

func TestReadCappedUnderCap(t *testing.T) {
	got := iox.ReadCapped(strings.NewReader("short output"), 1024)
	if got != "short output" {
		t.Errorf("ReadCapped under cap: got %q, want %q", got, "short output")
	}
}

func TestReadCappedExactCap(t *testing.T) {
	got := iox.ReadCapped(strings.NewReader("12345"), 5)
	if got != "12345" {
		t.Errorf("ReadCapped at cap: got %q, want %q (no truncation expected)", got, "12345")
	}
}

func TestReadCappedOverCapTruncatesAndDrains(t *testing.T) {
	r := bytes.NewReader([]byte(strings.Repeat("a", 100)))
	got := iox.ReadCapped(r, 10)
	if !strings.HasPrefix(got, strings.Repeat("a", 10)) {
		t.Errorf("ReadCapped over cap: capped prefix not retained, got %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("ReadCapped over cap: missing truncation marker, got %q", got)
	}
	if r.Len() != 0 {
		t.Errorf("ReadCapped over cap: reader not drained, %d bytes remain", r.Len())
	}
}
