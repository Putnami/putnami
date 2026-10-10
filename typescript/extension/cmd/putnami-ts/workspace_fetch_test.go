package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

const unitFetchBearer = "pkt_unit_fetch_bearer"

// handCredential makes readJobCredential answer credential, as the engine's
// descriptor would, and records when it was called in events.
func handCredential(t *testing.T, credential *registry.Credential, events *[]string) {
	t.Helper()
	orig := readJobCredential
	t.Cleanup(func() { readJobCredential = orig })
	readJobCredential = func() (*registry.Credential, error) {
		*events = append(*events, "credential")
		return credential, nil
	}
}

// recordCredentialRefresh replaces the native credential writer and counts
// the hosts it was asked for.
func recordCredentialRefresh(t *testing.T) *[]string {
	t.Helper()
	var hosts []string
	orig := ensureNpmRegistryCredential
	t.Cleanup(func() { ensureNpmRegistryCredential = orig })
	ensureNpmRegistryCredential = func(_ context.Context, _, host string) registrycred.Outcome {
		hosts = append(hosts, host)
		return registrycred.Outcome{Kind: registrycred.KindSkipped}
	}
	return &hosts
}

// fetchUnitWorkspace is a workspace with a lock, a member, a scope registry
// and a workspace .npmrc that carries a repository credential line. The root
// declares no packageManager, so shaping runs `bun --version`.
func fetchUnitWorkspace(t *testing.T) *pctx.Context {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	writeProbeFile(t, filepath.Join(dir, "putnami.extension.json"), `{}`)
	writeExtensionBiomeDefault(t, dir)
	writeProbeFile(t, filepath.Join(dir, "package.json"),
		`{"name":"ws","private":true,"workspaces":["pkgs/*"],"dependencies":{"@fake/marker":"1.0.0"}}`)
	writeProbeFile(t, filepath.Join(dir, "pkgs", "a", "package.json"), `{"name":"a"}`)
	writeProbeFile(t, filepath.Join(dir, committedLockFile), `{"lockfileVersion":1,"workspaces":{"":{"name":"ws"},"pkgs/a":{"name":"a"},},}`)
	writeProbeFile(t, filepath.Join(dir, ".npmrc"), "@fake:registry=https://npm.fake.test/\n//npm.fake.test/:_authToken=${NPM_TOKEN}\n")
	withRegistries(t, ctx, `{"npm":{"scopes":{"@fake":"https://npm.fake.test/"}}}`)
	return ctx
}

// privateTempDir points os.TempDir at a directory of the test's own, so the
// test sees every directory the job creates there. Unix reads TMPDIR; Windows
// reads TMP, then TEMP.
func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, dir)
	}
	if got := filepath.Clean(os.TempDir()); got != filepath.Clean(dir) {
		t.Fatalf("os.TempDir() = %q, want the test's own %q", got, dir)
	}
	return dir
}

func tempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// fetchCall is what the mocked bun saw when the scratch install started.
type fetchCall struct {
	events      []string
	scratch     map[string]string
	homeNpmrc   string
	homes       int
	installArgs []string
}

// runFetchWithMockedBun runs workspace-fetch with a bun that answers the
// version and cache probes and, at the scratch install, records the private
// directories the job created. install answers the scratch install.
func runFetchWithMockedBun(t *testing.T, ctx *pctx.Context, credential *registry.Credential, install func() (*exec.Result, error)) (fetchCall, string, error) {
	t.Helper()
	tmp := privateTempDir(t)
	var call fetchCall
	handCredential(t, credential, &call.events)
	mockBunResolution(t)
	cacheDir := filepath.Join(t.TempDir(), "cache")
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		call.events = append(call.events, strings.Join(args, " "))
		switch {
		case slices.Equal(args, []string{"--version"}):
			return &exec.Result{Success: true, Stdout: "1.4.0\n"}, nil
		case slices.Equal(args, []string{"pm", "cache"}):
			return &exec.Result{Success: true, Stdout: cacheDir}, nil
		case len(args) > 0 && args[0] == "install":
			call.installArgs = args
			call.scratch = map[string]string{}
			for _, name := range tempEntries(t, tmp) {
				dir := filepath.Join(tmp, name)
				if strings.HasPrefix(name, "putnami-ts-fetch-home-") {
					call.homes++
					data, _ := os.ReadFile(filepath.Join(dir, ".npmrc"))
					call.homeNpmrc = string(data)
					continue
				}
				_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
					if err == nil && !entry.IsDir() {
						rel, _ := filepath.Rel(dir, path)
						data, _ := os.ReadFile(path)
						call.scratch[filepath.ToSlash(rel)] = string(data)
					}
					return nil
				})
			}
			return install()
		}
		return &exec.Result{Success: true}, nil
	})
	status, _, err := runWorkspaceFetch(ctx, jsonl.New(), nil)
	if leftover := tempEntries(t, tmp); len(leftover) != 0 {
		t.Errorf("workspace-fetch left %v in the temporary directory", leftover)
	}
	return call, status, err
}

