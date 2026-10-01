package test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

// These tests pin ADR 0008: coverage-scope decides whose tests count toward a
// batched project's coverage, and project scope makes a member's profile and
// its `go test` arguments a function of the member alone.

// TestRunBatchDefaultScopeKeepsTheUnionSplit is the golden of today's batch
// scope. The default must keep the single union invocation, the union
// -coverpkg naming every member's own packages, the group scratch profile
// name, and the byte-for-byte split of that profile into each member's
// coverage.out.
func TestRunBatchDefaultScopeKeepsTheUnionSplit(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "project-scoped-coverage", "the-default-batch-scope-keeps-the-union-invocation-and-its-split")
	ctx := makeBatchTestContext(t, true)

	var invocations [][]string
	mockGoCommand(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
		invocations = append(invocations, normalizeScratch(t, args))
		profile := strings.Join([]string{
			"mode: set",
			"example.com/a/a.go:3.24,3.34 1 1",
			"example.com/b/b.go:3.24,3.34 1 0",
			"",
			"example.com/a/a.go:5.27,5.37 1 0",
			"",
		}, "\n")
		if err := os.WriteFile(argumentAfter(t, args, "-coverprofile"), []byte(profile), 0o644); err != nil {
			t.Fatal(err)
		}
		return []byte(passEvents("example.com/a", "example.com/b")), nil
	})

	status, data, err := runBatch(ctx, nil)
	if err != nil || status != "OK" {
		t.Fatalf("runBatch status=%q err=%v", status, err)
	}
	want := [][]string{{
		"test", "-cover", "-coverpkg", "example.com/a,example.com/b",
		"-coverprofile", "<scratch>/coverage.out",
		"-json", "./a/...", "./b/...",
	}}
	if !slices.EqualFunc(invocations, want, slices.Equal[[]string]) {
		t.Fatalf("batch-scope invocations = %q, want the single union invocation %q", invocations, want)
	}
	for _, result := range data["batchResults"].([]batchProjectResult) {
		if result.Status != "OK" {
			t.Fatalf("project %s = %+v, want OK", result.ProjectID, result)
		}
	}
	assertFileBytes(t, filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", "a", "test", "coverage.out"),
		"mode: set\nexample.com/a/a.go:3.24,3.34 1 1\nexample.com/a/a.go:5.27,5.37 1 0\n")
	assertFileBytes(t, filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", "b", "test", "coverage.out"),
		"mode: set\nexample.com/b/b.go:3.24,3.34 1 0\n")
}

