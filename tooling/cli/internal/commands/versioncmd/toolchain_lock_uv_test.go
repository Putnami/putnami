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

// Only an exact required-version in the [tool.uv] table declares a uv release.
// A range, a wildcard, a key of another table and a malformed value declare
// none, so they pin nothing and make no request.
func TestUvRequiredVersionReadsOnlyAnExactToolUvDeclaration(t *testing.T) {
	cases := []struct {
		name      string
		pyproject string
		want      string
	}{
		{"exact", "[tool.uv]\nrequired-version = \"==0.11.32\"\n", "0.11.32"},
		{"bare release", "[tool.uv]\nrequired-version = '0.11.32'\n", "0.11.32"},
		{"spaces and comment", "[ tool.uv ]  # uv settings\r\n  required-version   =   \"== 0.11.32\"   # pinned\r\n", "0.11.32"},
		{"major minor", "[tool.uv]\nrequired-version = \"==0.12\"\n", "0.12.0"},
		{"after other keys", "[tool.uv]\nmanaged = true\nrequired-version = \"==0.11.32\"\n[tool.uv.sources]\n", "0.11.32"},
		{"range", "[tool.uv]\nrequired-version = \">=0.11.0\"\n", ""},
		{"wildcard", "[tool.uv]\nrequired-version = \"==0.11.*\"\n", ""},
		{"arbitrary equality", "[tool.uv]\nrequired-version = \"===0.11.32\"\n", ""},
		{"compound", "[tool.uv]\nrequired-version = \"==0.11.32,<1\"\n", ""},
		{"prerelease", "[tool.uv]\nrequired-version = \"==0.11.32rc1\"\n", ""},
		{"another table", "[tool.uv.workspace]\nrequired-version = \"==0.11.32\"\n", ""},
		{"top level", "required-version = \"==0.11.32\"\n[tool.uv]\n", ""},
		{"array of tables", "[[tool.uv]]\nrequired-version = \"==0.11.32\"\n", ""},
		{"not a string", "[tool.uv]\nrequired-version = 0.11\n", ""},
		{"unterminated", "[tool.uv]\nrequired-version = \"==0.11.32\n", ""},
		{"no declaration", "[project]\nname = \"x\"\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uvRequiredVersion(tc.pyproject); got != tc.want {
				t.Errorf("uvRequiredVersion(%q) = %q, want %q", tc.pyproject, got, tc.want)
			}
		})
	}

	withToolchainTestClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("a uv range must not read release metadata")
	})})
	ws := t.TempDir()
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
	if _, err := readToolchainDeclarations(unreadable); err == nil || !strings.Contains(err.Error(), "read pyproject.toml") {
		t.Fatalf("error = %v, want the unreadable pyproject.toml reported", err)
	}
	spectest.Proves(t, "cli/toolchain-lock", "supported-runtimes-only", "only-an-exact-tool-uv-required-version-is-locked")
}
