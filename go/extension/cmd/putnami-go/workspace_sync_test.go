package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/toolchain"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

// THE MIGRATED REPLACE-CLOSURE GUARDS.
//
// These cases moved here verbatim in intent from
// tooling/cli/internal/workspace/gomod_closure_test.go. They are the reason the
// codemod exists: module-mode `go mod tidy` ignores go.work, so every workspace
// module in a member's TRANSITIVE require graph needs that member's own
// relative replace, and a missing one rots in silently until an offline build
// fails with a proxy 404 for a version that only ever existed in this
// repository.
//
// Retained, one test each: transitive requires, mixed local/module replace
// forms, deterministic byte-exact output, existing directives preserved
// verbatim, block-form replaces satisfying the closure, dry-run, idempotence,
// the no-go.work no-op, and the real-repository closure guard.

// closureWorkspace builds a temp workspace with three go.work members that
// exercise mixed directive styles and depths:
//
//	app (go/framework/app)         — single-line require of config, no replaces
//	config (go/framework/config)   — block require (// indirect) of the
//	                                 protocol, single-line replace already
//	                                 present (complete closure)
//	protocol (protocols/config)    — leaf, no requires
//
// The transitive chain app → config → protocol means app's closure must gain
// TWO replaces (config and protocol) even though app never requires the
// protocol directly, and the protocol target sits at ../../../protocols depth.
func closureWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()

	// go.work: block form AND single-line form, with a comment.
	writeProbeFile(t, filepath.Join(ws, "go.work"), `go 1.26

use (
	./go/framework/app
	./go/framework/config // a comment that must be stripped
)

use ./protocols/config
`)

	writeProbeFile(t, filepath.Join(ws, "go", "framework", "app", "go.mod"), `module example.com/app

go 1.26

require example.com/config v0.0.1
`)

	writeProbeFile(t, filepath.Join(ws, "go", "framework", "config", "go.mod"), `module example.com/config

go 1.26

require (
	example.com/protocol/config v0.0.0 // indirect
)

replace example.com/protocol/config => ../../../protocols/config
`)

	writeProbeFile(t, filepath.Join(ws, "protocols", "config", "go.mod"), `module example.com/protocol/config

go 1.26
`)

	return ws
}

func readModFile(t *testing.T, ws string, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws, filepath.Join(parts...), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	return string(data)
}

func TestSyncGoWorkspace_AddsMissingTransitiveReplaces(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "replace-closure", "sync-appends-the-missing-transitive-workspace-replaces")
	ws := closureWorkspace(t)
	configBefore := readModFile(t, ws, "go", "framework", "config")
	protocolBefore := readModFile(t, ws, "protocols", "config")

	changes, err := syncGoWorkspace(ws, false)
	if err != nil {
		t.Fatalf("syncGoWorkspace: %v", err)
	}

	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want exactly one (app)", changes)
	}
	if changes[0].Path != "go/framework/app/go.mod" {
		t.Errorf("Path = %q, want go/framework/app/go.mod", changes[0].Path)
	}
	if want := []string{"example.com/config", "example.com/protocol/config"}; !slices.Equal(changes[0].Added, want) {
		t.Errorf("Added = %v, want %v (sorted, transitive dep included)", changes[0].Added, want)
	}

	// Byte-exact output pins determinism: existing content untouched, sorted
	// single-line replaces appended, forward slashes, ../ depth correct.
	want := `module example.com/app

go 1.26

require example.com/config v0.0.1

replace example.com/config => ../config
replace example.com/protocol/config => ../../../protocols/config
`
	if got := readModFile(t, ws, "go", "framework", "app"); got != want {
		t.Errorf("app go.mod:\n%s\nwant:\n%s", got, want)
	}

	// Members with a complete closure must be byte-for-byte untouched.
	if got := readModFile(t, ws, "go", "framework", "config"); got != configBefore {
		t.Errorf("config go.mod was rewritten:\n%s", got)
	}
	if got := readModFile(t, ws, "protocols", "config"); got != protocolBefore {
		t.Errorf("protocol go.mod was rewritten:\n%s", got)
	}
}

