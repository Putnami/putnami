package lint

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"go.putnami.dev/go/extension/internal/parse"
	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
)

// Batch lint reuses one golangci-lint process across several compatible
// projects. golangci-lint applies a single --config per invocation, so the
// batch groups the selected projects by their effective config and governing
// go.work root, runs one process per group over the group's module roots with
// absolute output paths, and attributes each finding back to the owning project.
// This preserves per-project statuses, diagnostics, cache entries, and fix
// writes while amortizing golangci-lint's fixed startup and package-loading cost.

// batchProjectResult mirrors the tooling/cli batchWireResult contract exactly;
// the scheduler splits {"batchResults": [...]} back into per-project events.
type batchProjectResult struct {
	ProjectID   string                 `json:"projectId"`
	Status      string                 `json:"status"`
	Data        map[string]any         `json:"data,omitempty"`
	Diagnostics []parse.ToolDiagnostic `json:"diagnostics,omitempty"`
	Summary     batchSummary           `json:"summary,omitempty"`
}

type batchSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Infos    int `json:"infos"`
}

type lintBatchProject struct {
	ref      pctx.ProjectRef
	fullPath string
}

type lintBatchGroup struct {
	config    string
	groupRoot string
	projects  []*lintBatchProject
}

// runGolangciBatch runs one golangci-lint invocation per compatible config
// group and returns ordinary per-project results for the scheduler to split. A
// one-project group still uses this protocol: a mixed cache hit/miss batch can
// leave a single project to execute after the scheduler restores the hits.
func runGolangciBatch(ctx *pctx.Context, options lintOptions) (string, map[string]any, error) {
	if _, err := toolchain.ResolveGo(); err != nil {
		return "OK", map[string]any{
			"batchResults": failAllProjects(ctx.SelectedProjects, err.Error()),
		}, nil
	}
	binary, err := resolveGolangciBinary(ctx.WorkspaceRoot)
	if err != nil {
		return "OK", map[string]any{
			"batchResults": failAllProjects(
				ctx.SelectedProjects,
				err.Error()+"; install the pinned tools with `putnami deps install --tag go` before running lint",
			),
		}, nil
	}
	extensionRoot := resolveExtensionRoot()

	results := make([]batchProjectResult, len(ctx.SelectedProjects))
	index := make(map[string]int, len(ctx.SelectedProjects))
	groups := make(map[string]*lintBatchGroup)
	exclusions := workingDirExclusions{}
	var order []string

	for i, ref := range ctx.SelectedProjects {
		results[i] = batchProjectResult{ProjectID: ref.ID, Status: "OK"}
		index[ref.ID] = i

		fullPath := ref.FullPath
		if fullPath == "" {
			fullPath = filepath.Join(ctx.WorkspaceRoot, ref.Path)
		}
		// An explicit relative --config (e.g. `./.golangci-strict.yml`) is
		// documented as project-relative and, in the solo path, resolves against
		// the project dir golangci-lint runs in. The batch runs from the shared
		// group root, so anchor a relative override to each project before
		// grouping — otherwise batched projects would load a group-root config or
		// fail. This also keeps projects with distinct overrides in distinct
		// groups (distinct absolute config paths).
		explicit := options.config
		if explicit != "" && !filepath.IsAbs(explicit) {
			explicit = filepath.Join(fullPath, explicit)
		}
		config := toolchain.ResolveGolangciConfig(fullPath, ctx.WorkspaceRoot, extensionRoot, explicit)
		groupRoot := fullPath
		if goWork := toolchain.FindGoWork(fullPath); goWork != "" {
			groupRoot = filepath.Dir(goWork)
		}
		rel, err := batchRel(groupRoot, fullPath)
		if err != nil {
			results[i] = failedProjectResult(ref.ID, err.Error())
			continue
		}
		if exclusions.hidesProject(config, rel) {
			groupRoot = fullPath
		}

		key := config + "\x00" + groupRoot
		group := groups[key]
		if group == nil {
			group = &lintBatchGroup{config: config, groupRoot: groupRoot}
			groups[key] = group
			order = append(order, key)
		}
		group.projects = append(group.projects, &lintBatchProject{ref: ref, fullPath: fullPath})
	}

	// Run config groups concurrently. `configFiles` cannot express golangci's
	// walk-up config resolution, so a coarse scheduler batch may contain projects
	// whose effective configs differ, and golangci-lint takes one --config per
	// invocation. Executing the groups in parallel (rather than sequentially)
	// keeps a multi-config batch from serializing several analyses under the
	// single shared task timeout — it costs no more wall time than a same-sized
	// single-config batch. golangci's --allow-parallel-runners makes concurrent
	// processes safe, and the groups cover disjoint modules so --fix writes never
	// collide.
	groupResults := make([][]batchProjectResult, len(order))
	var wg sync.WaitGroup
	for i, key := range order {
		wg.Add(1)
		go func(slot int, group *lintBatchGroup) {
			defer wg.Done()
			groupResults[slot] = executeGolangciGroup(binary, options, group, ctx.WorkspaceRoot)
		}(i, groups[key])
	}
	wg.Wait()
	for _, groupResult := range groupResults {
		for _, result := range groupResult {
			if i, ok := index[result.ProjectID]; ok {
				results[i] = result
			}
		}
	}
	if options.skipGuard {
		for _, group := range groups {
			for _, project := range group.projects {
				addSkipGuardDiagnostics(&results[index[project.ref.ID]], ctx.WorkspaceRoot, project.fullPath)
			}
		}
	}

	// A successful batch protocol may still contain failed projects. Returning
	// OK lets the scheduler retain independent DAG and cache outcomes.
	return "OK", map[string]any{"batchResults": results}, nil
}

