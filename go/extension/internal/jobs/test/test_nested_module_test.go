package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

// These tests pin, against the real toolchain, that a provider's coverage
// counts the provider's own packages only, never the packages of a nested
// module that happens to live under the provider's directory. `go test
// -coverpkg=./...` matches by directory prefix against the test binary's
// build list, not by module, so a nested module imported by a provider test
// is instrumented and lands in the provider's profile, inflating its
// denominator with statements the provider does not own.

// Statement counts of the nested-module fixture. Project a owns x.go (one
// statement, hit by its own test) and y.go (two statements, no test), so a's
// profile holds exactly three statements. The nested module owns sub.go: four
// statements, Used() hit by its own test and by a's test. Each module covers
// exactly one statement, so a leaked or lost block always moves the total,
// and a hit charged to the wrong module always moves the covered count.
const (
	nestedFixtureATotal    = 3
	nestedFixtureSubTotal  = 4
	nestedFixtureCovered   = 1
	nestedFixtureYTotal    = 2
	nestedFixtureSubPrefix = "example.com/a/sub/"
	nestedFixtureXPrefix   = "example.com/a/x/"
	nestedFixtureYPrefix   = "example.com/a/y/"
)

// nestedModuleFixture is a go.work root using ./a and ./a/sub, where a is a
// project and a/sub is a second project nested inside a's directory with its
// own go.mod. a's test imports the nested module.
type nestedModuleFixture struct {
	root string
	a    pctx.ProjectRef
	sub  pctx.ProjectRef
}

// writeNestedModuleFixture lays the fixture out under a fresh temporary root.
func writeNestedModuleFixture(t *testing.T) nestedModuleFixture {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"a/x", "a/y", "a/sub"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(root, "go.work"), "go 1.21\n\nuse (\n\t./a\n\t./a/sub\n)\n")
	mustWrite(t, filepath.Join(root, "a", "go.mod"), "module example.com/a\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(root, "a", "x", "x.go"),
		"package x\n\n// X is reached by its own test.\nfunc X() int { return 1 }\n")
	mustWrite(t, filepath.Join(root, "a", "x", "x_test.go"),
		"package x\n\nimport (\n\t\"testing\"\n\n\t\"example.com/a/sub\"\n)\n\n"+
			"func TestX(t *testing.T) {\n\tif X() != 1 || sub.Used() != 1 {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n")
	// y is an ordinary same-module subpackage without a test: it must still
	// count in a's denominator.
	mustWrite(t, filepath.Join(root, "a", "y", "y.go"),
		"package y\n\n// Y has no test.\nfunc Y() int { return 1 }\n\n// YUnused has no test either.\nfunc YUnused() int { return 2 }\n")
	mustWrite(t, filepath.Join(root, "a", "sub", "go.mod"), "module example.com/a/sub\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(root, "a", "sub", "sub.go"),
		"package sub\n\n// Used is reached by sub's own test and by a's test.\nfunc Used() int { return 1 }\n\n"+
			"func Unused1() int { return 2 }\n\nfunc Unused2() int { return 3 }\n\nfunc Unused3() int { return 4 }\n")
	mustWrite(t, filepath.Join(root, "a", "sub", "sub_test.go"),
		"package sub\n\nimport \"testing\"\n\nfunc TestUsed(t *testing.T) {\n\tif Used() != 1 {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n")
	return nestedModuleFixture{
		root: root,
		a:    pctx.ProjectRef{ID: "/a", Name: "a", Path: "a", FullPath: filepath.Join(root, "a")},
		sub:  pctx.ProjectRef{ID: "/a/sub", Name: "sub", Path: "a/sub", FullPath: filepath.Join(root, "a", "sub")},
	}
}

// runSolo runs the solo path on one project and returns its raw profile and
// its coverageSummary.
func (fx nestedModuleFixture) runSolo(t *testing.T, ref pctx.ProjectRef) ([]byte, map[string]any) {
	t.Helper()
	outDir := t.TempDir()
	status, data, err := Run(&pctx.Context{
		WorkspaceRoot: fx.root,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: ref.Name, FullPath: ref.FullPath},
		Params:        pctx.Params{},
	}, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("solo Run(%s) status=%q err=%v", ref.Name, status, err)
	}
	profile, err := os.ReadFile(filepath.Join(outDir, "coverage.out"))
	if err != nil {
		t.Fatalf("solo Run(%s) wrote no profile: %v", ref.Name, err)
	}
	return profile, coverageSummaryOf(t, data)
}

