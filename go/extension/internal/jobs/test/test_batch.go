package test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.putnami.dev/go/extension/internal/parse"
	"go.putnami.dev/go/extension/internal/toolchain"
	protocolcli "go.putnami.dev/protocol/cli"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	coveragecheck "go.putnami.dev/sdk/extension/coverage"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/sdk/extension/specreport"
	"golang.org/x/mod/modfile"
)

type batchOptions struct {
	coverage          bool
	enforceCoverage   bool
	race              bool
	timeout           string
	short             bool
	runPattern        string
	count             string
	shuffle           bool
	bench             string
	benchtime         string
	verbose           bool
	useJSON           bool
	packageParallel   string
	parallel          string
	failfast          bool
	coverprofile      string
	covermode         string
	outputdir         string
	coverhtml         bool
	coverageThreshold float64
	coverageScope     string
	// members is how many projects the batch holds; their test cases share
	// one result line, so each gets TestCaseBatchMemberBytes(members).
	members int
}

// Coverage scopes of a batched run (ADR 0008). The scope decides which
// packages one project's profile instruments, and so whose tests it counts.
const (
	// coverageScopeBatch keeps one `go test` invocation for the whole group
	// with the union of every member's packages in -coverpkg. A member's lines
	// hit by a batch mate's tests count as covered.
	coverageScopeBatch = "batch"
	// coverageScopeProject runs one `go test` invocation per member with only
	// that member's packages in -coverpkg, exactly the package set a solo run
	// instruments. A member's profile counts its own tests only.
	coverageScopeProject = "project"
)

// resolveCoverageScope validates the coverage-scope parameter. An absent
// value is the default; an unknown one is an error so a typo never silently
// measures under the other scope.
func resolveCoverageScope(params pctx.Params) (string, error) {
	switch scope := params.String("coverage-scope", "coverageScope"); scope {
	case "", coverageScopeBatch:
		return coverageScopeBatch, nil
	case coverageScopeProject:
		return coverageScopeProject, nil
	default:
		return "", fmt.Errorf("coverage-scope must be %q or %q, got %q", coverageScopeBatch, coverageScopeProject, scope)
	}
}

type batchProject struct {
	ref        pctx.ProjectRef
	fullPath   string
	modulePath string
	// packages are the import paths the member's module owns, the one set
	// its -coverpkg names and its profile lines and test events are charged
	// against.
	packages  []string
	outputDir string
	coverFile string
	env       []string
	groupRoot string
	workspace string
}

type batchGroup struct {
	root     string
	env      []string
	projects []*batchProject
}

type batchProjectResult struct {
	ProjectID   string                 `json:"projectId"`
	Status      string                 `json:"status"`
	Data        map[string]any         `json:"data,omitempty"`
	Diagnostics []parse.ToolDiagnostic `json:"diagnostics,omitempty"`
	Artifacts   []batchArtifact        `json:"artifacts,omitempty"`
	Summary     batchSummary           `json:"summary,omitempty"`
}

type batchArtifact struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type batchSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Infos    int `json:"infos"`
}

// runGoCommand is the job's one subprocess seam, for the solo and the batch
// path alike, so tests can serve `go list` and `go test` from the same mock.
// A `go test` transcript is both streams interleaved. A `go list` result is
// data on stdout alone: its stderr, such as the exit-0 warning that a
// wildcard matched no packages, must never read as a package, so it is
// returned only with a failure.
var runGoCommand = func(binary string, args []string, dir string, env []string) ([]byte, error) {
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	if len(args) == 0 || args[0] != "list" {
		return cmd.CombinedOutput()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return append(stdout.Bytes(), stderr.Bytes()...), err
	}
	return stdout.Bytes(), nil
}