func executeGolangciGroup(
	binary string,
	options lintOptions,
	group *lintBatchGroup,
	workspaceRoot string,
) []batchProjectResult {
	if group == nil || len(group.projects) == 0 {
		return nil
	}

	patterns := make([]string, 0, len(group.projects))
	for _, project := range group.projects {
		rel, err := batchRel(group.groupRoot, project.fullPath)
		if err != nil {
			return failWholeGroup(group.projects, err.Error())
		}
		patterns = append(patterns, patternForRel(rel))
	}

	diagnostics := make(map[string][]parse.ToolDiagnostic, len(group.projects))
	failed := make(map[string]bool, len(group.projects))
	appendDiag := func(id string, diag parse.ToolDiagnostic) {
		diagnostics[id] = append(diagnostics[id], diag)
		failed[id] = true
	}
	failGroup := func(message string) {
		for _, project := range group.projects {
			appendDiag(project.ref.ID, parse.ToolDiagnostic{Severity: "error", Description: message})
		}
	}

	// Format gate: `run --fix` silently applies and drops the enabled formatters,
	// so unformatted code would pass green while the rewrite is reverted. Detect
	// drift with `fmt --diff` (never writes) before the run pass mutates the tree.
	// Only fixing needs it — a read-only `run` reports formatting drift itself.
	if options.fix {
		runGolangciFormatGate(binary, group, workspaceRoot, appendDiag)
	}

	args := []string{"run", "--allow-parallel-runners", "--path-mode", "abs"}
	if options.fix {
		args = append(args, "--fix")
	}
	if options.new {
		args = append(args, "--new")
	}
	if group.config != "" {
		args = append(args, "--config", group.config)
	}
	if options.timeout != "" {
		if isNumeric(options.timeout) {
			args = append(args, "--timeout", options.timeout+"ms")
		} else {
			args = append(args, "--timeout", options.timeout)
		}
	}
	args = append(args, patterns...)

	output, runErr := runGolangciWithRetry(binary, args, group.groupRoot)
	outStr := string(output)

	if runErr != nil && isToolchainCompatibilityError(outStr) {
		// The linter's Go toolchain is older than the workspace requires, so it
		// aborts type-checking with zero findings for every module in the group.
		failGroup("golangci-lint could not run: its Go toolchain is older than the " +
			"workspace (go.work) requires. Upgrade the local Go toolchain so the " +
			"linter matches, then re-run lint.")
		return buildGroupResults(group.projects, diagnostics, failed)
	}

	findings, leftover := parse.LintFindings(outStr)
	attributed := false
	for _, finding := range findings {
		id := projectForFile(finding.File, group.groupRoot, group.projects)
		diag := parse.ToolDiagnostic{
			Severity:    "error",
			Description: finding.Message,
			File:        parse.DiagPath(workspaceRoot, group.groupRoot, finding.File),
			Line:        finding.Line,
			Column:      finding.Column,
		}
		if id == "" {
			// A finding whose file matches no selected project: keep it as
			// unattributed evidence rather than silently dropping it.
			leftover = append(leftover, finding.File+": "+finding.Message)
			continue
		}
		appendDiag(id, diag)
		attributed = true
	}

	// The run failed without a finding we could attribute (config error, crash,
	// or a toolchain issue that isn't the compatibility abort): surface the raw
	// tail as a group-level failure rather than reporting bare FAILED.
	if runErr != nil && !attributed {
		tail := strings.TrimSpace(strings.Join(leftover, "\n"))
		if tail == "" {
			tail = "golangci-lint failed without attributable findings"
		}
		failGroup(tail)
	}

	return buildGroupResults(group.projects, diagnostics, failed)
}

