package php

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// runWorkspaceJob runs a workspace job over the workspace projects refs.
func runWorkspaceJob(t *testing.T, root string, jobFunc cli.JobFunc, refs ...job.ProjectRef) (string, error) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	status, _, runErr := jobFunc(&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs}, jsonl.New(), nil)
	return status, runErr
}

func phpProject(root, path, config string) job.ProjectRef {
	return job.ProjectRef{Name: path, Path: path, FullPath: filepath.Join(root, path), Extensions: []string{ExtensionName}, Config: []byte(config)}
}

// prepareComposer pins the stub Composer and a machine cache for the test.
func prepareComposer(t *testing.T) (log, cache string) {
	t.Helper()
	t.Cleanup(setComposerDigest(t))
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	cache = t.TempDir()
	t.Setenv(extensionproto.MachineCacheRootEnv, cache)
	composerServer(t)
	return composerStub(t, "8.4.10"), cache
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

func TestWorkspaceFetchDownloadsTheLockedPackagesAndRunsNoneOfTheirCode(t *testing.T) {
	spectest.Proves(t, feature, "fetches-the-locked-packages", "workspace-fetch-downloads-the-locked-packages-and-runs-none-of-their-code")
	log, cache := prepareComposer(t)
	root := fixture(t)

	status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), phpProject(root, ".", `{}`), phpProject(root, "packages/pricing", `{}`))
	if status != "OK" || err != nil {
		t.Fatalf("fetch: %s %v", status, err)
	}
	lines := readLines(t, log)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "install --no-interaction --no-progress --no-scripts --no-plugins --no-autoloader") {
		t.Fatalf("one fetch at the stack root, got %q", lines)
	}
	env := readLines(t, log+".env")[0]
	if !strings.Contains(env, "cache="+filepath.Join(cache, "composer")+" ") || strings.Contains(env, "vendor= ") {
		t.Errorf("env = %q", env)
	}
	if _, err := os.Stat(filepath.Join(root, "vendor")); !os.IsNotExist(err) {
		t.Errorf("the workspace gets no vendor directory: %v", err)
	}
}

func TestWorkspaceFetchSkipsARootWithoutALock(t *testing.T) {
	log, _ := prepareComposer(t)
	root := fixture(t)
	if err := os.Remove(filepath.Join(root, "composer.lock")); err != nil {
		t.Fatal(err)
	}
	if status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), phpProject(root, ".", `{}`)); status != "OK" || err != nil {
		t.Fatalf("fetch: %s %v", status, err)
	}
	if lines := readLines(t, log); lines != nil {
		t.Errorf("Composer ran: %q", lines)
	}
}

func TestWorkspaceJobsPassWithoutPHP(t *testing.T) {
	root := fixture(t)
	t.Setenv("PATH", t.TempDir())
	for name, jobFunc := range map[string]cli.JobFunc{"fetch": WorkspaceFetchJob(stack(t)), "install": WorkspaceInstallJob(stack(t))} {
		if status, err := runWorkspaceJob(t, root, jobFunc, phpProject(root, ".", `{}`)); status != "OK" || err != nil {
			t.Errorf("%s: %s %v", name, status, err)
		}
	}
}

func TestWorkspaceInstallInstallsEachStackRootTheWorkspaceIncludes(t *testing.T) {
	spectest.Proves(t, feature, "workspace-install-runs-composer", "workspace-install-installs-each-stack-root-the-workspace-includes")
	log, cache := prepareComposer(t)
	root := fixture(t)
	writeFile(t, root, "putnami.workspace.json", `{"disable": {"tags": ["e2e"]}}`)
	writeFile(t, root, "samples/app/composer.json", `{"type": "project"}`)
	writeFile(t, root, "samples/app/composer.lock", `{"packages": []}`)
	writeFile(t, root, "other/composer.json", `{"type": "project"}`)

	foreign := phpProject(root, "other", `{}`)
	foreign.Extensions = []string{"@putnami/typescript"}
	refs := []job.ProjectRef{phpProject(root, ".", `{}`), phpProject(root, "packages/pricing", `{}`), phpProject(root, "samples/app", `{"tags": ["e2e"]}`), foreign}

	for range 2 {
		if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), refs...); status != "OK" || err != nil {
			t.Fatalf("install: %s %v", status, err)
		}
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, log)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], real+": ") || !strings.HasSuffix(lines[0], "install --no-interaction --no-progress") {
		t.Fatalf("one install of the root, and none of a current one, got %q", lines)
	}
	env := readLines(t, log+".env")[0]
	if !strings.Contains(env, "cache="+filepath.Join(cache, "composer")+" ") || !strings.HasSuffix(env, "network=") {
		t.Errorf("env = %q", env)
	}
}

func TestAnOfflineWorkspaceInstallDisablesTheNetwork(t *testing.T) {
	log, _ := prepareComposer(t)
	root := fixture(t)
	if _, err := provideComposer(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), phpProject(root, ".", `{}`)); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if env := readLines(t, log+".env"); len(env) != 1 || !strings.HasSuffix(env[0], "network=1") {
		t.Errorf("env = %q", env)
	}
}

func TestAnOfflineWorkspaceInstallLeavesARootWithoutALock(t *testing.T) {
	log, _ := prepareComposer(t)
	root := fixture(t)
	if err := os.Remove(filepath.Join(root, "composer.lock")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), phpProject(root, ".", `{}`)); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if lines := readLines(t, log); lines != nil {
		t.Errorf("Composer ran: %q", lines)
	}
}

func TestAFailedFetchNamesTheComposerFailure(t *testing.T) {
	log, _ := prepareComposer(t)
	root := fixture(t)
	t.Setenv("STUB_COMPOSER_FAILS", "1")
	status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), phpProject(root, ".", `{}`))
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeComposerFetchFailed) {
		t.Fatalf("fetch: %s %v", status, err)
	}
	if lines := readLines(t, log); len(lines) != 1 {
		t.Errorf("Composer ran %d times", len(lines))
	}
}
