package workspacejob_test

import (
	"net/http"
	"path/filepath"
	"runtime"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// The tasks the CLI runs accept only the Go release the workspace lock pins.
// The Go that `putnami install` prepares is the one they run, so a pinned
// release is exact for the extension too: a go on PATH that runs another
// release is passed over for the managed install of the pinned one. Without a
// pin, or with a pin older than the workspace requires, which the install
// refuses, the workspace's Go version stays a minimum.
func TestResolveGoBinaryAcceptsOnlyTheLockedGo(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "locked-go-is-exact", "a-go-on-path-that-is-not-the-pin-is-passed-over")
	archive := hostArchive(t, goDistribution())
	for _, tc := range []struct {
		name        string
		lock        string // the pinned release, or "" for no lock
		pathGo      string // the release the go on PATH runs
		wantPath    bool   // the go on PATH is selected
		wantLog     string
		wantFetches int64
	}{
		{name: "the go on PATH is the pinned release", lock: lockedGo, pathGo: lockedGo, wantPath: true},
		{
			name: "the go on PATH is newer than the pin", lock: lockedGo, pathGo: "1.99.5",
			wantLog:     "System Go 1.99.5 is not the Go " + lockedGo + " the workspace lock pins; using managed Go",
			wantFetches: 1,
		},
		{
			name: "the go on PATH is older than the pin", lock: lockedGo, pathGo: "1.99.0",
			wantLog:     "System Go 1.99.0 is not the Go " + lockedGo + " the workspace lock pins; using managed Go",
			wantFetches: 1,
		},
		{name: "no lock", pathGo: "1.99.5", wantPath: true},
		{name: "a pin older than the workspace requires", lock: "1.98.3", pathGo: "1.99.5", wantPath: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, requests := distributionServer(t, archive, http.StatusOK)
			ws := jobtest.RealTempDir(t)
			jobtest.WriteFile(t, ws, "go.work", "go 1.99.0\n")
			if tc.lock != "" {
				writeLock(t, ws, map[string]any{
					"version":     tc.lock,
					"integrities": map[string]string{hostPlatform(): digest(archive)},
					"source":      server.URL + "/dl",
				})
			}
			j, rec, fakes := shellFreeJob(t, ws)
			pathGo := fakes.GoRelease(t, filepath.Join(fakes.Dir, pkgmeta.ExecutableName(runtime.GOOS, "go")), tc.pathGo)

			if !j.ResolveGoBinary() {
				t.Fatalf("ResolveGoBinary failed:\n%s", rec.Transcript())
			}
			want := workspacejob.ManagedGoBinary(j.ExtensionStateRoot(), lockedGo)
			if tc.wantPath {
				want = pathGo
			}
			if j.GoBinary != want {
				t.Fatalf("GoBinary = %q, want %q:\n%s", j.GoBinary, want, rec.Transcript())
			}
			if tc.wantLog != "" && !rec.Contains(tc.wantLog) {
				t.Errorf("missing log %q:\n%s", tc.wantLog, rec.Transcript())
			}
			if got := requests.Load(); got != tc.wantFetches {
				t.Errorf("archive requests = %d, want %d", got, tc.wantFetches)
			}

			// The next job finds the pinned install without a download.
			again, rec2, fakes2 := shellFreeJob(t, ws)
			fakes2.GoRelease(t, filepath.Join(fakes2.Dir, pkgmeta.ExecutableName(runtime.GOOS, "go")), tc.pathGo)
			if !again.FindGoBinary() {
				t.Fatalf("FindGoBinary found no go:\n%s", rec2.Transcript())
			}
			if tc.wantPath {
				want = filepath.Join(fakes2.Dir, pkgmeta.ExecutableName(runtime.GOOS, "go"))
			}
			if again.GoBinary != want {
				t.Fatalf("FindGoBinary = %q, want %q", again.GoBinary, want)
			}
			if got := requests.Load(); got != tc.wantFetches {
				t.Errorf("FindGoBinary downloaded: %d archive requests, want %d", got, tc.wantFetches)
			}
			if invocations := fakes.Invocations(t); len(invocations) != 0 {
				t.Fatalf("the resolution started shell programs: %v", invocations)
			}
		})
	}
}

// FindGoBinary installs nothing: with no go anywhere it reports false and
// leaves no diagnostic, so a caller decides whether a missing Go is an error.
func TestFindGoBinaryInstallsNothing(t *testing.T) {
	archive := hostArchive(t, goDistribution())
	server, requests := distributionServer(t, archive, http.StatusOK)
	ws := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, ws, "go.work", "go 1.99.0\n")
	writeLock(t, ws, map[string]any{
		"version":     lockedGo,
		"integrities": map[string]string{hostPlatform(): digest(archive)},
		"source":      server.URL + "/dl",
	})
	j, rec, _ := shellFreeJob(t, ws)
	if j.FindGoBinary() {
		t.Fatalf("FindGoBinary = %q with no go anywhere", j.GoBinary)
	}
	if j.GoBinary != "" {
		t.Fatalf("GoBinary = %q, want empty", j.GoBinary)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("FindGoBinary downloaded: %d archive requests", got)
	}
	for _, event := range rec.Events() {
		if event.Kind == "diagnostic" {
			t.Fatalf("FindGoBinary reported %s:\n%s", event, rec.Transcript())
		}
	}
}
