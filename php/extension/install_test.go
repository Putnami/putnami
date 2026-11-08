package php

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

const pharBody = "<?php // the stub Composer\n"

// composerServer serves pharBody as the pinned composer.phar and counts the
// downloads.
func composerServer(t *testing.T) *int {
	t.Helper()
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		_, _ = w.Write([]byte(pharBody))
	}))
	t.Cleanup(server.Close)
	url := composerURL
	composerURL = server.URL
	t.Cleanup(func() { composerURL = url })
	return &downloads
}

// composerDigest is the digest pharBody needs to be the pinned Composer.
func composerDigest() string {
	sum := sha256.Sum256([]byte(pharBody))
	return hex.EncodeToString(sum[:])
}

// setComposerDigest pins pharBody for the test; the returned function restores
// the pin.
func setComposerDigest(t *testing.T) func() {
	t.Helper()
	pinned := composerSHA256
	composerSHA256 = composerDigest()
	return func() { composerSHA256 = pinned }
}

func runInstall(t *testing.T, root, rel string) (string, error) {
	t.Helper()
	var meta json.RawMessage
	for _, p := range probe(t, root).Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	status, _, runErr := InstallJob(stack(t))(ctx, jsonl.New(), nil)
	return status, runErr
}

// composerStub replaces the machine php so that running composer.phar records
// its directory and writes vendor/composer/installed.json.
func composerStub(t *testing.T, version string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "composer.log")
	writeFile(t, bin, "php", `#!/bin/sh
if [ "$1" = "--version" ]; then echo "PHP `+version+` (cli)"; exit 0; fi
echo "$PWD: $*" >> "`+log+`"
mkdir -p vendor/composer && echo '{}' > vendor/composer/installed.json
if [ -n "$STUB_COMPOSER_FAILS" ]; then exit 3; fi
`)
	if err := os.Chmod(filepath.Join(bin, "php"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestTheInstallStepInstallsEachStackRootWithThePinnedComposer(t *testing.T) {
	spectest.Proves(t, feature, "installs-its-dependencies", "the-install-step-installs-each-stack-root-with-the-pinned-composer")
	restore := setComposerDigest(t)
	defer restore()
	root := fixture(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	downloads := composerServer(t)
	log := composerStub(t, "8.4.10")

	// packages/pricing has the root for stack root.
	for _, rel := range []string{"packages/pricing", "."} {
		if status, err := runInstall(t, root, rel); status != "OK" || err != nil {
			t.Fatalf("install %s: %s %v", rel, status, err)
		}
	}
	ran, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(ran)), "\n")
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], real+": ") || !strings.HasSuffix(lines[0], "composer.phar install --no-interaction --no-progress") {
		t.Fatalf("one install at the stack root, got %q", ran)
	}
	if *downloads != 1 {
		t.Errorf("downloads = %d", *downloads)
	}

	// A changed manifest installs again, with the phar it holds.
	writeFile(t, root, "composer.lock", `{"packages": [{"name": "demo/new"}]}`)
	if status, err := runInstall(t, root, "."); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	ran, _ = os.ReadFile(log)
	if lines := strings.Split(strings.TrimSpace(string(ran)), "\n"); len(lines) != 2 || *downloads != 1 {
		t.Errorf("runs %q, downloads %d", ran, *downloads)
	}

	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if err := os.RemoveAll(filepath.Join(root, "vendor")); err != nil {
		t.Fatal(err)
	}
	if status, err := runInstall(t, root, "."); status != "OK" || err != nil {
		t.Fatalf("offline install: %s %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(root, "vendor")); !os.IsNotExist(err) {
		t.Errorf("an offline run installs nothing: %v", err)
	}
}

func TestTheInstallStepReportsAFailedComposer(t *testing.T) {
	restore := setComposerDigest(t)
	defer restore()
	root := fixture(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	composerServer(t)
	composerStub(t, "8.4.10")
	t.Setenv("STUB_COMPOSER_FAILS", "1")
	if status, err := runInstall(t, root, "."); status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeComposerInstallFailed) {
		t.Errorf("a failed composer install fails the step: %s %v", status, err)
	}
	if status, err := runInstall(t, root, "."); status != "FAILED" || err == nil {
		t.Errorf("a failed install is not taken for a finished one: %s %v", status, err)
	}
}

