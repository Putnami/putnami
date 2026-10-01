package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The human-notice seam is an ACCEPTANCE CRITERION: the MCP
// adapter's stdout IS the JSON-RPC frame stream, so a single "Nothing to do."
// line written there desynchronizes the session's framing and kills the agent
// connection. Request.Stdout is how an adapter takes that stream over; these
// tests fail before a stage that bypasses it ships, not after.

// TestEngineWritesNoticesOnlyThroughTheRequestSeam is the structural half: after
// A4 exactly ONE production statement in this package names os.Stdout, and it is
// the default inside Request.stdout(). A stage that reaches for os.Stdout again
// silently escapes every adapter that supplied a writer.
func TestEngineWritesNoticesOnlyThroughTheRequestSeam(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read engine package dir: %v", err)
	}
	var sites []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			// Comments name the symbol on purpose (the seam is documented where it
			// is used); only statements count.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "os.Stdout") {
				sites = append(sites, name+":"+strconv.Itoa(i+1))
			}
		}
	}
	if len(sites) != 1 || !strings.HasPrefix(sites[0], "engine.go:") {
		t.Fatalf("os.Stdout statements in internal/engine = %v, want exactly one in engine.go "+
			"(the default inside Request.stdout()).\n"+
			"  Every notice the engine prints must go through req.stdout(), or the MCP adapter's "+
			"JSON-RPC transport gets it.", sites)
	}
}

// TestRun_RequestStdoutCapturesPreExecutionNotices is the behavioral half over
// the stages an adapter reaches without executing anything — project selection,
// planning, and the --plan table. Each one wrote straight to the process stdout
// before A4.
func TestRun_RequestStdoutCapturesPreExecutionNotices(t *testing.T) {
	cases := []struct {
		name     string
		projects string
		want     string
		wantCode int
	}{
		{
			name:     "no projects matched",
			projects: "does-not-exist",
			want:     "No projects matched",
			wantCode: ExitUsage,
		},
		{
			name:     "no jobs matched",
			projects: "*",
			want:     "No jobs matched",
			wantCode: ExitUsage,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wsRoot := noticeFixtureWorkspace(t)
			var notices strings.Builder
			req := &Request{
				WorkspaceRoot: wsRoot,
				Config:        wsproto.Load(wsRoot),
				Commands:      []string{"build"},
				Stdout:        &notices,
			}
			req.Global.Projects = tc.projects

			leaked := captureStdout(t, func() {
				_ = captureStderr(t, func() {
					if code := selectAndPlan(req); code != tc.wantCode {
						t.Errorf("exit code = %d, want %d", code, tc.wantCode)
					}
				})
			})
			if leaked != "" {
				t.Errorf("notice leaked to os.Stdout despite Request.Stdout: %q", leaked)
			}
			if !strings.Contains(notices.String(), tc.want) {
				t.Errorf("Request.Stdout = %q, want it to carry %q", notices.String(), tc.want)
			}
		})
	}
}

// TestPrintPlan_WritesToTheSuppliedWriter covers the third notice-producing
// stage: --plan renders the whole table, which is the largest thing that could
// land on a JSON-RPC transport.
func TestPrintPlan_WritesToTheSuppliedWriter(t *testing.T) {
	f := newExecuteFixture(t)
	var table strings.Builder
	leaked := captureStdout(t, func() { printPlan(&table, f.planned, []*workspace.Project{f.project}) })
	if leaked != "" {
		t.Errorf("PrintPlan leaked to os.Stdout: %q", leaked)
	}
	for _, want := range []string{"proj", "1 jobs"} {
		if !strings.Contains(table.String(), want) {
			t.Errorf("plan table = %q, want it to carry %q", table.String(), want)
		}
	}
}

// The footer's project count covers every project that owns a planned job,
// which is more than the selection when a job's `^step` need pulled a
// dependency in. The footer says so, and only then: a plan with no dependency
// builds prints the footer it always has.
func TestPrintPlan_FooterSeparatesSelectedFromDependencyBuilds(t *testing.T) {
	f := newExecuteFixture(t)
	dep := &workspace.Project{ID: "/dep", Name: "dep", Path: "dep"}
	depJob := &jobs.ScheduledJob{Project: dep, Extension: f.ext, JobDef: &extension.JobDefinition{
		Name: "build", ExtensionName: "@putnami/test", Command: f.procPath,
	}}

	var table strings.Builder
	printPlan(&table, append(f.planned, depJob), []*workspace.Project{f.project})
	if want := "  2 jobs  ·  0 edges  ·  2 projects (1 selected · 1 dependency builds)\n"; !strings.Contains(table.String(), want) {
		t.Errorf("plan table = %q, want it to carry %q", table.String(), want)
	}

	table.Reset()
	printPlan(&table, f.planned, []*workspace.Project{f.project})
	if want := "  1 jobs  ·  0 edges  ·  1 projects\n"; !strings.Contains(table.String(), want) {
		t.Errorf("plan table = %q, want the unchanged footer %q", table.String(), want)
	}

	// A selected project that planned nothing is not counted as selected: the
	// number must never exceed what the plan actually holds.
	table.Reset()
	printPlan(&table, append(f.planned, depJob), []*workspace.Project{f.project, {ID: "/idle", Name: "idle", Path: "idle"}})
	if want := "2 projects (1 selected · 1 dependency builds)"; !strings.Contains(table.String(), want) {
		t.Errorf("plan table = %q, want %q", table.String(), want)
	}
}

