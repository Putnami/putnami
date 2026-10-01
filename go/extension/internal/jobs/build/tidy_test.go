package build

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

// --- snapshotModFiles / restoreModFiles ---
//
// build-tidy's cache key hashes go.mod/go.sum, the very files `go mod tidy`
// rewrites. These tests pin the rollback invariants: a failed tidy must never
// leave the cache-key inputs churned.

func writeTidyFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func readTidyFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func TestRestoreModFiles_RestoresMutatedContent(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "modfile-restoration", "mutated-mod-file-content-is-restored")
	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod", "module example.com/app\n\ngo 1.24\n")
	writeTidyFile(t, dir, "go.sum", "example.com/dep v1.0.0 h1:abc\n")

	snap := snapshotModFiles(dir)

	// Simulate a failed tidy that partially rewrote both files.
	writeTidyFile(t, dir, "go.mod", "module example.com/app\n\ngo 1.24\n\nrequire example.com/dep v1.1.0\n")
	writeTidyFile(t, dir, "go.sum", "")

	changed, err := restoreModFiles(dir, snap)
	if err != nil {
		t.Fatalf("restoreModFiles: %v", err)
	}
	if got := readTidyFile(t, dir, "go.mod"); got != "module example.com/app\n\ngo 1.24\n" {
		t.Errorf("go.mod not restored, got %q", got)
	}
	if got := readTidyFile(t, dir, "go.sum"); got != "example.com/dep v1.0.0 h1:abc\n" {
		t.Errorf("go.sum not restored, got %q", got)
	}
	if !slices.Contains(changed, "go.mod") || !slices.Contains(changed, "go.sum") {
		t.Errorf("changed = %v, want both go.mod and go.sum", changed)
	}
}

func TestRestoreModFiles_DeletesFileCreatedAfterSnapshot(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "modfile-restoration", "a-go-sum-the-run-created-is-deleted")
	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod", "module example.com/app\n")
	// No go.sum at snapshot time: must be handled without error and
	// recorded as absent (distinct from empty).
	snap := snapshotModFiles(dir)

	// Simulate a failed tidy that newly created go.sum.
	writeTidyFile(t, dir, "go.sum", "example.com/dep v1.0.0 h1:abc\n")

	changed, err := restoreModFiles(dir, snap)
	if err != nil {
		t.Fatalf("restoreModFiles: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "go.sum")); !os.IsNotExist(statErr) {
		t.Errorf("go.sum should have been deleted (absent in snapshot), stat err = %v", statErr)
	}
	if got := readTidyFile(t, dir, "go.mod"); got != "module example.com/app\n" {
		t.Errorf("go.mod changed unexpectedly, got %q", got)
	}
	if !slices.Equal(changed, []string{"go.sum"}) {
		t.Errorf("changed = %v, want [go.sum]", changed)
	}
}

func TestRestoreModFiles_DeletesGoModCreatedAfterSnapshot(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "modfile-restoration", "a-go-mod-the-run-created-is-deleted")
	dir := t.TempDir()
	// Neither file exists at snapshot time.
	snap := snapshotModFiles(dir)

	writeTidyFile(t, dir, "go.mod", "module example.com/app\n")

	changed, err := restoreModFiles(dir, snap)
	if err != nil {
		t.Fatalf("restoreModFiles: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); !os.IsNotExist(statErr) {
		t.Errorf("go.mod should have been deleted (absent in snapshot), stat err = %v", statErr)
	}
	if !slices.Equal(changed, []string{"go.mod"}) {
		t.Errorf("changed = %v, want [go.mod]", changed)
	}
}

// --- tidyAddedWorkspaceRequirements ---
//
// The release-set probe derives a member's dependencies from the COMMITTED
// go.mod. `go mod tidy` requires what test files import too, so a test-only
// import of a sibling module grows go.mod during build and the plan no longer
// matches. These tests pin that the build names such additions.

