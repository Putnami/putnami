//go:build unix

package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// prepareManifest is the manifest of the fixture extension name, whose runtime
// member is runtimeBlock, or which has none when runtimeBlock is empty.
func prepareManifest(name, runtimeBlock string) string {
	return fmt.Sprintf(`{
	"name": %q,
	"version": "1.0.0",
	"cliContract": 4,
	%s
	"commands": {
		"build": {"run": [{"id": "b", "task": "t"}]},
		"cache-provider": {"visibility": "internal", "run": [{"id": "p", "task": "t"}]}
	},
	"tasks": {"t": {"kind": "command", "command": "echo"}}
}`, name, runtimeBlock)
}

// acmeRuntime is the runtime member of /tools/acme: its runtime.prepare copies
// bin/runtime-template into the runtime output.
const acmeRuntime = `"runtime": {"executable": "compiled/acme", "prepare": {"command": "{extensionRoot}/bin/prepare", "args": ["--output", "{runtimeOutput}"], "inputs": ["bin/**"]}},`

// acmeRuntimeWithToolchain is acmeRuntime whose runtime.prepare also requires
// the toolchain alias "compiler", locked as "go".
const acmeRuntimeWithToolchain = `"runtime": {
		"executable": "compiled/acme",
		"toolchains": {"compiler": {
			"lock": "go",
			"candidates": [{"from": "environment", "environment": "ACME_PREPARE_GOROOT", "path": "bin/go"}],
			"probe": {"args": ["env", "GOVERSION"], "expect": "go{version}"}
		}},
		"prepare": {"command": "{extensionRoot}/bin/prepare", "args": ["--output", "{runtimeOutput}"], "inputs": ["bin/**"], "toolchains": ["compiler"]}
	},`

// writePrepareFixture writes a workspace that declares the path extension
// /tools/acme, which serves the cache provider from a runtime its
// runtime.prepare builds, and the path extension /tools/plain, which declares
// no runtime.prepare. Its lock records cli. It returns the workspace config.
func writePrepareFixture(t *testing.T, ws string, cli lockfile.LockEntry) *wsproto.Config {
	t.Helper()
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	acme := filepath.Join(ws, "tools", "acme")
	write(filepath.Join(acme, "putnami.extension.json"), prepareManifest("@acme/cloud", acmeRuntime), 0o644)
	info := fmt.Sprintf(`{"extension":"@acme/cloud","version":"1.0.0","platform":%q,"cliContract":4,"runtimeProtocol":%d,"runtimeABI":%d}`,
		runtime.GOOS+"/"+runtime.GOARCH, runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
	write(filepath.Join(acme, "bin", "runtime-template"), "#!/bin/sh\nif [ \"$1\" = \"__putnami\" ] && [ \"$2\" = \"runtime-info\" ]; then\n  printf '%s\\n' '"+info+"'\nfi\nexit 0\n", 0o755)
	write(filepath.Join(acme, "bin", "prepare"), "#!/bin/sh\nset -eu\nmkdir -p \"$2/compiled\"\ncp bin/runtime-template \"$2/compiled/acme\"\nchmod +x \"$2/compiled/acme\"\n", 0o755)
	write(filepath.Join(ws, "tools", "plain", "putnami.extension.json"), prepareManifest("@acme/plain", ""), 0o644)
	write(filepath.Join(ws, wsproto.WorkspaceConfigFilename), `{"extensions": ["/tools/acme", "/tools/plain"]}`, 0o644)
	locked := lockfile.NewLockFile()
	locked.SetCLI(cli)
	if err := lockfile.WriteLockFile(ws, locked); err != nil {
		t.Fatal(err)
	}
	return &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"/tools/acme": "", "/tools/plain": ""}}}
}

