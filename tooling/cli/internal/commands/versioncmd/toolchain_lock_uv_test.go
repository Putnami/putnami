package versioncmd

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/uvtoolchain"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// uvTestChecksums is a sha256.sum as a uv release publishes it: one
// "<sha256> *<archive>" line per archive, including the archives the lock does
// not pin (the glibc Linux builds, other architectures, the installers).
const uvTestChecksums = `1111111111111111111111111111111111111111111111111111111111111111 *uv-x86_64-apple-darwin.tar.gz
2222222222222222222222222222222222222222222222222222222222222222 *uv-aarch64-apple-darwin.tar.gz
3333333333333333333333333333333333333333333333333333333333333333 *uv-x86_64-unknown-linux-musl.tar.gz
4444444444444444444444444444444444444444444444444444444444444444 *uv-aarch64-unknown-linux-musl.tar.gz
5555555555555555555555555555555555555555555555555555555555555555 *uv-x86_64-pc-windows-msvc.zip
6666666666666666666666666666666666666666666666666666666666666666 *uv-aarch64-pc-windows-msvc.zip
7777777777777777777777777777777777777777777777777777777777777777 *uv-x86_64-unknown-linux-gnu.tar.gz
8888888888888888888888888888888888888888888888888888888888888888 *uv-aarch64-unknown-linux-gnu.tar.gz
9999999999999999999999999999999999999999999999999999999999999999 *uv-i686-pc-windows-msvc.zip
`

// uvTestIntegrities are the digests resolveUvToolchain pins from
// uvTestChecksums: the musl archives on Linux, nothing for unsupported
// platforms.
var uvTestIntegrities = map[string]string{
	"darwin/amd64":  strings.Repeat("1", 64),
	"darwin/arm64":  strings.Repeat("2", 64),
	"linux/amd64":   strings.Repeat("3", 64),
	"linux/arm64":   strings.Repeat("4", 64),
	"windows/amd64": strings.Repeat("5", 64),
	"windows/arm64": strings.Repeat("6", 64),
}

const uvTestPyproject = `[project]
name = "workspace"

[tool.uv.workspace]
members = ["pkg"]

[tool.uv]
required-version = "==0.11.32"  # the release the lock pins

[tool.uv.sources]
pkg = { workspace = true }
`