// runBatch executes one go test process per compatible governing Go workspace,
// or one per member when coverage-scope project measures coverage (ADR 0008),
// and returns ordinary per-project results for the scheduler to split. A
// one-project group still uses this protocol: mixed cache hit/miss batches can
// leave only one project to execute after the scheduler restores the hits.
func runBatch(ctx *pctx.Context, emit *jsonl.Emitter) (string, map[string]any, error) {
	goBinary, err := toolchain.ResolveGo()
	if err != nil {
		return "FAILED", nil, err
	}
	options := readBatchOptions(ctx)
	options.members = len(ctx.SelectedProjects)
	results := make([]batchProjectResult, len(ctx.SelectedProjects))
	resultIndex := make(map[string]int, len(ctx.SelectedProjects))
	groups := make(map[string]*batchGroup)
	var groupOrder []string

	// The scheduler's batch key folds every resolved parameter, so all members
	// share this one value. An invalid value is a configuration error charged
	// to every member, before any go invocation.
	scope, scopeErr := resolveCoverageScope(ctx.Params)
	options.coverageScope = scope

	for i, ref := range ctx.SelectedProjects {
		results[i] = batchProjectResult{ProjectID: ref.ID, Status: "OK"}
		resultIndex[ref.ID] = i

		if scopeErr != nil {
			diagnostic := failedPreparation(scopeErr.Error())
			results[i].Status = diagnostic.status
			results[i].Diagnostics = diagnostic.diagnostics
			results[i].Summary = summarizeDiagnostics(diagnostic.diagnostics)
			continue
		}

		project, diagnostic := prepareBatchProject(ctx, ref, options, goBinary)
		if diagnostic != nil {
			results[i].Status = diagnostic.status
			results[i].Diagnostics = diagnostic.diagnostics
			results[i].Summary = summarizeDiagnostics(diagnostic.diagnostics)
			continue
		}

		key := batchGroupKey(project, options)
		group := groups[key]
		if group == nil {
			group = &batchGroup{root: project.groupRoot, env: project.env}
			groups[key] = group
			groupOrder = append(groupOrder, key)
		}
		group.projects = append(group.projects, project)
	}

	for _, key := range groupOrder {
		groupResults := executeBatchGroup(goBinary, options, groups[key], emit)
		for _, result := range groupResults {
			if i, ok := resultIndex[result.ProjectID]; ok {
				results[i] = result
			}
		}
	}

	// A successful batch protocol may contain failed projects. Returning OK here
	// lets the scheduler retain independent DAG and cache outcomes.
	return "OK", map[string]any{"batchResults": results}, nil
}

type preparedDiagnostic struct {
	status      string
	diagnostics []parse.ToolDiagnostic
}

func prepareBatchProject(
	ctx *pctx.Context,
	ref pctx.ProjectRef,
	options batchOptions,
	goBinary string,
) (*batchProject, *preparedDiagnostic) {
	fullPath := ref.FullPath
	if fullPath == "" {
		fullPath = filepath.Join(ctx.WorkspaceRoot, ref.Path)
	}
	goModPath := filepath.Join(fullPath, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &preparedDiagnostic{status: "SKIP"}
		}
		return nil, failedPreparation(fmt.Sprintf("read %s: %v", goModPath, err))
	}
	modulePath := strings.TrimSpace(modfile.ModulePath(goMod))
	env, err := buildTestEnv(ctx, fullPath, options.race, goBinary)
	if err != nil {
		return nil, failedPreparation(err.Error())
	}
	// The member's own packages, listed in its directory with its env: what
	// its -coverpkg names and what its output is attributed by. A listing
	// failure is this member's alone.
	packages, err := listModulePackages(goBinary, fullPath, env)
	if err != nil {
		return nil, failedPreparation(err.Error())
	}

	groupRoot := fullPath
	if goWork := toolchain.FindGoWork(fullPath); goWork != "" {
		groupRoot = filepath.Dir(goWork)
	}
	outputDir := projectOutputDir(ctx, ref, options.outputdir)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, failedPreparation(fmt.Sprintf("create test output: %v", err))
	}
	coverFile := ""
	if coverageEnabled(options) {
		coverFile = options.coverprofile
		if coverFile == "" {
			coverFile = "coverage.out"
		}
		if !filepath.IsAbs(coverFile) {
			coverFile = filepath.Join(outputDir, coverFile)
		}
	}
	return &batchProject{
		ref:        ref,
		fullPath:   fullPath,
		modulePath: modulePath,
		packages:   packages,
		outputDir:  outputDir,
		coverFile:  coverFile,
		env:        env,
		groupRoot:  groupRoot,
		workspace:  ctx.WorkspaceRoot,
	}, nil
}

