package stackstest

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// WriteFile writes content at rel under root, a slash-separated path, creating
// the directories it needs.
func WriteFile(t testing.TB, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Snapshot reads every file under root, by slash-separated path.
func Snapshot(t testing.TB, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // a fixture file under the test's root
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Changed lists, sorted, the files under root that differ from before, a
// Snapshot of it: each one added, removed or rewritten with other bytes.
func Changed(t testing.TB, root string, before map[string][]byte) []string {
	t.Helper()
	after := Snapshot(t, root)
	var out []string
	for rel, data := range after {
		if old, ok := before[rel]; !ok || !bytes.Equal(old, data) {
			out = append(out, rel)
		}
	}
	for rel := range before {
		if _, ok := after[rel]; !ok {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}
