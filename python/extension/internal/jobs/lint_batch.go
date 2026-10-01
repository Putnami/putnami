package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/python/extension/internal/parse"
	"go.putnami.dev/python/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// Batch lint modes. When the scheduler groups compatible projects (same tool +
// config digest) it runs the extension ONCE with ctx.SelectedProjects set; the
// lint jobs fork here so ruff scans every grouped project in a single process
// instead of once per project.
const (
	lintBatchFormat = "format"
	lintBatchCheck  = "check"
)

// ruffExecRun runs a subprocess and returns its captured result. It is a
// package-level seam so tests can assert a single ruff invocation and inject
// synthetic output without a real uv/ruff toolchain.
var ruffExecRun = exec.Run

// syncWorkspace performs the UV workspace sync. It is a seam so lint tests can
// bypass real `uv` resolution, which is not available in every CI environment.
var syncWorkspace = SyncWorkspace

// ruffBatchDiagnostic is the wire shape the scheduler expects per diagnostic.
// The json tags MUST match tooling/cli batchWireDiagnostic exactly, otherwise
// the per-project split silently drops fields.
type ruffBatchDiagnostic struct {
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
}

// ruffBatchSummary matches tooling/cli batchWireSummary.
type ruffBatchSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Infos    int `json:"infos"`
}

// lintBatchProjectResult is one entry of the "batchResults" payload. Every
// selected project must appear exactly once or the scheduler fails the whole
// split (fail-closed). Tags mirror tooling/cli batchWireResult.
type lintBatchProjectResult struct {
	ProjectID   string                `json:"projectId"`
	Status      string                `json:"status"`
	Diagnostics []ruffBatchDiagnostic `json:"diagnostics,omitempty"`
	Summary     ruffBatchSummary      `json:"summary,omitempty"`
}

// ruffBatchReport is the parsed result of a single ruff invocation over all
// grouped project paths.
type ruffBatchReport struct {
	findings []parse.RuffFinding
	success  bool
	rawTail  string
}

// runLintBatch runs ruff ONCE over every selected project and splits the
// findings back to their owning projects. It is the leader path of a batched
// lint-format / lint-check task.
func runLintBatch(ctx *pctx.Context, emit *jsonl.Emitter, mode string, fix bool) (string, map[string]any, error) {
	projects := append([]pctx.ProjectRef(nil), ctx.SelectedProjects...)
	if len(projects) == 0 {
		return "FAILED", nil, fmt.Errorf("lint batch requires at least one selected project")
	}
	wsRoot := ctx.WorkspaceRoot

	// Synchronize the UV workspace exactly once for the grouped invocation.
	if !syncWorkspace(wsRoot, emit) {
		return "FAILED", nil, nil
	}

	targets, err := ruffBatchTargets(projects)
	if err != nil {
		return "FAILED", nil, err
	}

	// ctx.Project is the batch leader (misses[0]); resolve uv against it. ruff
	// is a standalone tool (added via `--with ruff`), so the leader's package
	// only selects the uv environment, never which files get linted.
	packageName := ResolvePackageName(ctx)
	cacheDir := filepath.Join(wsRoot, ".putnami/bin/extensions/putnami-python/.ruff_cache")

	var report ruffBatchReport
	switch mode {
	case lintBatchFormat:
		report, err = runRuffFormat(wsRoot, cacheDir, packageName, targets, fix)
	case lintBatchCheck:
		report, err = runRuffCheck(wsRoot, cacheDir, packageName, targets, fix)
	default:
		return "FAILED", nil, fmt.Errorf("unsupported lint batch mode %q", mode)
	}
	if err != nil {
		return "FAILED", nil, err
	}

	results := splitRuffBatchReport(wsRoot, projects, report, mode)
	// The subprocess itself produced the split protocol. Per-project failures
	// live INSIDE batchResults so the scheduler keeps independent cache/DAG
	// outcomes rather than failing every peer.
	return "OK", map[string]any{"batchResults": results}, nil
}