func failedPreparation(message string) *preparedDiagnostic {
	return &preparedDiagnostic{
		status: "FAILED",
		diagnostics: []parse.ToolDiagnostic{{
			Category:    "GO_TEST_BATCH_PREPARE",
			Severity:    "error",
			Description: message,
		}},
	}
}

func batchGroupKey(project *batchProject, options batchOptions) string {
	envDigest := sha256.Sum256([]byte(strings.Join(project.env, "\x00")))
	key := project.groupRoot + "\x00" + hex.EncodeToString(envDigest[:])

	// These modes cannot preserve package-level attribution or per-project file
	// ownership safely, so retain a one-project Go invocation inside the shared
	// extension process.
	if !options.useJSON || options.failfast ||
		filepath.IsAbs(options.coverprofile) || filepath.IsAbs(options.outputdir) ||
		project.modulePath == "" {
		key += "\x00" + project.ref.ID
	}
	return key
}

func executeBatchGroup(
	goBinary string,
	options batchOptions,
	group *batchGroup,
	emit *jsonl.Emitter,
) []batchProjectResult {
	if group == nil || len(group.projects) == 0 {
		return nil
	}

	// Scratch-owned, so a batch killed before this deferred removal is reclaimed
	// by the next one.
	batchScratch, err := scratch.New("putnami-go-test-batch-")
	if err != nil {
		return failWholeGroup(group.projects, "create Go test batch scratch: "+err.Error())
	}
	defer func() { _ = batchScratch.Remove() }()
	tempDir := batchScratch.Path()

	// One fragment directory per group, shared by every go test
	// process the group runs: each test binary writes uniquely named fragments,
	// and member attribution happens at merge time through each fragment's
	// declaration file, so per-project reports survive batching. The tempDir
	// path is fresh per run, which is also what re-keys go's own test cache for
	// every test that consulted the variable.
	runEnv, err := withGoTempDir(append([]string(nil), group.env...), tempDir)
	if err != nil {
		emit.Diagnostic("warning", err.Error(), "", 0)
	}
	fragmentsDir := filepath.Join(tempDir, "spec-fragments")
	if err := os.MkdirAll(fragmentsDir, 0o755); err != nil {
		emit.Diagnostic("warning", "create spec fragment directory: "+err.Error(), "", 0)
		fragmentsDir = ""
	} else {
		runEnv = append(runEnv, spectest.FragmentDirEnv+"="+fragmentsDir)
	}

	invocations, planErr := planBatchInvocations(options, group, tempDir)
	if planErr != nil {
		return failWholeGroup(group.projects, planErr.Error())
	}

	outputs := make(map[string]string, len(group.projects))
	failed := make(map[string]bool, len(group.projects))
	unattributedFailures := make(map[string]string, len(group.projects))
	// Invocations run one after another: the batch holds one scheduler slot,
	// and each `go test` already spreads its own packages over the CPUs.
	for _, invocation := range invocations {
		args := batchTestArgs(options, tempDir, invocation.coverFile, invocation.coverPackages, invocation.patterns)
		output, commandErr := runGoCommand(goBinary, args, group.root, runEnv)
		runOutputs, runFailed, unattributedFailure := splitBatchTestOutput(string(output), invocation.projects, commandErr != nil)

		if invocation.coverFile != "" {
			splitErr := splitBatchCoverage(invocation.coverFile, invocation.projects)
			// A profile that was never written is not a batch failure. Now that coverage
			// is instrumented by default, this path runs for every Go test job, and a
			// toolchain that exits 0 without producing one must not take down every
			// project in the group. Each project then simply has no coverage data, which
			// its own threshold gate reports precisely ("no coverage data was produced")
			// instead of a shared file-not-found.
			if os.IsNotExist(splitErr) {
				splitErr = nil
			}
			if splitErr != nil && commandErr == nil {
				unattributedFailure = splitErr.Error()
				for _, project := range invocation.projects {
					runFailed[project.ref.ID] = true
				}
			}
		}
		emitBatchTestTranscript(emit, string(output), options.useJSON, options.verbose, invocation.projects)

		for _, project := range invocation.projects {
			outputs[project.ref.ID] = runOutputs[project.ref.ID]
			failed[project.ref.ID] = runFailed[project.ref.ID]
			unattributedFailures[project.ref.ID] = unattributedFailure
		}
	}

	var fragments []spectest.Fragment
	if fragmentsDir != "" {
		var fragmentWarnings []string
		fragments, fragmentWarnings = specreport.ReadFragments(fragmentsDir)
		for _, warning := range fragmentWarnings {
			emit.Diagnostic("warning", warning, "", 0)
		}
		for _, fragment := range fragments {
			if !fragmentOwnedByAnyMember(fragment, group.projects) {
				emit.Diagnostic("warning", fmt.Sprintf(
					"spec observation (%s, %s, %s) dropped: declaration %s belongs to no batched project",
					fragment.Feature, fragment.Requirement, fragment.Check, fragment.File), "", 0)
			}
		}
	}

	results := make([]batchProjectResult, 0, len(group.projects))
	for _, project := range group.projects {
		result := evaluateBatchProject(
			goBinary,
			options,
			project,
			outputs[project.ref.ID],
			failed[project.ref.ID],
			unattributedFailures[project.ref.ID],
		)
		attachBatchVerification(&result, fragments, project)
		results = append(results, result)
	}
	return results
}

