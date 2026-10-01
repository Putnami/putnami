package lint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/parse"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"

	"go.putnami.dev/protocol/features/spectest"
)

// recordedStaticcheck captures one stubbed staticcheck call.
type recordedStaticcheck struct {
	patterns []string
	dir      string
}

// stubStaticcheck replaces the tool runner and binary resolver for a test and
// returns the ordered list of invocations the batch made.
func stubStaticcheck(
	t *testing.T,
	respond func(inv recordedStaticcheck) ([]byte, error),
) *[]recordedStaticcheck {
	t.Helper()
	var invocations []recordedStaticcheck
	origCmd := runStaticcheckCommand
	origBin := resolveStaticcheckBinary
	runStaticcheckCommand = func(_ string, patterns []string, dir string) ([]byte, error) {
		inv := recordedStaticcheck{patterns: slices.Clone(patterns), dir: dir}
		invocations = append(invocations, inv)
		return respond(inv)
	}
	resolveStaticcheckBinary = func(string) (string, error) { return "staticcheck", nil }
	t.Cleanup(func() {
		runStaticcheckCommand = origCmd
		resolveStaticcheckBinary = origBin
	})
	return &invocations
}

// staticcheckWorkspace lays out a go.work root with the named module members.
func staticcheckWorkspace(t *testing.T, root string, members ...string) {
	t.Helper()
	var uses strings.Builder
	for _, member := range members {
		fmt.Fprintf(&uses, "\t./%s\n", member)
		mustWrite(t, filepath.Join(root, member, "go.mod"),
			"module example.com/"+filepath.Base(member)+"\n\ngo 1.26\n")
	}
	mustWrite(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse (\n"+uses.String()+")\n")
}

func staticcheckProjectRefs(root string, members ...string) []pctx.ProjectRef {
	refs := make([]pctx.ProjectRef, 0, len(members))
	for _, member := range members {
		refs = append(refs, pctx.ProjectRef{
			ID:       "/" + member,
			Name:     filepath.Base(member),
			Path:     member,
			FullPath: filepath.Join(root, member),
		})
	}
	return refs
}

func runStaticcheckBatchOn(t *testing.T, root string, members ...string) []batchProjectResult {
	t.Helper()
	ctx := &pctx.Context{
		WorkspaceRoot:    root,
		Job:              pctx.Job{Name: "lint"},
		SelectedProjects: staticcheckProjectRefs(root, members...),
	}
	options := parseLintOptions(ctx.Params, []string{"--tool", "staticcheck"})
	status, data, err := runStaticcheckBatch(ctx, options)
	if err != nil || status != "OK" {
		t.Fatalf("runStaticcheckBatch = %q, %v", status, err)
	}
	results, ok := data["batchResults"].([]batchProjectResult)
	if !ok {
		t.Fatalf("batchResults missing or wrong type: %T", data["batchResults"])
	}
	return results
}

// TestRunStaticcheckBatchGroupsOneGoWorkRootAndAttributes is the core
// equivalence guarantee: every member of one governing go.work root shares ONE
// staticcheck process, and each finding lands on the project that owns its file
// with the same workspace-relative path a solo run would emit.
//
// The stubbed output is the REAL shape, verified against staticcheck v0.7.0:
// paths are relative to the process's working directory, so a batch run from
// the go.work root reports "go/framework/api/handler.go" where the same solo run
// from the project directory reports "handler.go".
func TestRunStaticcheckBatchGroupsOneGoWorkRootAndAttributes(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "lint-batch-equivalence", "a-batched-staticcheck-run-groups-one-go-work-root-and-attributes-each-finding")
	root := t.TempDir()
	staticcheckWorkspace(t, root, "go/framework/api", "go/framework/errors", "tooling/cli")

	invocations := stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return []byte(
			"go/framework/api/handler.go:10:2: assignment to nil map (SA5000)\n" +
				"go/framework/errors/wrap.go:5:6: should omit comparison to bool constant (S1002)\n",
		), fmt.Errorf("exit status 1")
	})

	results := runStaticcheckBatchOn(t, root, "go/framework/api", "go/framework/errors", "tooling/cli")
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}

	if len(*invocations) != 1 {
		t.Fatalf("want ONE shared invocation for one go.work root, got %d: %+v",
			len(*invocations), *invocations)
	}
	invocation := (*invocations)[0]
	if invocation.dir != root {
		t.Fatalf("invocation ran from %q, want the go.work root %q", invocation.dir, root)
	}
	want := []string{"./go/framework/api/...", "./go/framework/errors/...", "./tooling/cli/..."}
	if fmt.Sprint(invocation.patterns) != fmt.Sprint(want) {
		t.Fatalf("invocation patterns = %v, want %v", invocation.patterns, want)
	}

	api := resultByID(t, results, "/go/framework/api")
	if api.Status != "FAILED" || len(api.Diagnostics) != 1 {
		t.Fatalf("api result = %+v", api)
	}
	if api.Diagnostics[0].File != "go/framework/api/handler.go" ||
		api.Diagnostics[0].Line != 10 || api.Diagnostics[0].Column != 2 ||
		api.Diagnostics[0].Description != "assignment to nil map (SA5000)" {
		t.Fatalf("api diagnostic mis-attributed: %+v", api.Diagnostics[0])
	}
	if api.Summary.Errors != 1 {
		t.Fatalf("api summary errors = %d, want 1", api.Summary.Errors)
	}

	errs := resultByID(t, results, "/go/framework/errors")
	if errs.Status != "FAILED" || len(errs.Diagnostics) != 1 ||
		errs.Diagnostics[0].File != "go/framework/errors/wrap.go" {
		t.Fatalf("errors result mis-attributed: %+v", errs)
	}

	cli := resultByID(t, results, "/tooling/cli")
	if cli.Status != "OK" || len(cli.Diagnostics) != 0 {
		t.Fatalf("a clean member must not inherit its batch-mates' failure: %+v", cli)
	}
}