func TestTidyAddedWorkspaceRequirements_ReportsATestOnlyWorkspaceImport(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "tidy-drift-visibility", "a-test-only-workspace-import-is-reported")
	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod", "module example.com/openapi\n\ngo 1.25\n\nrequire example.com/app v0.0.1\n")
	snap := snapshotModFiles(dir)

	// Simulate a successful tidy after openapi_test.go imported
	// example.com/security for the first time.
	writeTidyFile(t, dir, "go.mod",
		"module example.com/openapi\n\ngo 1.25\n\nrequire (\n\texample.com/app v0.0.1\n\texample.com/security v0.1.0\n)\n")

	added := tidyAddedWorkspaceRequirements(snap, dir, []string{"example.com/app", "example.com/openapi", "example.com/security"})
	if !slices.Equal(added, []string{"example.com/security"}) {
		t.Fatalf("added = %v, want [example.com/security]", added)
	}
	message := tidyDriftMessage(dir, added)
	for _, want := range []string{"example.com/security", "example.com/openapi", "commit go.mod", "putnami projects sync", "package~go"} {
		if !strings.Contains(message, want) {
			t.Errorf("diagnostic %q does not mention %q", message, want)
		}
	}
}

func TestTidyAddedWorkspaceRequirements_IgnoresExternalRequirements(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "tidy-drift-visibility", "an-external-requirement-is-not-reported")
	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod", "module example.com/openapi\n\ngo 1.25\n")
	snap := snapshotModFiles(dir)

	// An external module pulled in by tidy is resolvable from any proxy and
	// never enters the release set, so it is not drift the plan can miss.
	writeTidyFile(t, dir, "go.mod", "module example.com/openapi\n\ngo 1.25\n\nrequire github.com/google/uuid v1.6.0\n")

	if added := tidyAddedWorkspaceRequirements(snap, dir, []string{"example.com/openapi", "example.com/security"}); added != nil {
		t.Fatalf("added = %v, want nil", added)
	}
}

func TestTidyAddedWorkspaceRequirements_IsSilentWhenGoModIsUnchanged(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "tidy-drift-visibility", "an-unchanged-go-mod-is-silent")
	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod", "module example.com/openapi\n\ngo 1.25\n\nrequire example.com/security v0.0.1\n")
	snap := snapshotModFiles(dir)

	// Tidy touched only go.sum: the require set the probe saw is intact.
	writeTidyFile(t, dir, "go.sum", "example.com/security v0.0.1 h1:abc\n")

	if added := tidyAddedWorkspaceRequirements(snap, dir, []string{"example.com/openapi", "example.com/security"}); added != nil {
		t.Fatalf("added = %v, want nil", added)
	}
	// No go.mod at snapshot time: nothing to compare against, never a report.
	empty := t.TempDir()
	snap = snapshotModFiles(empty)
	writeTidyFile(t, empty, "go.mod", "module example.com/new\n\nrequire example.com/security v0.0.1\n")
	if added := tidyAddedWorkspaceRequirements(snap, empty, []string{"example.com/security"}); added != nil {
		t.Fatalf("added = %v, want nil for an unobserved go.mod", added)
	}
}

func TestRestoreModFiles_RecreatesFileDeletedAfterSnapshot(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "modfile-restoration", "a-file-the-run-removed-is-recreated")
	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod", "module example.com/app\n")
	writeTidyFile(t, dir, "go.sum", "example.com/dep v1.0.0 h1:abc\n")
	snap := snapshotModFiles(dir)

	// Simulate a failed tidy that removed go.sum.
	if err := os.Remove(filepath.Join(dir, "go.sum")); err != nil {
		t.Fatalf("remove go.sum: %v", err)
	}

	changed, err := restoreModFiles(dir, snap)
	if err != nil {
		t.Fatalf("restoreModFiles: %v", err)
	}
	if got := readTidyFile(t, dir, "go.sum"); got != "example.com/dep v1.0.0 h1:abc\n" {
		t.Errorf("go.sum not recreated, got %q", got)
	}
	if !slices.Equal(changed, []string{"go.sum"}) {
		t.Errorf("changed = %v, want [go.sum]", changed)
	}
}