// attachBatchVerification merges the fragments one member owns into its own
// report artifact, keeping per-project attribution under batching exactly as
// a solo run produces it. Reporting problems become warning diagnostics and
// never change the member's test verdict.
func attachBatchVerification(result *batchProjectResult, fragments []spectest.Fragment, project *batchProject) {
	report, _, warnings := specreport.ProjectReport(fragments, project.fullPath)
	for _, warning := range warnings {
		result.Diagnostics = append(result.Diagnostics, parse.ToolDiagnostic{
			Category: "SPEC_VERIFICATION", Severity: "warning", Description: warning,
		})
	}
	if report == nil {
		return
	}
	destination, err := specreport.EmitReport(nil, report, project.outputDir)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, parse.ToolDiagnostic{
			Category: "SPEC_VERIFICATION", Severity: "warning", Description: err.Error(),
		})
		return
	}
	result.Artifacts = append(result.Artifacts, batchArtifact{
		ID:   featureproto.VerificationReportArtifactID,
		Name: "Feature Verification Report",
		Kind: "report",
		Path: batchArtifactPath(project.workspace, destination),
	})
}

// fragmentOwnedByAnyMember reports whether one fragment's declaration file
// lives inside any batched project, so an orphaned observation is surfaced
// instead of silently vanishing between members.
func fragmentOwnedByAnyMember(fragment spectest.Fragment, projects []*batchProject) bool {
	for _, project := range projects {
		if specreport.OwnedBy(fragment, project.fullPath) {
			return true
		}
	}
	return false
}

func failWholeGroup(projects []*batchProject, message string) []batchProjectResult {
	results := make([]batchProjectResult, 0, len(projects))
	for _, project := range projects {
		diagnostics := []parse.ToolDiagnostic{{
			Category:    "GO_TEST_BATCH",
			Severity:    "error",
			Description: message,
		}}
		results = append(results, batchProjectResult{
			ProjectID:   project.ref.ID,
			Status:      "FAILED",
			Diagnostics: diagnostics,
			Summary:     summarizeDiagnostics(diagnostics),
		})
	}
	return results
}

