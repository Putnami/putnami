package workspacejob_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// The tasks the CLI runs accept only the Go release the workspace lock pins.
// The Go the CLI provisions for `putnami install` is the one they run, so a
// pinned release is exact for the extension too: a go on PATH that runs
// another release is passed over for the provisioned one. Without a pin, or
// with a pin older than the workspace requires, the workspace's Go version
// stays a minimum.
func TestFindGoBinaryAcceptsOnlyTheLockedGo(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "locked-go-is-exact", "a-go-on-path-that-is-not-the-pin-is-passed-over")
	for _, tc := range []struct {
		name     string
		lock     string // the pinned release, or "" for no lock
		pathGo   string // the release the go on PATH runs
		wantPath bool   // the go on PATH is selected
		wantLog  string
	}{
		{name: "the go on PATH is the pinned release", lock: lockedGo, pathGo: lockedGo, wantPath: true},
		{
			name: "the go on PATH is newer than the pin", lock: lockedGo, pathGo: "1.99.5",
			wantLog: "System Go 1.99.5 is not the Go " + lockedGo + " the workspace lock pins; using managed Go",
		},
		{
			name: "the go on PATH is older than the pin", lock: lockedGo, pathGo: "1.99.0",
			wantLog: "System Go 1.99.0 is not the Go " + lockedGo + " the workspace lock pins; using managed Go",
		},
		{name: "no lock", pathGo: "1.99.5", wantPath: true},
		{name: "a pin older than the workspace requires", lock: "1.98.3", pathGo: "1.99.5", wantPath: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := jobtest.RealTempDir(t)
			jobtest.WriteFile(t, ws, "go.work", "go 1.99.0\n")
			if tc.lock != "" {
				writeLock(t, ws, map[string]any{
					"version":     tc.lock,
					"integrities": map[string]string{hostPlatform(): strings.Repeat("a", 64)},
					"source":      forbiddenSource(t),
				})
			}
			j, rec, fakes := shellFreeJob(t, ws)
			provisioned := provisionGo(t, j.GoToolchainRoot())
			pathGo := fakes.GoRelease(t, filepath.Join(fakes.Dir, pkgmeta.ExecutableName(runtime.GOOS, "go")), tc.pathGo)

			if !j.FindGoBinary() {
				t.Fatalf("FindGoBinary found no go:\n%s", rec.Transcript())
			}
			want := provisioned
			if tc.wantPath {
				want = pathGo
			}
			if j.GoBinary != want {
				t.Fatalf("GoBinary = %q, want %q:\n%s", j.GoBinary, want, rec.Transcript())
			}
			if tc.wantLog != "" && !rec.Contains(tc.wantLog) {
				t.Errorf("missing log %q:\n%s", tc.wantLog, rec.Transcript())
			}
			if invocations := fakes.Invocations(t); len(invocations) != 0 {
				t.Fatalf("the selection started shell programs: %v", invocations)
			}
		})
	}
}

// FindGoBinary installs nothing: with a lock that pins Go and no go anywhere,
// it reports false, asks the lock's source for nothing, writes nothing under
// the Putnami home or into the workspace, and leaves no diagnostic, so a
// caller decides whether a missing Go is an error.
func TestFindGoBinaryInstallsNothing(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "managed-go-is-shared",
		"the-job-downloads-no-go-and-writes-nothing")
	ws := lockedWorkspace(t)
	putnamiHome := jobtest.RealTempDir(t)
	j, rec, fakes := shellFreeJobAt(t, ws, putnamiHome)
	if j.FindGoBinary() {
		t.Fatalf("FindGoBinary = %q with no go anywhere", j.GoBinary)
	}
	if j.GoBinary != "" {
		t.Fatalf("GoBinary = %q, want empty", j.GoBinary)
	}
	for _, event := range rec.Events() {
		if event.Kind == "diagnostic" {
			t.Fatalf("FindGoBinary reported %s:\n%s", event, rec.Transcript())
		}
	}
	if got := entryNames(t, putnamiHome); len(got) != 0 {
		t.Fatalf("the Putnami home holds %v, want nothing", got)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".putnami")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FindGoBinary wrote into the workspace: %v", err)
	}
	if invocations := fakes.Invocations(t); len(invocations) != 0 {
		t.Fatalf("FindGoBinary started shell programs: %v", invocations)
	}
	if _, err := os.Stat(workspacejob.ManagedGoDir(j.GoToolchainRoot(), lockedGo)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a Go release appeared under the Putnami home: %v", err)
	}
}