// TestStaticcheckBatchDiagnosticsMatchSoloRun pins the per-member byte identity
// the slice promises: the diagnostic a batched member reports is the one the
// SOLO path would have emitted for the same tool findings — same message, same
// position, same workspace-relative file.
//
// The two inputs are the same staticcheck run seen from two working directories
// (verified against the real tool): solo reports "wrap.go:5:6", the shared
// process reports "go/framework/errors/wrap.go:5:6".
func TestStaticcheckBatchDiagnosticsMatchSoloRun(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "lint-batch-equivalence", "batched-staticcheck-diagnostics-match-a-solo-run")
	root := t.TempDir()
	staticcheckWorkspace(t, root, "go/framework/errors")

	const soloOutput = "wrap.go:5:6: should omit comparison to bool constant (S1002)\n" +
		"nested/deep.go:12:9: assignment to nil map (SA5000)\n"
	const batchOutput = "go/framework/errors/wrap.go:5:6: should omit comparison to bool constant (S1002)\n" +
		"go/framework/errors/nested/deep.go:12:9: assignment to nil map (SA5000)\n"

	solo := soloDiagnostics(t, soloOutput, root, filepath.Join(root, "go/framework/errors"))

	stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return []byte(batchOutput), fmt.Errorf("exit status 1")
	})
	results := runStaticcheckBatchOn(t, root, "go/framework/errors")
	batched := resultByID(t, results, "/go/framework/errors").Diagnostics

	if len(batched) != len(solo) {
		t.Fatalf("batched %d diagnostics, solo emitted %d", len(batched), len(solo))
	}
	for i := range solo {
		if batched[i] != solo[i] {
			t.Fatalf("diagnostic %d diverges:\n batch %+v\n solo  %+v", i, batched[i], solo[i])
		}
	}
}