// batchInvocation is one `go test` process of a group: the members it tests,
// the package patterns that select their tests, the packages -coverpkg
// instruments (nil when coverage is off), and the scratch profile it writes
// ("" when coverage is off). Selection and instrumentation are separate:
// a pattern selects by directory and would also reach a module nested under
// a member, the package list names what the member's module owns.
type batchInvocation struct {
	projects      []*batchProject
	patterns      []string
	coverPackages []string
	coverFile     string
}

// planBatchInvocations decides how many `go test` processes one group needs.
// Batch scope, and every run without coverage, keeps the single union
// invocation. Project scope with coverage runs one invocation per member whose
// -coverpkg names only that member's packages: its profile counts its own
// tests, and the instrumentation that shapes the compiled objects depends on
// the member alone, never on its batch mates (ADR 0008).
func planBatchInvocations(options batchOptions, group *batchGroup, tempDir string) ([]batchInvocation, error) {
	patterns := make([]string, 0, len(group.projects))
	for _, project := range group.projects {
		pattern, err := batchPackagePattern(group.root, project.fullPath)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, pattern)
	}
	if !coverageEnabled(options) {
		return []batchInvocation{{projects: group.projects, patterns: patterns}}, nil
	}
	if options.coverageScope != coverageScopeProject {
		return []batchInvocation{{
			projects:      group.projects,
			patterns:      patterns,
			coverPackages: unionPackages(group.projects),
			coverFile:     filepath.Join(tempDir, "coverage.out"),
		}}, nil
	}
	invocations := make([]batchInvocation, 0, len(group.projects))
	for i, project := range group.projects {
		invocations = append(invocations, batchInvocation{
			projects:      []*batchProject{project},
			patterns:      []string{patterns[i]},
			coverPackages: project.packages,
			coverFile:     memberScratchProfile(tempDir, project),
		})
	}
	return invocations, nil
}

// memberScratchProfile names one member's scratch profile after the member
// alone. Each invocation gets its own file, so a run that writes none is never
// split against the profile another member left behind. The name never
// depends on the member's position in the group, so the member's `go test`
// arguments are the same in every batch it lands in.
func memberScratchProfile(tempDir string, project *batchProject) string {
	digest := sha256.Sum256([]byte(project.ref.ID))
	return filepath.Join(tempDir, "coverage-"+hex.EncodeToString(digest[:8])+".out")
}

func batchPackagePattern(groupRoot, projectRoot string) (string, error) {
	rel, err := filepath.Rel(groupRoot, projectRoot)
	if err != nil || filepath.IsAbs(rel) || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("project %s is outside Go batch root %s", projectRoot, groupRoot)
	}
	if rel == "." {
		return "./...", nil
	}
	return "./" + filepath.ToSlash(rel) + "/...", nil
}

