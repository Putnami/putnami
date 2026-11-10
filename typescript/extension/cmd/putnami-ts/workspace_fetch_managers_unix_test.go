//go:build unix

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// managerFetchWorkspace is a workspace on one manager, with one member, a
// scope registry and a workspace .npmrc and .yarnrc.yml that carry repository
// credential lines.
func managerFetchWorkspace(t *testing.T, packageManagerField string, files map[string]string) *pctx.Context {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	writeProbeFile(t, filepath.Join(dir, "package.json"),
		`{"name":"ws","private":true,"packageManager":"`+packageManagerField+`","workspaces":["pkgs/*"],"dependencies":{"@fake/marker":"1.0.0","local":"link:./vendor/local"}}`)
	writeProbeFile(t, filepath.Join(dir, "pkgs", "a", "package.json"), `{"name":"a"}`)
	writeProbeFile(t, filepath.Join(dir, "vendor", "local", "package.json"), `{"name":"local"}`)
	writeProbeFile(t, filepath.Join(dir, ".npmrc"), "@fake:registry=https://npm.fake.test/\n//npm.fake.test/:_authToken=${NPM_TOKEN}\n")
	for name, content := range files {
		writeProbeFile(t, filepath.Join(dir, name), content)
	}
	withRegistries(t, ctx, `{"npm":{"scopes":{"@fake":"https://npm.fake.test/"}}}`)
	return ctx
}

// managerFetchCall is what the mocked manager saw.
type managerFetchCall struct {
	events     []string
	bin        string
	installArg []string
	installEnv map[string]string
	installDir string
	scratch    map[string]string
	home       map[string]string
}

// runFetchWithMockedManager runs workspace-fetch with the program on PATH at
// bin, answering each probe from answers by its argv and recording the
// scratch install.
func runFetchWithMockedManager(t *testing.T, ctx *pctx.Context, program, bin string, credential *registry.Credential, answers map[string]string) (managerFetchCall, string, error) {
	t.Helper()
	tmp := privateTempDir(t)
	var call managerFetchCall
	handCredential(t, credential, &call.events)
	mockLookPath(t, map[string]string{program: bin})
	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		joined := strings.Join(args, " ")
		call.events = append(call.events, joined)
		if answer, ok := answers[joined]; ok {
			return &exec.Result{Success: true, Stdout: answer + "\n"}, nil
		}
		if len(args) == 0 || (args[0] != "install" && args[0] != "ci") {
			t.Errorf("%s %v is no probe this test answers", name, args)
			return &exec.Result{Success: false, ExitCode: 1}, nil
		}
		call.bin = name
		call.installArg = args
		// The options are opaque: a shell started with them reports the
		// directory and the environment the install would have had.
		probe, err := exec.Run("/bin/sh", []string{"-c", "pwd -P; env"}, opts...)
		if err != nil || !probe.Success {
			t.Fatalf("probe the install options: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(probe.Stdout), "\n")
		call.installDir = lines[0]
		call.installEnv = map[string]string{}
		for _, line := range lines[1:] {
			if name, value, ok := strings.Cut(line, "="); ok {
				call.installEnv[name] = value
			}
		}
		call.scratch = map[string]string{}
		call.home = map[string]string{}
		for _, entry := range tempEntries(t, tmp) {
			dir := filepath.Join(tmp, entry)
			target := call.scratch
			if strings.HasPrefix(entry, "putnami-ts-fetch-home-") {
				target = call.home
			}
			_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					rel, _ := filepath.Rel(dir, path)
					data, _ := os.ReadFile(path)
					target[filepath.ToSlash(rel)] = string(data)
				}
				return nil
			})
		}
		return &exec.Result{Success: true}, nil
	})
	status, _, err := runWorkspaceFetch(ctx, jsonl.New(), nil)
	if leftover := tempEntries(t, tmp); len(leftover) != 0 {
		t.Errorf("workspace-fetch left %v in the temporary directory", leftover)
	}
	return call, status, err
}