func unitCredential() *registry.Credential {
	return &registry.Credential{
		Bearer:    unitFetchBearer,
		ExpiresAt: "2099-01-01T00:00:00Z",
		Hosts:     []string{"npm.fake.test", "registry.fake.test:8443"},
	}
}

// The credential is read before any process starts, the scratch copy holds
// the manifests and the stripped .npmrc, the private home holds exactly the
// credential's hosts, and both are gone when the job returns.
func TestRunWorkspaceFetch_ReadsTheCredentialFirstAndRemovesItsPrivateFiles(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"the-credential-is-read-before-any-process-and-its-files-are-removed")
	ctx := fetchUnitWorkspace(t)
	refreshed := recordCredentialRefresh(t)

	call, status, err := runFetchWithMockedBun(t, ctx, unitCredential(), func() (*exec.Result, error) {
		return &exec.Result{Success: true}, nil
	})
	if err != nil || status != "OK" {
		t.Fatalf("runWorkspaceFetch = %q, %v; want OK", status, err)
	}
	if len(call.events) == 0 || call.events[0] != "credential" {
		t.Fatalf("events = %v, want the credential read before any process", call.events)
	}
	if slices.Contains(call.events, "--version") || !slices.Contains(call.events, "pm cache") {
		t.Fatalf("events = %v, want the cache probe and no manifest shaping", call.events)
	}
	if !slices.Equal(call.installArgs, []string{"install", "--ignore-scripts", "--frozen-lockfile"}) {
		t.Fatalf("scratch install argv = %v", call.installArgs)
	}
	if call.homes != 1 {
		t.Fatalf("private homes at the scratch install = %d, want 1", call.homes)
	}
	wantHome := "//npm.fake.test/:_authToken=" + unitFetchBearer + "\n//registry.fake.test:8443/:_authToken=" + unitFetchBearer + "\n"
	if call.homeNpmrc != wantHome {
		t.Fatalf("private .npmrc = %q, want one line per credential host", call.homeNpmrc)
	}
	for _, rel := range []string{"package.json", committedLockFile, "pkgs/a/package.json", ".npmrc"} {
		if _, ok := call.scratch[rel]; !ok {
			t.Errorf("the scratch copy lacks %s (has %v)", rel, keys(call.scratch))
		}
	}
	if got := call.scratch[".npmrc"]; got != "@fake:registry=https://npm.fake.test/\n" {
		t.Errorf("scratch .npmrc = %q, want the scope line without the repository credential line", got)
	}
	for rel, content := range call.scratch {
		if strings.Contains(content, unitFetchBearer) {
			t.Errorf("scratch %s holds the bearer", rel)
		}
	}
	if len(*refreshed) != 0 {
		t.Errorf("a handed credential still refreshed the native credential for %v", *refreshed)
	}
	if _, err := os.Stat(filepath.Join(ctx.WorkspaceRoot, "node_modules")); !os.IsNotExist(err) {
		t.Errorf("workspace-fetch created node_modules in the workspace (%v)", err)
	}
}

// A failed scratch install and a stopped one remove the private files too, and
// the report never carries the bearer.
func TestRunWorkspaceFetch_RemovesItsPrivateFilesOnFailure(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"the-credential-is-read-before-any-process-and-its-files-are-removed")
	for name, tc := range map[string]struct {
		result *exec.Result
		err    error
	}{
		"bun fails":        {result: &exec.Result{ExitCode: 1, Stderr: "error: 401 for token " + unitFetchBearer}},
		"bun cannot start": {err: errors.New("exec: bun vanished")},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := fetchUnitWorkspace(t)
			recordCredentialRefresh(t)
			_, status, err := runFetchWithMockedBun(t, ctx, unitCredential(), func() (*exec.Result, error) {
				return tc.result, tc.err
			})
			if status != "FAILED" || err == nil {
				t.Fatalf("runWorkspaceFetch = %q, %v; want a failure", status, err)
			}
			if strings.Contains(err.Error(), unitFetchBearer) {
				t.Fatalf("the failure echoes the bearer: %v", err)
			}
		})
	}
}

