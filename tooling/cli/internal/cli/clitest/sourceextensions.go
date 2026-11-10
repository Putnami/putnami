package clitest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// LinkSourceExtensions links @putnami/typescript and @putnami/dev from the
// repository at repoRoot under .ext of the workspace at root, which the
// project scan never enters: the workspace declares them as /.ext/typescript
// and /.ext/dev.
func LinkSourceExtensions(t *testing.T, repoRoot, root string) {
	t.Helper()
	for name, rel := range map[string]string{"typescript": "typescript/extension", "dev": "tooling/dev"} {
		target, err := filepath.Abs(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, ".ext"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, ".ext", name)); err != nil {
			t.Fatal(err)
		}
	}
}

// PinToolchains copies the pins of the named toolchains from the lock of the
// repository at repoRoot into the lock of the workspace at root, which it
// starts when there is none: extensions loaded from source compile their
// runtimes with the Go release the repository pins, and the Bun pin keeps an
// install from reading release metadata.
func PinToolchains(t *testing.T, repoRoot, root string, names ...string) {
	t.Helper()
	var source map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(repoRoot, "putnami.lock.json"))
	if err != nil || json.Unmarshal(data, &source) != nil {
		t.Fatalf("read the repository's lock: %v", err)
	}
	lock := map[string]json.RawMessage{"version": source["version"]}
	if data, err := os.ReadFile(filepath.Join(root, "putnami.lock.json")); err == nil {
		if err := json.Unmarshal(data, &lock); err != nil {
			t.Fatal(err)
		}
	}
	var from, into map[string]json.RawMessage
	if err := json.Unmarshal(source["toolchains"], &from); err != nil {
		t.Fatal(err)
	}
	if raw, ok := lock["toolchains"]; ok {
		if err := json.Unmarshal(raw, &into); err != nil {
			t.Fatal(err)
		}
	}
	if into == nil {
		into = map[string]json.RawMessage{}
	}
	for _, name := range names {
		into[name] = from[name]
	}
	if lock["toolchains"], err = json.Marshal(into); err != nil {
		t.Fatal(err)
	}
	if data, err = json.MarshalIndent(lock, "", "  "); err != nil {
		t.Fatal(err)
	}
	WriteFile(t, filepath.Join(root, "putnami.lock.json"), string(data)+"\n")
}