func TestEachManagerFetchesItsCommittedLockfileIntoItsCache(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first", "each-manager-fetches-its-committed-lockfile-into-its-cache")
	t.Setenv("COREPACK_HOME", "/users/ci/corepack")
	cache := "/users/ci/cache"
	homeNpmrc := "//npm.fake.test/:_authToken=" + unitFetchBearer + "\n//registry.fake.test:8443/:_authToken=" + unitFetchBearer + "\n"
	for _, tc := range []struct {
		name     string
		declared string
		files    map[string]string
		answers  map[string]string
		args     []string
		env      map[string]string
		scratch  []string
		home     map[string]string
	}{
		{
			name:     "pnpm",
			declared: "pnpm@10.26.1",
			files: map[string]string{
				"pnpm-lock.yaml":       "lockfileVersion: '9.0'\n\nimporters:\n\n  .:\n    dependencies: {}\n\n  deep/b:\n    dependencies: {}\n\npackages: {}\n",
				"pnpm-workspace.yaml":  "packages:\n  - pkgs/*\npatchedDependencies:\n  '@fake/marker@1.0.0': patches/marker.patch\n",
				"deep/b/package.json":  `{"name":"b"}`,
				"patches/marker.patch": "diff",
				".pnpmfile.cjs":        "module.exports = {}",
			},
			answers: map[string]string{"store path": cache},
			args:    []string{"install", "--frozen-lockfile", "--ignore-scripts", "--ignore-pnpmfile", "--store-dir", cache},
			env:     map[string]string{"COREPACK_HOME": "/users/ci/corepack"},
			scratch: []string{".npmrc", "deep/b/package.json", "package.json", "patches/marker.patch", "pkgs/a/package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "vendor/local/package.json"},
			home:    map[string]string{".npmrc": homeNpmrc},
		},
		{
			name:     "npm",
			declared: "npm@10.9.0",
			files: map[string]string{
				"package-lock.json":   `{"lockfileVersion":3,"packages":{"":{"name":"ws"},"pkgs/a":{"name":"a"},"deep/b":{"name":"b"},"node_modules/a":{"link":true},"node_modules/x/node_modules/y":{}}}`,
				"deep/b/package.json": `{"name":"b"}`,
			},
			answers: map[string]string{"config get cache": cache},
			args:    []string{"ci", "--ignore-scripts", "--no-audit", "--no-fund", "--cache", cache},
			env:     map[string]string{"COREPACK_HOME": "/users/ci/corepack"},
			scratch: []string{".npmrc", "deep/b/package.json", "package-lock.json", "package.json", "pkgs/a/package.json", "vendor/local/package.json"},
			home:    map[string]string{".npmrc": homeNpmrc},
		},
		{
			name:     "yarn 1",
			declared: "yarn@1.22.22",
			files:    map[string]string{"yarn.lock": "# yarn lockfile v1\n", ".yarnrc": "network-timeout 600000\n"},
			answers:  map[string]string{"cache dir": cache + "/v6"},
			args:     []string{"install", "--frozen-lockfile", "--ignore-scripts", "--non-interactive", "--cache-folder", cache},
			env:      map[string]string{"COREPACK_HOME": "/users/ci/corepack"},
			scratch:  []string{".npmrc", ".yarnrc", "package.json", "pkgs/a/package.json", "vendor/local/package.json", "yarn.lock"},
			home:     map[string]string{".npmrc": homeNpmrc},
		},
		{
			name:     "yarn 4",
			declared: "yarn@4.5.0",
			files: map[string]string{
				"yarn.lock":             "__metadata:\n  version: 8\n\n\"b@workspace:deep/b\":\n  version: 0.0.0-use.local\n  resolution: \"b@workspace:deep/b\"\n",
				"deep/b/package.json":   `{"name":"b"}`,
				".yarnrc.yml":           "nodeLinker: node-modules\nnpmScopes:\n  fake:\n    npmRegistryServer: https://npm.fake.test\n    npmAuthToken: ${NPM_TOKEN}\n",
				".yarn/patches/x.patch": "diff",
			},
			answers: map[string]string{
				"config get cacheFolder":       "/repo/.yarn/cache",
				"config get globalFolder":      "/users/ci/.yarn/berry",
				"config get enableGlobalCache": "true",
			},
			args: []string{"install", "--immutable", "--mode=skip-build"},
			env: map[string]string{
				"COREPACK_HOME":            "/users/ci/corepack",
				"YARN_ENABLE_SCRIPTS":      "0",
				"YARN_CACHE_FOLDER":        "/users/ci/.yarn/berry/cache",
				"YARN_GLOBAL_FOLDER":       "/users/ci/.yarn/berry",
				"YARN_ENABLE_GLOBAL_CACHE": "true",
			},
			scratch: []string{".npmrc", ".yarn/patches/x.patch", ".yarnrc.yml", "deep/b/package.json", "package.json", "pkgs/a/package.json", "vendor/local/package.json", "yarn.lock"},
			home: map[string]string{
				".npmrc":      homeNpmrc,
				".yarnrc.yml": "npmRegistries:\n  \"//npm.fake.test\":\n    npmAuthToken: \"" + unitFetchBearer + "\"\n  \"//registry.fake.test:8443\":\n    npmAuthToken: \"" + unitFetchBearer + "\"\n",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := managerFetchWorkspace(t, tc.declared, tc.files)
			program := strings.Fields(tc.name)[0]
			bin := filepath.Join(t.TempDir(), program)
			call, status, err := runFetchWithMockedManager(t, ctx, program, bin, unitCredential(), tc.answers)
			if err != nil || status != "OK" {
				t.Fatalf("runWorkspaceFetch = %q, %v; want OK", status, err)
			}
			if call.events[0] != "credential" {
				t.Fatalf("events = %v, want the credential read before any process", call.events)
			}
			if call.bin != bin || !slices.Equal(call.installArg, tc.args) {
				t.Fatalf("scratch install = %s %v, want %s %v", call.bin, call.installArg, bin, tc.args)
			}
			if root, _ := filepath.EvalSymlinks(ctx.WorkspaceRoot); call.installDir == root {
				t.Fatal("the scratch install ran in the workspace")
			}
			for name, value := range tc.env {
				if call.installEnv[name] != value {
					t.Errorf("scratch install env %s = %q, want %q", name, call.installEnv[name], value)
				}
			}
			if !strings.HasPrefix(call.installEnv["HOME"], filepath.Join(os.TempDir(), "putnami-ts-fetch-home-")) {
				t.Errorf("scratch install HOME = %q, want the private home", call.installEnv["HOME"])
			}
			if got := keys(call.scratch); !slices.Equal(got, tc.scratch) {
				t.Errorf("scratch copy = %v, want %v", got, tc.scratch)
			}
			if !slices.Equal(keys(call.home), keys(tc.home)) {
				t.Errorf("private home = %v, want %v", keys(call.home), keys(tc.home))
			}
			for name, want := range tc.home {
				if call.home[name] != want {
					t.Errorf("private %s = %q, want %q", name, call.home[name], want)
				}
			}
			if got := call.scratch[".npmrc"]; got != "@fake:registry=https://npm.fake.test/\n" {
				t.Errorf("scratch .npmrc = %q, want no repository credential line", got)
			}
			if rc, ok := call.scratch[".yarnrc.yml"]; ok && strings.Contains(rc, "npmAuthToken") {
				t.Errorf("scratch .yarnrc.yml = %q, want no repository credential line", rc)
			}
			for rel, content := range call.scratch {
				if strings.Contains(content, unitFetchBearer) {
					t.Errorf("scratch %s holds the bearer", rel)
				}
			}
			if _, err := os.Stat(filepath.Join(ctx.WorkspaceRoot, "node_modules")); !os.IsNotExist(err) {
				t.Errorf("workspace-fetch created node_modules in the workspace")
			}
		})
	}
}

func TestAFetchWithoutTheManagersLockFails(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first", "each-manager-fetches-its-committed-lockfile-into-its-cache")
	for declared, want := range map[string]string{
		"pnpm@10.26.1": "committed pnpm-lock.yaml",
		"npm@10.9.0":   "committed package-lock.json or npm-shrinkwrap.json",
		"yarn@4.5.0":   "committed yarn.lock",
	} {
		ctx := managerFetchWorkspace(t, declared, map[string]string{"bun.lock": "{}"})
		program, _, _ := strings.Cut(declared, "@")
		call, status, err := runFetchWithMockedManager(t, ctx, program, "/usr/bin/"+program, nil, nil)
		if status != "FAILED" || err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "putnami install") {
			t.Errorf("%s: runWorkspaceFetch = %q, %v; want a failure naming the %s", declared, status, err, want)
		}
		if len(call.events) != 1 {
			t.Errorf("%s: processes ran without a lock: %v", declared, call.events[1:])
		}
	}
}