// TestRunBatchProjectScopeArgumentsIgnoreBatchMates pins that, under project
// scope, a member's `go test` arguments, -coverpkg included, are those of its
// one-member batch whichever projects share its batch and wherever it sits in
// the group. The comparison is against a one-member batch, not the solo path,
// whose arguments differ; it does not measure build-cache reuse either. The
// only difference between two runs is the per-run scratch root. Batch scope is
// the control: there a member's -coverpkg names its batch mates.
func TestRunBatchProjectScopeArgumentsIgnoreBatchMates(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "project-scoped-coverage", "a-project-scoped-members-arguments-do-not-depend-on-its-batch-mates")
	ctx := makeScopedBatchContext(t, "a", "b", "c")
	t.Setenv(envDatabaseTestBindings, "")

	argsFor := func(scope string, members ...string) map[string][]string {
		t.Helper()
		run := *ctx
		run.Params = pctx.Params{"coverage-scope": json.RawMessage(`"` + scope + `"`)}
		run.SelectedProjects = selectProjects(t, ctx, members...)
		byMember := map[string][]string{}
		mockGoCommand(t, func(_ string, args []string, dir string, _ []string) ([]byte, error) {
			if dir != ctx.WorkspaceRoot {
				t.Fatalf("go test ran in %q, want the go.work root %q", dir, ctx.WorkspaceRoot)
			}
			normalized := normalizeScratch(t, args)
			// The package patterns close the argument list.
			var packages []string
			for i := len(normalized) - 1; i >= 0 && strings.HasPrefix(normalized[i], "./"); i-- {
				member := strings.TrimSuffix(strings.TrimPrefix(normalized[i], "./"), "/...")
				byMember[member] = normalized
				packages = append(packages, "example.com/"+member)
			}
			return []byte(passEvents(packages...)), nil
		})
		status, data, err := runBatch(&run, nil)
		if err != nil || status != "OK" {
			t.Fatalf("runBatch(%s, %v) status=%q err=%v", scope, members, status, err)
		}
		for _, result := range data["batchResults"].([]batchProjectResult) {
			if result.Status != "OK" {
				t.Fatalf("runBatch(%s, %v) project %s = %+v", scope, members, result.ProjectID, result)
			}
		}
		return byMember
	}

	alone := argsFor(coverageScopeProject, "a")["a"]
	if got := argumentAfter(t, alone, "-coverpkg"); got != "example.com/a" {
		t.Fatalf("project-scope -coverpkg = %q, want the member's own packages", got)
	}
	if last := alone[len(alone)-1]; last != "./a/..." {
		t.Fatalf("project-scope args = %q, want only the member's pattern", alone)
	}
	for _, members := range [][]string{{"a", "b"}, {"c", "a"}, {"b", "a", "c"}} {
		got := argsFor(coverageScopeProject, members...)
		if len(got) != len(members) {
			t.Fatalf("project scope over %v ran %d member invocations, want one per member", members, len(got))
		}
		if !slices.Equal(got["a"], alone) {
			t.Fatalf("project scope over %v gave a %q, want its one-member arguments %q", members, got["a"], alone)
		}
	}

	withB := argsFor(coverageScopeBatch, "a", "b")["a"]
	withC := argsFor(coverageScopeBatch, "a", "c")["a"]
	if slices.Equal(withB, withC) {
		t.Fatalf("batch scope gave a identical arguments with different batch mates (%q); the control no longer shows the dependence", withB)
	}
}