// soloDiagnostics replays tool output through the SOLO lint path and returns the
// diagnostics it emitted, in the batch wire's own shape so the two are directly
// comparable.
func soloDiagnostics(t *testing.T, output, workspaceRoot, projectPath string) []parse.ToolDiagnostic {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	parse.LintErrors(output, workspaceRoot, projectPath, jsonl.New())
	w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	r.Close()

	var diagnostics []parse.ToolDiagnostic
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
			Location struct {
				File   string `json:"file"`
				Line   int    `json:"line"`
				Column int    `json:"column"`
			} `json:"location"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.Type != "diagnostic" {
			continue
		}
		diagnostics = append(diagnostics, parse.ToolDiagnostic{
			Severity:    event.Severity,
			Description: event.Message,
			File:        event.Location.File,
			Line:        event.Location.Line,
			Column:      event.Location.Column,
		})
	}
	if len(diagnostics) == 0 {
		t.Fatal("solo path emitted no diagnostics; the comparison would be vacuous")
	}
	return diagnostics
}

// TestBatchProjectForFileChargesTheLongestRoot pins the attribution rule both
// lint batches share. A nested module's file sits under its parent's root too,
// so a first-match walk would charge the parent; only the longest matching root
// names the module that actually owns the file.
func TestBatchProjectForFileChargesTheLongestRoot(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "lint-batch-equivalence", "a-finding-is-charged-to-the-longest-matching-project-root")
	root := t.TempDir()
	owners := []batchFileOwner{
		{id: "/go/framework/migration", root: filepath.Join(root, "go/framework/migration")},
		{id: "/go/framework/migration/migratecli", root: filepath.Join(root, "go/framework/migration/migratecli")},
	}
	for _, tc := range []struct{ file, want string }{
		{"go/framework/migration/apply.go", "/go/framework/migration"},
		{"go/framework/migration/migratecli/main.go", "/go/framework/migration/migratecli"},
		{filepath.Join(root, "go/framework/migration/migratecli/main.go"), "/go/framework/migration/migratecli"},
		{"go/framework/migrationsuffix/x.go", ""},
		{"tooling/cli/main.go", ""},
	} {
		if got := batchProjectForFile(tc.file, root, owners); got != tc.want {
			t.Errorf("batchProjectForFile(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

// TestRunStaticcheckBatchSplitsDistinctGoWorkRoots guards the one grouping key
// a shared staticcheck invocation actually needs. Members governed by different
// go.work files cannot share a run directory, and a pattern reaching outside the
// workspace file would not resolve at all.
func TestRunStaticcheckBatchSplitsDistinctGoWorkRoots(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "lint-batch-equivalence", "projects-from-distinct-go-work-roots-are-never-grouped-together")
	root := t.TempDir()
	staticcheckWorkspace(t, root, "primary/a", "primary/b")
	// A second, nested workspace: `other` governs itself.
	staticcheckWorkspace(t, filepath.Join(root, "other"), "c")

	invocations := stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return nil, nil
	})
	results := runStaticcheckBatchOn(t, root, "primary/a", "primary/b", "other/c")
	for _, result := range results {
		if result.Status != "OK" {
			t.Fatalf("clean member %s = %+v", result.ProjectID, result)
		}
	}

	if len(*invocations) != 2 {
		t.Fatalf("want one invocation per go.work root (2), got %d: %+v",
			len(*invocations), *invocations)
	}
	byDir := map[string][]string{}
	for _, invocation := range *invocations {
		byDir[invocation.dir] = invocation.patterns
	}
	primary := byDir[root]
	if fmt.Sprint(primary) != fmt.Sprint([]string{"./primary/a/...", "./primary/b/..."}) {
		t.Fatalf("primary root patterns = %v", primary)
	}
	other := byDir[filepath.Join(root, "other")]
	if fmt.Sprint(other) != fmt.Sprint([]string{"./c/..."}) {
		t.Fatalf("nested workspace patterns = %v", other)
	}
}

// TestRunStaticcheckBatchAddsNoCheckSelection is the audit's NO-GO, pinned. The
// W2c measurement proved `-checks` is a post-analysis DISPLAY filter — every
// analyzer runs regardless — so narrowing buys no time, hard-codes an assumption
// about a golangci config the consumer can override, and would silently drop
// coverage on the paths golangci's unanchored exclusions already miss. The batch
// invocation must therefore carry patterns and nothing else.
func TestRunStaticcheckBatchAddsNoCheckSelection(t *testing.T) {
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a", "b")
	invocations := stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return nil, nil
	})
	runStaticcheckBatchOn(t, root, "a", "b")

	for _, invocation := range *invocations {
		for _, arg := range invocation.patterns {
			if strings.HasPrefix(arg, "-") {
				t.Fatalf("batch invocation carries a flag %q; the solo path passes none "+
					"and -checks cannot reduce analysis (W2c audit)", arg)
			}
		}
	}
}

// compatLine is the toolchain-mismatch abort staticcheck emits for a file whose
// Go version requirement exceeds the pinned binary's. It carries a position, so
// it attributes to a member like any other finding.
func compatLine(file string) string {
	return file + ":1:1: file requires newer Go version go1.99 " +
		"(application built with go1.26) (compile)\n"
}

// TestExecuteStaticcheckGroupToolchainCompatSkipsLikeSolo mirrors the solo
// path's SKIP on a toolchain-mismatch abort. staticcheck type-checks nothing
// there and reports nothing, so a batch must not manufacture a failure the same
// task would not have had alone.
func TestExecuteStaticcheckGroupToolchainCompatSkipsLikeSolo(t *testing.T) {
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a", "b")
	stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return []byte(compatLine("a/x.go") + compatLine("b/y.go")), fmt.Errorf("exit status 1")
	})

	for _, result := range runStaticcheckBatchOn(t, root, "a", "b") {
		if result.Status != "OK" {
			t.Fatalf("%s = %s, want the solo path's skip-as-OK", result.ProjectID, result.Status)
		}
		if len(result.Diagnostics) != 0 {
			t.Fatalf("%s carries diagnostics a solo skip would not emit: %+v",
				result.ProjectID, result.Diagnostics)
		}
	}
}

// TestExecuteStaticcheckGroupCompatSkipsOnlyTheAffectedMember is the
// silent-green regression this batch must never have.
//
// A Go version requirement is per FILE and per MODULE, so during a toolchain
// upgrade one member can carry a `//go:build go1.NN` file the pinned binary
// cannot type-check while its batch-mates analyze perfectly and report real
// findings. Testing the COMBINED output for the abort and skipping the whole
// group would drop those findings AND cache each member's bare OK under its own
// key — served warm until that member's sources change. Only the member the
// abort names may be silenced.
func TestExecuteStaticcheckGroupCompatSkipsOnlyTheAffectedMember(t *testing.T) {
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a", "b")
	stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return []byte(
			"a/x.go:3:2: assignment to nil map (SA5000)\n" +
				compatLine("b/gen.go") +
				"a/y.go:9:6: func helper is unused (U1000)\n",
		), fmt.Errorf("exit status 1")
	})

	results := runStaticcheckBatchOn(t, root, "a", "b")

	a := resultByID(t, results, "/a")
	if a.Status != "FAILED" || len(a.Diagnostics) != 2 {
		t.Fatalf("a batch-mate's real findings were suppressed by another member's "+
			"toolchain abort: %+v", a)
	}
	if a.Diagnostics[0].File != "a/x.go" || a.Diagnostics[1].File != "a/y.go" {
		t.Fatalf("member a diagnostics mis-attributed: %+v", a.Diagnostics)
	}
	if a.Summary.Errors != 2 {
		t.Fatalf("member a summary errors = %d, want 2", a.Summary.Errors)
	}

	b := resultByID(t, results, "/b")
	if b.Status != "OK" || len(b.Diagnostics) != 0 {
		t.Fatalf("the member the abort names must skip as OK like its solo run: %+v", b)
	}
}