// A run that changes nothing writes nothing. `projects sync` runs on every
// membership change, so a rewrite-always sync would move file mtimes — and
// every downstream cache key that observes them — on every invocation.
func TestSyncGoWorkspace_IsIdempotentAndDoesNotTouchUnchangedFiles(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "replace-closure", "a-second-run-changes-nothing")
	ws := closureWorkspace(t)
	if _, err := syncGoWorkspace(ws, false); err != nil {
		t.Fatalf("first run: %v", err)
	}
	appAfterFirst := readModFile(t, ws, "go", "framework", "app")

	pinned := time.Unix(1_700_000_000, 0)
	touched := []string{
		filepath.Join(ws, "go", "framework", "app", "go.mod"),
		filepath.Join(ws, "go", "framework", "config", "go.mod"),
		filepath.Join(ws, "protocols", "config", "go.mod"),
	}
	for _, path := range touched {
		if err := os.Chtimes(path, pinned, pinned); err != nil {
			t.Fatal(err)
		}
	}

	changes, err := syncGoWorkspace(ws, false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("second run changes = %+v, want none (idempotent)", changes)
	}
	if got := readModFile(t, ws, "go", "framework", "app"); got != appAfterFirst {
		t.Errorf("second run changed app go.mod:\n%s", got)
	}
	for _, path := range touched {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(pinned) {
			t.Errorf("%s was rewritten by a no-op sync", path)
		}
	}
}

// THE MALFORMED-MANIFEST IDEMPOTENCE GUARD.
//
// Idempotence must survive the very inputs the hand-rolled parser exists for. A
// go.mod caught mid-edit can carry a `require (` whose `)` never arrives; the
// parser is deliberately tolerant of that (see the HAND-ROLLED ON PURPOSE note
// in internal/toolchain/gomod.go) because a probe that refuses to read a
// half-written manifest makes the project vanish with no diagnostic.
//
// Tolerating it is not enough: before the block-recovery fix, the phantom
// require block swallowed every line after it, so the `replace` this very task
// had just appended was re-read as a require entry, ReplacedModules() came back
// empty, and the SAME directive was appended again on every run — three runs,
// three identical replace lines, a file growing without bound that `go mod
// tidy` cannot repair. Three runs is the minimum that separates "converged" from
// "grows by one each time".
func TestSyncGoWorkspace_IsIdempotentOnAnUnterminatedRequireBlock(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "replace-closure", "an-unterminated-require-block-stays-idempotent-too")
	ws := t.TempDir()
	writeProbeFile(t, filepath.Join(ws, "go.work"), "go 1.26\n\nuse (\n\t./a\n\t./b\n)\n")
	// a/go.mod's require block is never closed — the mid-edit state.
	writeProbeFile(t, filepath.Join(ws, "a", "go.mod"),
		"module example.com/a\n\ngo 1.26\n\nrequire (\n\texample.com/b v0.0.0\n")
	writeProbeFile(t, filepath.Join(ws, "b", "go.mod"), "module example.com/b\n\ngo 1.26\n")

	changes, err := syncGoWorkspace(ws, false)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(changes) != 1 || !slices.Equal(changes[0].Added, []string{"example.com/b"}) {
		t.Fatalf("first run changes = %+v, want a's missing replace for example.com/b", changes)
	}
	afterFirst := readModFile(t, ws, "a")

	for run := 2; run <= 3; run++ {
		changes, err := syncGoWorkspace(ws, false)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if len(changes) != 0 {
			t.Errorf("run %d changes = %+v, want none (already converged)", run, changes)
		}
		if got := readModFile(t, ws, "a"); got != afterFirst {
			t.Fatalf("run %d rewrote a/go.mod:\n%s\nwant byte-identical to run 1:\n%s", run, got, afterFirst)
		}
	}

	if n := strings.Count(readModFile(t, ws, "a"), "replace example.com/b => ../b\n"); n != 1 {
		t.Errorf("a/go.mod carries %d copies of the replace directive, want exactly 1:\n%s",
			n, readModFile(t, ws, "a"))
	}
}