// declareUvRuntime makes ws a workspace whose one extension runs uv: its
// runtime resolves the uv lock identity, as the @putnami/python runtime does.
// Only then does the root pyproject.toml declare the uv the lock pins.
func declareUvRuntime(t *testing.T, ws string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(ws, "python-ext"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteToolchainTestFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"name":"uv-workspace","includes":["python-ext"]}`)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "python-ext", "putnami.json"), `{"name":"@example/python"}`)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "python-ext", "putnami.extension.json"), `{
  "name": "@example/python",
  "version": "1.0.0",
  "cliContract": 4,
  "runtime": {
    "executable": "compiled/example",
    "toolchains": {
      "uv": {"lock": "uv", "candidates": [{"from": "path", "path": "uv"}], "probe": {"args": ["self", "version", "--short"], "expect": "{version}"}}
    }
  },
  "commands": {"uv-version": {"description": "Print the uv release", "run": [{"id": "uv-version", "task": "uv-version"}]}},
  "tasks": {"uv-version": {"kind": "command", "command": "{extensionRuntime}", "toolchains": ["uv"], "cache": false}}
}`)
}

// runsUv is the runtimeLocks of readToolchainDeclarations for a workspace
// whose extensions resolve the uv lock identity.
func runsUv() ([]string, error) { return []string{"go", "uv"}, nil }

// A root pyproject.toml that declares one exact uv release pins it like go.work
// pins Go and packageManager pins Bun: the version, the release download source
// and the digest of each supported platform's archive, read from the checksum
// file the release publishes.
func TestRefreshLockMetadataPinsDeclaredUvFromTheReleaseChecksums(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/uv/0.11.32/sha256.sum" {
			requests++
			_, _ = w.Write([]byte(uvTestChecksums))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	withToolchainTestEndpoints(t, server.URL)

	ws := t.TempDir()
	declareUvRuntime(t, ws)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "pyproject.toml"), uvTestPyproject)
	if err := lockfile.WriteLockFile(ws, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}

	if err := RefreshLockMetadata(context.Background(), ws, "dev"); err != nil {
		t.Fatal(err)
	}
	got, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := got.GetToolchain("uv")
	if !ok {
		t.Fatalf("toolchains = %+v, want a uv pin", got.Toolchains)
	}
	if entry.Version != "0.11.32" || entry.Source != server.URL+"/uv/0.11.32/" {
		t.Errorf("toolchains.uv = %+v, want version 0.11.32 from %s/uv/0.11.32/", entry, server.URL)
	}
	if !maps.Equal(entry.Integrities, uvTestIntegrities) {
		t.Errorf("toolchains.uv.integrities = %v, want %v", entry.Integrities, uvTestIntegrities)
	}
	if requests != 1 {
		t.Errorf("checksum file read %d times, want once", requests)
	}
	spectest.Proves(t, "cli/toolchain-lock", "verified-release-metadata", "uv-pins-carry-version-source-and-digests")
}

// A uv pin that already names the declared release is kept as committed with no
// request, and a declaration of another release is resolved again, as for Bun.
func TestRefreshToolchainLockReusesAMatchingUvPinAndReResolvesAnother(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/uv/0.11.32/sha256.sum" {
			requests++
			_, _ = w.Write([]byte(uvTestChecksums))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	withToolchainTestEndpoints(t, server.URL)

	ws := t.TempDir()
	declareUvRuntime(t, ws)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "pyproject.toml"), uvTestPyproject)
	committed := lockfile.LockEntry{
		Version:     "0.11.32",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)},
		Source:      "https://github.com/astral-sh/uv/releases/download/0.11.32/",
	}
	lf := lockfile.NewLockFile()
	lf.SetToolchain("uv", committed)
	if err := refreshToolchainLock(context.Background(), ws, lf); err != nil {
		t.Fatal(err)
	}
	if entry, _ := lf.GetToolchain("uv"); entry.Source != committed.Source || !maps.Equal(entry.Integrities, committed.Integrities) {
		t.Errorf("toolchains.uv = %+v, want the committed pin kept as is", entry)
	}
	if requests != 0 {
		t.Fatalf("a uv pin at the declared release read the checksum file %d times", requests)
	}

	lf.SetToolchain("uv", lockfile.LockEntry{
		Version:     "0.10.0",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("b", 64)},
		Source:      "https://github.com/astral-sh/uv/releases/download/0.10.0/",
	})
	if err := refreshToolchainLock(context.Background(), ws, lf); err != nil {
		t.Fatal(err)
	}
	if entry, _ := lf.GetToolchain("uv"); entry.Version != "0.11.32" || !maps.Equal(entry.Integrities, uvTestIntegrities) {
		t.Errorf("toolchains.uv = %+v, want the declared 0.11.32 resolved again", entry)
	}
	if requests != 1 {
		t.Fatalf("checksum file read %d times, want once", requests)
	}
}

// An explicit install pins a declared uv the lock lacks before the installers
// run, so the Python extension's workspace-install installs that release.
func TestPinDeclaredToolchainsPinsADeclaredUv(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/uv/0.11.32/sha256.sum" {
			_, _ = w.Write([]byte(uvTestChecksums))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	withToolchainTestEndpoints(t, server.URL)

	ws := t.TempDir()
	declareUvRuntime(t, ws)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "pyproject.toml"), uvTestPyproject)
	if err := lockfile.WriteLockFile(ws, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}
	changed, err := PinDeclaredToolchains(context.Background(), ws)
	if err != nil || !changed {
		t.Fatalf("PinDeclaredToolchains = %v, %v; want the declared uv pinned", changed, err)
	}
	got, err := lockfile.ReadLockFile(ws)
	if err != nil {
		t.Fatal(err)
	}
	if entry, _ := got.GetToolchain("uv"); entry.Version != "0.11.32" || !maps.Equal(entry.Integrities, uvTestIntegrities) {
		t.Errorf("toolchains.uv = %+v, want the declared 0.11.32", entry)
	}
}

// A checksum file that names no supported archive fails the pin rather than
// writing an entry no install can verify.
func TestResolveUvToolchainFailsWithoutASupportedDigest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("1", 64) + " *uv-x86_64-unknown-linux-gnu.tar.gz\nnot-a-digest *uv-aarch64-apple-darwin.tar.gz\n"))
	}))
	defer server.Close()
	withToolchainTestEndpoints(t, server.URL)

	_, err := resolveUvToolchain(context.Background(), "0.11.32", lockfile.LockEntry{})
	if err == nil || !strings.Contains(err.Error(), "publishes no SHA-256 for a supported archive") {
		t.Fatalf("error = %v, want a refusal naming the missing digests", err)
	}
}

// A declared release older than the oldest one Putnami runs is refused before
// any request, with the minimum, even when the lock already pins it: its
// sha256.sum lists no archive (or does not exist before 0.5.0), and its
// version probe could not identify it anyway.
func TestResolveUvToolchainRefusesAReleaseOlderThanTheMinimum(t *testing.T) {
	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("an unsupported uv release must not read release metadata")
	})})
	pinned := lockfile.LockEntry{
		Version:     "0.4.30",
		Integrities: map[string]string{"linux/amd64": strings.Repeat("a", 64)},
		Source:      "https://github.com/astral-sh/uv/releases/download/0.4.30/",
	}
	for _, current := range []lockfile.LockEntry{{}, pinned} {
		_, err := resolveUvToolchain(context.Background(), "0.4.30", current)
		if err == nil {
			t.Fatal("uv 0.4.30 was pinned")
		}
		for _, want := range []string{"uv 0.4.30 is older than uv " + uvtoolchain.MinimumVersion, `required-version = "==` + uvtoolchain.MinimumVersion + `"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not name %q: %v", want, err)
			}
		}
	}

	ws := t.TempDir()
	declareUvRuntime(t, ws)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "pyproject.toml"), "[tool.uv]\nrequired-version = \"==0.10.0\"\n")
	if err := lockfile.WriteLockFile(ws, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := PinDeclaredToolchains(context.Background(), ws); err == nil || !strings.Contains(err.Error(), "resolve uv toolchain: uv 0.10.0 is older") {
		t.Fatalf("PinDeclaredToolchains error = %v, want the refusal of uv 0.10.0", err)
	}
	spectest.Proves(t, "cli/toolchain-lock", "verified-release-metadata", "a-uv-release-older-than-the-minimum-is-refused")
}