// batchTestArgs builds one invocation's `go test` arguments. coverPackages
// is what -coverpkg instruments and patterns is what the run selects; with
// coverage on and nothing to instrument, -cover goes alone and `go test`
// reports the empty selection itself.
func batchTestArgs(
	options batchOptions,
	outputDir string,
	coverFile string,
	coverPackages []string,
	patterns []string,
) []string {
	args := []string{"test"}
	if options.outputdir != "" {
		args = append(args, "-outputdir", outputDir)
	}
	if options.short {
		args = append(args, "-short")
	}
	count := resolveCount(options.count, options.coverageThreshold)
	if count != "" {
		args = append(args, "-count", count)
	}
	if options.shuffle {
		args = append(args, "-shuffle", "on")
	}
	if options.bench != "" {
		args = append(args, "-bench", options.bench)
	}
	if options.benchtime != "" {
		args = append(args, "-benchtime", options.benchtime)
	}
	if coverageEnabled(options) {
		args = append(args, "-cover")
		if len(coverPackages) > 0 {
			args = append(args, "-coverpkg", strings.Join(coverPackages, ","))
		}
		// Only ask for a profile when there is somewhere to put it. Passing an empty
		// -coverprofile makes `go test` treat the next argument as the path.
		if coverFile != "" {
			args = append(args, "-coverprofile", coverFile)
		}
	}
	if options.covermode != "" {
		args = append(args, "-covermode", options.covermode)
	}
	if options.race {
		args = append(args, "-race")
	}
	if options.timeout != "" {
		timeout := options.timeout
		if isNumeric(timeout) {
			timeout += "ms"
		}
		args = append(args, "-timeout", timeout)
	}
	if options.runPattern != "" {
		args = append(args, "-run", options.runPattern)
	}
	if options.verbose {
		args = append(args, "-v")
	}
	if options.useJSON {
		args = append(args, "-json")
	}
	args = appendParallelArgs(args, options.packageParallel, options.parallel)
	if options.failfast {
		args = append(args, "-failfast")
	}
	return append(args, patterns...)
}

func splitBatchTestOutput(
	output string,
	projects []*batchProject,
	commandFailed bool,
) (map[string]string, map[string]bool, string) {
	outputs := make(map[string]string, len(projects))
	failed := make(map[string]bool, len(projects))
	if len(projects) == 1 {
		attributed := testOutputFailed(output)
		outputs[projects[0].ref.ID] = output
		failed[projects[0].ref.ID] = commandFailed || attributed
		// The lone project owns every line, but a non-zero exit that no fail
		// event explains still has to be reported. Hardcoding "" here meant the
		// safety net below never fired for the common one-project case.
		if commandFailed && !attributed {
			return outputs, failed, parse.TestFailureText(output)
		}
		return outputs, failed, ""
	}

	var unattributed []string
	attributedFailure := false
	owners := packageOwners(projects)
	// The subprocess output is already in memory, so split it directly instead
	// of imposing bufio.Scanner's 64 KiB token limit. A successful test may log a
	// large JSON value on one line; dropping that line here would discard its
	// transcript during batch attribution even though the command passed.
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		var event parse.TestEvent
		if json.Unmarshal([]byte(line), &event) != nil || event.Package == "" {
			if strings.TrimSpace(line) != "" {
				unattributed = append(unattributed, line)
			}
			continue
		}
		projectID := owners[event.Package]
		if projectID == "" {
			unattributed = append(unattributed, line)
			continue
		}
		outputs[projectID] += line + "\n"
		if event.Action == "fail" {
			failed[projectID] = true
			attributedFailure = true
		}
	}

	unattributedFailure := ""
	if commandFailed && (len(unattributed) > 0 || !attributedFailure) {
		unattributedFailure = strings.TrimSpace(strings.Join(unattributed, "\n"))
		if unattributedFailure == "" {
			unattributedFailure = "go test failed without package attribution"
		}
		for _, project := range projects {
			failed[project.ref.ID] = true
		}
	}
	return outputs, failed, unattributedFailure
}

func testOutputFailed(output string) bool {
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		var event parse.TestEvent
		if json.Unmarshal([]byte(line), &event) == nil && event.Action == "fail" {
			return true
		}
	}
	return false
}

