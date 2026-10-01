package versioncmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/recorded"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// In a workspace pinned to a private CLI build, `putnami pin <newer
// build>` downloaded the archive without a credential and failed with a bare
// HTTP status. `pin` runs the credential command through its own executable;
// that executable ran its first-use install inside the command and printed the
// install's progress ahead of the bearer, so the bearer was dropped. The error
// did not say that no credential was sent, or why.
//
// These tests run the real credential command, played by this test binary (see
// TestMain), against a registry that serves the archive only to that bearer.

func putRegistryRecording(t *testing.T, name string) recorded.Response {
	t.Helper()
	return recorded.HTTP(t, filepath.Join("..", "..", "..", "testdata", "recorded", "put-registry", name))
}

// pinWorkspace writes a workspace whose put registry is registryURL and
// isolates the machine-global store. It clears the two variables the
// credential command must receive from pin, so a value this test inherits
// (./putnamiw sets PUTNAMI_NO_RELAUNCH) cannot stand in for them.
func pinWorkspace(t *testing.T, registryURL string) string {
	t.Helper()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	t.Setenv(launch.NoRelaunchEnv, "")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")
	ws := t.TempDir()
	config := `{"registries":{"put":{"registry":"` + registryURL + `"}}}`
	if err := os.WriteFile(filepath.Join(ws, "putnami.workspace.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestCLIPinDownloadsAPrivateArchiveWithTheCredentialItMints(t *testing.T) {
	spectest.Proves(t, "cli/channels", "archives-follow-the-put-projection",
		"pin-mints-its-credential-and-names-the-credential-command-on-refusal")

	const token = "pkt_minted_by_the_running_cli"
	t.Setenv(fakeCLIRegistryTokenEnv, token)
	refusal := putRegistryRecording(t, "download-anonymous.401.http")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			refusal.ServeHTTP(w, r)
			return
		}
		goos, goarch := r.URL.Query().Get("os"), r.URL.Query().Get("arch")
		archive, _, err := buildTarGzArchiveAt(
			"compiled/"+pkgmeta.ExecutableName(goos, "putnami"), "private-build-for-"+lockfile.PlatformKey(goos, goarch))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer srv.Close()
	ws := pinWorkspace(t, srv.URL)

	if err := CLIPinForVersion(context.Background(), ws, []string{"1.2.3"}, ""); err != nil {
		t.Fatalf("pin with a minted credential: %v", err)
	}
	lf, err := lockfile.ReadLockFile(ws)
	if err != nil || lf == nil {
		t.Fatalf("read lock after pin: lf=%v err=%v", lf, err)
	}
	entry, _ := lf.GetCLI()
	_, want, err := buildTarGzArchiveAt("compiled/"+pkgmeta.ExecutableName(runtime.GOOS, "putnami"),
		"private-build-for-"+lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH))
	if err != nil {
		t.Fatal(err)
	}
	if got := entry.IntegrityFor(runtime.GOOS, runtime.GOARCH); got != want {
		t.Fatalf("pinned digest = %q, want the private archive's %q", got, want)
	}
}

func TestCLIPinRefusalNamesTheCredentialCommandThatReturnedNone(t *testing.T) {
	spectest.Proves(t, "cli/channels", "archives-follow-the-put-projection",
		"pin-mints-its-credential-and-names-the-credential-command-on-refusal")

	// The registry answers an anonymous reader of a private CLI archive with a
	// 401 or, as it does today, with the 404 it gives a version that does not
	// exist.
	for _, name := range []string{"download-anonymous.401.http", "cli-download-private-anonymous.404.http"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(fakeCLIRegistryTokenEnv, "")
			refusal := putRegistryRecording(t, name)
			srv := recorded.NewServer(t, nil, refusal)
			host, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			ws := pinWorkspace(t, srv.URL)

			err = CLIPinForVersion(context.Background(), ws, []string{"1.2.3"}, "")
			if err == nil {
				t.Fatal("pin succeeded without a credential")
			}
			for _, want := range []string{
				"registry " + srv.URL + "/putnami/cli/download",
				"HTTP " + strconv.Itoa(refusal.Status()),
				"`putnami cloud registry-token --host " + host.Host + "` provided no credential: " + fakeCLINotSignedIn,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q\nwant it to contain %q", err, want)
				}
			}
			// Signing in cures a 401. A 404 may be a missing version, which it
			// does not cure.
			wantNext := "putnami cloud login"
			if refusal.Status() == http.StatusNotFound {
				wantNext = ""
			}
			if next := protocolcli.SuggestedNext(err); next != wantNext {
				t.Errorf("suggested next = %q, want %q", next, wantNext)
			}
			if lf, _ := lockfile.ReadLockFile(ws); lf != nil {
				if _, pinned := lf.GetCLI(); pinned {
					t.Error("a refused pin wrote the lock")
				}
			}
		})
	}
}