// TestExecuteStaticcheckGroupCompatSilencesItsMemberWholesale: a member that
// cannot be type-checked skips ENTIRELY, dropping the partial findings the same
// run also reported for it. That is what its solo run does with the same output
// (`isStaticcheckToolchainCompatibilityError` short-circuits the whole project),
// so reporting those partial findings would make the batched member stricter
// than the solo one.
func TestExecuteStaticcheckGroupCompatSilencesItsMemberWholesale(t *testing.T) {
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a", "b")
	stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return []byte(
			compatLine("b/gen.go") +
				"b/other.go:4:4: assignment to nil map (SA5000)\n" +
				"a/x.go:3:2: assignment to nil map (SA5000)\n",
		), fmt.Errorf("exit status 1")
	})

	results := runStaticcheckBatchOn(t, root, "a", "b")
	b := resultByID(t, results, "/b")
	if b.Status != "OK" || len(b.Diagnostics) != 0 {
		t.Fatalf("a skipped member must report nothing, like its solo run: %+v", b)
	}
	if a := resultByID(t, results, "/a"); a.Status != "FAILED" || len(a.Diagnostics) != 1 {
		t.Fatalf("member a must keep its own finding: %+v", a)
	}
}

// TestExecuteStaticcheckGroupUnnamedCompatDoesNotSilenceAttributedFindings
// covers the fallback arm: compat evidence whose two signature halves land on
// separate lines names no member, so it may not silence one. It only keeps the
// run from being reported as an unattributable crash when nothing else was
// charged.
func TestExecuteStaticcheckGroupUnnamedCompatDoesNotSilenceAttributedFindings(t *testing.T) {
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a", "b")
	stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return []byte(
			"-: internal error: loading packages\n" +
				"-: unsupported version 24\n" +
				"a/x.go:3:2: assignment to nil map (SA5000)\n",
		), fmt.Errorf("exit status 1")
	})

	results := runStaticcheckBatchOn(t, root, "a", "b")
	if a := resultByID(t, results, "/a"); a.Status != "FAILED" || len(a.Diagnostics) != 1 {
		t.Fatalf("unnamed compat evidence must not silence an attributed finding: %+v", a)
	}
	if b := resultByID(t, results, "/b"); b.Status != "OK" || len(b.Diagnostics) != 0 {
		t.Fatalf("member b was charged for evidence naming nobody: %+v", b)
	}
}