// TestRunBatchProjectScopeProfileIsIdenticalAloneAndInABatch is the acceptance
// test of ADR 0008 against the real toolchain. Project b's tests call a
// function of project a that a's own tests never reach. Under project scope
// a's coverage.out holds the same bytes when a runs in a batch with b as when a
// runs alone through the solo path. That byte equality, not equal `go test`
// arguments, is what ties project scope to the solo run. Batch scope is the
// control: there b's tests mark that function covered in a's profile, so the
// fixture really exercises a cross-project hit.
func TestRunBatchProjectScopeProfileIsIdenticalAloneAndInABatch(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "project-scoped-coverage", "a-project-scoped-profile-is-identical-alone-and-in-a-batch")
	t.Setenv(envDatabaseTestBindings, "")
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.work"), "go 1.21\n\nuse (\n\t./a\n\t./b\n)\n")
	for _, dir := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, "a", "go.mod"), "module example.com/a\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(root, "a", "a.go"),
		"package a\n\n// Own is reached by a's own test.\nfunc Own() int { return 1 }\n\n// Shared is reached only by b's test.\nfunc Shared() int { return 2 }\n")
	mustWrite(t, filepath.Join(root, "a", "a_test.go"),
		"package a\n\nimport \"testing\"\n\nfunc TestOwn(t *testing.T) {\n\tif Own() != 1 {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n")
	mustWrite(t, filepath.Join(root, "b", "go.mod"), "module example.com/b\n\ngo 1.21\n\nrequire example.com/a v0.0.0\n")
	mustWrite(t, filepath.Join(root, "b", "b.go"),
		"package b\n\nimport \"example.com/a\"\n\n// Total reaches into project a.\nfunc Total() int { return a.Shared() + 1 }\n")
	mustWrite(t, filepath.Join(root, "b", "b_test.go"),
		"package b\n\nimport \"testing\"\n\nfunc TestTotal(t *testing.T) {\n\tif Total() != 3 {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n")

	refs := []pctx.ProjectRef{
		{ID: "/a", Name: "a", Path: "a", FullPath: filepath.Join(root, "a")},
		{ID: "/b", Name: "b", Path: "b", FullPath: filepath.Join(root, "b")},
	}
	aProfile := filepath.Join(root, ".putnami", "out", "a", "test", "coverage.out")
	batchProfile := func(scope string, selected ...pctx.ProjectRef) []byte {
		t.Helper()
		if err := os.RemoveAll(filepath.Join(root, ".putnami", "out")); err != nil {
			t.Fatal(err)
		}
		ctx := &pctx.Context{
			WorkspaceRoot:    root,
			CacheRoot:        filepath.Join(root, ".putnami", "cache"),
			Job:              pctx.Job{Name: "test"},
			Params:           pctx.Params{"coverage-scope": json.RawMessage(`"` + scope + `"`)},
			SelectedProjects: selected,
		}
		status, data, err := runBatch(ctx, nil)
		if err != nil || status != "OK" {
			t.Fatalf("runBatch(%s) status=%q err=%v", scope, status, err)
		}
		for _, result := range data["batchResults"].([]batchProjectResult) {
			if result.Status != "OK" {
				t.Fatalf("runBatch(%s) project %s = %+v", scope, result.ProjectID, result)
			}
		}
		profile, err := os.ReadFile(aProfile)
		if err != nil {
			t.Fatalf("runBatch(%s) wrote no profile for a: %v", scope, err)
		}
		return profile
	}

	inBatch := batchProfile(coverageScopeProject, refs...)
	if got := blockCount(t, inBatch, "example.com/a/a.go:7."); got != 0 {
		t.Fatalf("project-scope profile counts b's hit on a.Shared (count %d):\n%s", got, inBatch)
	}
	if got := blockCount(t, inBatch, "example.com/a/a.go:4."); got != 1 {
		t.Fatalf("project-scope profile lost a's own hit on a.Own (count %d):\n%s", got, inBatch)
	}

	soloOut := t.TempDir()
	status, _, err := Run(&pctx.Context{
		WorkspaceRoot: root,
		OutputPath:    soloOut,
		Project:       pctx.Project{Name: "a", FullPath: refs[0].FullPath},
		Params:        pctx.Params{},
	}, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("solo Run status=%q err=%v", status, err)
	}
	solo, err := os.ReadFile(filepath.Join(soloOut, "coverage.out"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(solo, inBatch) {
		t.Fatalf("a's project-scope profile depends on its batch mates:\nin a batch with b:\n%s\nalone:\n%s", inBatch, solo)
	}

	union := batchProfile(coverageScopeBatch, refs...)
	if got := blockCount(t, union, "example.com/a/a.go:7."); got != 1 {
		t.Fatalf("batch-scope control: a.Shared count %d, want b's hit counted:\n%s", got, union)
	}
}

// TestUnknownCoverageScopeFailsClosed pins that a typo never silently measures
// under the other scope: the batch path charges the error to every member
// before any go invocation, and the solo path fails the same way.
func TestUnknownCoverageScopeFailsClosed(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "project-scoped-coverage", "an-unknown-coverage-scope-fails-closed")
	ctx := makeBatchTestContext(t, true)
	ctx.Params["coverage-scope"] = json.RawMessage(`"projects"`)
	mockGoCommand(t, func(_ string, args []string, _ string, _ []string) ([]byte, error) {
		t.Fatalf("go test ran with an unknown coverage scope: %v", args)
		return nil, nil
	})
	status, data, err := runBatch(ctx, nil)
	if err != nil || status != "OK" {
		t.Fatalf("runBatch status=%q err=%v", status, err)
	}
	results := data["batchResults"].([]batchProjectResult)
	if len(results) != 2 {
		t.Fatalf("results = %+v, want one per member", results)
	}
	for _, result := range results {
		if result.Status != "FAILED" || !hasDiagnosticCode(result.Diagnostics, "GO_TEST_BATCH_PREPARE") ||
			!strings.Contains(result.Diagnostics[0].Description, `got "projects"`) {
			t.Fatalf("project %s = %+v, want a coverage-scope preparation failure", result.ProjectID, result)
		}
	}

	dir := t.TempDir()
	outDir := t.TempDir()
	writeCoverageModule(t, dir)
	var soloStatus string
	events := captureJobEvents(t, func(emit *jsonl.Emitter) {
		soloStatus, _, err = Run(&pctx.Context{
			WorkspaceRoot: dir,
			OutputPath:    outDir,
			Project:       pctx.Project{Name: "covmod", FullPath: dir},
			Params:        pctx.Params{"coverage-scope": json.RawMessage(`"projects"`)},
		}, emit, nil)
	})
	if err != nil || soloStatus != "FAILED" {
		t.Fatalf("solo Run status=%q err=%v, want FAILED", soloStatus, err)
	}
	if !eventsMention(eventsOfType(events, "diagnostic"), "coverage-scope") {
		t.Fatalf("solo Run emitted no coverage-scope diagnostic: %+v", events)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "coverage.out")); !os.IsNotExist(statErr) {
		t.Fatalf("solo Run wrote a profile under an unknown scope (stat err %v)", statErr)
	}
}

