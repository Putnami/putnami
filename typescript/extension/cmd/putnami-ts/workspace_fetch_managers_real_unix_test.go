//go:build unix

package main

import (
	"crypto/sha256"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// requireManager returns the program on PATH, and skips the test without it.
func requireManager(t *testing.T, program string) string {
	t.Helper()
	bin, err := stdexec.LookPath(program)
	if err != nil {
		t.Skipf("%s is not installed", program)
	}
	return bin
}

// managerLocationVariables move the caches and the user configuration of npm,
// pnpm and Yarn 1, or carry a registry credential.
var managerLocationVariables = []string{
	"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "PNPM_HOME", "npm_config_cache", "NPM_CONFIG_CACHE",
	"YARN_CACHE_FOLDER", "NPM_TOKEN", "NPM_CONFIG_USERCONFIG", "npm_config_userconfig",
	extensionproto.OfflineDependenciesEnv, extensionproto.JobCredentialFDEnv,
}

// For npm, pnpm and Yarn 1, a hosted run fetches what the committed lock names
// with the handed bearer and no lifecycle script, from a scratch copy, into
// the cache the hosted install reads; that install then runs with the
// registry gone, and neither job rewrites the lock or leaves the bearer in a
// file.
func TestEachRealManagerFetchesThenInstallsOffline(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first", "each-manager-fetches-its-committed-lockfile-into-its-cache")
	for _, tc := range []struct {
		program string
		lock    string
		author  func(cache string) []string
	}{
		{"npm", "package-lock.json", func(cache string) []string {
			return []string{"install", "--ignore-scripts", "--no-audit", "--no-fund", "--cache", cache}
		}},
		{"pnpm", "pnpm-lock.yaml", func(cache string) []string {
			return []string{"install", "--ignore-scripts", "--store-dir", cache}
		}},
		{"yarn", "yarn.lock", func(cache string) []string {
			return []string{"install", "--ignore-scripts", "--non-interactive", "--cache-folder", cache}
		}},
	} {
		t.Run(tc.program, func(t *testing.T) {
			bin := requireManager(t, tc.program)
			if tc.program == "yarn" {
				if out, err := stdexec.Command(bin, "--version").Output(); err != nil || !strings.HasPrefix(string(out), "1.") {
					t.Skipf("yarn on PATH is not Yarn 1 (%s)", strings.TrimSpace(string(out)))
				}
			}
			fakeRegistry := startFakeNPMRegistry(t, fetchTestBearer,
				fakeNPMPackage{
					name:    "@fake/marker",
					version: "1.0.0",
					files:   map[string]string{"postinstall.sh": postinstallScript},
					scripts: map[string]string{"postinstall": "sh ./postinstall.sh"},
				},
				fakeNPMPackage{name: "@fake/plain", version: "2.0.0", files: map[string]string{"index.js": "module.exports = 2\n"}},
			)

			root := t.TempDir()
			manifest := `{"name":"fetch-fixture","private":true,"workspaces":["pkgs/*"],"dependencies":{"@fake/marker":"1.0.0"}}`
			if tc.program == "pnpm" {
				manifest = `{"name":"fetch-fixture","private":true,"dependencies":{"@fake/marker":"1.0.0"}}`
				writeProbeFile(t, filepath.Join(root, "pnpm-workspace.yaml"), "packages:\n  - pkgs/*\n")
			}
			writeProbeFile(t, filepath.Join(root, "package.json"), manifest)
			writeProbeFile(t, filepath.Join(root, "pkgs", "a", "package.json"), `{"name":"a","version":"0.0.0","dependencies":{"@fake/plain":"2.0.0"}}`)
			writeProbeFile(t, filepath.Join(root, ".npmrc"), "@fake:registry="+fakeRegistry.URL()+"\n")

			// The author writes the lock with a credential and a cache of
			// their own.
			authorHome := t.TempDir()
			writeProbeFile(t, filepath.Join(authorHome, ".npmrc"), "//"+fakeRegistry.Host()+"/:_authToken="+fetchTestBearer+"\n")
			author := stdexec.Command(bin, tc.author(filepath.Join(t.TempDir(), "cache"))...)
			author.Dir = root
			author.Env = append(scrubbedManagerEnv(), "HOME="+authorHome, "NPM_CONFIG_USERCONFIG="+filepath.Join(authorHome, ".npmrc"))
			if out, err := author.CombinedOutput(); err != nil {
				t.Fatalf("%s writes the lock: %v\n%s", tc.program, err, out)
			}
			for _, dir := range []string{"node_modules", "pkgs/a/node_modules"} {
				if err := os.RemoveAll(filepath.Join(root, dir)); err != nil {
					t.Fatal(err)
				}
			}
			lockSum := fileSum(t, filepath.Join(root, tc.lock))
			npmrcSum := fileSum(t, filepath.Join(root, ".npmrc"))

			// The jobs run with a home of their own, whose caches are empty,
			// and a user .npmrc that holds another token.
			home, tmp := t.TempDir(), t.TempDir()
			for _, name := range managerLocationVariables {
				t.Setenv(name, "")
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("TMPDIR", tmp)
			// Yarn 1 run as root caches under /usr/local/share whatever the
			// home; the job's environment names a folder of its own.
			t.Setenv("YARN_CACHE_FOLDER", filepath.Join(home, "yarn-cache"))
			t.Setenv("MARKER_WORKSPACE", root)
			userConfig := filepath.Join(t.TempDir(), ".npmrc")
			writeProbeFile(t, userConfig, "//"+fakeRegistry.Host()+"/:_authToken=not-the-handed-bearer\n")
			t.Setenv("NPM_CONFIG_USERCONFIG", userConfig)
			ctx := &pctx.Context{WorkspaceRoot: root, Extension: pctx.Extension{Name: "@putnami/typescript", Root: t.TempDir()}, Params: pctx.Params{}}
			withRegistries(t, ctx, `{"npm":{"scopes":{"@fake":"`+fakeRegistry.URL()+`"}}}`)

			var events []string
			handCredential(t, &registry.Credential{Bearer: fetchTestBearer, ExpiresAt: "2099-01-01T00:00:00Z", Hosts: []string{fakeRegistry.Host()}}, &events)
			status, _, err := runWorkspaceFetch(ctx, jsonl.New(), nil)
			if err != nil || status != "OK" {
				t.Fatalf("runWorkspaceFetch = %q, %v; want OK", status, err)
			}
			if served, refused := fakeRegistry.served.Load(), fakeRegistry.unauthorized.Load(); served == 0 || refused != 0 {
				t.Fatalf("registry served %d and refused %d requests; want the handed bearer", served, refused)
			}
			for _, rel := range []string{"node_modules", "pkgs/a/node_modules", "postinstall.marker"} {
				if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
					t.Errorf("workspace-fetch left %s in the workspace (%v)", rel, err)
				}
			}
			if left := fetchLeftovers(t, tmp); len(left) != 0 {
				t.Errorf("workspace-fetch left %v", left)
			}

			fakeRegistry.Close()
			t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
			status, _, err = runWorkspaceInstall(ctx, jsonl.New(), nil)
			if err != nil || status != "OK" {
				t.Fatalf("the hosted install = %q, %v; want OK from the fetched cache", status, err)
			}
			if !isRegularFile(filepath.Join(root, "node_modules", "@fake", "plain", "index.js")) &&
				!isRegularFile(filepath.Join(root, "pkgs", "a", "node_modules", "@fake", "plain", "index.js")) {
				t.Fatal("the hosted install did not install @fake/plain")
			}
			if fileSum(t, filepath.Join(root, tc.lock)) != lockSum {
				t.Errorf("a job rewrote %s", tc.lock)
			}
			if fileSum(t, filepath.Join(root, ".npmrc")) != npmrcSum {
				t.Error("a job rewrote .npmrc")
			}
			for _, name := range []string{"bun.lock", "bun.lockb"} {
				if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Errorf("a job wrote %s", name)
				}
			}
			assertNoFileHolds(t, fetchTestBearer, root, tmp, home)
		})
	}
}

// scrubbedManagerEnv is this process's environment without the variables that
// move a manager's cache or configuration.
func scrubbedManagerEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		skip := name == "HOME"
		for _, moved := range managerLocationVariables {
			skip = skip || name == moved
		}
		if !skip {
			env = append(env, entry)
		}
	}
	return env
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}