func TestAManagerInsideTheWorkspaceAYarnPathOrPluginIsRefused(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first", "a-manager-inside-the-workspace-a-yarn-path-or-plugin-is-refused")
	for _, tc := range []struct {
		name     string
		declared string
		files    map[string]string
		inside   bool
		want     string
	}{
		{"pnpm inside the workspace", "pnpm@10.26.1", map[string]string{"pnpm-lock.yaml": ""}, true, "inside the workspace"},
		{"yarnPath", "yarn@4.5.0", map[string]string{"yarn.lock": "", ".yarnrc.yml": "yarnPath: .yarn/releases/yarn-4.5.0.cjs\n"}, false, "yarnPath"},
		{"yarn 1 reading yarnPath", "yarn@1.22.22", map[string]string{"yarn.lock": "", ".yarnrc.yml": "yarnPath: .yarn/releases/yarn-4.5.0.cjs\n"}, false, "yarnPath"},
		{"yarn-path", "yarn@1.22.22", map[string]string{"yarn.lock": "", ".yarnrc": "yarn-path \".yarn/releases/yarn-1.22.22.js\"\n"}, false, "yarn-path"},
		{"plugins", "yarn@4.5.0", map[string]string{"yarn.lock": "", ".yarnrc.yml": "plugins:\n  - path: .yarn/plugins/x.cjs\n"}, false, "plugins"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := managerFetchWorkspace(t, tc.declared, tc.files)
			program, _, _ := strings.Cut(tc.declared, "@")
			bin := "/usr/bin/" + program
			if tc.inside {
				bin = filepath.Join(ctx.WorkspaceRoot, "node_modules", ".bin", program)
			}
			call, status, err := runFetchWithMockedManager(t, ctx, program, bin, unitCredential(), nil)
			if status != "FAILED" || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runWorkspaceFetch = %q, %v; want a refusal naming %q", status, err, tc.want)
			}
			if len(call.events) != 1 {
				t.Fatalf("processes ran before the refusal: %v", call.events[1:])
			}
		})
	}
	// An empty plugin list loads nothing.
	if err := refuseRepositoryManagerCode(writeRoot(t, map[string]string{".yarnrc.yml": "plugins: []\nnodeLinker: node-modules\n"}), packageManager{Name: managerYarn, Berry: true}); err != nil {
		t.Fatalf("an empty plugin list was refused: %v", err)
	}
}

