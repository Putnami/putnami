package deliverycli

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	deliveryapiclient "go.putnami.dev/cloud/clients/delivery-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// ciResultsOutputLines bounds how many lines of one failed case's output the
// human view prints. The server keeps up to 4096 characters per case; the JSON
// document carries all of them.
const ciResultsOutputLines = 20

// CIRunResultsResponse twins the control plane's derived results document
// (the delivery API's CIRunResultsResponse): per-test results,
// per-project coverage, lint diagnostics and publications, read from the
// run's event stream.
type CIRunResultsResponse struct {
	RunID                 string                `json:"runId"`
	SessionID             string                `json:"sessionId,omitempty"`
	Tests                 []CIRunTestTask       `json:"tests"`
	ElidedTestTasks       int                   `json:"elidedTestTasks"`
	OmittedCases          int                   `json:"omittedCases"`
	Coverage              []CIRunCoverage       `json:"coverage"`
	ElidedCoverage        int                   `json:"elidedCoverage"`
	Diagnostics           []CIRunDiagnosticTask `json:"diagnostics"`
	ElidedDiagnosticTasks int                   `json:"elidedDiagnosticTasks"`
	Publications          []CIRunPublication    `json:"publications"`
	ElidedPublications    int                   `json:"elidedPublications"`
	MalformedLines        int                   `json:"malformedLines"`
	DerivedAt             time.Time             `json:"derivedAt"`
}

// CIRunTestTask is one test task and its cases, failed first.
type CIRunTestTask struct {
	Key       string          `json:"key"`
	Project   string          `json:"project"`
	ProjectID string          `json:"projectId,omitempty"`
	Task      string          `json:"task"`
	Status    string          `json:"status,omitempty"`
	Passed    int             `json:"passed"`
	Failed    int             `json:"failed"`
	Skipped   int             `json:"skipped"`
	Dropped   int             `json:"dropped,omitempty"`
	Omitted   int             `json:"omitted,omitempty"`
	Cases     []CIRunTestCase `json:"cases"`
}

// CIRunTestCase is one test case.
type CIRunTestCase struct {
	Name            string `json:"name"`
	Suite           string `json:"suite"`
	Status          string `json:"status"`
	DurationMs      int64  `json:"durationMs"`
	Output          string `json:"output,omitempty"`
	OutputTruncated bool   `json:"outputTruncated,omitempty"`
	File            string `json:"file,omitempty"`
	Line            int64  `json:"line,omitempty"`
}

// CIRunCoverage is one task's coverage summary.
type CIRunCoverage struct {
	Key            string   `json:"key"`
	Project        string   `json:"project"`
	ProjectID      string   `json:"projectId,omitempty"`
	Task           string   `json:"task"`
	Percentage     float64  `json:"percentage"`
	Granularity    string   `json:"granularity,omitempty"`
	Covered        int64    `json:"covered,omitempty"`
	Total          int64    `json:"total,omitempty"`
	Threshold      *float64 `json:"threshold,omitempty"`
	BelowThreshold bool     `json:"belowThreshold,omitempty"`
}

// CIRunDiagnosticTask is one task's lint counts and diagnostics.
type CIRunDiagnosticTask struct {
	Key       string            `json:"key"`
	Project   string            `json:"project"`
	ProjectID string            `json:"projectId,omitempty"`
	Task      string            `json:"task"`
	Status    string            `json:"status,omitempty"`
	Errors    int               `json:"errors"`
	Warnings  int               `json:"warnings"`
	Infos     int               `json:"infos"`
	Omitted   int               `json:"omitted,omitempty"`
	Items     []CIRunDiagnostic `json:"items"`
}

// CIRunDiagnostic is one diagnostic.
type CIRunDiagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int64  `json:"line,omitempty"`
	Column   int64  `json:"column,omitempty"`
	Message  string `json:"message"`
}

