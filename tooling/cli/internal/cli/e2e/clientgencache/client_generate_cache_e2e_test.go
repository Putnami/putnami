// Package clientgencache drives the generation of a TypeScript client through
// the real CLI: @putnami/clientgen, loaded from this tree, runs the
// @putnami/client emitter of this tree on a provider's committed contract, and
// a later run that hits the cache restores the src tree the execution wrote,
// file for file, whether src was deleted or left in place. These tests swap
// the process streams and the working directory, so they run in their own
// test binary.
package clientgencache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	abs, err := filepath.Abs("../../../../../..")
	if err != nil {
		panic(err)
	}
	repoRoot = abs
	os.Exit(clitest.Main(m))
}

// repoRoot is the root of this repository, absolute, since the CLI runs
// change the working directory.
var repoRoot string

// providerContract is the committed contract of a TypeScript provider of this
// tree whose first-party operations span JSON, binary and streaming bodies.
const providerContract = "typescript/samples/10-service-to-service/schema/openapi.json"

// files is a workspace of a provider, which commits only its contract, and
// client, a TypeScript client project generated from it.
var files = map[string]string{
	".gitignore": "node_modules\n.putnami\n.gen\nclients/ts/src\n",
	// The emitter formats the client it writes with the workspace's Biome
	// configuration.
	"biome.json":             `{"root": true, "formatter": {"indentStyle": "space", "indentWidth": 2, "lineWidth": 120}}` + "\n",
	"package.json":           `{"name": "ws", "private": true, "packageManager": "bun@1.4.0", "workspaces": ["clients/ts"]}` + "\n",
	"putnami.workspace.json": `{"name": "ws", "extensions": ["/.ext/typescript", "/.ext/dev", "/.ext/clientgen"], "includes": ["provider", "clients/ts"]}` + "\n",
	"provider/putnami.json":  `{"name": "provider", "extensions": ["@putnami/typescript", "@putnami/clientgen"]}` + "\n",
	"provider/package.json":  `{"name": "provider", "private": true}` + "\n",
	"clients/ts/putnami.json": `{"name": "client", "type": "library", "extensions": ["@putnami/typescript", "@putnami/clientgen"],` +
		` "generatedFrom": {"service": "catalog.items", "language": "ts"}}` + "\n",
	"clients/ts/package.json": `{"name": "@example/items-client", "version": "0.0.0", "main": "src/index.ts",` +
		` "exports": {".": "./src/index.ts"}, "dependencies": {"@putnami/client": "workspace:*"}}` + "\n",
}

// writeWorkspace writes files and the provider's contract under a new Git
// work tree, links @putnami/typescript, @putnami/dev and @putnami/clientgen
// from this tree under .ext, which the project scan never enters, and
// installs this tree's @putnami/client in the client project the way a
// package manager would, with its emitter shim under node_modules/.bin.
func writeWorkspace(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		clitest.WriteFile(t, filepath.Join(root, rel), content)
	}
	contract, err := os.ReadFile(filepath.Join(repoRoot, providerContract))
	if err != nil {
		t.Fatalf("read the provider's contract: %v", err)
	}
	clitest.WriteFile(t, filepath.Join(root, "provider/schema/openapi.json"), string(contract))
	links := map[string]string{
		".ext/typescript": filepath.Join(repoRoot, "typescript/extension"),
		".ext/dev":        filepath.Join(repoRoot, "tooling/dev"),
		".ext/clientgen":  filepath.Join(repoRoot, "tooling/clientgen-extension"),
		"clients/ts/node_modules/@putnami/client": filepath.Join(repoRoot, "typescript/framework/client"),
	}
	for rel, target := range links {
		real, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Skipf("%s is not installed: %v", target, err)
		}
		link(t, real, filepath.Join(root, rel))
	}
	// The emitter imports its own dependencies from this tree's install.
	if _, err := os.Stat(filepath.Join(repoRoot, "typescript/framework/client/node_modules")); err != nil {
		t.Skipf("@putnami/client is not installed in this tree: %v", err)
	}
	link(t, "../@putnami/client/bin/generate.ts", filepath.Join(root, "clients/ts/node_modules/.bin/putnami-client-generate"))
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	pinToolchains(t, root, "bun", "go")
	return root
}