// A hosted install of each manager runs its frozen, offline argv in the
// workspace and writes no file.
func TestAHostedPNPMInstallIsFrozenAndOfflineAndWritesNothing(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first", "each-manager-installs-frozen-and-offline")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	ctx := managerFetchWorkspace(t, "pnpm@10.26.1", map[string]string{"pnpm-lock.yaml": "lockfileVersion: '9.0'\n"})
	ctx.WorkspaceProjects = []pctx.ProjectRef{{ID: "/pkgs/a", Name: "a", SourceName: "a", Path: "pkgs/a"}}
	before := snapshotTree(t, ctx.WorkspaceRoot)
	mockLookPath(t, map[string]string{"pnpm": "/usr/bin/pnpm"})
	calls := recordCalls(t)
	if status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	if len(*calls) != 1 || !slices.Equal((*calls)[0].args, []string{"install", "--frozen-lockfile", "--offline"}) {
		t.Fatalf("the hosted install ran %v, want only pnpm install --frozen-lockfile --offline", *calls)
	}
	if after := snapshotTree(t, ctx.WorkspaceRoot); !slices.Equal(after, before) {
		t.Fatalf("the hosted install changed the workspace:\n%v\nwant\n%v", after, before)
	}
}

func writeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		writeProbeFile(t, filepath.Join(root, name), content)
	}
	return root
}

// snapshotTree lists every file under root with its content.
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			data, _ := os.ReadFile(path)
			out = append(out, filepath.ToSlash(rel)+"="+string(data))
		}
		return nil
	})
	return out
}