// CIRunPublication is one published artifact or release member.
type CIRunPublication struct {
	Key           string   `json:"key"`
	Project       string   `json:"project"`
	ProjectID     string   `json:"projectId,omitempty"`
	Task          string   `json:"task"`
	Kind          string   `json:"kind"`
	Registry      string   `json:"registry,omitempty"`
	Name          string   `json:"name"`
	Version       string   `json:"version,omitempty"`
	Digest        string   `json:"digest,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	ContentStatus string   `json:"contentStatus,omitempty"`
	DryRun        bool     `json:"dryRun,omitempty"`
}

// ciResults prints what ONE run's gate found: failed tests with their output,
// coverage per project against its threshold, diagnostics by file and line, and
// what it published. The control plane derives the document from the event
// stream the runner ships, so it reads the same bytes as `timings`.
//
// It fetches the run first, as `timings` does, so a run that does not exist and
// a run with no results give different answers.
func ciResults(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	run := ciRunID(args)
	if run == "" {
		return clicore.NewError("cloud ci results requires a run id: `putnami cloud ci results <run>`", clicore.ExitUsage)
	}
	ctx, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	reqCtx, stop := ciRequestContext(ioctx)
	defer stop()
	runResp, resolved, err := ciRunRequest(reqCtx, ctx, run, func(id string) (*CIRunResponse, error) {
		return ciGetRun(reqCtx, ctx, "results", id, false)
	})
	if err != nil {
		return err
	}
	if runResp.ID != "" {
		resolved = runResp.ID
	}
	target := ctx.WorkspaceURL("/runs/" + url.PathEscape(resolved) + "/insights/results")
	results, err := ciCall[CIRunResultsResponse](reqCtx, ctx, "results", target,
		func(callCtx context.Context, api *deliveryapiclient.DeliveryClient) (*deliveryapiclient.CIRunResultsResponse, error) {
			return api.GetV1WorkspacesRunsInsightsResults(callCtx, deliveryapiclient.GetV1WorkspacesRunsInsightsResultsInput{
				Path: deliveryapiclient.GetV1WorkspacesRunsInsightsResultsPath{Workspace: ctx.WorkspaceID, Run: resolved},
			})
		})
	if err != nil {
		if ciIsNotFound(err) {
			return clicore.NewError("run "+ciShortRunID(resolved)+" carries no results: the control plane derives them from the "+
				"event stream the runner ships, so a run that never dispatched, died before its gate, or ran a runner older than the "+
				"session-record transport has none", clicore.ExitAPI)
		}
		return err
	}
	if results.RunID == "" {
		results.RunID = resolved
	}
	if clicore.StructuredOutput(params) {
		ioctx.Stdout(ciCompactJSON(*results))
		return nil
	}
	ciRenderResults(ioctx, runResp, results)
	return nil
}

// ciRenderResults prints the findings first: failed cases with their output,
// coverage under its threshold, and diagnostics. Passed and skipped cases are
// counted, not listed; `--output=json` carries every case the server kept.
// Every bound the server applied is stated, so a sample never reads as a total.
func ciRenderResults(ioctx clicore.IO, run *CIRunResponse, r *CIRunResultsResponse) {
	ioctx.Stdout(fmt.Sprintf("Results — run %s (%s%s)", ciShortRunID(run.ID), run.Status, ciConclusionSuffix(run.Conclusion)))
	if r.SessionID != "" {
		ioctx.Stdout(fmt.Sprintf("  session   %s", r.SessionID))
	}
	if r.MalformedLines > 0 {
		ioctx.Stdout(fmt.Sprintf("  stream    %d event line(s) were not records: these results are derived from a damaged stream",
			r.MalformedLines))
	}
	ciRenderResultTests(ioctx, r)
	ciRenderResultCoverage(ioctx, r)
	ciRenderResultDiagnostics(ioctx, r)
	ciRenderResultPublications(ioctx, r)
}

func ciRenderResultTests(ioctx clicore.IO, r *CIRunResultsResponse) {
	var passed, failed, skipped int
	for _, task := range r.Tests {
		passed, failed, skipped = passed+task.Passed, failed+task.Failed, skipped+task.Skipped
	}
	ioctx.Stdout(fmt.Sprintf("Tests (%d task(s): %d passed, %d failed, %d skipped):", len(r.Tests), passed, failed, skipped))
	for _, task := range r.Tests {
		line := fmt.Sprintf("  %-9s %-34s %-22s %d passed, %d failed, %d skipped",
			textOrDash(task.Status), task.Project, task.Task, task.Passed, task.Failed, task.Skipped)
		if task.Dropped > 0 {
			line += fmt.Sprintf(" (%d dropped by the test runner)", task.Dropped)
		}
		ioctx.Stdout(line)
		for _, c := range task.Cases {
			if c.Status != "failed" {
				continue
			}
			ioctx.Stdout("      FAIL " + ciResultCaseName(c) + ciResultLocation(c.File, c.Line, 0) +
				" (" + ciDurationMs(c.DurationMs) + ")")
			ciRenderResultOutput(ioctx, c)
		}
		if task.Omitted > 0 {
			ioctx.Stdout(fmt.Sprintf("      … %d case(s) left out by the document's bounds", task.Omitted))
		}
	}
	if r.ElidedTestTasks > 0 {
		ioctx.Stdout(fmt.Sprintf("  … %d further test task(s) have no row: the control plane's task bound left them out", r.ElidedTestTasks))
	}
}

func ciRenderResultOutput(ioctx clicore.IO, c CIRunTestCase) {
	output := strings.TrimRight(c.Output, "\n")
	if output == "" {
		return
	}
	lines := strings.Split(output, "\n")
	shown := lines
	if len(shown) > ciResultsOutputLines {
		shown = shown[:ciResultsOutputLines]
	}
	for _, line := range shown {
		ioctx.Stdout("          " + line)
	}
	switch {
	case len(lines) > len(shown):
		ioctx.Stdout(fmt.Sprintf("          … %d more line(s); --output=json carries the whole output", len(lines)-len(shown)))
	case c.OutputTruncated:
		ioctx.Stdout("          … output truncated at the source")
	}
}

func ciRenderResultCoverage(ioctx clicore.IO, r *CIRunResultsResponse) {
	if len(r.Coverage) == 0 && r.ElidedCoverage == 0 {
		return
	}
	ioctx.Stdout(fmt.Sprintf("Coverage (%d task(s)):", len(r.Coverage)))
	for _, row := range r.Coverage {
		line := fmt.Sprintf("  %6.1f%%  %-34s %-22s", row.Percentage, row.Project, row.Task)
		if row.Threshold != nil {
			line += fmt.Sprintf("  threshold %s%%", strconv.FormatFloat(*row.Threshold, 'f', -1, 64))
			if row.BelowThreshold {
				line += "  BELOW"
			}
		}
		if row.Total > 0 {
			line += fmt.Sprintf("  (%d of %d %s)", row.Covered, row.Total, textOrDash(row.Granularity))
		}
		ioctx.Stdout(line)
	}
	if r.ElidedCoverage > 0 {
		ioctx.Stdout(fmt.Sprintf("  … %d further coverage row(s) left out by the control plane's bound", r.ElidedCoverage))
	}
}

func ciRenderResultDiagnostics(ioctx clicore.IO, r *CIRunResultsResponse) {
	if len(r.Diagnostics) == 0 && r.ElidedDiagnosticTasks == 0 {
		return
	}
	ioctx.Stdout(fmt.Sprintf("Diagnostics (%d task(s) with findings):", len(r.Diagnostics)))
	for _, task := range r.Diagnostics {
		ioctx.Stdout(fmt.Sprintf("  %-9s %-34s %-22s %d error(s), %d warning(s), %d info",
			textOrDash(task.Status), task.Project, task.Task, task.Errors, task.Warnings, task.Infos))
		for _, d := range task.Items {
			line := "      " + d.Severity + ciResultLocation(d.File, d.Line, d.Column)
			if d.Message != "" {
				line += " " + d.Message
			}
			if d.Code != "" {
				line += " (" + d.Code + ")"
			}
			ioctx.Stdout(line)
		}
		if task.Omitted > 0 {
			ioctx.Stdout(fmt.Sprintf("      … %d more diagnostic(s) left out by the per-task bound", task.Omitted))
		}
	}
	if r.ElidedDiagnosticTasks > 0 {
		ioctx.Stdout(fmt.Sprintf("  … %d further task(s) with findings left out by the control plane's bound", r.ElidedDiagnosticTasks))
	}
}

func ciRenderResultPublications(ioctx clicore.IO, r *CIRunResultsResponse) {
	if len(r.Publications) == 0 && r.ElidedPublications == 0 {
		return
	}
	ioctx.Stdout(fmt.Sprintf("Publications (%d):", len(r.Publications)))
	for _, p := range r.Publications {
		line := "  " + p.Name
		if p.Version != "" {
			line += "@" + p.Version
		}
		if p.Registry != "" {
			line += "  " + p.Registry
		}
		if p.Digest != "" {
			line += "  " + p.Digest
		}
		if len(p.Tags) > 0 {
			line += "  tags " + strings.Join(p.Tags, ",")
		}
		if p.ContentStatus != "" {
			line += "  " + p.ContentStatus
		}
		if p.DryRun {
			line += "  (dry run)"
		}
		ioctx.Stdout(line + "  from " + p.Project)
	}
	if r.ElidedPublications > 0 {
		ioctx.Stdout(fmt.Sprintf("  … %d further publication(s) left out by the control plane's bound", r.ElidedPublications))
	}
}

func ciResultCaseName(c CIRunTestCase) string {
	if c.Suite == "" {
		return c.Name
	}
	return c.Suite + " " + c.Name
}

// ciResultLocation renders " file:line:column", leaving out what is unknown.
func ciResultLocation(file string, line, column int64) string {
	if file == "" {
		return ""
	}
	out := " " + file
	if line > 0 {
		out += ":" + strconv.FormatInt(line, 10)
		if column > 0 {
			out += ":" + strconv.FormatInt(column, 10)
		}
	}
	return out
}

func textOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