func TestSyncGoWorkspace_DryRunWritesNothing(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "replace-closure", "a-dry-run-writes-nothing")
	ws := closureWorkspace(t)
	before := readModFile(t, ws, "go", "framework", "app")

	changes, err := syncGoWorkspace(ws, true)
	if err != nil {
		t.Fatalf("syncGoWorkspace dry-run: %v", err)
	}
	if len(changes) != 1 || len(changes[0].Added) != 2 {
		t.Fatalf("dry-run changes = %+v, want the same report as a real run", changes)
	}
	if got := readModFile(t, ws, "go", "framework", "app"); got != before {
		t.Errorf("dry-run modified app go.mod:\nbefore=%q\nafter=%q", before, got)
	}
}

// A replace with no matching require — e.g. a manually added
// `replace example.com/schema => ../schema` — must be preserved verbatim: the
// codemod only ever appends, never prunes.
func TestSyncGoWorkspace_PreservesExtraReplace(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "replace-closure", "an-existing-unrelated-replace-is-preserved")
	ws := t.TempDir()
	writeProbeFile(t, filepath.Join(ws, "go.work"), "go 1.26\n\nuse (\n\t./a\n\t./b\n)\n")
	writeProbeFile(t, filepath.Join(ws, "a", "go.mod"), `module example.com/a

go 1.26

require example.com/b v0.0.0

replace (
	example.com/schema => ../schema
)
`)
	writeProbeFile(t, filepath.Join(ws, "b", "go.mod"), "module example.com/b\n\ngo 1.26\n")

	changes, err := syncGoWorkspace(ws, false)
	if err != nil {
		t.Fatalf("syncGoWorkspace: %v", err)
	}
	if len(changes) != 1 || !slices.Equal(changes[0].Added, []string{"example.com/b"}) {
		t.Fatalf("changes = %+v, want a's missing replace for example.com/b", changes)
	}

	want := `module example.com/a

go 1.26

require example.com/b v0.0.0

replace (
	example.com/schema => ../schema
)

replace example.com/b => ../b
`
	if got := readModFile(t, ws, "a"); got != want {
		t.Errorf("a go.mod:\n%s\nwant:\n%s", got, want)
	}
}

// A replace already present in a `replace (...)` block satisfies the closure:
// only the genuinely missing directive is appended.
func TestSyncGoWorkspace_BlockReplaceSatisfiesClosure(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "replace-closure", "a-block-form-replace-already-satisfies-the-closure")
	ws := t.TempDir()
	writeProbeFile(t, filepath.Join(ws, "go.work"), "go 1.26\n\nuse (\n\t./a\n\t./b\n\t./c\n)\n")
	writeProbeFile(t, filepath.Join(ws, "a", "go.mod"), `module example.com/a

go 1.26

require (
	example.com/b v0.0.0
	example.com/c v0.0.0 // indirect
)

replace (
	example.com/b => ../b
)
`)
	writeProbeFile(t, filepath.Join(ws, "b", "go.mod"), "module example.com/b\n\ngo 1.26\n")
	writeProbeFile(t, filepath.Join(ws, "c", "go.mod"), "module example.com/c\n\ngo 1.26\n")

	changes, err := syncGoWorkspace(ws, false)
	if err != nil {
		t.Fatalf("syncGoWorkspace: %v", err)
	}
	if len(changes) != 1 || !slices.Equal(changes[0].Added, []string{"example.com/c"}) {
		t.Fatalf("changes = %+v, want only example.com/c (b's block replace already present)", changes)
	}
	got := readModFile(t, ws, "a")
	if want := "replace example.com/c => ../c\n"; !strings.HasSuffix(got, want) {
		t.Errorf("a go.mod should end with %q:\n%s", want, got)
	}
}

func TestSyncGoWorkspace_NoGoWorkIsNoOp(t *testing.T) {
	ws := t.TempDir()
	changes, err := syncGoWorkspace(ws, false)
	if err != nil {
		t.Fatalf("syncGoWorkspace without go.work: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none for a workspace without go.work", changes)
	}
}