// runBatch runs the batch path over the selected projects under one coverage
// scope ("" for the default) and returns each member's result by project ID.
// Every member's coverage.out lands under <root>/.putnami/out/<path>/test/.
func (fx nestedModuleFixture) runBatch(t *testing.T, scope string, selected ...pctx.ProjectRef) map[string]batchProjectResult {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(fx.root, ".putnami", "out")); err != nil {
		t.Fatal(err)
	}
	params := pctx.Params{}
	if scope != "" {
		params["coverage-scope"] = json.RawMessage(`"` + scope + `"`)
	}
	status, data, err := runBatch(&pctx.Context{
		WorkspaceRoot:    fx.root,
		CacheRoot:        filepath.Join(fx.root, ".putnami", "cache"),
		Job:              pctx.Job{Name: "test"},
		Params:           params,
		SelectedProjects: selected,
	}, nil)
	if err != nil || status != "OK" {
		t.Fatalf("runBatch(scope=%q) status=%q err=%v", scope, status, err)
	}
	results := map[string]batchProjectResult{}
	for _, result := range data["batchResults"].([]batchProjectResult) {
		if result.Status != "OK" {
			t.Fatalf("runBatch(scope=%q) project %s = %+v, want OK", scope, result.ProjectID, result)
		}
		results[result.ProjectID] = result
	}
	return results
}

// batchProfile reads the coverage.out the batch path wrote for one member.
func (fx nestedModuleFixture) batchProfile(t *testing.T, ref pctx.ProjectRef) []byte {
	t.Helper()
	path := filepath.Join(fx.root, ".putnami", "out", filepath.FromSlash(ref.Path), "test", "coverage.out")
	profile, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("batch wrote no profile for %s: %v", ref.Name, err)
	}
	return profile
}

func coverageSummaryOf(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	summary, ok := data["coverageSummary"].(map[string]any)
	if !ok {
		t.Fatalf("result data carries no coverageSummary: %+v", data)
	}
	return summary
}

// profileStatements sums the statements of the profile blocks whose position
// starts with prefix, deduplicated by block position the way go tool cover
// reads a union profile: one statement count per block, the highest hit
// count wins. An empty prefix selects the whole profile.
func profileStatements(t *testing.T, profile []byte, prefix string) (total, covered int) {
	t.Helper()
	type block struct{ statements, hits int }
	blocks := map[string]block{}
	for _, line := range strings.Split(string(profile), "\n") {
		if line == "" || strings.HasPrefix(line, "mode:") || !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("profile line %q is not a coverage block", line)
		}
		statements, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("profile block %q has no statement count: %v", line, err)
		}
		hits, err := strconv.Atoi(fields[2])
		if err != nil {
			t.Fatalf("profile block %q has no hit count: %v", line, err)
		}
		seen := blocks[fields[0]]
		blocks[fields[0]] = block{statements: statements, hits: max(seen.hits, hits)}
	}
	for _, b := range blocks {
		total += b.statements
		if b.hits > 0 {
			covered += b.statements
		}
	}
	return total, covered
}