func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// pinToolchains copies this repository's pins of the toolchains into the
// workspace lock: the extensions loaded from this tree compile their runtimes
// with the Go release it pins, and the emitter runs under the Bun it pins.
func pinToolchains(t *testing.T, root string, names ...string) {
	t.Helper()
	var source map[string]json.RawMessage
	data, err := os.ReadFile(filepath.Join(repoRoot, "putnami.lock.json"))
	if err != nil || json.Unmarshal(data, &source) != nil {
		t.Fatalf("read this repository's lock: %v", err)
	}
	var from map[string]json.RawMessage
	if err := json.Unmarshal(source["toolchains"], &from); err != nil {
		t.Fatal(err)
	}
	into := map[string]json.RawMessage{}
	for _, name := range names {
		into[name] = from[name]
	}
	lock := struct {
		Version    json.RawMessage            `json:"version"`
		Toolchains map[string]json.RawMessage `json:"toolchains"`
	}{source["version"], into}
	if data, err = json.MarshalIndent(lock, "", "  "); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "putnami.lock.json"), string(data)+"\n")
}

// pinnedRuntimeMissing is what @putnami/clientgen answers when no Bun of the
// version the workspace lock pins is on PATH or provisioned under the Putnami
// home: a host the test cannot run on, not a failure of the restore.
const pinnedRuntimeMissing = "requires the exact runtime pinned by the workspace lock"

// cacheSummary returns the local hits and misses the run's cache summary
// line reports, such as "0 local hit · 1 miss", or "" when it printed none.
func cacheSummary(out string) string {
	m := regexp.MustCompile(`(?m)^\s*Cache: (\d+ local hit · \d+ miss)`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

// tree returns the digest of every file under dir, by its slash path relative
// to dir. A missing dir is an empty tree.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == dir && os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: a file of the test's own workspace
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestACachedTypeScriptClientGenerationRestoresTheTreeItsExecutionWrote is the
// acceptance of a cached client-generate~ts: the first generation runs the
// emitter, and each later one hits the cache and leaves src exactly as the
// execution wrote it, from a deleted src and over the restored one.
func TestACachedTypeScriptClientGenerationRestoresTheTreeItsExecutionWrote(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "executed-directory-output-starts-empty",
		"a-hit-of-a-generated-typescript-client-restores-what-its-execution-wrote")
	clitest.RequireShell(t)
	for _, tool := range []string{"bun", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the generation runs %s, which is not on PATH: %v", tool, err)
		}
	}
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	// A store of its own: the first generation executes, whatever another run
	// of this test, such as the race run, stored before.
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	root := writeWorkspace(t)
	src := filepath.Join(root, "clients/ts/src")

	code, out := clitest.RunGateArgs(t, root, "client-generate", "--projects", "client")
	if code != 0 && strings.Contains(out, pinnedRuntimeMissing) {
		t.Skipf("the emitter runs under the Bun this repository pins, which this host neither runs nor provisioned:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("cold generation exit %d:\n%s", code, out)
	}
	if summary := cacheSummary(out); summary != "0 local hit · 1 miss" {
		t.Fatalf("cold generation cache = %q, want the one task executed:\n%s", summary, out)
	}
	cold := tree(t, src)
	if _, ok := cold["index.ts"]; !ok || len(cold) < 2 {
		t.Fatalf("the cold generation wrote %v, want the client's index.ts and its modules", slices.Sorted(maps.Keys(cold)))
	}

	for _, step := range []struct {
		name   string
		before func() error
	}{
		{"hit over a deleted src", func() error { return os.RemoveAll(src) }},
		{"hit over the restored src", func() error { return nil }},
	} {
		if err := step.before(); err != nil {
			t.Fatal(err)
		}
		code, out := clitest.RunGateArgs(t, root, "client-generate", "--projects", "client")
		if code != 0 {
			t.Fatalf("%s: exit %d:\n%s", step.name, code, out)
		}
		if summary := cacheSummary(out); summary != "1 local hit · 0 miss" {
			t.Errorf("%s: cache = %q, want the one task restored:\n%s", step.name, summary, out)
		}
		if got := tree(t, src); !maps.Equal(got, cold) {
			t.Errorf("%s: src holds %v, want the cold generation's %v",
				step.name, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(cold)))
		}
	}
}