// runGolangciFormatGate reports formatting drift (gofmt/goimports/gci per the
// resolved config) for a whole group with one `fmt --diff` invocation, then
// attributes each unformatted file to its project.
func runGolangciFormatGate(
	binary string,
	group *lintBatchGroup,
	workspaceRoot string,
	appendDiag func(string, parse.ToolDiagnostic),
) {
	args := []string{"fmt", "--diff"}
	if group.config != "" {
		args = append(args, "--config", group.config)
	}
	for _, project := range group.projects {
		rel, err := batchRel(group.groupRoot, project.fullPath)
		if err != nil {
			continue
		}
		args = append(args, patternForRel(rel))
	}

	output, err := runGolangciCommand(binary, args, group.groupRoot)
	out := strings.TrimSpace(string(output))
	files := parseUnformattedFiles(out)
	if len(files) > 0 {
		for _, file := range files {
			id := projectForFile(file, group.groupRoot, group.projects)
			diag := parse.ToolDiagnostic{
				Severity:    "error",
				Description: "File is not properly formatted; run `putnami lint --fix` or `gofmt -w` (gofmt)",
				File:        parse.DiagPath(workspaceRoot, group.groupRoot, file),
			}
			if id == "" {
				// Unattributable formatting drift is still a group-level failure.
				for _, project := range group.projects {
					appendDiag(project.ref.ID, diag)
				}
				continue
			}
			appendDiag(id, diag)
		}
		return
	}
	if err != nil && out != "" {
		// Non-zero exit without a parseable diff — golangci-lint itself errored.
		for _, project := range group.projects {
			appendDiag(project.ref.ID, parse.ToolDiagnostic{Severity: "error", Description: out})
		}
	}
}

func buildGroupResults(
	projects []*lintBatchProject,
	diagnostics map[string][]parse.ToolDiagnostic,
	failed map[string]bool,
) []batchProjectResult {
	results := make([]batchProjectResult, 0, len(projects))
	for _, project := range projects {
		diags := diagnostics[project.ref.ID]
		summary := summarizeLintDiagnostics(diags)
		status := "OK"
		if failed[project.ref.ID] {
			status = "FAILED"
		}
		results = append(results, batchProjectResult{
			ProjectID:   project.ref.ID,
			Status:      status,
			Data:        map[string]any{"lintSummary": map[string]any{"errors": summary.Errors}},
			Diagnostics: diags,
			Summary:     summary,
		})
	}
	return results
}

