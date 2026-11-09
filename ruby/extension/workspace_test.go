package ruby

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// runWorkspaceJob runs a workspace job over the projects at paths.
func runWorkspaceJob(t *testing.T, root string, jobFunc func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error), paths ...string) (string, error) {
	t.Helper()
	var refs []job.ProjectRef
	for _, p := range paths {
		refs = append(refs, job.ProjectRef{Name: p, Path: p, FullPath: filepath.Join(root, p), Extensions: []string{ExtensionName}})
	}
	status, _, err := quietly(t, func() (string, map[string]any, error) {
		return jobFunc(&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs}, jsonl.New(), nil)
	})
	return status, err
}

// machineCache points the extension's machine cache at a new directory.
func machineCache(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	t.Setenv(extensionproto.MachineCacheRootEnv, cache)
	return cache
}

func TestWorkspaceFetchDownloadsTheLockedGemsAndRunsNoneOfTheirCode(t *testing.T) {
	spectest.Proves(t, feature, "fetches-the-locked-gems", "workspace-fetch-downloads-the-locked-gems-and-runs-none-of-their-code")
	log := rubyTools(t)
	cache := machineCache(t)
	root := fixture(t)

	status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), "gems/audit_log", "packs/checkout", "packs/pricing")
	if status != "OK" || err != nil {
		t.Fatalf("fetch: %s %v", status, err)
	}
	if lines := readLines(t, log); len(lines) != 1 || lines[0] != root+": bundle cache" {
		t.Fatalf("one fetch at the stack root with a Gemfile.lock, got %q", lines)
	}
	env := readLines(t, log+".env")[0]
	for _, want := range []string{"BUNDLE_CACHE_PATH=" + filepath.Join(cache, "gems") + " ", "BUNDLE_NO_INSTALL=true ", "BUNDLE_FROZEN=true ", "BUNDLE_NO_PRUNE=true ", "BUNDLE_GEMFILE=" + filepath.Join(root, "Gemfile") + " "} {
		if !strings.Contains(env, want) {
			t.Errorf("env has no %s: %q", want, env)
		}
	}
	for _, private := range []string{"BUNDLE_APP_CONFIG=" + root, "BUNDLE_PATH=" + root} {
		if strings.Contains(env, private) {
			t.Errorf("Bundler writes in the repository: %q", env)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, "gems", "rack-3.1.0.gem")); err != nil {
		t.Errorf("the gem cache holds no gem: %v", err)
	}
	for _, written := range []string{".bundle", "vendor"} {
		if _, err := os.Stat(filepath.Join(root, written)); !os.IsNotExist(err) {
			t.Errorf("the workspace gets %s: %v", written, err)
		}
	}
}

func TestWorkspaceFetchSkipsARootWithoutALock(t *testing.T) {
	log := rubyTools(t)
	machineCache(t)
	root := fixture(t)
	if err := os.Remove(filepath.Join(root, gemfileLock)); err != nil {
		t.Fatal(err)
	}
	if status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), "packs/pricing"); status != "OK" || err != nil {
		t.Fatalf("fetch: %s %v", status, err)
	}
	if lines := readLines(t, log); lines != nil {
		t.Errorf("Bundler ran: %q", lines)
	}
}

func TestWorkspaceFetchPassesWithoutRuby(t *testing.T) {
	root := fixture(t)
	t.Setenv("PATH", t.TempDir())
	if status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), "packs/pricing"); status != "OK" || err != nil {
		t.Errorf("fetch: %s %v", status, err)
	}
}

func TestAFailedFetchNamesTheBundlerFailure(t *testing.T) {
	rubyTools(t)
	machineCache(t)
	root := fixture(t)
	t.Setenv("STUB_EXIT", "1")
	status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), "packs/pricing")
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeBundleFetchFailed) {
		t.Errorf("fetch: %s %v", status, err)
	}
}

func TestAnOfflineInstallReadsTheGemCache(t *testing.T) {
	spectest.Proves(t, feature, "workspace-install-runs-bundler", "an-offline-workspace-install-installs-from-the-gem-cache")
	log := rubyTools(t)
	cache := machineCache(t)
	root := fixture(t)
	t.Setenv("STUB_BUNDLE_CHECK", "1")
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), "packs/pricing"); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if lines := readLines(t, log); len(lines) != 1 || lines[0] != root+": bundle install --local" {
		t.Fatalf("bundle ran with %q", lines)
	}
	if env := readLines(t, log+".env")[0]; !strings.Contains(env, "BUNDLE_CACHE_PATH="+filepath.Join(cache, "gems")+" ") || !strings.Contains(env, "BUNDLE_NO_PRUNE=true") {
		t.Errorf("env = %q", env)
	}
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), "packs/pricing"); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if lines := readLines(t, log); len(lines) != 2 || lines[1] != root+": bundle install" {
		t.Fatalf("bundle ran with %q", lines)
	}
	if env, _ := os.ReadFile(log + ".env"); strings.Count(string(env), "BUNDLE_CACHE_PATH=") != 1 {
		t.Errorf("an online install reads the gem cache: %q", env)
	}
}