// splitBatchCoverage writes each member's coverage.out from the invocation's
// scratch profile. A block belongs to the member whose module lists its
// package, never to a member whose module path is merely a prefix of it: a
// nested module's blocks go to the nested project when it is a member and
// fail the split otherwise, so they can never dilute the parent's coverage.
func splitBatchCoverage(combinedPath string, projects []*batchProject) error {
	data, err := os.ReadFile(combinedPath)
	if err != nil {
		return err
	}
	mode := "mode: set"
	owners := packageOwners(projects)
	linesByProject := make(map[string][]string, len(projects))
	scanner := parse.LineScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "mode:") {
			mode = line
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		pkg := coverageBlockPackage(line)
		projectID := owners[pkg]
		if projectID == "" {
			return fmt.Errorf("coverage package %q does not belong to a selected project", pkg)
		}
		linesByProject[projectID] = append(linesByProject[projectID], line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	for _, project := range projects {
		if project.coverFile == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(project.coverFile), 0o755); err != nil {
			return err
		}
		lines := append([]string{mode}, linesByProject[project.ref.ID]...)
		if err := os.WriteFile(project.coverFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func evaluateBatchProject(
	goBinary string,
	options batchOptions,
	project *batchProject,
	output string,
	testFailed bool,
	unattributedFailure string,
) batchProjectResult {
	testCounts := parse.TestJSON(output)
	var coverageResult parse.CoverageResult
	hasCoverage := false
	var coverageUnreadable error
	if coverageEnabled(options) && project.coverFile != "" {
		result, err := parse.CoverageDetails(project.coverFile)
		if errors.Is(err, parse.ErrCoverageProfileUnreadable) {
			coverageUnreadable = err
		}
		if err == nil {
			coverageResult = result
			hasCoverage = true
			if options.coverhtml || options.coverage {
				htmlPath := filepath.Join(project.outputDir, "coverage.html")
				cmd := exec.Command(goBinary, "tool", "cover", "-html="+project.coverFile, "-o", htmlPath)
				cmd.Env = project.env
				_ = cmd.Run()
			}
		}
	}

	diagnostics := parse.CoverageDiagnosticList(
		coverageResult,
		project.workspace,
		project.fullPath,
	)
	// One locator resolves the test files that both the test cases and the
	// failure diagnostics name.
	locate := parse.ModuleTestFileLocator(project.workspace, project.fullPath, project.modulePath)
	failureDetailsTruncated := 0
	if testFailed {
		var failures []parse.ToolDiagnostic
		if unattributedFailure != "" {
			// The group-level output already explains the failure, so the
			// per-project parse stays pure and is not padded with a raw dump.
			failures = append(failures, parse.TestDiagnosticList(output, locate)...)
			failures = append(failures, parse.ToolDiagnostic{
				Category:    "GO_TEST_BATCH",
				Severity:    "error",
				Description: unattributedFailure,
			})
		} else {
			// Nothing else explains this project's failure, so the parse has to
			// produce a cause — falling back to the raw output when it cannot.
			failures = append(failures, parse.TestFailureDiagnosticList(output, locate)...)
		}
		bounded, omitted := boundFailureDiagnostics(failures)
		diagnostics = append(diagnostics, bounded...)
		failureDetailsTruncated = omitted
	}

	thresholdFailed := false
	if coverageUnreadable != nil {
		diagnostic := coverageUnreadableDiagnostic(coverageUnreadable, options.coverageThreshold)
		diagnostics = append(diagnostics, diagnostic)
		thresholdFailed = diagnostic.Severity == "error"
	} else if options.coverageThreshold > 0 {
		hasData := hasCoverage && coverageResult.TotalStatements > 0
		if ok, message := coveragecheck.CheckThreshold(
			options.coverageThreshold,
			coverageResult.Percentage,
			hasData,
		); !ok {
			diagnostics = append(diagnostics, parse.ToolDiagnostic{
				Category:    "COVERAGE_THRESHOLD_NOT_MET",
				Severity:    "error",
				Description: capitalize(message),
			})
			thresholdFailed = true
		}
	}

	status := "OK"
	if testFailed || thresholdFailed {
		status = "FAILED"
	}
	var artifacts []batchArtifact
	if hasCoverage {
		artifacts = append(artifacts, batchArtifact{
			ID:   "coverage",
			Name: "Coverage Profile",
			Kind: "coverage",
			Path: batchArtifactPath(project.workspace, project.coverFile),
		})
	}
	data := buildResultData(
		testCounts,
		coverageResult,
		hasCoverage,
		options.coverageThreshold,
	)
	recordFailureDetailsTruncated(data, failureDetailsTruncated)
	// output is the part of the stream split to this member, the same events
	// its testSummary counts, so its cases never name a batch mate's package.
	recordTestCases(data, moduleTestCases(output, options.useJSON, locate), protocolcli.TestCaseBatchMemberBytes(options.members))
	return batchProjectResult{
		ProjectID:   project.ref.ID,
		Status:      status,
		Data:        data,
		Diagnostics: diagnostics,
		Artifacts:   artifacts,
		Summary:     summarizeDiagnostics(diagnostics),
	}
}

func batchArtifactPath(workspaceRoot, path string) string {
	rel, err := filepath.Rel(workspaceRoot, path)
	if err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return path
}

func summarizeDiagnostics(diagnostics []parse.ToolDiagnostic) batchSummary {
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

func projectOutputDir(ctx *pctx.Context, project pctx.ProjectRef, outputdir string) string {
	jobName := ctx.Job.Name
	if jobName == "" {
		jobName = "test"
	}
	outDir := filepath.Join(
		ctx.WorkspaceRoot,
		".putnami",
		"out",
		filepath.FromSlash(project.Path),
		jobName,
	)
	if outputdir == "" {
		return outDir
	}
	if filepath.IsAbs(outputdir) {
		return outputdir
	}
	return filepath.Join(outDir, outputdir)
}

// coverageEnabled mirrors the solo path: coverage is collected whenever the
// project allows it (`coverage`, default true) or a profile is explicitly
// requested. Measurement does not depend on --enforce-coverage, which now gates
// only the threshold.
func coverageEnabled(options batchOptions) bool {
	return options.coverage ||
		options.coverprofile != "" ||
		options.covermode != "" ||
		options.coverhtml
}

func readBatchOptions(ctx *pctx.Context) batchOptions {
	options := batchOptions{
		coverage:          ctx.Params.Bool("coverage", true),
		enforceCoverage:   ctx.Params.Bool("enforce-coverage", true, "enforceCoverage"),
		race:              ctx.Params.Bool("race", false),
		timeout:           ctx.Params.String("timeout"),
		short:             ctx.Params.Bool("short", false),
		runPattern:        ctx.Params.String("run"),
		count:             ctx.Params.String("count"),
		shuffle:           ctx.Params.Bool("shuffle", false),
		bench:             ctx.Params.String("bench"),
		benchtime:         ctx.Params.String("benchtime"),
		verbose:           ctx.Params.Bool("test-verbose", false, "testVerbose", "verbose", "v"),
		useJSON:           ctx.Params.Bool("test-json", true, "testJson", "json"),
		packageParallel:   ctx.Params.String("package-parallel", "packageParallel"),
		parallel:          ctx.Params.String("parallel"),
		failfast:          ctx.Params.Bool("failfast", false),
		coverprofile:      ctx.Params.String("coverprofile"),
		covermode:         ctx.Params.String("covermode"),
		outputdir:         ctx.Params.String("outputdir"),
		coverhtml:         ctx.Params.Bool("coverhtml", false),
		coverageThreshold: ctx.Params.Float("coverage-threshold", 0, "coverageThreshold"),
	}
	// An explicit `coverage: false` opt-out zeroes the inherited threshold so it is
	// authoritative even under the enforce cadence. Mirrors the solo path (test.go).
	if !options.coverage {
		options.coverageThreshold = 0
	}
	// The gate is on by default; --no-enforce-coverage zeroes only the threshold so
	// the batch still instruments and still reports every project's percentage.
	// Skipping instrumentation is the `coverage` opt-out above, which this never
	// overrides. Mirrors the solo path (test.go).
	if !options.enforceCoverage {
		options.coverageThreshold = 0
	}
	return options
}