func TestRestoreModFiles_NoChangeIsNoOp(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "modfile-restoration", "an-untouched-tree-is-left-alone")
	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod", "module example.com/app\n")
	writeTidyFile(t, dir, "go.sum", "example.com/dep v1.0.0 h1:abc\n")
	snap := snapshotModFiles(dir)

	changed, err := restoreModFiles(dir, snap)
	if err != nil {
		t.Fatalf("restoreModFiles: %v", err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none (nothing mutated)", changed)
	}
	if got := readTidyFile(t, dir, "go.mod"); got != "module example.com/app\n" {
		t.Errorf("go.mod mutated by no-op restore, got %q", got)
	}
	if got := readTidyFile(t, dir, "go.sum"); got != "example.com/dep v1.0.0 h1:abc\n" {
		t.Errorf("go.sum mutated by no-op restore, got %q", got)
	}
}

func TestSnapshotModFiles_EmptyDistinctFromAbsent(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "modfile-restoration", "an-empty-file-is-distinguished-from-an-absent-one")
	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod", "module example.com/app\n")
	writeTidyFile(t, dir, "go.sum", "") // present but empty
	snap := snapshotModFiles(dir)

	state, ok := snap.files["go.sum"]
	if !ok || !state.exists {
		t.Fatalf("empty go.sum recorded as absent, want present with empty content")
	}
	if len(state.content) != 0 {
		t.Errorf("empty go.sum content = %q, want empty", state.content)
	}

	// A failed tidy fills the empty go.sum; restore must truncate it back,
	// not delete it.
	writeTidyFile(t, dir, "go.sum", "example.com/dep v1.0.0 h1:abc\n")
	changed, err := restoreModFiles(dir, snap)
	if err != nil {
		t.Fatalf("restoreModFiles: %v", err)
	}
	if got := readTidyFile(t, dir, "go.sum"); got != "" {
		t.Errorf("go.sum = %q, want empty (present-but-empty in snapshot)", got)
	}
	if !slices.Equal(changed, []string{"go.sum"}) {
		t.Errorf("changed = %v, want [go.sum]", changed)
	}
}

func TestRunTidyFailureReportsSkip(t *testing.T) {
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	// A proxy the phase runs against, whatever the host exports: an inherited
	// GOPROXY=off is the configuration case, which runs no command at all.
	t.Setenv("GOPROXY", "file://"+filepath.ToSlash(t.TempDir()))

	dir := t.TempDir()
	writeTidyFile(t, dir, "go.mod", "not valid go.mod\n")

	status, _, err := Run(&pctx.Context{
		Project: pctx.Project{Name: "badmod", FullPath: dir},
	}, jsonl.New(), []string{"--phase", "tidy"})
	if err != nil {
		t.Fatalf("Run tidy: %v", err)
	}
	if status != "SKIP" {
		t.Fatalf("status = %q, want SKIP for failed tidy so it is not cached as success", status)
	}
}

// --- tidy failure visibility ---
//
// A failed tidy used to emit only a warn-level log, suppressed at default
// verbosity, so a rotten replace closure silently re-ran multi-second network
// round trips on every build. It must surface as a "warning" diagnostic
// (canonical DiagnosticSeverity, not "warn") that names the unresolvable
// workspace modules and points at `putnami projects sync`.

// tidyWorkspace builds a workspace root with a go.work covering dep and app,
// where app requires the workspace module example.com/dep without the replace
// that module-mode tidy needs.
func tidyWorkspace(t *testing.T) (root, appDir string) {
	t.Helper()
	root = t.TempDir()
	writeTidyFile(t, root, "go.work", "go 1.24\n\nuse (\n\t./dep\n\t./app\n)\n")

	depDir := filepath.Join(root, "dep")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTidyFile(t, depDir, "go.mod", "module example.com/dep\n\ngo 1.24\n")

	appDir = filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTidyFile(t, appDir, "go.mod", "module example.com/app\n\ngo 1.24\n\nrequire example.com/dep v0.0.0\n")
	writeTidyFile(t, appDir, "main.go", "package main\n\nimport _ \"example.com/dep\"\n\nfunc main() {}\n")
	return root, appDir
}