// `putnami extensions prepare` prepares into the artifact store the runtime
// of each path extension of a source workspace that serves a provider from
// its runtime.prepare, and prints one line for each. A path extension without
// runtime.prepare has no runtime to prepare, and a workspace whose lock pins a
// published CLI prepares nothing.
func TestExtensionsPrepare_PreparesTheSourceBuiltRuntimes(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "source-built-providers", "extensions-prepare-prepares-the-source-built-runtimes")
	store := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", store)

	ws := t.TempDir()
	cfg := writePrepareFixture(t, ws, lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	var out bytes.Buffer
	if err := extensionsPrepare(context.Background(), ws, cfg, "jsonl", &out); err != nil {
		t.Fatalf("extensions prepare: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("extensions prepare printed %q, want one runtime", out.String())
	}
	var entry preparedRuntimeEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("decode %q: %v", lines[0], err)
	}
	if entry.Kind != "extension-runtime" || entry.Name != "@acme/cloud" || entry.Digest == "" {
		t.Fatalf("extensions prepare printed %+v, want the runtime of @acme/cloud", entry)
	}
	artifacts := artifactstore.New(store)
	if !artifacts.Has(entry.Digest) {
		t.Errorf("the artifact store has no entry %s", entry.Digest)
	}
	if want := filepath.Join(artifacts.Path(entry.Digest), "runtime", "compiled", "acme"); entry.Executable != want {
		t.Errorf("the runtime is %s, want %s", entry.Executable, want)
	}
	if _, err := os.Stat(entry.Executable); err != nil {
		t.Errorf("the prepared runtime is missing: %v", err)
	}

	consumer := t.TempDir()
	cfg = writePrepareFixture(t, consumer, lockfile.LockEntry{Version: "1.0.0"})
	out.Reset()
	if err := extensionsPrepare(context.Background(), consumer, cfg, "", &out); err != nil {
		t.Fatalf("extensions prepare in a workspace that pins a published CLI: %v", err)
	}
	if !strings.Contains(out.String(), "No extension runtime to prepare") {
		t.Errorf("extensions prepare in a workspace that pins a published CLI printed %q, want nothing prepared", out.String())
	}
}

// declarePrepareToolchain makes the runtime.prepare of /tools/acme in ws
// require the toolchain alias "compiler", locked as "go".
func declarePrepareToolchain(t *testing.T, ws string) {
	t.Helper()
	path := filepath.Join(ws, "tools", "acme", "putnami.extension.json")
	if err := os.WriteFile(path, []byte(prepareManifest("@acme/cloud", acmeRuntimeWithToolchain)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// storeFiles counts the regular files under the artifact store root.
func storeFiles(t *testing.T, root string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// `putnami extensions prepare` resolves the prepare toolchains strictly: a
// toolchain the workspace lock does not pin fails the command, and nothing
// reaches the artifact store.
func TestExtensionsPrepare_FailsWithoutTheLockedToolchain(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "source-built-providers", "extensions-prepare-prepares-the-source-built-runtimes")
	store := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", store)
	t.Setenv("ACME_PREPARE_GOROOT", t.TempDir())

	ws := t.TempDir()
	cfg := writePrepareFixture(t, ws, lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	declarePrepareToolchain(t, ws)
	var out bytes.Buffer
	err := extensionsPrepare(context.Background(), ws, cfg, "", &out)
	if err == nil || !strings.Contains(err.Error(), `"compiler"`) {
		t.Fatalf("extensions prepare without a go pin returned %v, want a failure naming the toolchain", err)
	}
	if got := storeFiles(t, store); got != 0 {
		t.Errorf("a failed preparation left %d files in the artifact store", got)
	}
}

// `putnami extensions prepare` refuses a hosted run before it reads the
// workspace: a hosted run prepares no runtime.
func TestExtensionsPrepare_RefusesAHostedRun(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "source-built-providers", "extensions-prepare-prepares-the-source-built-runtimes")
	store := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", store)
	ws := t.TempDir()
	cfg := writePrepareFixture(t, ws, lockfile.LockEntry{Source: lockfile.SourceWorkspace})

	restore := runcredential.SetForTest("run-bearer")
	defer restore()
	var out bytes.Buffer
	err := extensionsPrepare(context.Background(), ws, cfg, "", &out)
	if !errors.Is(err, ErrHostedPrepare) || !errors.Is(err, cmderr.ErrUsage) {
		t.Fatalf("a hosted extensions prepare returned %v, want the usage error ErrHostedPrepare", err)
	}
	if got := storeFiles(t, store); got != 0 {
		t.Errorf("a refused preparation left %d files in the artifact store", got)
	}
}