// TestExecuteStaticcheckGroupUnnamedCompatAloneSkipsTheGroup is the other half
// of the fallback: with nothing else attributable, split-line compat evidence
// stays the solo path's skip rather than becoming a group-wide crash failure.
func TestExecuteStaticcheckGroupUnnamedCompatAloneSkipsTheGroup(t *testing.T) {
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a", "b")
	stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return []byte("-: internal error: loading packages\n-: unsupported version 24\n"),
			fmt.Errorf("exit status 1")
	})

	for _, result := range runStaticcheckBatchOn(t, root, "a", "b") {
		if result.Status != "OK" || len(result.Diagnostics) != 0 {
			t.Fatalf("%s = %+v, want the solo path's skip-as-OK", result.ProjectID, result)
		}
	}
}

// TestExecuteStaticcheckGroupAttributesPatternError guards the one failure a
// batch can see that a solo run reports differently: staticcheck rejects an
// unloadable package pattern with a position-less `-: pattern ./b/...: …` line.
// It names exactly one member, so it must fail exactly that member — the solo
// run of `b` fails too — and must not turn its batch-mates red or, worse, be
// dropped as unattributed noise while `b` reports green.
func TestExecuteStaticcheckGroupAttributesPatternError(t *testing.T) {
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a", "b")
	stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return []byte(
			"-: pattern ./b/...: directory prefix b does not contain modules " +
				"listed in go.work or their selected dependencies (compile)\n" +
				"a/x.go:3:2: assignment to nil map (SA5000)\n",
		), fmt.Errorf("exit status 1")
	})

	results := runStaticcheckBatchOn(t, root, "a", "b")
	b := resultByID(t, results, "/b")
	if b.Status != "FAILED" || len(b.Diagnostics) != 1 {
		t.Fatalf("pattern error not attributed to its own member: %+v", b)
	}
	if !strings.Contains(b.Diagnostics[0].Description, "does not contain modules") {
		t.Fatalf("member b lost the pattern error text: %+v", b.Diagnostics[0])
	}
	a := resultByID(t, results, "/a")
	if a.Status != "FAILED" || len(a.Diagnostics) != 1 ||
		a.Diagnostics[0].File != "a/x.go" {
		t.Fatalf("member a must keep only its own finding: %+v", a)
	}
}