// TestPrintPlan_NamesWhatCannotConsultTheCache is the readability half of
// this: `--plan` used to print MISS for a task that never asks the cache at
// all, so a cold cache and a structurally uncacheable task looked identical —
// which is how a whole command's tasks executed on every run without anyone
// reading it as a defect. A job that cannot consult the cache now says so.
func TestPrintPlan_NamesWhatCannotConsultTheCache(t *testing.T) {
	f := newExecuteFixture(t)
	declared := &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
		"owned": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootCommandOutput, Path: "owned"},
	}}
	cacheable := &jobs.ScheduledJob{
		Project:   f.project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Tasks: map[string]extension.TaskDefinition{"owned": {Declares: declared}}},
		JobDef:    &extension.JobDefinition{Name: "package~owned", Cache: true},
		Step:      &extension.PipelineStep{Task: "owned"},
	}
	// Same command, same cache:true — but no declaration, so nothing states what
	// a restore would have to reproduce and the job never consults the cache.
	undeclared := &jobs.ScheduledJob{
		Project:   f.project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Tasks: map[string]extension.TaskDefinition{"legacy": {}}},
		JobDef:    &extension.JobDefinition{Name: "package~legacy", Cache: true},
		Step:      &extension.PipelineStep{Task: "legacy"},
	}

	var table strings.Builder
	printPlan(&table, []*jobs.ScheduledJob{cacheable, undeclared}, []*workspace.Project{f.project})

	rendered := table.String()
	for _, line := range strings.Split(rendered, "\n") {
		switch {
		case strings.Contains(line, "package~owned"):
			if !strings.Contains(line, "MISS") {
				t.Errorf("a declared, cache-eligible package task rendered as %q, want MISS", strings.TrimSpace(line))
			}
		case strings.Contains(line, "package~legacy"):
			if !strings.Contains(line, "NO-CACHE") {
				t.Errorf("a job that cannot consult the cache rendered as %q, want NO-CACHE", strings.TrimSpace(line))
			}
		}
	}
	if !strings.Contains(rendered, "NO-CACHE") {
		t.Fatalf("plan table never printed NO-CACHE:\n%s", rendered)
	}
}

