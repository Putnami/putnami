package shared

import (
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// EnsureProjectExtension patches a project manifest a human wrote and keeps
// reading. Two properties matter beyond "the name is in the array": it must
// leave the manifest byte-identical when there is nothing to add, and it must
// preserve member order when there is, because the result is committed and
// reviewed as a diff.

func TestEnsureProjectExtensionAddsTheNameAndKeepsMemberOrder(t *testing.T) {
	dir := t.TempDir()
	writeProjectManifest(t, dir, `{
  "name": "@acme/api",
  "extensions": ["@putnami/go"],
  "version": "1.0.0"
}
`)

	if err := EnsureProjectExtension(dir, "@putnami/sdd"); err != nil {
		t.Fatalf("EnsureProjectExtension: %v", err)
	}

	// Order is asserted as text, not as a set: a manifest whose keys reshuffle
	// on every patch produces a diff nobody can review.
	want := `{
  "name": "@acme/api",
  "extensions": [
    "@putnami/go",
    "@putnami/sdd"
  ],
  "version": "1.0.0"
}
`
	if got := readProjectManifest(t, dir); got != want {
		t.Errorf("patched manifest =\n%s\nwant\n%s", got, want)
	}
}

func TestEnsureProjectExtensionCreatesTheArrayWhenTheManifestHasNone(t *testing.T) {
	dir := t.TempDir()
	writeProjectManifest(t, dir, `{
  "name": "@acme/api"
}
`)

	if err := EnsureProjectExtension(dir, "@putnami/go"); err != nil {
		t.Fatalf("EnsureProjectExtension: %v", err)
	}

	want := `{
  "name": "@acme/api",
  "extensions": [
    "@putnami/go"
  ]
}
`
	if got := readProjectManifest(t, dir); got != want {
		t.Errorf("patched manifest =\n%s\nwant\n%s", got, want)
	}
}

// A second call must be a no-op down to the bytes. `projects create` and the
// template installer both call this on the same project, so a version that
// appended unconditionally would grow a duplicate entry per run.
func TestEnsureProjectExtensionIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	original := `{
  "name": "@acme/api",
  "extensions": [
    "@putnami/go",
    "@putnami/sdd"
  ]
}
`
	writeProjectManifest(t, dir, original)

	for range 2 {
		if err := EnsureProjectExtension(dir, "@putnami/go"); err != nil {
			t.Fatalf("EnsureProjectExtension: %v", err)
		}
	}
	if got := readProjectManifest(t, dir); got != original {
		t.Errorf("an already-listed extension rewrote the manifest:\n%s\nwant\n%s", got, original)
	}
}

// The two paths that return nil without patching anything. Both are deliberate:
// a project with no manifest has nothing to declare an extension in, and a
// manifest this function cannot parse belongs to whoever reads it for real —
// clobbering it here would destroy the file instead of reporting it.
func TestEnsureProjectExtensionLeavesAnUnreadableManifestAlone(t *testing.T) {
	t.Run("no manifest", func(t *testing.T) {
		dir := t.TempDir()
		if err := EnsureProjectExtension(dir, "@putnami/go"); err != nil {
			t.Fatalf("EnsureProjectExtension: %v", err)
		}
		if _, err := os.Stat(wsproto.ResolveFile(dir, wsproto.ConfigFilename)); !os.IsNotExist(err) {
			t.Errorf("a project with no manifest gained one: %v", err)
		}
	})

	t.Run("malformed manifest", func(t *testing.T) {
		dir := t.TempDir()
		const broken = "{ not json\n"
		writeProjectManifest(t, dir, broken)
		if err := EnsureProjectExtension(dir, "@putnami/go"); err != nil {
			t.Fatalf("EnsureProjectExtension: %v", err)
		}
		if got := readProjectManifest(t, dir); got != broken {
			t.Errorf("a malformed manifest was rewritten to %q", got)
		}
	})
}

// A non-array `extensions` is replaced rather than merged, and this pins that
// as the intended answer: the value is unusable as an extension list, and the
// job matcher needs a usable one.
func TestEnsureProjectExtensionReplacesANonArrayExtensionsMember(t *testing.T) {
	dir := t.TempDir()
	writeProjectManifest(t, dir, `{
  "extensions": "@putnami/go"
}
`)

	if err := EnsureProjectExtension(dir, "@putnami/sdd"); err != nil {
		t.Fatalf("EnsureProjectExtension: %v", err)
	}

	want := `{
  "extensions": [
    "@putnami/sdd"
  ]
}
`
	if got := readProjectManifest(t, dir); got != want {
		t.Errorf("patched manifest =\n%s\nwant\n%s", got, want)
	}
}

func writeProjectManifest(t *testing.T, dir, contents string) {
	t.Helper()
	path := wsproto.ResolveFile(dir, wsproto.ConfigFilename)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readProjectManifest(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, wsproto.ConfigFilename))
	if err != nil {
		t.Fatalf("read the patched manifest: %v", err)
	}
	return string(data)
}