func TestAFailedInstallLeavesNoRecordOfTheLastOne(t *testing.T) {
	restore := setComposerDigest(t)
	defer restore()
	root := fixture(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	composerServer(t)
	log := composerStub(t, "8.4.10")
	if status, err := runInstall(t, root, "."); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	lock := filepath.Join(root, "composer.lock")
	finished, err := os.ReadFile(lock)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "composer.lock", `{"packages": [{"name": "demo/branch"}]}`)
	t.Setenv("STUB_COMPOSER_FAILS", "1")
	if status, _ := runInstall(t, root, "."); status != "FAILED" {
		t.Fatalf("install on the changed lock: %s", status)
	}
	t.Setenv("STUB_COMPOSER_FAILS", "")
	if err := os.WriteFile(lock, finished, 0o644); err != nil {
		t.Fatal(err)
	}
	if status, err := runInstall(t, root, "."); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	ran, _ := os.ReadFile(log)
	if lines := strings.Split(strings.TrimSpace(string(ran)), "\n"); len(lines) != 3 {
		t.Errorf("the return to the old lock installs again: %q", ran)
	}
}

func TestASuccessfulInstallWithAnotherVendorDirectoryStaysGreen(t *testing.T) {
	restore := setComposerDigest(t)
	defer restore()
	root := fixture(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	composerServer(t)
	bin := t.TempDir()
	writeFile(t, bin, "php", `#!/bin/sh
if [ "$1" = "--version" ]; then echo "PHP 8.4.10 (cli)"; exit 0; fi
mkdir -p lib
`)
	if err := os.Chmod(filepath.Join(bin, "php"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if status, err := runInstall(t, root, "."); status != "OK" || err != nil {
		t.Errorf("an unrecorded install still succeeds: %s %v", status, err)
	}
}

func TestAPinnedWindowsPHPIsPreparedEvenWhenNothingInstalls(t *testing.T) {
	restore := setComposerDigest(t)
	defer restore()
	root := fixture(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	composerServer(t)
	composerStub(t, "8.3.6")
	if status, err := runInstall(t, root, "."); status != "FAILED" || err == nil {
		t.Fatalf("a machine PHP outside the range: %s %v", status, err)
	}

	pinnedDir := t.TempDir()
	writeFile(t, pinnedDir, "php", "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"PHP 8.4.23 (cli)\"; exit 0; fi\nexit 9\n")
	pinned := filepath.Join(pinnedDir, "php")
	if err := os.Chmod(pinned, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_PHP_EXECUTABLE", pinned)
	hostOS = "windows"
	t.Cleanup(func() { hostOS = runtime.GOOS })
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if status, err := runInstall(t, root, "."); status != "OK" || err != nil {
		t.Fatalf("offline: %s %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(pinnedDir, "php.ini")); err != nil {
		t.Errorf("an offline run still prepares the pinned PHP: %v", err)
	}
}

func TestAPinnedWindowsPHPLoadsTheExtensionsComposerNeeds(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "php.exe")
	if err := enableWindowsExtensions(program); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "php.ini")); !os.IsNotExist(err) {
		t.Fatalf("another host writes no php.ini: %v", err)
	}
	hostOS = "windows"
	t.Cleanup(func() { hostOS = runtime.GOOS })
	if err := enableWindowsExtensions("php"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "php.ini")); !os.IsNotExist(err) {
		t.Fatalf("a machine PHP keeps its own php.ini: %v", err)
	}
	if err := enableWindowsExtensions(program); err != nil {
		t.Fatal(err)
	}
	ini, err := os.ReadFile(filepath.Join(dir, "php.ini"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(dir, "ext"), "extension=openssl", "extension=mbstring"} {
		if !strings.Contains(string(ini), want) {
			t.Errorf("php.ini lacks %q:\n%s", want, ini)
		}
	}
}

func TestComposerIsRefusedWhenItsDigestDiffers(t *testing.T) {
	root := fixture(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	composerServer(t)
	composerStub(t, "8.4.10")
	if status, err := runInstall(t, root, "."); status != "FAILED" || err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("a Composer with another digest is refused: %s %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(root, "vendor")); !os.IsNotExist(err) {
		t.Errorf("nothing ran: %v", err)
	}
}
