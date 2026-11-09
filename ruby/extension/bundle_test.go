package ruby

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

// componentContext is the job context of the component at rel, with the
// metadata the probe answers for it.
func componentContext(t *testing.T, root, rel string, params job.Params) *pctx.Context {
	t.Helper()
	var meta json.RawMessage
	for _, p := range probe(t, root).Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	return &pctx.Context{WorkspaceRoot: root, Params: params, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
}

// workspaceContext is the context of a workspace job over the fixture's
// components.
func workspaceContext(root string, params job.Params) *pctx.Context {
	var refs []job.ProjectRef
	for _, p := range []string{"gems/audit_log", "packs/checkout", "packs/docs", "packs/pricing"} {
		refs = append(refs, job.ProjectRef{Name: p, Path: p, FullPath: filepath.Join(root, p), Extensions: []string{ExtensionName}})
	}
	return &pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs, Params: params}
}

func TestTheInstallStepInstallsTheBundleOfEachStackRoot(t *testing.T) {
	spectest.Proves(t, feature, "installs-its-bundle", "the-install-step-installs-the-bundle-of-each-stack-root")
	root := fixture(t)
	log := stubTools(t, "3.3.6", "2.5.22", 0)
	s := stack(t)
	t.Setenv("STUB_BUNDLE_CHECK", "0")
	if status, _, err := InstallJob(s)(componentContext(t, root, "packs/pricing", nil), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if got := calls(t, log); len(got) != 1 || got[0] != "" {
		t.Errorf("a complete bundle installs nothing: %q", got)
	}
	t.Setenv("STUB_BUNDLE_CHECK", "1")
	if status, _, err := InstallJob(s)(componentContext(t, root, "packs/pricing", nil), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if got := calls(t, log); !reflect.DeepEqual(got, []string{root + ": bundle install"}) {
		t.Errorf("an incomplete bundle installs at the stack root: %q", got)
	}
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")
	if status, _, err := InstallJob(s)(componentContext(t, root, "gems/audit_log", nil), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if got := calls(t, log); got[len(got)-1] != filepath.Join(root, "gems/audit_log")+": bundle install --local" {
		t.Errorf("offline, a gem with its own Gemfile installs from the machine's gems: %q", got)
	}
}

func TestWorkspaceInstallInstallsEachStackRootOnce(t *testing.T) {
	spectest.Proves(t, feature, "workspace-install-runs-bundler", "workspace-install-installs-each-stack-root-the-workspace-includes")
	spectest.Proves(t, feature, "workspace-install-runs-bundler", "workspace-install-force-installs-a-root-whose-bundle-is-complete")
	root := fixture(t)
	log := stubTools(t, "3.3.6", "2.5.22", 0)
	s := stack(t)
	t.Setenv("STUB_BUNDLE_CHECK", "1")
	if status, _, err := WorkspaceInstallJob(s)(workspaceContext(root, nil), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("workspace-install: %s %v", status, err)
	}
	if got := calls(t, log); !reflect.DeepEqual(got, []string{filepath.Join(root, "gems/audit_log") + ": bundle install", root + ": bundle install"}) {
		t.Errorf("each stack root installs once, in path order, the application's for its three packs: %q", got)
	}
	t.Setenv("STUB_BUNDLE_CHECK", "0")
	if status, _, err := WorkspaceInstallJob(s)(workspaceContext(root, job.Params{"force": json.RawMessage(`true`)}), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("workspace-install --force: %s %v", status, err)
	}
	if got := calls(t, log); len(got) != 4 {
		t.Errorf("the force flag installs a complete bundle again: %q", got)
	}
}

func TestTheInstallStepInstallsAgainWhenTheBundlerDiffersFromTheLock(t *testing.T) {
	spectest.Proves(t, feature, "installs-its-bundle", "the-install-step-installs-again-when-the-bundler-differs-from-the-lock")
	spectest.Proves(t, feature, "workspace-install-runs-bundler", "workspace-install-installs-again-when-the-bundler-differs-from-the-lock")
	root := fixture(t)
	s := stack(t)
	t.Setenv("STUB_BUNDLE_CHECK", "0")

	// The fixture's Gemfile.lock states Bundler 2.5.22; 2.6.9 runs.
	log := stubTools(t, "3.3.6", "2.6.9", 0)
	if status, _, err := InstallJob(s)(componentContext(t, root, "packs/pricing", nil), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if got := calls(t, log); !reflect.DeepEqual(got, []string{root + ": bundle install"}) {
		t.Errorf("a complete bundle with another Bundler installs again: %q", got)
	}

	log = stubTools(t, "3.3.6", "2.6.9", 0)
	if status, _, err := WorkspaceInstallJob(s)(workspaceContext(root, nil), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("workspace-install: %s %v", status, err)
	}
	if got := calls(t, log); !reflect.DeepEqual(got, []string{root + ": bundle install"}) {
		t.Errorf("only the root whose lockfile states another Bundler installs again: %q", got)
	}

	log = stubTools(t, "3.3.6", "2.5.22", 0)
	if status, _, err := InstallJob(s)(componentContext(t, root, "packs/pricing", nil), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if got := calls(t, log); len(got) != 1 || got[0] != "" {
		t.Errorf("a complete bundle with the locked Bundler installs nothing: %q", got)
	}

	log = stubTools(t, "3.3.6", "2.6.9", 0)
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")
	if status, _, err := InstallJob(s)(componentContext(t, root, "packs/pricing", nil), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if got := calls(t, log); len(got) != 1 || got[0] != "" {
		t.Errorf("offline, another Bundler alone installs nothing: %q", got)
	}
}

func TestDepsUpgradeRunsBundleUpdate(t *testing.T) {
	spectest.Proves(t, feature, "upgrades-the-dependencies", "deps-upgrade-runs-bundle-update")
	root := fixture(t)
	log := stubTools(t, "3.3.6", "2.5.22", 0)
	if status, _, err := DepsUpgradeJob(stack(t))(workspaceContext(root, nil), jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("deps-upgrade: %s %v", status, err)
	}
	if got := calls(t, log); !reflect.DeepEqual(got, []string{filepath.Join(root, "gems/audit_log") + ": bundle update", root + ": bundle update"}) {
		t.Errorf("deps-upgrade updates each stack root: %q", got)
	}
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")
	if status, _, err := DepsUpgradeJob(stack(t))(workspaceContext(root, nil), jsonl.New(), nil); status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodeBundleUpdateFailed) {
		t.Errorf("offline, bundle update fails: %s %v", status, err)
	}
}

func TestTestPassesTheFilterToTheRunner(t *testing.T) {
	spectest.Proves(t, feature, "filters-the-tests", "test-passes-the-filter-to-rspec-or-rails-test-and-keys-the-task-on-it")
	root := fixture(t)
	log := stubTools(t, "3.3.6", "2.5.22", 0)
	run := func(rel string, params job.Params) {
		t.Helper()
		ctx := componentContext(t, root, rel, params)
		out := filepath.Join(root, ".putnami/out", strings.ReplaceAll(rel, "/", "_"))
		if status, _, err := TestJob(stack(t))(ctx, jsonl.New(), []string{"--report-dir", out}); status != "OK" || err != nil {
			t.Fatalf("test %s: %s %v", rel, status, err)
		}
	}
	run("packs/pricing", job.Params{"test": json.RawMessage(`"sums the lines"`)})
	run("packs/checkout", job.Params{"t": json.RawMessage(`"totals (a)"`)})
	got := calls(t, log)
	if len(got) != 2 || !strings.HasSuffix(got[0], ` -e sums the lines`) || !strings.HasSuffix(got[1], ` -n /totals \(a\)/`) {
		t.Errorf("the filter reaches RSpec as -e and rails test as -n: %q", got)
	}
}

func TestTheManifestKeysTheFilterAndInstallsFirst(t *testing.T) {
	var m struct {
		Commands map[string]struct {
			Run []struct {
				ID        string   `json:"id"`
				DependsOn []string `json:"dependsOn"`
			} `json:"run"`
		} `json:"commands"`
		Tasks map[string]struct {
			Inputs map[string]struct {
				From string `json:"from"`
			} `json:"inputs"`
		} `json:"tasks"`
	}
	data, err := os.ReadFile("putnami.extension.json")
	if err != nil || json.Unmarshal(data, &m) != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	for _, c := range []string{stacks.TaskLint, stacks.TaskTest} {
		steps := m.Commands[c].Run
		i := slices.IndexFunc(steps, func(s struct {
			ID        string   `json:"id"`
			DependsOn []string `json:"dependsOn"`
		}) bool {
			return s.ID == c
		})
		if i < 0 || !slices.Contains(steps[i].DependsOn, "install") {
			t.Errorf("%s runs after the install step: %+v", c, steps)
		}
	}
	for _, flag := range []string{"test", "t"} {
		if m.Tasks["test-run"].Inputs[flag].From != "params" {
			t.Errorf("the test task keys on the %s flag: %+v", flag, m.Tasks["test-run"].Inputs)
		}
	}
}

func TestIsComponentFollowsTheDetectorsWalk(t *testing.T) {
	if !IsComponent("", "packs/pricing") || IsComponent("", "vendor/bundle/gems/rake") || IsComponent("", "packs/.cache") || !IsComponent("", ".") {
		t.Error("IsComponent follows the directories the detector walks")
	}
}
