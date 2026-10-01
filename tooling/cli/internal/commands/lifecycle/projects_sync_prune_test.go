package lifecycle

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// `projects sync --prune` DELETES Putnami membership, and it decides what to
// delete from the answer of a scan whose reach depends entirely on which
// adapters resolved.
//
// Since slice C4b core's own marker table is gone: core recognizes `putnami.json`
// and nothing else. So a run whose adapters did not resolve scans a workspace in
// which every package.json / go.mod / pyproject.toml member is invisible — and
// `--prune` reads that as "they were all deleted". One failed extension
// resolution empties the workspace config, silently, in a command people run to
// REPAIR things.
//
// The tests below pin the refusal and its two limits: it must not fire when the
// scan really is complete, and it must not stop the rest of the sync.

// prunableWorkspace declares two members: one core can see on its own, and one
// only a marker scan could have found.
func prunableWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"includes":["tool","apps/web"]}`)
	writeProjectFile(t, filepath.Join(root, "tool", "putnami.json"), `{"name":"tool"}`)
	writeProjectFile(t, filepath.Join(root, "apps", "web", "package.json"), `{"name":"@acme/web"}`)

	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root
}

func TestProjectsSync_PruneRefusesWhenARecordedProviderDidNotResolve(t *testing.T) {
	root := prunableWorkspace(t)
	// The index records an adapter this workspace has no extension for, so the
	// resolved set is empty while the recorded set is not: exactly the shape a
	// fresh worktree, an unmaterialized extension artifact or a failed discovery
	// produces.
	declareAdapterInputs(t, root, "package.json")

	output, err := captureStdout(t, func() error {
		return ProjectsSync(context.Background(), root, wsproto.Load(root),
			[]string{"--prune", "--skip-install"}, false, LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	if !strings.Contains(output, "--prune was ignored") {
		t.Errorf("the refusal was silent:\n%s", output)
	}
	if !strings.Contains(output, "@fixture/adapter") {
		t.Errorf("the refusal does not name the provider that went missing:\n%s", output)
	}

	membership := readText(t, filepath.Join(root, "putnami.workspace.json"))
	if !strings.Contains(membership, "apps/web") {
		t.Fatalf("the marker-discovered member was pruned by a scan that could not see it:\n%s", membership)
	}
	if !strings.Contains(membership, "tool") {
		t.Errorf("a putnami.json member was pruned too:\n%s", membership)
	}
	// The refusal is scoped to the prune: the additive half of the sync still ran.
	if strings.Contains(output, "0 added, 0 removed") && !strings.Contains(output, "in sync") {
		t.Logf("sync report:\n%s", output)
	}
}

// pruneAdapterManifest is a minimal local extension that declares a workspace
// adapter, so a fixture can express "an adapter resolved for this run" without
// installing anything.
const pruneAdapterManifest = `{
  "name": "@fixture/lang",
  "version": "0.1.0",
  "cliContract": 4,
  "workspace": {
    "markers": ["package.json"],
    "inputs": ["package.json"]
  }
}`

// The refusal must not become a blanket "never prune": a workspace whose scan is
// complete still prunes, or the flag stops working the moment anyone installs an
// extension.
//
// "Complete" means an adapter actually resolved. The fixture therefore declares
// one — a local extension whose manifest is right there in the tree — because a
// scan with NO adapter recognizes only putnami.json, which is exactly the blind
// scan the fresh-clone refusal below exists for.
func TestProjectsSync_PruneStillRemovesVanishedMembers(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"includes":["ext","tool","gone"]}`)
	writeProjectFile(t, filepath.Join(root, "ext", "putnami.json"), `{"name":"@fixture/lang"}`)
	writeProjectFile(t, filepath.Join(root, "ext", "putnami.extension.json"), pruneAdapterManifest)
	writeProjectFile(t, filepath.Join(root, "tool", "putnami.json"), `{"name":"tool"}`)
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	output, err := captureStdout(t, func() error {
		return ProjectsSync(context.Background(), root, wsproto.Load(root),
			[]string{"--prune", "--skip-install"}, false, LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}
	if strings.Contains(output, "--prune was ignored") {
		t.Fatalf("a complete scan was refused a prune:\n%s", output)
	}

	membership := readText(t, filepath.Join(root, "putnami.workspace.json"))
	if strings.Contains(membership, "gone") {
		t.Errorf("a member whose directory no longer exists was kept:\n%s", membership)
	}
	if !strings.Contains(membership, "tool") {
		t.Errorf("a live member was pruned:\n%s", membership)
	}
}

// THE fresh-clone case, and the one both existing arms miss.
//
// `.putnami/` is gitignored, so a fresh clone or a new worktree has NO
// workspace-index.json: the recorded-provider arm compares an empty recorded set
// against an empty resolved set and is vacuously satisfied. And discovery
// returns no error for an extension that is simply not installed — it is not
// even a skip, because there is no manifest anywhere to fail on. So the scan
// recognizes putnami.json and nothing else, and `--prune` reads every
// package.json / go.mod / pyproject.toml member as deleted.
func TestProjectsSync_PruneRefusesOnAFreshCloneWithNoAdapters(t *testing.T) {
	root := t.TempDir()
	// Exactly what `git clone` leaves behind: membership on disk, no `.putnami/`,
	// and an extension reference nothing has materialized yet.
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"includes":["tool","apps/web","svc"],"extensions":["@fixture/not-installed"]}`)
	writeProjectFile(t, filepath.Join(root, "tool", "putnami.json"), `{"name":"tool"}`)
	writeProjectFile(t, filepath.Join(root, "apps", "web", "package.json"), `{"name":"@acme/web"}`)
	writeProjectFile(t, filepath.Join(root, "svc", "go.mod"), "module acme/svc\n\ngo 1.25\n")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	output, err := captureStdout(t, func() error {
		return ProjectsSync(context.Background(), root, wsproto.Load(root),
			[]string{"--prune", "--skip-install"}, false, LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	if !strings.Contains(output, "--prune was ignored") {
		t.Errorf("a fresh clone with no adapters pruned silently:\n%s", output)
	}
	if !strings.Contains(output, "no workspace adapter resolved") {
		t.Errorf("the refusal does not name the cause:\n%s", output)
	}

	membership := readText(t, filepath.Join(root, "putnami.workspace.json"))
	for _, member := range []string{"tool", "apps/web", "svc"} {
		if !strings.Contains(membership, member) {
			t.Fatalf("%q was pruned on a fresh clone while its files are still on disk:\n%s", member, membership)
		}
	}
	// Still a refusal to PRUNE, not a refusal to sync.
	if strings.Contains(output, "Updated") == false && strings.Contains(output, "in sync") == false {
		t.Errorf("the additive half of the sync did not run:\n%s", output)
	}
}

// An adapter exclude stops the WALK, for every provider and for core's own
// marker. The scan therefore reports nothing about anything under an excluded
// directory — which is not the same fact as "there is nothing there", and
// `--prune` is the one caller for which the difference is destructive.
const excludingExtensionManifest = `{
  "name": "@fixture/lang",
  "version": "0.1.0",
  "cliContract": 4,
  "workspace": {
    "markers": ["package.json"],
    "inputs": ["package.json"],
    "excludes": ["build"]
  }
}`

func TestProjectsSync_PruneKeepsMembersInsideAnExcludedDirectory(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"includes":["ext","pkgs/build"]}`)
	writeProjectFile(t, filepath.Join(root, "ext", "putnami.json"), `{"name":"@fixture/lang"}`)
	writeProjectFile(t, filepath.Join(root, "ext", "putnami.extension.json"), excludingExtensionManifest)
	writeProjectFile(t, filepath.Join(root, "pkgs", "build", "putnami.json"), `{"name":"buildish"}`)

	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	if _, err := captureStdout(t, func() error {
		return ProjectsSync(context.Background(), root, wsproto.Load(root),
			[]string{"--prune", "--skip-install"}, false, LifecycleEnv{})
	}); err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	membership := readText(t, filepath.Join(root, "putnami.workspace.json"))
	if !strings.Contains(membership, "pkgs/build") {
		t.Fatalf("a declared member was pruned because an adapter excluded its directory name:\n%s", membership)
	}
}