// Only an exact required-version in the [tool.uv] table declares a uv release,
// read by uvtoolchain.ReadDeclaration, whose test holds the cases. A range
// pins nothing and makes no request, and an unreadable pyproject.toml is
// reported.
func TestUvDeclarationPinsOnlyAnExactToolUvRequiredVersion(t *testing.T) {
	for pyproject, want := range map[string]string{
		uvTestPyproject: "0.11.32",
		"[tool.uv]\nrequired-version = \">=0.11\"\n":            "",
		"[tool]\nuv.required-version = \"==0.11.32\"\n":         "",
		"[tool.uv.workspace]\nrequired-version = \"0.11.32\"\n": "",
	} {
		ws := t.TempDir()
		mustWriteToolchainTestFile(t, filepath.Join(ws, "pyproject.toml"), pyproject)
		declared, err := readToolchainDeclarations(ws, runsUv)
		if err != nil {
			t.Fatal(err)
		}
		if got := declared["uv"]; got != want {
			t.Errorf("declared uv = %q for %q, want %q", got, pyproject, want)
		}
	}

	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("a uv range must not read release metadata")
	})})
	ws := t.TempDir()
	declareUvRuntime(t, ws)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "pyproject.toml"), "[tool.uv]\nrequired-version = \">=0.11\"\n")
	lf := lockfile.NewLockFile()
	if err := refreshToolchainLock(context.Background(), ws, lf); err != nil {
		t.Fatal(err)
	}
	if _, ok := lf.GetToolchain("uv"); ok {
		t.Fatalf("a uv range was pinned: %+v", lf.Toolchains)
	}

	unreadable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unreadable, "pyproject.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readToolchainDeclarations(unreadable, runsUv); err == nil || !strings.Contains(err.Error(), "read pyproject.toml") {
		t.Fatalf("error = %v, want the unreadable pyproject.toml reported", err)
	}
	spectest.Proves(t, "cli/toolchain-lock", "supported-runtimes-only", "only-an-exact-tool-uv-required-version-is-locked")
}