// ruffBatchTargets converts selected projects into workspace-relative ruff
// targets. Passing project directories (not `--config`) lets ruff resolve each
// file's configuration by walking UP the tree, preserving per-project
// pyproject.toml / ruff.toml / .ruff.toml discovery.
func ruffBatchTargets(projects []pctx.ProjectRef) ([]string, error) {
	targets := make([]string, 0, len(projects))
	for _, p := range projects {
		rel := filepath.ToSlash(filepath.Clean(p.Path))
		if p.Path == "" || rel == "." {
			// The workspace-root project lints from the root; ruff walks up per
			// file regardless.
			targets = append(targets, ".")
			continue
		}
		if filepath.IsAbs(p.Path) || rel == ".." || strings.HasPrefix(rel, "../") {
			return nil, fmt.Errorf("ruff batch: project %q path %q escapes workspace root", p.Name, p.Path)
		}
		targets = append(targets, rel)
	}
	return targets, nil
}

// runRuffFormat runs `ruff format` once over all targets. Without --fix it adds
// --check so ruff reports (instead of writing) each file that needs formatting.
func runRuffFormat(wsRoot, cacheDir, packageName string, targets []string, fix bool) (ruffBatchReport, error) {
	ruffArgs := append([]string{"format"}, targets...)
	ruffArgs = append(ruffArgs, "--cache-dir", cacheDir)
	if !fix {
		ruffArgs = append(ruffArgs, "--check")
	}
	res, err := runRuff(wsRoot, packageName, ruffArgs)
	if err != nil {
		return ruffBatchReport{}, err
	}
	combined := res.Stdout + "\n" + res.Stderr
	files := parse.RuffFormatFiles(combined)
	findings := make([]parse.RuffFinding, 0, len(files))
	for _, f := range files {
		findings = append(findings, parse.RuffFinding{
			File:     f,
			Code:     "format",
			Message:  "File would be reformatted",
			Severity: "error",
		})
	}
	return ruffBatchReport{findings: findings, success: res.Success, rawTail: toolchain.TailText(res.Stderr, res.Stdout, 10)}, nil
}

// runRuffCheck runs `ruff check --output-format=json` once over all targets.
// JSON output is required so each finding carries its filename for reliable
// per-project attribution.
func runRuffCheck(wsRoot, cacheDir, packageName string, targets []string, fix bool) (ruffBatchReport, error) {
	ruffArgs := []string{"check", "--output-format=json"}
	ruffArgs = append(ruffArgs, targets...)
	ruffArgs = append(ruffArgs, "--cache-dir", cacheDir)
	if fix {
		ruffArgs = append(ruffArgs, "--fix")
	}
	res, err := runRuff(wsRoot, packageName, ruffArgs)
	if err != nil {
		return ruffBatchReport{}, err
	}
	findings, ok := parse.RuffCheckJSONFindings(res.Stdout)
	if !ok {
		// Not JSON (e.g. a ruff crash): keep the tool's success flag so the
		// split fails every project closed rather than reporting a false pass.
		return ruffBatchReport{findings: nil, success: res.Success, rawTail: toolchain.TailText(res.Stderr, res.Stdout, 10)}, nil
	}
	return ruffBatchReport{findings: findings, success: res.Success, rawTail: toolchain.TailText(res.Stderr, res.Stdout, 10)}, nil
}

func runRuff(wsRoot, packageName string, ruffArgs []string) (*exec.Result, error) {
	cmdArgs := toolchain.UVRunWithToolArgs("ruff", packageName, wsRoot, ruffArgs...)
	return ruffExecRun(cmdArgs[0], cmdArgs[1:], exec.Dir(wsRoot), exec.Env(batchRuffEnv(wsRoot)))
}

// batchRuffEnv disables color so the "Would reformat:" paths and JSON stay
// clean for parsing, pins the workspace env vars the toolchain expects, and
// keeps uv's managed Pythons and cache in the workspace (toolchain.UVDirEnv).
// exec.Env layers this map onto the inherited environment.
func batchRuffEnv(wsRoot string) map[string]string {
	env := map[string]string{
		"NO_COLOR":            "1",
		"PUTNAMI_WORKSPACE":   wsRoot,
		"PUTNAMI_WORKING_DIR": wsRoot,
	}
	for k, v := range toolchain.UVDirEnv(wsRoot, os.Environ()) {
		env[k] = v
	}
	return env
}