// noticeFixtureWorkspace writes a one-project workspace with no extensions, so
// selection succeeds and planning finds nothing to do.
func noticeFixtureWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"notice-fixture","includes":["app"]}`)
	write("app/putnami.json", `{"name":"app"}`)
	workspace.InvalidateLoadCache(dir)
	return dir
}

// selectAndPlan drives the two pre-execution stages that print on the human
// stream, without constructing a scheduler.
func selectAndPlan(req *Request) int {
	ws, err := workspace.Load(req.WorkspaceRoot)
	if err != nil {
		return ExitError
	}
	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		return code
	}
	if _, code := buildPlan(req, ws, selected, nil, nil); code != ExitSuccess {
		return code
	}
	return ExitSuccess
}

func TestStructuredNoOpSelectionAndPlanningEmitNoHumanNotices(t *testing.T) {
	for _, mode := range []string{"json", "jsonl"} {
		for _, projects := range []string{"does-not-exist", "*"} {
			t.Run(mode+"/"+projects, func(t *testing.T) {
				root := noticeFixtureWorkspace(t)
				var notices strings.Builder
				req := &Request{WorkspaceRoot: root, Config: wsproto.Load(root), Commands: []string{"build"}, Stdout: &notices}
				req.Global.Output = mode
				req.Global.Projects = projects
				code := ExitSuccess
				_ = captureStderr(t, func() { code = selectAndPlan(req) })
				if code != ExitUsage {
					t.Fatalf("exit code = %d, want %d", code, ExitUsage)
				}
				if notices.Len() != 0 {
					t.Fatalf("human notice corrupted %s output: %q", mode, notices.String())
				}
			})
		}
	}
}

// TestAFailingNoOpNamesItsCauseOnStderrInEveryOutputFormat is the other half of
// the test above, and the two are only correct together: suppressing the human
// notice keeps the machine document clean, but it must not leave the FAILING
// exit silent. `--output=json` over an empty selection or an empty plan exited 2
// with nothing on stdout, nothing on stderr and no envelope, so a caller could
// not tell a refused selection from a crash — the failure mode that made an
// intermittent CI red unreadable for four runs.
//
// The assertion is over the machine formats AND the human one: the diagnostic
// is the failure's explanation, not a fallback for when stdout is taken.
func TestAFailingNoOpNamesItsCauseOnStderrInEveryOutputFormat(t *testing.T) {
	cases := []struct {
		name     string
		projects string
		want     []string
	}{
		{
			name:     "empty selection names the selector and the workspace size",
			projects: "does-not-exist",
			want:     []string{"no project matched", `--projects "does-not-exist"`, "1 project(s)"},
		},
		{
			name:     "empty plan names the commands, the projects and the extension count",
			projects: "*",
			want:     []string{"no job matched", "build", "1 selected project(s)", "app", "0 loaded extension(s)"},
		},
	}
	for _, tc := range cases {
		for _, mode := range []string{"", "json", "jsonl"} {
			t.Run(tc.name+"/output="+mode, func(t *testing.T) {
				root := noticeFixtureWorkspace(t)
				req := &Request{
					WorkspaceRoot: root,
					Config:        wsproto.Load(root),
					Commands:      []string{"build"},
					Stdout:        &strings.Builder{},
				}
				req.Global.Output = mode
				req.Global.Projects = tc.projects
				code := ExitSuccess
				diagnostic := captureStderr(t, func() { code = selectAndPlan(req) })
				if code != ExitUsage {
					t.Fatalf("exit code = %d, want %d", code, ExitUsage)
				}
				for _, want := range tc.want {
					if !strings.Contains(diagnostic, want) {
						t.Errorf("stderr = %q, want it to carry %q; exit %d has to explain itself "+
							"on the stream no output format owns", diagnostic, want, ExitUsage)
					}
				}
			})
		}
	}
}

// TestTheSelectionDiagnosticQuotesTheRequestNotTheOutcome pins the wording
// choice: an empty selection is a question about what was ASKED FOR, so the
// line has to carry the selector and any filter that narrowed it. A line that
// only reported "0 projects" would send a reader back to the command they
// already have.
func TestTheSelectionDiagnosticQuotesTheRequestNotTheOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		flags GlobalFlags
		want  string
	}{
		{name: "explicit projects", flags: GlobalFlags{Projects: "/a,/b"}, want: `--projects "/a,/b"`},
		{name: "star is the default", flags: GlobalFlags{Projects: "*"}, want: "the default selection (every project)"},
		{name: "unset is the default", flags: GlobalFlags{}, want: "the default selection (every project)"},
		{name: "impacted mode", flags: GlobalFlags{Impacted: true}, want: "the impacted selection"},
		{
			name:  "the impacted sentinel is a mode, not a selector",
			flags: GlobalFlags{Projects: impactedProjectsSentinel},
			want:  "the impacted selection",
		},
		{
			name:  "filters are part of the request",
			flags: GlobalFlags{Projects: "/a", FilterTag: "go", ExcludeTag: "e2e", Exclude: "/b"},
			want:  `--projects "/a" filtered by --filter-tag "go" --exclude-tag "e2e" --exclude "/b"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := describeProjectSelection(tc.flags); got != tc.want {
				t.Errorf("describeProjectSelection = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheEmptyPlanDiagnosticElidesALongProjectList keeps the line readable in a
// CI log: the count is exact and precedes the ids, so truncating the list costs
// a reader nothing.
func TestTheEmptyPlanDiagnosticElidesALongProjectList(t *testing.T) {
	t.Parallel()
	if got := summarizeProjectIDs(nil); got != "none" {
		t.Errorf("summarizeProjectIDs(nil) = %q, want %q", got, "none")
	}
	many := make([]*workspace.Project, 0, summarizeProjectIDsLimit+3)
	for i := range cap(many) {
		many = append(many, &workspace.Project{ID: "/p" + strconv.Itoa(i)})
	}
	got := summarizeProjectIDs(many)
	if !strings.HasSuffix(got, ", …") {
		t.Errorf("summarizeProjectIDs(%d projects) = %q, want it elided", len(many), got)
	}
	if strings.Count(got, ",") != summarizeProjectIDsLimit {
		t.Errorf("summarizeProjectIDs(%d projects) = %q, want %d ids before the elision",
			len(many), got, summarizeProjectIDsLimit)
	}
	if strings.Contains(got, "/p"+strconv.Itoa(summarizeProjectIDsLimit)) {
		t.Errorf("summarizeProjectIDs = %q, want no id past the limit", got)
	}
}