func TestTidyFailureMessage_WorkspaceDepsPointAtProjectsSync(t *testing.T) {
	root, appDir := tidyWorkspace(t)

	output := "go: example.com/dep@v0.0.0: module lookup disabled by GOPROXY=off\n"
	msg := tidyFailureMessage(root, appDir, output)

	for _, want := range []string{"example.com/app", "[example.com/dep]", "putnami projects sync"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q should contain %q", msg, want)
		}
	}
}

func TestTidyFailureMessage_NonWorkspaceFailureCarriesOutput(t *testing.T) {
	root, appDir := tidyWorkspace(t)

	output := "go: example.com/other@v1.2.3: module lookup disabled by GOPROXY=off\n"
	msg := tidyFailureMessage(root, appDir, output)

	if !strings.Contains(msg, "example.com/app") || !strings.Contains(msg, "example.com/other") {
		t.Errorf("message %q should name the module and carry the tidy output", msg)
	}
	if strings.Contains(msg, "projects sync") {
		t.Errorf("message %q should not suggest projects sync for a non-workspace failure", msg)
	}
}

// moduleRoot resolves the directory each spawned `go` subprocess (tidy, compile,
// cross-compile) resolves GOWORK against. It must prefer Project.FullPath and
// fall back to WorkspaceRoot/Project.Path so the leaked-GOWORK guard always
// targets the real module root.
func TestModuleRoot(t *testing.T) {
	full := &pctx.Context{Project: pctx.Project{FullPath: "/abs/module"}}
	if got := moduleRoot(full); got != "/abs/module" {
		t.Errorf("moduleRoot(FullPath) = %q, want /abs/module", got)
	}

	fallback := &pctx.Context{
		WorkspaceRoot: "/ws",
		Project:       pctx.Project{Path: "go/extension"},
	}
	if got, want := moduleRoot(fallback), filepath.Join("/ws", "go/extension"); got != want {
		t.Errorf("moduleRoot(fallback) = %q, want %q", got, want)
	}
}

func TestUnresolvableWorkspaceModules_ExcludesSelf(t *testing.T) {
	output := "go: example.com/app imports\n\texample.com/dep: cannot find module: module lookup disabled\n"
	got := unresolvableWorkspaceModules(output, []string{"example.com/dep", "example.com/app"}, "example.com/app")
	if !slices.Equal(got, []string{"example.com/dep"}) {
		t.Errorf("missing = %v, want [example.com/dep] (self excluded)", got)
	}
}

// End to end: a tidy phase that RAN and failed on an unresolvable workspace dep
// keeps the SKIP status and emits the warning diagnostic naming the dep.
//
// SKIP is what keeps such a failure out of the cache: build-tidy declares no
// `cache.deterministic`, and the orchestrator caches a skip only for a task that
// does (isCacheableResult, tooling/cli/internal/jobs/executor.go). The proxy
// here is an empty directory rather than `off`, so the failure is the transient
// shape — a proxy that answers nothing — and not the configuration one.
func TestRunTidyFailure_EmitsWarningDiagnostic(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "offline-tidy-is-a-cacheable-no-op",
		"a-tidy-that-ran-and-failed-stays-a-skip")
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	root, appDir := tidyWorkspace(t)
	// Force module mode so the missing replace fails fast, and point the proxy
	// at an empty directory so nothing resolves without reaching the network.
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "file://"+filepath.ToSlash(t.TempDir()))
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "")

	var status string
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		var err error
		status, _, err = Run(&pctx.Context{
			WorkspaceRoot: root,
			Project:       pctx.Project{Name: "app", FullPath: appDir},
		}, emit, []string{"--phase", "tidy"})
		if err != nil {
			t.Errorf("Run tidy: %v", err)
		}
	})

	if status != "SKIP" {
		t.Fatalf("status = %q, want SKIP (snapshot/restore semantics unchanged)", status)
	}
	var diag map[string]any
	for _, ev := range events {
		if ev["type"] == "diagnostic" {
			diag = ev
			break
		}
	}
	if diag == nil {
		t.Fatalf("no diagnostic event emitted, got %+v", events)
	}
	if sev, _ := diag["severity"].(string); sev != "warning" {
		t.Errorf("severity = %q, want canonical %q", sev, "warning")
	}
	msg, _ := diag["message"].(string)
	for _, want := range []string{"example.com/dep", "putnami projects sync"} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic %q should contain %q", msg, want)
		}
	}
}