// TestRepoGoModReplaceClosureComplete pins the real repository's replace
// closure without invoking cmd/go, accessing the network, or depending on a
// populated module cache. A dry-run over the actual go.work must report nothing
// missing. It fails as soon as a module gains a workspace dependency without
// the matching replace; the fix is `putnami projects sync`.
func TestRepoGoModReplaceClosureComplete(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "replace-closure", "the-committed-repository-closure-is-complete")
	root, ok := repoRootFromTest()
	if !ok {
		t.Skip("repo root (go.work) not found walking up from test dir; out-of-repo test run")
	}
	rels, err := toolchain.ParseGoWorkUses(filepath.Join(root, "go.work"))
	if err != nil {
		t.Fatalf("parse repo go.work: %v", err)
	}
	if len(rels) == 0 {
		t.Fatal("repo go.work has no use directives")
	}
	for _, rel := range rels {
		mod, readErr := toolchain.ReadGoMod(filepath.Join(root, filepath.FromSlash(rel), "go.mod"))
		if readErr != nil || mod == nil {
			t.Errorf("go.work member %s has no readable go.mod: %v", rel, readErr)
			continue
		}
		if mod.Module == "" {
			t.Errorf("go.work member %s/go.mod declares no module path", rel)
		}
	}
	changes, err := syncGoWorkspace(root, true)
	if err != nil {
		t.Fatalf("syncGoWorkspace dry-run on repo: %v", err)
	}
	for _, change := range changes {
		t.Errorf("%s is missing workspace replaces for %v — run `putnami projects sync`", change.Path, change.Added)
	}
}

// The real-repository tests above read go.work and every member's go.mod. Each
// of those files is a declared test input of this project, so editing one
// moves the test task's cache key: a fixed go.mod re-runs the suite instead of
// replaying the verdict recorded before the fix.
func TestRepositoryGoModulesAreDeclaredTestInputs(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "repository-reads-are-declared-inputs", "every-go-mod-the-repository-tests-read-is-a-declared-test-input")
	root, ok := repoRootFromTest()
	if !ok {
		t.Skip("repo root (go.work) not found walking up from test dir; out-of-repo test run")
	}
	projectRoot := filepath.Join(root, "go", "extension")
	patterns := declaredTestInputs(t, projectRoot)
	rels, err := toolchain.ParseGoWorkUses(filepath.Join(root, "go.work"))
	if err != nil {
		t.Fatalf("parse repo go.work: %v", err)
	}
	read := make([]string, 0, len(rels)+1)
	read = append(read, "go.work")
	for _, rel := range rels {
		read = append(read, filepath.ToSlash(filepath.Join(filepath.FromSlash(rel), "go.mod")))
	}
	for _, file := range read {
		fromProject, err := filepath.Rel(projectRoot, filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		if !wsproto.SelectsPath(filepath.ToSlash(fromProject), patterns) {
			t.Errorf("%s is read by this project's tests but is not a declared test input (options.test.filePatterns in go/extension/putnami.json)", file)
		}
	}
}