func TestFetchWorkspaceDependencies_StoppedParentRemovesItsPrivateFiles(t *testing.T) {
	ctx := fetchUnitWorkspace(t)
	tmp := privateTempDir(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if slices.Equal(args, []string{"pm", "cache"}) {
			return &exec.Result{Success: true, Stdout: t.TempDir()}, nil
		}
		// A termination signal cancels the parent while bun runs, and bun
		// dies with it.
		cancel()
		return &exec.Result{ExitCode: -1}, nil
	})
	err := fetchWorkspaceDependencies(parent, ctx.WorkspaceRoot, "/mock/bun", unitCredential(), jsonl.New())
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("err = %v, want the stop reported", err)
	}
	if leftover := tempEntries(t, tmp); len(leftover) != 0 {
		t.Fatalf("a stopped fetch left %v", leftover)
	}
}

// Without a committed bun.lock the fetch fails before it writes anything or
// starts bun, and names the command that fixes it.
func TestRunWorkspaceFetch_RequiresACommittedLock(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"a-workspace-without-a-committed-lock-fails")
	ctx := fetchUnitWorkspace(t)
	if err := os.Remove(filepath.Join(ctx.WorkspaceRoot, committedLockFile)); err != nil {
		t.Fatal(err)
	}
	before := readTextFile(t, filepath.Join(ctx.WorkspaceRoot, "package.json"))
	var events []string
	handCredential(t, nil, &events)
	mockBunResolution(t)
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		t.Errorf("bun %v ran in a workspace without a lock", args)
		return &exec.Result{Success: true}, nil
	})

	status, _, err := runWorkspaceFetch(ctx, jsonl.New(), nil)
	if status != "FAILED" || err == nil {
		t.Fatalf("runWorkspaceFetch = %q, %v; want a failure", status, err)
	}
	for _, want := range []string{"committed bun.lock", "putnami install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err, want)
		}
	}
	if after := readTextFile(t, filepath.Join(ctx.WorkspaceRoot, "package.json")); after != before {
		t.Errorf("package.json changed to %s", after)
	}
}

func TestRunWorkspaceFetch_ReportsAnUnreadableCredential(t *testing.T) {
	ctx := fetchUnitWorkspace(t)
	orig := readJobCredential
	t.Cleanup(func() { readJobCredential = orig })
	readJobCredential = func() (*registry.Credential, error) {
		return nil, errors.New("the job credential descriptor was empty")
	}
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		t.Errorf("bun %v ran after an unreadable credential", args)
		return &exec.Result{Success: true}, nil
	})
	if status, _, err := runWorkspaceFetch(ctx, jsonl.New(), nil); status != "FAILED" || err == nil {
		t.Fatalf("runWorkspaceFetch = %q, %v; want a failure", status, err)
	}
}

// Without a handed credential bun runs with the machine's native credentials:
// no private home, and the native refresh workspace-install does.
func TestRunWorkspaceFetch_WithoutACredentialUsesTheNativeOne(t *testing.T) {
	ctx := fetchUnitWorkspace(t)
	refreshed := recordCredentialRefresh(t)
	call, status, err := runFetchWithMockedBun(t, ctx, nil, func() (*exec.Result, error) {
		return &exec.Result{Success: true}, nil
	})
	if err != nil || status != "OK" {
		t.Fatalf("runWorkspaceFetch = %q, %v; want OK", status, err)
	}
	if call.homes != 0 {
		t.Errorf("private homes = %d, want none without a credential", call.homes)
	}
	if got, want := call.scratch[".npmrc"], readTextFile(t, filepath.Join(ctx.WorkspaceRoot, ".npmrc")); got != want {
		t.Errorf("scratch .npmrc = %q, want the workspace .npmrc verbatim %q", got, want)
	}
	if !slices.Equal(*refreshed, []string{"npm.fake.test"}) {
		t.Errorf("native refresh hosts = %v, want [npm.fake.test]", *refreshed)
	}
}