// End to end: with module downloads disabled by configuration, the tidy phase
// runs no command at all and reports a no-op the cache can serve.
//
// The fixture is the one the test above fails on: `app` requires the workspace
// module `example.com/dep` with no replace, so a `go mod tidy` that ran here
// could only fail. An OK with no diagnostic is therefore proof that no command
// was spawned, and the modfile comparison proves the phase left this task's own
// cache-key inputs alone.
func TestRunTidy_DisabledModuleDownloadsRunNoCommand(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "offline-tidy-is-a-cacheable-no-op",
		"disabled-module-downloads-run-no-tidy-command")
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	root, appDir := tidyWorkspace(t)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "")

	before := readTidyFile(t, appDir, "go.mod")

	var status string
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		var err error
		status, _, err = Run(&pctx.Context{
			WorkspaceRoot: root,
			Project:       pctx.Project{Name: "app", FullPath: appDir},
		}, emit, []string{"--phase", "tidy"})
		if err != nil {
			t.Errorf("Run tidy: %v", err)
		}
	})

	if status != "OK" {
		t.Fatalf("status = %q, want OK: a no-op the cache serves, not a skip recomputed on every build", status)
	}
	for _, ev := range events {
		if ev["type"] == "diagnostic" {
			t.Fatalf("a diagnostic was emitted, so tidy ran after all: %+v", ev)
		}
	}
	var explained bool
	for _, ev := range events {
		if ev["type"] != "log" {
			continue
		}
		if msg, _ := ev["message"].(string); strings.Contains(msg, "GOPROXY=off") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("no log explains why tidy did not run, got %+v", events)
	}
	if got := readTidyFile(t, appDir, "go.mod"); got != before {
		t.Errorf("go.mod = %q, want the committed content %q", got, before)
	}
	if _, err := os.Stat(filepath.Join(appDir, "go.sum")); !os.IsNotExist(err) {
		t.Errorf("go.sum stat = %v, want the file the phase found: absent", err)
	}
}

// On a hosted run the offline signal alone turns module downloads off: the
// inherited GOPROXY names a reachable proxy, and the tidy phase still runs no
// command. It is the no-op build-tidy keys on the signal for.
func TestRunTidy_OfflineSignalRunsNoCommand(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"the-offline-signal-runs-no-tidy-command")
	if _, err := toolchain.ResolveGo(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	root, appDir := tidyWorkspace(t)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	t.Setenv("GOFLAGS", "")
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")

	before := readTidyFile(t, appDir, "go.mod")

	var status string
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		var err error
		status, _, err = Run(&pctx.Context{
			WorkspaceRoot: root,
			Project:       pctx.Project{Name: "app", FullPath: appDir},
		}, emit, []string{"--phase", "tidy"})
		if err != nil {
			t.Errorf("Run tidy: %v", err)
		}
	})

	if status != "OK" {
		t.Fatalf("status = %q, want OK: the offline no-op, not a tidy that could only fail", status)
	}
	explained := false
	for _, ev := range events {
		if ev["type"] == "diagnostic" {
			t.Fatalf("a diagnostic was emitted, so tidy ran after all: %+v", ev)
		}
		if msg, _ := ev["message"].(string); ev["type"] == "log" && msg == tidyOfflineMessage {
			explained = true
		}
	}
	if !explained {
		t.Errorf("no log explains why tidy did not run, got %+v", events)
	}
	if got := readTidyFile(t, appDir, "go.mod"); got != before {
		t.Errorf("go.mod = %q, want the committed content %q", got, before)
	}
}

// captureEvents captures JSONL events emitted to stdout during fn.
func captureEvents(t *testing.T, fn func(emit *jsonl.Emitter)) []map[string]any {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w

	fn(jsonl.New())

	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	r.Close()

	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) == nil {
			events = append(events, event)
		}
	}
	return events
}