// profileLinesWithPrefix counts the raw profile lines whose position starts
// with prefix, duplicates included.
func profileLinesWithPrefix(profile []byte, prefix string) int {
	count := 0
	for _, line := range strings.Split(string(profile), "\n") {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

// assertNestedProfile checks one measured project's raw profile and its
// reported summary: no line under any forbidden prefix, and exactly the
// expected statement total with the fixture's one covered statement, both
// in the profile and in coverageSummary.
func assertNestedProfile(t *testing.T, who string, profile []byte, summary map[string]any, wantTotal int, forbidden ...string) {
	t.Helper()
	wantCovered := nestedFixtureCovered
	for _, prefix := range forbidden {
		if n := profileLinesWithPrefix(profile, prefix); n != 0 {
			t.Errorf("%s: profile holds %d line(s) under %s, want none:\n%s", who, n, prefix, profile)
		}
	}
	total, covered := profileStatements(t, profile, "")
	if total != wantTotal || covered != wantCovered {
		t.Errorf("%s: profile statements = %d/%d (covered/total), want %d/%d:\n%s", who, covered, total, wantCovered, wantTotal, profile)
	}
	if got, want := summary["total"], wantTotal; got != want {
		t.Errorf("%s: coverageSummary.total = %v, want %d", who, got, want)
	}
	if got, want := summary["covered"], wantCovered; got != want {
		t.Errorf("%s: coverageSummary.covered = %v, want %d", who, got, want)
	}
}

// assertNestedProfileOfA is assertNestedProfile for project a: no nested
// module line, three statements with one covered, and y.go present as two
// uncovered statements.
func assertNestedProfileOfA(t *testing.T, who string, profile []byte, summary map[string]any) {
	t.Helper()
	assertNestedProfile(t, who, profile, summary, nestedFixtureATotal, nestedFixtureSubPrefix)
	if hits := blockCount(t, profile, nestedFixtureYPrefix); hits != 0 {
		t.Errorf("%s: y.go is hit %d time(s), want an untested subpackage", who, hits)
	}
	if total, _ := profileStatements(t, profile, nestedFixtureYPrefix); total != nestedFixtureYTotal {
		t.Errorf("%s: y.go holds %d statement(s), want %d", who, total, nestedFixtureYTotal)
	}
}

// TestRunNestedModuleStaysOutOfTheProviderProfile pins that rule on every path
// that measures a project: the solo path, a one-member batch, a batch with
// the nested module as a mate, under both coverage scopes. In each one, a's
// profile holds a's packages only and the nested module keeps its own
// independent measurement.
func TestRunNestedModuleStaysOutOfTheProviderProfile(t *testing.T) {
	t.Setenv(envDatabaseTestBindings, "")
	fx := writeNestedModuleFixture(t)

	t.Run("solo a", func(t *testing.T) {
		spectest.Proves(t, "go/go-project-toolchain", "module-bounded-coverage", "a-nested-module-stays-out-of-the-solo-profile")
		profile, summary := fx.runSolo(t, fx.a)
		assertNestedProfileOfA(t, "solo a", profile, summary)
	})

	t.Run("batch default scope a alone", func(t *testing.T) {
		spectest.Proves(t, "go/go-project-toolchain", "module-bounded-coverage", "a-nested-module-stays-out-of-a-one-member-batch")
		results := fx.runBatch(t, "", fx.a)
		assertNestedProfileOfA(t, "batch a alone", fx.batchProfile(t, fx.a), coverageSummaryOf(t, results[fx.a.ID].Data))
	})

	t.Run("batch default scope a and sub", func(t *testing.T) {
		spectest.Proves(t, "go/go-project-toolchain", "module-bounded-coverage", "a-nested-module-stays-out-of-a-union-batch")
		results := fx.runBatch(t, "", fx.a, fx.sub)
		assertNestedProfileOfA(t, "batch a with sub", fx.batchProfile(t, fx.a), coverageSummaryOf(t, results[fx.a.ID].Data))
		assertNestedProfile(t, "batch sub with a", fx.batchProfile(t, fx.sub), coverageSummaryOf(t, results[fx.sub.ID].Data),
			nestedFixtureSubTotal, nestedFixtureXPrefix, nestedFixtureYPrefix)
	})

	t.Run("batch project scope a and sub", func(t *testing.T) {
		spectest.Proves(t, "go/go-project-toolchain", "module-bounded-coverage", "a-nested-module-stays-out-of-a-project-scoped-batch")
		results := fx.runBatch(t, coverageScopeProject, fx.a, fx.sub)
		assertNestedProfileOfA(t, "project-scope a with sub", fx.batchProfile(t, fx.a), coverageSummaryOf(t, results[fx.a.ID].Data))
		assertNestedProfile(t, "project-scope sub with a", fx.batchProfile(t, fx.sub), coverageSummaryOf(t, results[fx.sub.ID].Data),
			nestedFixtureSubTotal, nestedFixtureXPrefix, nestedFixtureYPrefix)
	})

	t.Run("solo sub", func(t *testing.T) {
		spectest.Proves(t, "go/go-project-toolchain", "module-bounded-coverage", "a-nested-module-keeps-its-own-measurement")
		profile, summary := fx.runSolo(t, fx.sub)
		assertNestedProfile(t, "solo sub", profile, summary,
			nestedFixtureSubTotal, nestedFixtureXPrefix, nestedFixtureYPrefix)
	})
}