func TestWorkspaceInstallArgs(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"the-hosted-install-is-frozen-and-materializes-no-credential")
	for _, tc := range []struct {
		force, hosted bool
		want          []string
	}{
		{false, false, []string{"install"}},
		{true, false, []string{"install", "--force"}},
		{false, true, []string{"install", "--frozen-lockfile"}},
		{true, true, []string{"install", "--frozen-lockfile", "--force"}},
	} {
		if got := workspaceInstallArgs(tc.force, tc.hosted); !slices.Equal(got, tc.want) {
			t.Errorf("workspaceInstallArgs(force=%t, hosted=%t) = %v, want %v", tc.force, tc.hosted, got, tc.want)
		}
	}
}

// installArgv runs workspace-install with a mocked bun and returns the argv of
// `bun install` and the hosts the native credential refresh was asked for.
func installArgv(t *testing.T, force bool) ([]string, []string) {
	t.Helper()
	ctx := fetchUnitWorkspace(t)
	if force {
		ctx.Params["force"] = json.RawMessage(`true`)
	}
	refreshed := recordCredentialRefresh(t)
	mockBunResolution(t)
	var argv []string
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if len(args) > 0 && args[0] == "install" {
			argv = args
		}
		return &exec.Result{Success: true, Stdout: "1.4.0"}, nil
	})
	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("runWorkspaceInstall = %q, %v; want OK", status, err)
	}
	return argv, *refreshed
}

func TestRunWorkspaceInstall_OutsideAHostedRunKeepsTodaysArgv(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"the-hosted-install-is-frozen-and-materializes-no-credential")
	argv, refreshed := installArgv(t, false)
	if !slices.Equal(argv, []string{"install"}) {
		t.Errorf("argv = %v, want [install]", argv)
	}
	if !slices.Equal(refreshed, []string{"npm.fake.test"}) {
		t.Errorf("native refresh hosts = %v, want [npm.fake.test]", refreshed)
	}
	argv, _ = installArgv(t, true)
	if !slices.Equal(argv, []string{"install", "--force"}) {
		t.Errorf("forced argv = %v, want [install --force]", argv)
	}
}

func TestRunWorkspaceInstall_HostedRunInstallsFrozenAndMaterializesNothing(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"the-hosted-install-is-frozen-and-materializes-no-credential")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	for _, force := range []bool{false, true} {
		argv, refreshed := installArgv(t, force)
		want := workspaceInstallArgs(force, true)
		if !slices.Equal(argv, want) {
			t.Errorf("force=%t argv = %v, want %v", force, argv, want)
		}
		if len(refreshed) != 0 {
			t.Errorf("force=%t: a hosted install refreshed the native credential for %v", force, refreshed)
		}
	}
}

