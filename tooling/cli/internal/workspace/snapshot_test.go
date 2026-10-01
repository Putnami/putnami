package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
)

// snapshotWorkspace lays out a two-project workspace on disk and loads it.
func snapshotWorkspace(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()

	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("putnami.workspace.json", `{"name":"ws","version":"1.0.0","includes":["lib","app","svc"]}`)
	write("lib/putnami.json", `{"name":"lib","type":"library","tags":["go"]}`)
	write("lib/go.mod", "module example.com/lib\n\ngo 1.25\n")
	write("app/putnami.json", `{"name":"app","type":"application","dependencies":["lib"]}`)
	write("app/go.mod", "module example.com/app\n\ngo 1.25\n\nrequire example.com/lib v0.0.0\n")
	// svc deliberately carries NO putnami.json: its core metadata input is
	// recorded as ABSENT, which is what makes "creating one invalidates" testable.
	write("svc/go.mod", "module example.com/svc\n\ngo 1.25\n")

	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })

	ws, err := Load(root)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	return ws
}

func TestSnapshot_RoundTripAndValidity(t *testing.T) {
	ws := snapshotWorkspace(t)

	snapshot, err := RefreshSnapshot(ws, SnapshotWritePolicy{})
	if err != nil {
		t.Fatalf("refresh snapshot: %v", err)
	}
	if snapshot.Version != snapshotFormatVersion {
		t.Errorf("version = %d", snapshot.Version)
	}
	if len(snapshot.Projects) != 3 {
		t.Fatalf("projects = %d, want 3", len(snapshot.Projects))
	}
	if snapshot.Projects[0].ID > snapshot.Projects[1].ID {
		t.Error("projects are not in canonical ID order")
	}
	for _, project := range snapshot.Projects {
		if project.MetadataDigest == "" {
			t.Errorf("project %s has no metadata digest", project.ID)
		}
	}

	loaded, err := LoadSnapshot(ws.Root)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if loaded == nil {
		t.Fatal("snapshot was not persisted")
	}
	if loaded.IdentityDigest != snapshot.IdentityDigest {
		t.Errorf("identity digest did not round-trip: %q vs %q", loaded.IdentityDigest, snapshot.IdentityDigest)
	}
	if verdict := loaded.Validate(ws.Root); !verdict.Valid {
		t.Errorf("freshly written snapshot is invalid: %+v", verdict)
	}
}

