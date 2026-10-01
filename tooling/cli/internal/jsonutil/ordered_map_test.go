package jsonutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnmarshalPreservesOrder(t *testing.T) {
	input := `{"name": "my-app", "version": "1.0.0", "description": "A test", "type": "module"}`
	var m OrderedMap
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"name", "version", "description", "type"}
	got := m.keys
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRoundTripPreservesOrder(t *testing.T) {
	// package.json-style ordering (not alphabetical)
	input := `{
  "name": "@putnami/app",
  "version": "1.0.0",
  "type": "module",
  "scripts": {
    "start": "node index.js",
    "test": "bun test"
  },
  "dependencies": {
    "express": "^4.0.0"
  },
  "devDependencies": {
    "@putnami/typescript": "workspace:*"
  }
}
`
	m := &OrderedMap{}
	if err := json.Unmarshal([]byte(input), m); err != nil {
		t.Fatal(err)
	}

	data, err := m.marshalIndent("  ", 0)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data) + "\n"

	if output != input {
		t.Errorf("round-trip changed output:\n--- want ---\n%s\n--- got ---\n%s", input, output)
	}
}

func TestSetNewKeyAppendsAtEnd(t *testing.T) {
	input := `{"name": "app", "version": "1.0.0"}`
	var m OrderedMap
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		t.Fatal(err)
	}

	m.Set("description", "new field")

	want := []string{"name", "version", "description"}
	got := m.keys
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSetExistingKeyPreservesPosition(t *testing.T) {
	input := `{"name": "app", "version": "1.0.0", "type": "module"}`
	var m OrderedMap
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		t.Fatal(err)
	}

	m.Set("version", "2.0.0")

	want := []string{"name", "version", "type"}
	got := m.keys
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	v, _ := m.Get("version")
	if v != "2.0.0" {
		t.Errorf("version = %v, want 2.0.0", v)
	}
}

func TestDeletePreservesOrder(t *testing.T) {
	input := `{"a": 1, "b": 2, "c": 3}`
	var m OrderedMap
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		t.Fatal(err)
	}

	m.Delete("b")

	want := []string{"a", "c"}
	got := m.keys
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNestedObjectPreservesOrder(t *testing.T) {
	input := `{"scripts": {"start": "node .", "build": "tsc", "test": "bun test"}}`
	var m OrderedMap
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		t.Fatal(err)
	}

	scripts := m.GetMap("scripts")
	if scripts == nil {
		t.Fatal("scripts should be an OrderedMap")
	}

	want := []string{"start", "build", "test"}
	got := scripts.keys
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scripts keys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReadWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	input := `{
  "name": "test",
  "version": "1.0.0",
  "scripts": {
    "start": "node .",
    "test": "bun test"
  }
}
`
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Modify a value
	m.Set("version", "2.0.0")

	if err := WriteFile(path, m); err != nil {
		t.Fatal(err)
	}

	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Replace(input, "1.0.0", "2.0.0", 1)
	if string(output) != want {
		t.Errorf("file content:\n--- want ---\n%s\n--- got ---\n%s", want, string(output))
	}
}

func TestEmptyMap(t *testing.T) {
	m := New()
	data, err := m.marshalIndent("  ", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Errorf("empty map = %q, want {}", string(data))
	}
}

func TestGetMapReturnsNilForNonObject(t *testing.T) {
	m := New()
	m.Set("name", "test")

	if m.GetMap("name") != nil {
		t.Error("GetMap should return nil for string value")
	}
	if m.GetMap("missing") != nil {
		t.Error("GetMap should return nil for missing key")
	}
}

func TestDeleteNonExistentKey(t *testing.T) {
	m := New()
	m.Set("a", 1)
	m.Delete("nonexistent") // should not panic
	if m.Len() != 1 {
		t.Errorf("len = %d, want 1", m.Len())
	}
}

// TestBytesDoesNotEscapeHTMLCharacters covers the same papercut as
// lockfile.MarshalLockFile: encoding/json's default escapes '&', '<', and '>'
// to their \uXXXX forms, a setting meant for values embedded in an HTML
// <script> tag. This package's output only ever lands in a committed config
// file (putnami.json, putnami.workspace.json), so a value round-tripped
// through Set/Bytes unchanged must come back unchanged, byte for byte.
func TestBytesDoesNotEscapeHTMLCharacters(t *testing.T) {
	m := New()
	m.Set("source", "https://put.putnami.dev/putnami/cli/download?channel=1.4.2&os=darwin")
	m.Set("note", "a <b> & c")

	data, err := m.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	for _, escaped := range []string{`\u0026`, `\u003c`, `\u003e`, "&amp;", "&lt;", "&gt;"} {
		if strings.Contains(string(data), escaped) {
			t.Errorf("Bytes HTML-escaped a value as %q where it should have left it alone:\n%s", escaped, data)
		}
	}
	if !strings.Contains(string(data), "channel=1.4.2&os=darwin") || !strings.Contains(string(data), "a <b> & c") {
		t.Errorf("Bytes did not contain the plain values verbatim:\n%s", data)
	}
}

// TestReadWriteFileRoundTripsAmpersandUnchanged is the write half of the
// dirty-tree reproduction for putnami.json/putnami.workspace.json: a file
// already on disk with a literal '&' in a string value must come back
// byte-identical when read and written back with no field changed. Before the
// fix, WriteFile after ReadFile re-escaped the '&', turning a no-op config
// rewrite (for example `projects tag`) into a working-tree diff.
func TestReadWriteFileRoundTripsAmpersandUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "putnami.json")

	input := `{
  "name": "test",
  "source": "https://put.putnami.dev/putnami/cli/download?channel=1.4.2&os=darwin"
}
`
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, m); err != nil {
		t.Fatal(err)
	}

	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != input {
		t.Errorf("unmodified read/write round trip changed the file:\n--- want ---\n%s\n--- got ---\n%s", input, string(output))
	}
}