// A pyproject.toml is not only a uv workspace manifest. In a workspace whose
// extensions run no uv, its [tool.uv] required-version pins nothing, even a
// release Putnami would refuse to pin: the explicit install, the implicit one
// and the refresh all pass, read no release metadata and write no uv pin.
// The extensions are discovered only for a declaration that could pin.
func TestAUvDeclarationPinsNothingWithoutAnExtensionThatRunsUv(t *testing.T) {
	spectest.Proves(t, "cli/toolchain-lock", "supported-runtimes-only", "a-uv-declaration-pins-nothing-without-an-extension-that-runs-uv")
	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("a workspace that runs no uv must not read uv release metadata")
	})})
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteToolchainTestFile(t, filepath.Join(ws, "putnami.workspace.json"), `{"name":"go-workspace","includes":["app"]}`)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "app", "putnami.json"), `{"name":"app"}`)
	mustWriteToolchainTestFile(t, filepath.Join(ws, "pyproject.toml"), "[tool.uv]\nrequired-version = \"==0.10.4\"\n")
	if err := lockfile.WriteLockFile(ws, lockfile.NewLockFile()); err != nil {
		t.Fatal(err)
	}

	for name, pin := range map[string]func(context.Context, string) (bool, error){
		"PinDeclaredToolchains":    PinDeclaredToolchains,
		"FillMissingToolchainPins": FillMissingToolchainPins,
		"RefreshLockMetadataResult": func(ctx context.Context, ws string) (bool, error) {
			return RefreshLockMetadataWithResult(ctx, ws, "dev")
		},
	} {
		if _, err := pin(context.Background(), ws); err != nil {
			t.Fatalf("%s in a workspace that runs no uv: %v", name, err)
		}
		got, err := lockfile.ReadLockFile(ws)
		if err != nil {
			t.Fatal(err)
		}
		if entry, ok := got.GetToolchain("uv"); ok {
			t.Fatalf("%s pinned uv %+v in a workspace that runs no uv", name, entry)
		}
	}

	for _, locks := range [][]string{nil, {"go", "bun"}} {
		declared, err := readToolchainDeclarations(ws, func() ([]string, error) { return locks, nil })
		if err != nil {
			t.Fatal(err)
		}
		if version, ok := declared["uv"]; ok {
			t.Fatalf("extensions resolving %v: declared uv %q, want none", locks, version)
		}
	}
	mustWriteToolchainTestFile(t, filepath.Join(ws, "pyproject.toml"), "[tool.uv]\nrequired-version = \">=0.10\"\n")
	if _, err := readToolchainDeclarations(ws, func() ([]string, error) {
		t.Fatal("the extensions were discovered for a declaration that pins nothing")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// MissingPinCause explains a missing uv pin only when the root pyproject.toml
// declares a uv that `putnami install` cannot pin: a range, or a release older
// than uvtoolchain.MinimumVersion. An exact supported release, no declaration
// and every other lock identity explain nothing, and an unreadable
// pyproject.toml is reported.
func TestMissingPinCauseNamesOnlyAUvDeclarationInstallCannotPin(t *testing.T) {
	for _, tc := range []struct {
		name, lock, pyproject, want string
	}{
		{"range", "uv", "[tool.uv]\nrequired-version = \">=0.5\"\n",
			`pyproject.toml [tool.uv] required-version is ">=0.5", which names no single uv release: declare one exact release, such as required-version = "==` + uvtoolchain.DefaultVersion + `"`},
		{"older than the minimum", "uv", "[tool.uv]\nrequired-version = \"==0.10.4\"\n",
			`pyproject.toml [tool.uv] required-version declares a release Putnami cannot run: `},
		{"exact supported release", "uv", uvTestPyproject, ""},
		{"no declaration", "uv", "[project]\nname = \"workspace\"\n", ""},
		{"no pyproject.toml", "uv", "", ""},
		{"another lock identity", "go", "[tool.uv]\nrequired-version = \">=0.5\"\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			if tc.pyproject != "" {
				mustWriteToolchainTestFile(t, filepath.Join(ws, "pyproject.toml"), tc.pyproject)
			}
			err := MissingPinCause(ws, tc.lock)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("MissingPinCause(%q) = %v, want nil", tc.lock, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("MissingPinCause(%q) = %v, want it to name %q", tc.lock, err, tc.want)
			}
		})
	}

	unreadable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unreadable, "pyproject.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := MissingPinCause(unreadable, "uv"); err == nil || !strings.Contains(err.Error(), "read pyproject.toml") {
		t.Fatalf("MissingPinCause with an unreadable pyproject.toml = %v, want it reported", err)
	}
}