func TestFetchInputs_CopiesWhatAFrozenInstallReads(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"), `{
  "name": "ws",
  "workspaces": ["pkgs/*"],
  "dependencies": {"local": "file:./vendor/local", "@fake/marker": "1.0.0", "ws-a": "workspace:*"},
  "patchedDependencies": {"@fake/marker@1.0.0": "patches/marker.patch"}
}`)
	writeProbeFile(t, filepath.Join(root, committedLockFile), `{
  "lockfileVersion": 1,
  "workspaces": {
    "": {"name": "ws"},
    "pkgs/a": {"name": "a"},
    "deep/nested/b": {"name": "b"},
  },
}`)
	writeProbeFile(t, filepath.Join(root, "bunfig.toml"), "[install]\nlinker = \"isolated\"\n")
	writeProbeFile(t, filepath.Join(root, "pkgs", "a", "package.json"), `{"name":"a","devDependencies":{"tb":"file:../../vendor/tb.tgz"}}`)
	writeProbeFile(t, filepath.Join(root, "pkgs", "no-manifest", "README.md"), "not a member")
	writeProbeFile(t, filepath.Join(root, "deep", "nested", "b", "package.json"), `{"name":"b"}`)
	writeProbeFile(t, filepath.Join(root, "vendor", "local", "package.json"),
		`{"name":"local","dependencies":{"inner":"../inner"},"devDependencies":{"ignored":"link:ignored"}}`)
	writeProbeFile(t, filepath.Join(root, "vendor", "local", "index.js"), "module.exports = 1\n")
	writeProbeFile(t, filepath.Join(root, "vendor", "inner", "package.json"), `{"name":"inner"}`)
	writeProbeFile(t, filepath.Join(root, "vendor", "tb.tgz"), "tarball")
	writeProbeFile(t, filepath.Join(root, "patches", "marker.patch"), "diff")

	got, err := fetchInputs(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		committedLockFile,
		"bunfig.toml",
		"deep/nested/b/package.json",
		"package.json",
		"patches/marker.patch",
		"pkgs/a/package.json",
		"vendor/inner/package.json",
		"vendor/local/package.json",
		"vendor/tb.tgz",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("fetchInputs =\n%v\nwant\n%v", got, want)
	}
}

func TestFetchInputs_RefusesWhatAHostedRunCannotHold(t *testing.T) {
	for name, manifest := range map[string]string{
		"link":            `{"name":"ws","dependencies":{"linked":"link:linked"}}`,
		"outside file":    `{"name":"ws","dependencies":{"out":"file:../outside"}}`,
		"outside bare":    `{"name":"ws","dependencies":{"out":"../outside"}}`,
		"absolute":        `{"name":"ws","dependencies":{"out":"/opt/pkg"}}`,
		"home":            `{"name":"ws","dependencies":{"out":"~/pkg"}}`,
		"outside patch":   `{"name":"ws","patchedDependencies":{"x@1.0.0":"../x.patch"}}`,
		"member link dev": `{"name":"ws","workspaces":["pkgs/*"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeProbeFile(t, filepath.Join(root, "package.json"), manifest)
			writeProbeFile(t, filepath.Join(root, committedLockFile), `{"lockfileVersion":1}`)
			writeProbeFile(t, filepath.Join(root, "pkgs", "a", "package.json"), `{"name":"a","devDependencies":{"l":"link:l"}}`)
			_, err := fetchInputs(root)
			if err == nil || !strings.Contains(err.Error(), "hosted run") {
				t.Fatalf("fetchInputs err = %v, want a refusal naming the hosted run", err)
			}
		})
	}
}

func TestFetchInputs_RequiresARootManifest(t *testing.T) {
	if _, err := fetchInputs(t.TempDir()); err == nil {
		t.Fatal("fetchInputs accepted a workspace without package.json")
	}
}

func TestStripNpmrcCredentials(t *testing.T) {
	in := "@fake:registry=https://npm.fake.test/\n" +
		"//npm.fake.test/:_authToken=${NPM_TOKEN}\n" +
		"  //npm.fake.test/:_AUTHTOKEN = secret\n" +
		"_auth=dXNlcjpwYXNz\n" +
		"//npm.fake.test/:_password=cGFzcw==\n" +
		"//npm.fake.test/:username=user\n" +
		"; a comment\n" +
		"linker=isolated"
	want := "@fake:registry=https://npm.fake.test/\n" +
		"//npm.fake.test/:username=user\n" +
		"; a comment\n" +
		"linker=isolated"
	if got := string(stripNpmrcCredentials([]byte(in))); got != want {
		t.Fatalf("stripNpmrcCredentials =\n%q\nwant\n%q", got, want)
	}
}

func TestCredentialNpmrc(t *testing.T) {
	if lines, err := credentialNpmrc(nil); err != nil || len(lines) != 0 {
		t.Fatalf("credentialNpmrc(nil) = %q, %v; want nothing", lines, err)
	}
	for name, bearer := range map[string]string{
		"comment":   "pkt;secret",
		"hash":      "pkt#secret",
		"variable":  "pkt${HOME}",
		"escape":    `pkt\secret`,
		"quote":     `"pkt"`,
		"not ascii": "pkt\x7fsecret",
	} {
		t.Run(name, func(t *testing.T) {
			credential := unitCredential()
			credential.Bearer = bearer
			_, err := credentialNpmrc(credential)
			if err == nil {
				t.Fatalf("bearer %q was written to an .npmrc", bearer)
			}
			if strings.Contains(err.Error(), bearer) {
				t.Fatalf("the refusal echoes the bearer: %v", err)
			}
		})
	}
	invalid := unitCredential()
	invalid.Hosts = []string{"https://npm.fake.test"}
	if _, err := credentialNpmrc(invalid); err == nil {
		t.Fatal("a credential with a URL for a host was accepted")
	}
}