// declaredTestInputs returns the test filePatterns the project at projectRoot
// declares in its putnami.json.
func declaredTestInputs(t *testing.T, projectRoot string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectRoot, "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Options struct {
			Test struct {
				FilePatterns []string `json:"filePatterns"`
			} `json:"test"`
		} `json:"options"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse %s/putnami.json: %v", projectRoot, err)
	}
	return config.Options.Test.FilePatterns
}

// --- task wiring --------------------------------------------------------

// A workspace-sync that ran without the resolved selection must stop, loudly,
// having written nothing. The activation that delivers the selection is pinned
// alongside it: either half alone leaves a task mutating go.mod files while
// reporting on a membership nobody supplied.
func TestRunWorkspaceSync_FailsClosedWithoutASelection(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "replace-closure", "a-run-with-no-resolved-project-selection-fails-closed")
	ws := closureWorkspace(t)
	before := readModFile(t, ws, "go", "framework", "app")

	status, _, err := runWorkspaceSync(&pctx.Context{WorkspaceRoot: ws}, jsonl.New(), nil)
	if err == nil || status != "FAILED" {
		t.Fatalf("runWorkspaceSync = (%q, %v), want a loud failure on an unknown membership", status, err)
	}
	if after := readModFile(t, ws, "go", "framework", "app"); after != before {
		t.Fatalf("the failing task still wrote a go.mod:\n%s", after)
	}
}

func TestManifest_WorkspaceSyncRunsOncePerWorkspace(t *testing.T) {
	m := loadExtensionManifest(t)

	command, ok := m.Commands["workspace-sync"]
	if !ok {
		t.Fatal(`manifest lost the "workspace-sync" command`)
	}
	// Without workspace-once activation the command is planned per project and
	// the planner attaches no resolved selection, so the task would fail closed
	// on every run.
	if command.Activation != "workspace-once" {
		t.Fatalf(`workspace-sync activation = %q, want "workspace-once"`, command.Activation)
	}
	if m.Workspace == nil || m.Workspace.SyncTask == "" {
		t.Fatal("the workspace adapter no longer names a syncTask; core would keep writing go.mod itself")
	}
	if _, ok := m.Tasks[m.Workspace.SyncTask]; !ok {
		t.Fatalf("workspace.syncTask %q is not defined in tasks", m.Workspace.SyncTask)
	}
}

// Every MARKER must also be an INPUT: the probe reads the marker's content, so
// editing it has to invalidate the workspace snapshot.
//
// `**/*.go` is the classification witness. Classification reads every Go
// source package clause under the module, so the witness has to cover the
// matched SET recursively: editing, creating, or deleting the first package
// main must invalidate the recorded provider answer before core adopts its
// Type. The generic workspace snapshot contract gives recursive globs those
// set-invalidation semantics; this remains provider-declared rather than
// teaching core which source files decide a Go project's type.
//
// `go.work` joins those inputs. Two things read go.work on this side — the
// sync task's closure walk and its unmanaged-module report — and since slice
// C4b core's install-state fingerprint is assembled from the adapters' declared
// ROOT-level inputs instead of a hard-coded list of language filenames, so
// dropping it would stop `putnami install` from noticing that the Go workspace
// membership moved. `go work use` is a rare edit and `go mod tidy` does not touch
// go.work, so the extra re-probe it can cause is bounded.
//
// `go.sum` and `go.work.sum` stay out, and that exclusion is load-bearing:
// ordinary build and tidy flows rewrite them constantly, and hashing them here
// would re-probe the Go provider — and re-run the first-use install check — on
// every build.
func TestManifest_WorkspaceAdapterDeclaresExactlyWhatTheProbeReads(t *testing.T) {
	m := loadExtensionManifest(t)
	if m.Workspace == nil {
		t.Fatal("the manifest declares no workspace adapter")
	}
	if !slices.Equal(m.Workspace.Markers, []string{goWorkspaceMarker}) {
		t.Errorf("markers = %v, want [%s]", m.Workspace.Markers, goWorkspaceMarker)
	}
	if !slices.Equal(m.Workspace.Inputs, []string{goWorkspaceMarker, goClassificationInput, "go.work"}) {
		t.Errorf("inputs = %v, want exactly [%s %s go.work]", m.Workspace.Inputs,
			goWorkspaceMarker, goClassificationInput)
	}
	for _, forbidden := range []string{"go.sum", "go.work.sum"} {
		if slices.Contains(m.Workspace.Inputs, forbidden) {
			t.Errorf("inputs = %v, want %q excluded: ordinary build flows rewrite it, and hashing it "+
				"would re-probe on every build", m.Workspace.Inputs, forbidden)
		}
	}
	for _, want := range []string{"testdata", "vendor"} {
		if !slices.Contains(m.Workspace.Excludes, want) {
			t.Errorf("excludes = %v, want it to contain %q: a go.mod under it is a fixture or a vendored "+
				"copy, not a workspace project", m.Workspace.Excludes, want)
		}
	}
}

// --- go.work coverage ---------------------------------------------------

// The resolved selection is what makes a Go project missing from go.work
// visible: the closure walks go.work, so such a project is invisible to it and
// its module-mode tidy keeps failing with no explanation.
func TestUnmanagedGoProjects_ReportsSelectedModulesMissingFromGoWork(t *testing.T) {
	ws := closureWorkspace(t)
	writeProbeFile(t, filepath.Join(ws, "svc", "go.mod"), "module example.com/svc\n\ngo 1.26\n")

	got := unmanagedGoProjects(ws, []pctx.ProjectRef{
		{Name: "svc", Path: "svc"},
		{Name: "app", Path: "go/framework/app"},
		{Name: "web", Path: "web"},
		{Name: "root", Path: "."},
	})
	if !slices.Equal(got, []string{"svc"}) {
		t.Errorf("unmanaged = %v, want [svc] (go.work members and non-Go directories excluded)", got)
	}
}

// A workspace with no go.work at all is an ordinary state — a repository with
// no Go modules, or one that has not run `deps install` yet. Flagging every Go
// project in it would be noise, not a finding.
func TestUnmanagedGoProjects_NoGoWorkReportsNothing(t *testing.T) {
	ws := t.TempDir()
	writeProbeFile(t, filepath.Join(ws, "svc", "go.mod"), "module example.com/svc\n\ngo 1.26\n")

	if got := unmanagedGoProjects(ws, []pctx.ProjectRef{{Name: "svc", Path: "svc"}}); len(got) != 0 {
		t.Errorf("unmanaged = %v, want none without a go.work", got)
	}
}

// The tidy pass is best effort: `projects sync` is the repair command, so a
// module whose external dependencies need an unreachable proxy must produce a
// warning, not a failed sync.
func TestSettleGoModules_TidyFailureWarnsAndContinues(t *testing.T) {
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	original := goModTidy
	t.Cleanup(func() { goModTidy = original })

	var tidied []string
	goModTidy = func(_, dir string) ([]byte, error) {
		tidied = append(tidied, filepath.Base(dir))
		if filepath.Base(dir) == "broken" {
			return []byte("go: example.com/x@v1: 404"), os.ErrInvalid
		}
		return nil, nil
	}

	warnings := settleGoModules(t.TempDir(), []workspaceSyncChange{
		{Path: "ok/go.mod", Field: "replace", Added: []string{"example.com/a"}},
		{Path: "broken/go.mod", Field: "replace", Added: []string{"example.com/b"}},
	})
	if !slices.Equal(tidied, []string{"ok", "broken"}) {
		t.Errorf("tidied = %v, want both changed modules in report order", tidied)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "broken") {
		t.Errorf("warnings = %v, want exactly the failing module", warnings)
	}
}

// TestSyncGoWorkspace_ProducesTheCorpusEdits is the NEW side of the
// native-manifest-edit equivalence. Its twin,
// TestCoreClosureProducesTheCorpusEdits in
// tooling/cli/internal/workspace/go_probe_equivalence_test.go, runs the SAME
// fixtures through the CLI codemod being retired, and both assert the same
// byte-exact output.
func TestSyncGoWorkspace_ProducesTheCorpusEdits(t *testing.T) {
	if got := goClosureDigest(goClosureCorpus()); got != goClosureFingerprint {
		t.Fatalf("closure corpus fingerprint = %q, want %q\n"+
			"apply the SAME edit to its twin in "+
			"tooling/cli/internal/workspace/go_probe_equivalence_test.go and update "+
			"goClosureFingerprint in both files", got, goClosureFingerprint)
	}

	for _, c := range goClosureCorpus() {
		t.Run(c.Name, func(t *testing.T) {
			ws := t.TempDir()
			for rel, content := range c.Files {
				writeProbeFile(t, filepath.Join(ws, filepath.FromSlash(rel)), content)
			}

			changes, err := syncGoWorkspace(ws, false)
			if err != nil {
				t.Fatalf("syncGoWorkspace: %v", err)
			}
			added := make(map[string][]string, len(changes))
			for _, change := range changes {
				added[strings.TrimSuffix(change.Path, "/go.mod")] = change.Added
			}
			if len(added) != len(c.Added) {
				t.Fatalf("changed modules = %v, want %v", added, c.Added)
			}
			for dir, want := range c.Added {
				if !slices.Equal(added[dir], want) {
					t.Errorf("%s added = %v, want %v", dir, added[dir], want)
				}
			}

			for rel, want := range c.Want {
				data, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(rel)))
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != want {
					t.Errorf("%s:\n%s\nwant:\n%s", rel, data, want)
				}
			}
		})
	}
}