// projectForFile attributes a tool-reported path (absolute from `run --path-mode
// abs`, or group-root-relative from `fmt --diff`) to the owning project.
func projectForFile(file, groupRoot string, projects []*lintBatchProject) string {
	owners := make([]batchFileOwner, 0, len(projects))
	for _, project := range projects {
		owners = append(owners, batchFileOwner{id: project.ref.ID, root: project.fullPath})
	}
	return batchProjectForFile(file, groupRoot, owners)
}

// batchFileOwner is one candidate project a tool-reported path may belong to.
type batchFileOwner struct {
	id   string
	root string
}

// batchProjectForFile attributes a tool-reported path to the owning project by
// the LONGEST matching absolute project root, so nested modules keep their own
// findings. A relative path is resolved against runDir first — the directory the
// shared process actually ran from — which is what makes the batch's
// `go/framework/errors/wrap.go` and a solo run's `wrap.go` the same file.
//
// Both lint batches share it: golangci-lint and staticcheck report positions the
// same two ways (absolute, or relative to the run directory), so they must
// attribute them the same way or the same finding would land on different
// projects depending on which tool found it.
func batchProjectForFile(file, runDir string, owners []batchFileOwner) string {
	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(runDir, abs)
	}
	abs = filepath.Clean(abs)

	bestID := ""
	bestLen := -1
	for _, owner := range owners {
		root := filepath.Clean(owner.root)
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			if len(root) > bestLen {
				bestID = owner.id
				bestLen = len(root)
			}
		}
	}
	return bestID
}

// addSkipGuardDiagnostics appends the skip guard's findings for one project
// to its batch result; an error fails the project.
func addSkipGuardDiagnostics(result *batchProjectResult, workspaceRoot, projectPath string) {
	diagnostics := skipGuardDiagnostics(workspaceRoot, projectPath)
	if len(diagnostics) == 0 {
		return
	}
	result.Diagnostics = append(result.Diagnostics, diagnostics...)
	result.Summary = summarizeLintDiagnostics(result.Diagnostics)
	result.Data = map[string]any{"lintSummary": map[string]any{"errors": result.Summary.Errors}}
	if result.Summary.Errors > 0 {
		result.Status = "FAILED"
	}
}

func summarizeLintDiagnostics(diagnostics []parse.ToolDiagnostic) batchSummary {
	var summary batchSummary
	for _, diagnostic := range diagnostics {
		switch diagnostic.Severity {
		case "error":
			summary.Errors++
		case "warning":
			summary.Warnings++
		default:
			summary.Infos++
		}
	}
	return summary
}

func failAllProjects(projects []pctx.ProjectRef, message string) []batchProjectResult {
	results := make([]batchProjectResult, 0, len(projects))
	for _, project := range projects {
		results = append(results, failedProjectResult(project.ID, message))
	}
	return results
}

func failWholeGroup(projects []*lintBatchProject, message string) []batchProjectResult {
	results := make([]batchProjectResult, 0, len(projects))
	for _, project := range projects {
		results = append(results, failedProjectResult(project.ref.ID, message))
	}
	return results
}

func failedProjectResult(id, message string) batchProjectResult {
	diagnostics := []parse.ToolDiagnostic{{Severity: "error", Description: message}}
	return batchProjectResult{
		ProjectID:   id,
		Status:      "FAILED",
		Data:        map[string]any{"lintSummary": map[string]any{"errors": 1}},
		Diagnostics: diagnostics,
		Summary:     summarizeLintDiagnostics(diagnostics),
	}
}

// workingDirExclusions caches, per config file, the path patterns of the
// exclusions of a config whose run.relative-path-mode is wd: exclusions.paths,
// exclusions.paths-except and each rule's path and path-except. Such a pattern
// matches paths relative to the directory golangci-lint runs in: the project in
// a solo run, the go.work directory in a batch. A project whose go.work-relative
// path a pattern matches runs in its own group, rooted at the project, so the
// batch applies its exclusions as a solo run does.
type workingDirExclusions map[string][]*regexp.Regexp

