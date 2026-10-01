package lockfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// The source-workspace sentinel says "this workspace builds its
// own CLI". These tests pin the three properties the rest of the system relies
// on: it survives a round trip byte for byte, it is distinguishable from a
// published pin, and `pin` can still replace or remove it.

// sentinelLock is the exact shape a self-hosting workspace commits: a `cli`
// object with nothing but the sentinel source. `version` and `integrities` are
// absent by construction — a workspace that builds its own engine pins no
// published artifact, and recording an empty version would claim one. The
// `toolchains` member is omitempty, so an empty one is not rendered at v3.
const sentinelLock = `{
  "version": 3,
  "cli": {
    "source": "workspace"
  },
  "extensions": {},
  "templates": {}
}
`

func TestWorkspaceSourceSentinelIsDistinguishableFromAPin(t *testing.T) {
	source := LockEntry{Source: SourceWorkspace}
	if !source.IsWorkspaceSource() {
		t.Fatalf("%+v must report as a source workspace", source)
	}
	if source.Version != "" {
		t.Errorf("a source-workspace entry must carry no version, got %q", source.Version)
	}

	// Whitespace around the literal is tolerated (a hand-edited lock), but no
	// other value is: a download URL is a PIN and must never be mistaken for
	// self-hosting, and the empty entry is the hand-edited versionless pin the
	// launcher already reports separately.
	for _, entry := range []LockEntry{
		{},
		{Version: "1.2.3"},
		{Source: "https://put.putnami.dev/putnami/cli/download?channel=1.2.3"},
		{Source: "workspaces"},
		{Source: "Workspace"},
	} {
		if entry.IsWorkspaceSource() {
			t.Errorf("%+v must not report as a source workspace", entry)
		}
	}
	if !(LockEntry{Source: "  workspace\n"}).IsWorkspaceSource() {
		t.Error("surrounding whitespace must not hide the sentinel")
	}
}

// The committed bytes must survive read → canonical write unchanged. If they
// did not, the first `putnami install` in the workspace would rewrite the
// committed lock as a side effect, dirtying the tree and failing the
// tracked-worktree mutation guard on CI — the exact class of failure the pin
// was introduced to fix.
func TestWorkspaceSourceSentinelRoundTripsByteForByte(t *testing.T) {
	lf, err := ParseLockFile([]byte(sentinelLock))
	if err != nil {
		t.Fatalf("ParseLockFile: %v", err)
	}
	entry, ok := lf.GetCLI()
	if !ok || !entry.IsWorkspaceSource() {
		t.Fatalf("GetCLI() = %+v, %v; want the source-workspace sentinel", entry, ok)
	}
	if lf.Version != FormatVersionV3 {
		t.Errorf("the sentinel must not promote the format: version = %d, want %d", lf.Version, FormatVersionV3)
	}

	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatalf("MarshalLockFile: %v", err)
	}
	if string(data) != sentinelLock {
		t.Errorf("canonical bytes changed.\n got:\n%s\nwant:\n%s", data, sentinelLock)
	}
	if strings.Contains(string(data), `"version": ""`) {
		t.Error(`the writer must not add an empty "version" to a source-workspace entry`)
	}
	spectest.Proves(t, "cli/engine-provenance", "source-workspace-declaration",
		"the-source-sentinel-round-trips-and-a-metadata-refresh-leaves-it-intact")
}

// A real workspace lock also survives the write path that install uses.
func TestWorkspaceSourceSentinelSurvivesWriteLockFile(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, LockFilename)
	if err := os.WriteFile(path, []byte(sentinelLock), 0o644); err != nil {
		t.Fatal(err)
	}
	lf, err := ReadLockFile(ws)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	if err := WriteLockFile(ws, lf); err != nil {
		t.Fatalf("WriteLockFile: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != sentinelLock {
		t.Errorf("an ordinary write rewrote the committed sentinel:\n%s", after)
	}

	changed, err := WriteLockFileIfChanged(ws, lf)
	if err != nil {
		t.Fatalf("WriteLockFileIfChanged: %v", err)
	}
	if changed {
		t.Error("re-writing an unchanged sentinel lock must be a no-op")
	}
}

// A published pin still round-trips with its version, so adding omitempty to
// Version cannot silently drop a real pin's identity.
func TestPublishedPinStillRecordsItsVersion(t *testing.T) {
	lf := NewLockFile()
	entry := LockEntry{Version: "1.4.2", Source: "https://example.test/cli"}
	entry.SetPlatformIntegrity(PlatformKey("linux", "amd64"), strings.Repeat("a", 64))
	lf.SetCLI(entry)
	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatalf("MarshalLockFile: %v", err)
	}
	if !strings.Contains(string(data), `"version": "1.4.2"`) {
		t.Errorf("a published pin must record its version:\n%s", data)
	}
	back, err := ParseLockFile(data)
	if err != nil {
		t.Fatalf("ParseLockFile: %v", err)
	}
	got, ok := back.GetCLI()
	if !ok || got.IsWorkspaceSource() || got.Version != "1.4.2" {
		t.Errorf("round-tripped pin = %+v, %v", got, ok)
	}
}

// RemoveCLI clears the sentinel exactly as it clears a pin, so `putnami pin
// --remove` remains the way out of self-hosting.
func TestRemoveCLIClearsTheSentinel(t *testing.T) {
	lf, err := ParseLockFile([]byte(sentinelLock))
	if err != nil {
		t.Fatal(err)
	}
	lf.RemoveCLI()
	if _, ok := lf.GetCLI(); ok {
		t.Fatal("RemoveCLI must clear the sentinel")
	}
	data, err := MarshalLockFile(lf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), SourceWorkspace) {
		t.Errorf("the sentinel survived removal:\n%s", data)
	}
}