// splitRuffBatchReport attributes each finding to the owning project by longest
// matching workspace path prefix, mirroring the TypeScript/biome split. A
// project fails only when ruff failed AND that project owns errors; findings
// that match no project fan out to every project and fail them all. When a
// project is failed without any parseable finding (ruff rejected the config or
// crashed before emitting JSON/paths), the captured error tail is forwarded as
// a diagnostic so the cause is not lost — the batch analog of the fallback
// diagnostic in the single-project LintCheck/LintFormat paths. mode is
// "format" or "check", used only for the fallback message.
func splitRuffBatchReport(wsRoot string, projects []pctx.ProjectRef, report ruffBatchReport, mode string) []lintBatchProjectResult {
	results := make([]lintBatchProjectResult, len(projects))
	indexByID := make(map[string]int, len(projects))
	for i, p := range projects {
		results[i] = lintBatchProjectResult{ProjectID: p.ID, Status: "OK"}
		indexByID[p.ID] = i
	}

	type projectPrefix struct {
		id   string
		path string
	}
	prefixes := make([]projectPrefix, 0, len(projects))
	rootProjectID := ""
	for _, p := range projects {
		pp := filepath.ToSlash(filepath.Clean(p.Path))
		if pp == "." || p.Path == "" {
			rootProjectID = p.ID
			continue
		}
		prefixes = append(prefixes, projectPrefix{id: p.ID, path: pp})
	}
	sort.SliceStable(prefixes, func(i, j int) bool {
		return len(prefixes[i].path) > len(prefixes[j].path)
	})

	unattributedFailure := false
	for _, f := range report.findings {
		file := batchWorkspacePath(wsRoot, f.File)
		diagnostic := ruffBatchDiagnostic{
			Category:    f.Code,
			Severity:    severityOrError(f.Severity),
			Description: f.Message,
			File:        file,
			Line:        f.Line,
			Column:      f.Column,
		}
		projectID := ""
		for _, prefix := range prefixes {
			if file == prefix.path || strings.HasPrefix(file, prefix.path+"/") {
				projectID = prefix.id
				break
			}
		}
		if projectID == "" {
			projectID = rootProjectID
		}
		if projectID == "" {
			if diagnostic.Severity == "error" {
				unattributedFailure = true
			}
			for i := range results {
				appendRuffDiagnostic(&results[i], diagnostic)
			}
			continue
		}
		appendRuffDiagnostic(&results[indexByID[projectID]], diagnostic)
	}

	anyAttributedError := false
	if !report.success {
		for i := range results {
			if results[i].Summary.Errors > 0 {
				results[i].Status = "FAILED"
				anyAttributedError = true
			}
		}
	}
	if !report.success && (!anyAttributedError || unattributedFailure) {
		for i := range results {
			results[i].Status = "FAILED"
		}
	}

	// Give every failed-but-diagnostic-less project the captured error tail so a
	// config error or crash surfaces its cause instead of a bare FAILED status.
	if !report.success {
		for i := range results {
			if results[i].Status != "FAILED" || len(results[i].Diagnostics) > 0 {
				continue
			}
			desc := fmt.Sprintf("Ruff %s failed", mode)
			if tail := strings.TrimSpace(report.rawTail); tail != "" {
				desc += ":\n" + tail
			}
			appendRuffDiagnostic(&results[i], ruffBatchDiagnostic{
				Category:    "ruff",
				Severity:    "error",
				Description: desc,
			})
		}
	}
	return results
}

func appendRuffDiagnostic(result *lintBatchProjectResult, diagnostic ruffBatchDiagnostic) {
	switch diagnostic.Severity {
	case "error":
		result.Summary.Errors++
	case "warning":
		result.Summary.Warnings++
	default:
		result.Summary.Infos++
	}
	result.Diagnostics = append(result.Diagnostics, diagnostic)
}

func severityOrError(severity string) string {
	if severity == "" {
		return "error"
	}
	return severity
}

// batchWorkspacePath normalizes a ruff-reported path (absolute for JSON check
// findings, workspace-relative for format findings) to a clean slash-separated
// workspace-relative path for prefix attribution.
func batchWorkspacePath(wsRoot, path string) string {
	if path == "" {
		return ""
	}
	native := filepath.Clean(filepath.FromSlash(path))
	if filepath.IsAbs(native) {
		if rel, err := filepath.Rel(wsRoot, native); err == nil &&
			rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			native = rel
		}
	}
	return filepath.ToSlash(strings.TrimPrefix(filepath.Clean(native), "."+string(filepath.Separator)))
}