// The whole point of the snapshot: content is the oracle. A rewrite that keeps
// the byte length AND the modification time — which is what a fast editor, a
// code generator, or a checkout that preserves mtimes produces — must still
// invalidate. A stat-based oracle cannot see this.
func TestSnapshot_SameSizeSameMtimeRewriteInvalidates(t *testing.T) {
	ws := snapshotWorkspace(t)
	snapshot, err := RefreshSnapshot(ws, SnapshotWritePolicy{})
	if err != nil {
		t.Fatalf("refresh snapshot: %v", err)
	}

	target := filepath.Join(ws.Root, "lib", "putnami.json")
	original, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	// Same length, different content: "library" → "librarY".
	rewritten := strings.Replace(string(before), `"library"`, `"librarY"`, 1)
	if len(rewritten) != len(before) || rewritten == string(before) {
		t.Fatalf("test setup: rewrite must keep the length and change the bytes (%d vs %d)", len(rewritten), len(before))
	}
	if err := os.WriteFile(target, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	// Restore the exact modification time so stat data agrees with the record.
	if err := os.Chtimes(target, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != original.Size() || !after.ModTime().Equal(original.ModTime()) {
		t.Fatalf("test setup: stat data must be identical (size %d/%d, mtime %v/%v)",
			after.Size(), original.Size(), after.ModTime(), original.ModTime())
	}

	verdict := snapshot.Validate(ws.Root)
	if verdict.Valid {
		t.Fatal("a same-size, same-mtime rewrite was accepted — stat, not content, decided validity")
	}
	if len(verdict.Changed) != 1 || verdict.Changed[0] != "lib/putnami.json" {
		t.Errorf("changed = %v, want [lib/putnami.json]", verdict.Changed)
	}
}

// A metadata input that did not exist must be RECORDED as absent, so creating
// it later invalidates. A snapshot that only listed existing files would stay
// valid across "this directory just became a Go module".
func TestSnapshot_CreatedMetadataInputInvalidates(t *testing.T) {
	ws := snapshotWorkspace(t)
	snapshot, err := RefreshSnapshot(ws, SnapshotWritePolicy{})
	if err != nil {
		t.Fatalf("refresh snapshot: %v", err)
	}
	if verdict := snapshot.Validate(ws.Root); !verdict.Valid {
		t.Fatalf("snapshot invalid before the change: %+v", verdict)
	}

	created := filepath.Join(ws.Root, "svc", "putnami.json")
	if err := os.WriteFile(created, []byte(`{"name":"svc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	verdict := snapshot.Validate(ws.Root)
	if verdict.Valid {
		t.Fatal("creating a metadata input did not invalidate the snapshot")
	}
	if len(verdict.Changed) != 1 || verdict.Changed[0] != "svc/putnami.json" {
		t.Errorf("changed = %v", verdict.Changed)
	}

	// And removal invalidates symmetrically.
	if err := os.Remove(filepath.Join(ws.Root, "lib", "putnami.json")); err != nil {
		t.Fatal(err)
	}
	if verdict := snapshot.Validate(ws.Root); verdict.Valid {
		t.Fatal("removing a metadata input did not invalidate the snapshot")
	}
}

// Stat data never establishes validity. Touching a file without changing its
// bytes must leave the snapshot VALID, even though the cheap evidence says
// "suspect" — the digest is what answers.
func TestSnapshot_TouchWithoutContentChangeStaysValid(t *testing.T) {
	ws := snapshotWorkspace(t)
	snapshot, err := RefreshSnapshot(ws, SnapshotWritePolicy{})
	if err != nil {
		t.Fatalf("refresh snapshot: %v", err)
	}

	target := filepath.Join(ws.Root, "app", "putnami.json")
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatal(err)
	}

	// The cheap check must flag it...
	var recorded SnapshotInput
	for _, project := range snapshot.Projects {
		for _, input := range project.Inputs {
			if input.Path == "app/putnami.json" {
				recorded = input
			}
		}
	}
	if recorded.Path == "" {
		t.Fatal("app/putnami.json was not recorded as a metadata input")
	}
	if !statDisagrees(ws.Root, recorded) {
		t.Error("stat did not flag a touched file, so this fixture no longer tests the disagreement")
	}
	// ...and the content oracle must still accept it.
	if verdict := snapshot.Validate(ws.Root); !verdict.Valid {
		t.Errorf("a touched-but-unchanged file invalidated the snapshot: %+v", verdict)
	}
}

// Validate reports EVERY changed input, not just the first.
//
// This replaces the old prioritizeInputs test, and it pins the property that
// made the prioritization pointless: planProbe consumes the complete Changed
// list to attribute each changed file to the provider that declared it, so
// Validate has no early exit to reorder work in front of. Every input is read
// on every call, whatever order they are in.
func TestSnapshot_ValidateReportsEveryChangedInput(t *testing.T) {
	ws := snapshotWorkspace(t)
	snapshot, err := RefreshSnapshot(ws, SnapshotWritePolicy{})
	if err != nil {
		t.Fatalf("refresh snapshot: %v", err)
	}

	for _, project := range []string{"app", "lib"} {
		target := filepath.Join(ws.Root, project, "putnami.json")
		if err := os.WriteFile(target, []byte(`{"name":"`+project+`","version":"9.9.9"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	verdict := snapshot.Validate(ws.Root)
	if verdict.Valid {
		t.Fatal("two changed metadata inputs left the snapshot valid")
	}
	want := []string{"app/putnami.json", "lib/putnami.json"}
	if !slices.Equal(verdict.Changed, want) {
		t.Fatalf("changed = %v, want %v — attribution needs the COMPLETE list", verdict.Changed, want)
	}
}

func pathsOf(inputs []SnapshotInput) []string {
	out := make([]string, len(inputs))
	for i, input := range inputs {
		out[i] = input.Path
	}
	return out
}

// --plan and --dry-run may probe in memory but must never write. The refusal is
// enforced by the write path itself, not by every caller remembering to skip.
func TestSnapshot_PlanAndDryRunNeverWrite(t *testing.T) {
	for _, policy := range []SnapshotWritePolicy{{Plan: true}, {DryRun: true}, {Plan: true, DryRun: true}} {
		ws := snapshotWorkspace(t)

		snapshot, err := RefreshSnapshot(ws, policy)
		if !errors.Is(err, ErrSnapshotWriteNotPermitted) {
			t.Fatalf("policy %+v: err = %v, want ErrSnapshotWriteNotPermitted", policy, err)
		}
		if snapshot == nil {
			t.Fatalf("policy %+v: the in-memory snapshot must still be returned", policy)
		}
		if _, err := os.Stat(SnapshotPath(ws.Root)); !os.IsNotExist(err) {
			t.Fatalf("policy %+v: snapshot was written to disk", policy)
		}

		if err := WriteSnapshot(ws.Root, snapshot, policy); !errors.Is(err, ErrSnapshotWriteNotPermitted) {
			t.Fatalf("policy %+v: WriteSnapshot err = %v", policy, err)
		}
		if _, err := os.Stat(SnapshotPath(ws.Root)); !os.IsNotExist(err) {
			t.Fatalf("policy %+v: WriteSnapshot wrote despite the policy", policy)
		}
	}
}

func TestSnapshotWritePolicy_Persists(t *testing.T) {
	if !(SnapshotWritePolicy{}).Persists() {
		t.Error("an ordinary run must persist")
	}
	if (SnapshotWritePolicy{Plan: true}).Persists() || (SnapshotWritePolicy{DryRun: true}).Persists() {
		t.Error("plan/dry-run must not persist")
	}
}

// The write must be atomic: a reader concurrent with a writer sees either the
// previous complete snapshot or the new one, never a truncated prefix. The
// observable proxy is that no partial file survives a completed write and the
// staging temp file is gone.
func TestWriteSnapshot_AtomicAndLeavesNoStaging(t *testing.T) {
	ws := snapshotWorkspace(t)
	first := NewSnapshot(ws, "wp1:first")
	if err := WriteSnapshot(ws.Root, first, SnapshotWritePolicy{}); err != nil {
		t.Fatal(err)
	}
	second := NewSnapshot(ws, "wp1:second")
	if err := WriteSnapshot(ws.Root, second, SnapshotWritePolicy{}); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSnapshot(ws.Root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProbeDigest != "wp1:second" {
		t.Errorf("probe digest = %q, want the second write", loaded.ProbeDigest)
	}

	entries, err := os.ReadDir(filepath.Join(ws.Root, ".putnami"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != WorkspaceIndexFilename {
			t.Errorf("staging artifact left behind: %s", entry.Name())
		}
	}

	if err := WriteSnapshot(ws.Root, nil, SnapshotWritePolicy{}); err == nil {
		t.Error("writing a nil snapshot must fail")
	}
}

func TestLoadSnapshot_MissingIsNotAnErrorButCorruptIs(t *testing.T) {
	root := t.TempDir()
	snapshot, err := LoadSnapshot(root)
	if err != nil || snapshot != nil {
		t.Fatalf("missing snapshot = (%v, %v), want (nil, nil)", snapshot, err)
	}

	if err := os.MkdirAll(filepath.Join(root, ".putnami"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SnapshotPath(root), []byte(`{"version":1,"surprise":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(root); err == nil {
		t.Error("an unknown field was accepted; a corrupted snapshot must not read as an empty one")
	}
}

func TestSnapshotValidate_RejectsForeignFormatVersion(t *testing.T) {
	root := t.TempDir()
	if verdict := (*Snapshot)(nil).Validate(root); verdict.Valid {
		t.Error("a nil snapshot is not valid")
	}
	for _, version := range []int{1, 2, snapshotFormatVersion + 1} {
		foreign := &Snapshot{Version: version}
		verdict := foreign.Validate(root)
		if verdict.Valid || !strings.Contains(verdict.Reason, "snapshot format") {
			t.Errorf("version %d verdict = %+v, want a format-version rejection", version, verdict)
		}
	}
}

// Workspace-level metadata inputs must be covered too: a scope's putnami.json
// carries namePattern/tags that reach every project under it.
func TestSnapshot_CoversWorkspaceAndScopeInputs(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(wsproto.WorkspaceConfigFilename, `{"name":"ws","version":"1.0.0","includes":["group"]}`)
	write("group/putnami.json", `{"includes":["leaf"],"tags":["scoped"]}`)
	write("group/leaf/putnami.json", `{"name":"leaf"}`)

	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	snapshot := NewSnapshot(ws, ws.ProbeDigest())
	covered := make(map[string]bool, len(snapshot.Inputs))
	for _, input := range snapshot.Inputs {
		covered[input.Path] = true
	}
	// The root package.json is deliberately NOT here since slice C4b: it is no
	// longer a core discovery source, and it is recorded — with the same content
	// digest — by whichever adapter declares it, so a change to it re-probes that
	// one provider instead of invalidating every provider at once.
	for _, want := range []string{wsproto.WorkspaceConfigFilename, "group/putnami.json"} {
		if !covered[want] {
			t.Errorf("workspace input %q is not recorded (recorded: %v)", want, pathsOf(snapshot.Inputs))
		}
	}
	if verdict := snapshot.Validate(ws.Root); !verdict.Valid {
		t.Fatalf("fresh snapshot invalid: %+v", verdict)
	}

	// Changing the scope config must invalidate.
	write("group/putnami.json", `{"includes":["leaf"],"tags":["scoped","extra"]}`)
	if verdict := snapshot.Validate(ws.Root); verdict.Valid {
		t.Error("a scope config change did not invalidate the snapshot")
	}
}