func (c workingDirExclusions) hidesProject(config, rel string) bool {
	if config == "" || rel == "." {
		return false
	}
	patterns, ok := c[config]
	if !ok {
		patterns = readWorkingDirExclusions(config)
		c[config] = patterns
	}
	for _, pattern := range patterns {
		if pattern.MatchString(rel + "/") {
			return true
		}
	}
	return false
}

// readWorkingDirExclusions returns no pattern for a config it cannot read or
// parse: golangci-lint reports that config error itself.
func readWorkingDirExclusions(config string) []*regexp.Regexp {
	data, err := os.ReadFile(config)
	if err != nil {
		return nil
	}
	var cfg struct {
		Run struct {
			RelativePathMode string `yaml:"relative-path-mode"`
		} `yaml:"run"`
		Linters struct {
			Exclusions struct {
				Paths       []string `yaml:"paths"`
				PathsExcept []string `yaml:"paths-except"`
				Rules       []struct {
					Path       string `yaml:"path"`
					PathExcept string `yaml:"path-except"`
				} `yaml:"rules"`
			} `yaml:"exclusions"`
		} `yaml:"linters"`
	}
	if yaml.Unmarshal(data, &cfg) != nil || cfg.Run.RelativePathMode != "wd" {
		return nil
	}
	exclusions := cfg.Linters.Exclusions
	sources := slices.Concat(exclusions.Paths, exclusions.PathsExcept)
	for _, rule := range exclusions.Rules {
		sources = append(sources, rule.Path, rule.PathExcept)
	}
	patterns := make([]*regexp.Regexp, 0, len(sources))
	for _, source := range sources {
		if source == "" {
			continue
		}
		if pattern, err := regexp.Compile(source); err == nil {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

func batchRel(groupRoot, projectRoot string) (string, error) {
	rel, err := filepath.Rel(groupRoot, projectRoot)
	if err != nil || filepath.IsAbs(rel) || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("project %s is outside Go lint batch root %s", projectRoot, groupRoot)
	}
	return filepath.ToSlash(rel), nil
}

func patternForRel(rel string) string {
	if rel == "." || rel == "" {
		return "./..."
	}
	return "./" + rel + "/..."
}

// runGolangciCommand is a package variable so tests can substitute the tool.
var runGolangciCommand = func(binary string, args []string, dir string) ([]byte, error) {
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = toolchain.WorkspaceBuildEnv(os.Environ(), dir, toolchain.CurrentGoBinary())
	return cmd.CombinedOutput()
}

// golangciRetryDelay backs off between ETXTBSY retries; overridable in tests.
var golangciRetryDelay = func(attempt int) time.Duration {
	return time.Duration(attempt+1) * time.Second
}

// runGolangciWithRetry mirrors the singleton path's ETXTBSY handling: a freshly
// installed golangci-lint binary — or a concurrently executing one — can
// transiently fail with "text file busy" on Linux. Retry a few times before
// surfacing failure so one transient contention does not fail every project in
// the batch.
func runGolangciWithRetry(binary string, args []string, dir string) ([]byte, error) {
	const maxRetries = 3
	var output []byte
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		output, err = runGolangciCommand(binary, args, dir)
		if err == nil {
			return output, nil
		}
		if attempt < maxRetries && strings.Contains(string(output), "ETXTBSY") {
			time.Sleep(golangciRetryDelay(attempt))
			continue
		}
		return output, err
	}
	return output, err
}

// resolveGolangciBinary is a package variable so tests can bypass the real
// toolchain resolution and install fallback.
var resolveGolangciBinary = func(workspaceRoot string) (string, error) {
	binary, err := toolchain.ResolvePinnedToolBinary("golangci-lint", workspaceRoot)
	if err == nil {
		return binary, nil
	}
	binary, installErr := toolchain.InstallTool("golangci-lint", toolchain.CurrentGoBinary(), workspaceRoot)
	if installErr != nil {
		return "", fmt.Errorf("%w; unable to install pinned golangci-lint: %w", err, installErr)
	}
	return binary, nil
}