// The name-divergence report compares a project's SOURCE identity to its
// resolved name — and without an adopted provider view the source identity is
// core's own directory-basename fallback, not what the manifest says. Every
// convention-named project then looks unaligned, so the load-time report is
// suppressed and what is printed instead says what it is worth.
func TestProjectsSync_DivergenceReportIsNotClaimedWithoutAProviderView(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["apps"]}`)
	writeProjectFile(t, filepath.Join(root, "apps", "putnami.json"),
		`{"includes":["web"],"publishConfig":{"npm":{"namePattern":"@acme/{name}"}}}`)
	// The manifest ALREADY spells the name the scope pattern resolves to, so
	// there is nothing to align — core just cannot know that without a probe.
	writeProjectFile(t, filepath.Join(root, "apps", "web", "package.json"), `{"name":"@acme/web"}`)

	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })

	output, err := captureStdout(t, func() error {
		return ProjectsSync(context.Background(), root, wsproto.Load(root),
			[]string{"--skip-install"}, false, LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("ProjectsSync: %v", err)
	}

	// The load-time report is suppressed, so core's own verdict is "nothing to
	// change" rather than a divergence it cannot substantiate…
	if !strings.Contains(output, "Putnami membership is in sync.") {
		t.Errorf("an un-probed divergence was counted as a membership finding:\n%s", output)
	}
	if strings.Contains(output, "names aligned") {
		t.Errorf("core still claims to have aligned names it no longer writes:\n%s", output)
	}
	// …and the line is still shown, labeled for exactly what it is worth.
	if strings.Contains(output, "web → @acme/web") && !strings.Contains(output, "unverified") {
		t.Errorf("an unverified divergence is presented as a confirmed finding:\n%s", output)
	}
}

// The scan's blind spot, stated directly: the predicate `--prune` consults.
func TestExcludedByProviderScopes_MatchesNestedSegments(t *testing.T) {
	scopes := []workspace.ProviderScope{
		mustScope(t, "@fixture/go", []string{"go.mod"}, []string{"go.mod"}, []string{"testdata", "vendor"}),
		mustScope(t, "@fixture/py", []string{"pyproject.toml"}, []string{"pyproject.toml"}, []string{"build"}),
	}
	for _, rel := range []string{"testdata", "go/framework/api/testdata", "x/vendor/y", "pkgs/build"} {
		if !workspace.ExcludedByProviderScopes(scopes, rel) {
			t.Errorf("%q is not reported as excluded", rel)
		}
	}
	for _, rel := range []string{"", "apps/web", "buildings", "testdatax"} {
		if workspace.ExcludedByProviderScopes(scopes, rel) {
			t.Errorf("%q is reported as excluded", rel)
		}
	}
}

func mustScope(t *testing.T, name string, markers, inputs, excludes []string) workspace.ProviderScope {
	t.Helper()
	scope, ok := workspace.NewProviderScope(name, &extproto.WorkspaceAdapter{
		Markers: markers, Inputs: inputs, Excludes: excludes,
	})
	if !ok {
		t.Fatalf("NewProviderScope(%q) rejected a valid adapter", name)
	}
	return scope
}
