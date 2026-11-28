package sessionrefs

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/store"
)

// TestAReferenceThisCLICannotRebuildIsNamed reads a reference recorded by
// another projection, one whose records this CLI rebuilds to other bytes than
// the ones its producer digested, and one whose result, verified byte for
// byte, yields another count of records than the reference names: Resolve
// rebuilds nothing and names the projection, and Resolved returns no byte, so
// a reporter sends none rather than other ones.
func TestAReferenceThisCLICannotRebuildIsNamed(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-reference-this-cli-cannot-rebuild-is-named")
	entry := testEntry(t)
	entries := testEntries{testKey: entry}
	end := lines(recordHit(t, entry, true))[2]
	_, reference, ok := output.SplitCacheReference(end)
	if !ok || reference.Projection != output.CachedRecordsProjection || !strings.HasPrefix(reference.Records, "sha256:") {
		t.Fatalf("the recorded reference = %+v, want this CLI's projection and the digest of its records", reference)
	}
	if _, _, err := Resolve(entries, end); err != nil {
		t.Fatalf("this CLI's own reference does not resolve: %v", err)
	}

	other := bytes.Replace(end, []byte(`"projection":1,`), []byte(`"projection":2,`), 1)
	digest := bytes.Replace(end, []byte(reference.Records), []byte("sha256:"+strings.Repeat("0", 64)), 1)
	fewer := *entry
	fewerResult := *entry.Result
	fewerResult.Events = fewerResult.Events[:1]
	fewer.Result = &fewerResult
	for name, tc := range map[string]struct {
		entries  Entries
		line     []byte
		recorded int
		says     string
	}{
		"another projection": {entries, other, 2, "projection 2"},
		"other bytes":        {entries, digest, output.CachedRecordsProjection, "projection 1 rebuilds"},
		"other counts":       {testEntries{testKey: &fewer}, end, output.CachedRecordsProjection, "projection 1 rebuilds"},
	} {
		records, _, err := Resolve(tc.entries, tc.line)
		var projection *ProjectionError
		if !errors.As(err, &projection) || projection.Recorded != tc.recorded || records != nil || !strings.Contains(err.Error(), tc.says) {
			t.Fatalf("%s: Resolve = %d records, %v; want a projection error of %d that says %q", name, len(records), err, tc.recorded, tc.says)
		}
		if data, err := Resolved(tc.entries, tc.line); data != nil || !errors.As(err, &projection) {
			t.Fatalf("%s: Resolved = %q, %v; want no byte and the projection error", name, data, err)
		}
	}
}

// TestADeclaredSessionResolvesInTheStoreItNames declares that a session
// directory outside any workspace records by reference: it then resolves in
// the store its declaration names. An undeclared one declares nothing.
func TestADeclaredSessionResolvesInTheStoreItNames(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "readers-rebuild-the-replayed-records")
	dir, storeRoot := t.TempDir(), t.TempDir()
	if Declared(dir) || Of(dir) != nil {
		t.Fatal("an undeclared directory outside a workspace declares references, or resolves in a store")
	}
	if err := Declare(dir, storeRoot); err != nil {
		t.Fatal(err)
	}
	local, ok := Of(dir).(*store.LocalStore)
	if !Declared(dir) || !ok || local.Root() != storeRoot {
		t.Fatalf("a declared session resolves in %v, want the store at %s", Of(dir), storeRoot)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, File+".tmp-*")); len(matches) != 0 {
		t.Fatalf("declaring left %v", matches)
	}
}