func TestBunCacheDir(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "cache")
	for name, tc := range map[string]struct {
		result *exec.Result
		want   string
	}{
		"absolute":    {&exec.Result{Success: true, Stdout: absolute + "\n"}, absolute},
		"relative":    {&exec.Result{Success: true, Stdout: "cache"}, ""},
		"empty":       {&exec.Result{Success: true}, ""},
		"bun failed":  {&exec.Result{ExitCode: 1, Stderr: "no package.json"}, ""},
		"two answers": {&exec.Result{Success: true, Stdout: absolute + "\n" + absolute}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			withMockWsExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
				if !slices.Equal(args, []string{"pm", "cache"}) {
					t.Errorf("args = %v", args)
				}
				return tc.result, nil
			})
			got, err := bunCacheDir(context.Background(), "bun", t.TempDir())
			if tc.want == "" {
				if err == nil {
					t.Fatalf("bunCacheDir = %q, want a failure", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("bunCacheDir = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestRunBunWithin_ReportsAStoppedParent(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	withMockWsExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{ExitCode: -1}, nil
	})
	if _, err := runBunWithin(parent, "bun install", "bun", []string{"install"}, t.TempDir(), time.Minute); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("err = %v, want the stop reported", err)
	}
}

// Shaping twice rewrites nothing the second time: every file it writes keeps
// its bytes and its modification time.
func TestShapeWorkspaceManifests_IsIdempotent(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"fetch-and-install-shape-the-same-manifests")
	ctx, dir := membershipTree(t)
	ctx.WorkspaceProjects = membership()
	writeProbeFile(t, filepath.Join(dir, "putnami.extension.json"), `{"workspaceDevDependencies":{"typescript":"^6.0.2"}}`)
	writeProbeFile(t, filepath.Join(dir, "web", "package.json"), `{"name":"web","dependencies":{"@putnami/web":"catalog:"}}`)
	withRegistries(t, ctx, `{"npm":{"scopes":{"@putnami":"https://npm.putnami.dev"}}}`)
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, Stdout: "1.4.0"}, nil
	})

	if err := shapeWorkspaceManifests(ctx, jsonl.New(), "/mock/bun"); err != nil {
		t.Fatal(err)
	}
	shaped := []string{"package.json", ".npmrc", "tsconfig.json", "biome.json"}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	before := map[string]string{}
	for _, rel := range shaped {
		path := filepath.Join(dir, rel)
		before[rel] = readTextFile(t, path)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{`"typescript"`, `"web"`, `"@putnami/web"`, `"packageManager"`} {
		if !strings.Contains(before["package.json"], want) {
			t.Fatalf("the first shaping did not write %s:\n%s", want, before["package.json"])
		}
	}

	if err := shapeWorkspaceManifests(ctx, jsonl.New(), "/mock/bun"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range shaped {
		path := filepath.Join(dir, rel)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(past) || readTextFile(t, path) != before[rel] {
			t.Errorf("the second shaping rewrote %s", rel)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

func TestRunWorkspaceFetch_RunsNoBunFromTheWorkspace(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hosted-install-fetches-first",
		"workspace-fetch-runs-no-bun-from-the-workspace")
	ctx := fetchUnitWorkspace(t)
	committed := filepath.Join(ctx.WorkspaceRoot, "node_modules", ".bin", "bun")
	orig := provisionBunBin
	t.Cleanup(func() { provisionBunBin = orig })
	provisionBunBin = func(*pctx.Context, *jsonl.Emitter, toolchain.BunMode, func(string) string) (string, error) {
		return committed, nil
	}
	var events []string
	handCredential(t, nil, &events)
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		t.Errorf("bun %v ran from inside the workspace", args)
		return &exec.Result{Success: true}, nil
	})

	status, _, err := runWorkspaceFetch(ctx, jsonl.New(), nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("runWorkspaceFetch = %q, %v; want a refusal naming the workspace", status, err)
	}
}

func TestWithin(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(outside, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip("symbolic links are unavailable:", err)
	}
	for path, want := range map[string]bool{
		root:                                  true,
		filepath.Join(root, "bin", "bun"):     true,
		filepath.Join(link, "bin", "bun"):     true,
		filepath.Join(outside, "bun"):         false,
		filepath.Join(root+"-sibling", "bun"): false,
	} {
		if got := within(root, path); got != want {
			t.Errorf("within(%q) = %v, want %v", path, got, want)
		}
	}
	if !within(filepath.Join(outside, "missing"), filepath.Join(outside, "bun")) {
		t.Error("an unresolvable root must contain every path")
	}
}