// makeScopedBatchContext builds a go.work workspace with one module per name,
// every module selected.
func makeScopedBatchContext(t *testing.T, names ...string) *pctx.Context {
	t.Helper()
	root := t.TempDir()
	var use strings.Builder
	refs := make([]pctx.ProjectRef, 0, len(names))
	for _, name := range names {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/"+name+"\n\ngo 1.25.7\n")
		use.WriteString("\t./" + name + "\n")
		refs = append(refs, pctx.ProjectRef{ID: "/" + name, Name: name, Path: name, FullPath: dir})
	}
	mustWrite(t, filepath.Join(root, "go.work"), "go 1.25.7\n\nuse (\n"+use.String()+")\n")
	return &pctx.Context{
		WorkspaceRoot:    root,
		CacheRoot:        filepath.Join(root, ".putnami", "cache"),
		Job:              pctx.Job{Name: "test"},
		Params:           pctx.Params{},
		SelectedProjects: refs,
	}
}

func selectProjects(t *testing.T, ctx *pctx.Context, names ...string) []pctx.ProjectRef {
	t.Helper()
	selected := make([]pctx.ProjectRef, 0, len(names))
	for _, name := range names {
		index := slices.IndexFunc(ctx.SelectedProjects, func(ref pctx.ProjectRef) bool { return ref.Name == name })
		if index < 0 {
			t.Fatalf("no project %q in the fixture", name)
		}
		selected = append(selected, ctx.SelectedProjects[index])
	}
	return selected
}

// normalizeScratch replaces the per-run scratch directory, the parent of the
// -coverprofile value, with a fixed token so two runs compare equal.
func normalizeScratch(t *testing.T, args []string) []string {
	t.Helper()
	scratch := filepath.Dir(argumentAfter(t, args, "-coverprofile"))
	normalized := make([]string, 0, len(args))
	for _, arg := range args {
		// Slash form, so one golden holds on Windows, where the profile path
		// ends in "<scratch>\coverage.out".
		normalized = append(normalized, filepath.ToSlash(strings.ReplaceAll(arg, scratch, "<scratch>")))
	}
	return normalized
}

func passEvents(packages ...string) string {
	lines := make([]string, 0, len(packages))
	for _, pkg := range packages {
		lines = append(lines, testEvent("pass", pkg, "", ""))
	}
	return strings.Join(lines, "\n") + "\n"
}

func assertFileBytes(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

// blockCount returns the hit count of the profile blocks whose position starts
// with prefix. A union profile may repeat a block once per test binary, so the
// highest count wins, the way go tool cover reads it.
func blockCount(t *testing.T, profile []byte, prefix string) int {
	t.Helper()
	count, found := 0, false
	for _, line := range strings.Split(string(profile), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		hits, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			t.Fatalf("profile block %q has no hit count: %v", line, err)
		}
		count, found = max(count, hits), true
	}
	if !found {
		t.Fatalf("profile has no block starting with %q:\n%s", prefix, profile)
	}
	return count
}