// TestExecuteStaticcheckGroupUnattributedFailureFailsClosed: a run that fails
// with nothing any member can be charged for (a crash, a bad flag) fails the
// whole group and carries the raw tail, rather than reporting every member green
// from an answer nobody could read.
func TestExecuteStaticcheckGroupUnattributedFailureFailsClosed(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "lint-batch-equivalence", "an-unattributable-batch-failure-fails-closed")
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a", "b")
	stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return []byte("staticcheck: flag provided but not defined: -bogus\n"), fmt.Errorf("exit status 2")
	})

	for _, result := range runStaticcheckBatchOn(t, root, "a", "b") {
		if result.Status != "FAILED" {
			t.Fatalf("%s must fail closed on an unreadable answer: %+v", result.ProjectID, result)
		}
		if len(result.Diagnostics) != 1 ||
			!strings.Contains(result.Diagnostics[0].Description, "flag provided but not defined") {
			t.Fatalf("%s should carry the raw error tail: %+v", result.ProjectID, result.Diagnostics)
		}
	}
}

// TestRunStaticcheckBatchMissingBinaryFailsEveryMemberWithTheInstallHint keeps
// the solo path's actionable message when the pinned tool cannot be resolved.
func TestRunStaticcheckBatchMissingBinaryFailsEveryMemberWithTheInstallHint(t *testing.T) {
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a", "b")
	origBin := resolveStaticcheckBinary
	resolveStaticcheckBinary = func(string) (string, error) {
		return "", fmt.Errorf("staticcheck not found")
	}
	t.Cleanup(func() { resolveStaticcheckBinary = origBin })

	for _, result := range runStaticcheckBatchOn(t, root, "a", "b") {
		if result.Status != "FAILED" || len(result.Diagnostics) != 1 {
			t.Fatalf("%s = %+v, want a failed member", result.ProjectID, result)
		}
		if !strings.Contains(result.Diagnostics[0].Description, "putnami deps install --tag go") {
			t.Fatalf("%s lost the install hint: %+v", result.ProjectID, result.Diagnostics[0])
		}
	}
}

// TestLintRunDispatchesStaticcheckSelectionToTheBatch pins the dispatch itself:
// a populated selection carrying `--tool staticcheck` must reach the staticcheck
// batch, not the golangci one and not the singleton path.
func TestLintRunDispatchesStaticcheckSelectionToTheBatch(t *testing.T) {
	root := t.TempDir()
	staticcheckWorkspace(t, root, "a")
	invocations := stubStaticcheck(t, func(recordedStaticcheck) ([]byte, error) {
		return nil, nil
	})
	golangci := stubGolangci(t, func(recordedInvocation) ([]byte, error) {
		return []byte("0 issues.\n"), nil
	})

	ctx := &pctx.Context{
		WorkspaceRoot:    root,
		Job:              pctx.Job{Name: "lint"},
		Project:          pctx.Project{Name: "a", Path: "a", FullPath: filepath.Join(root, "a")},
		SelectedProjects: staticcheckProjectRefs(root, "a"),
	}
	status, data, err := Run(ctx, jsonl.New(), []string{"--tool", "staticcheck"})
	if err != nil || status != "OK" {
		t.Fatalf("Run = %q, %v", status, err)
	}
	if _, ok := data["batchResults"]; !ok {
		t.Fatalf("staticcheck selection did not take the batch protocol: %+v", data)
	}
	if len(*invocations) != 1 {
		t.Fatalf("want exactly one staticcheck invocation, got %d", len(*invocations))
	}
	if len(*golangci) != 0 {
		t.Fatalf("staticcheck selection leaked into the golangci batch: %+v", *golangci)
	}
}
