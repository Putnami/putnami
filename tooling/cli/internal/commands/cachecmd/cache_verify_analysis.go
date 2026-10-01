// Package cachecmd evaluates cache determinism from ordinary engine runs.
// It owns report policy only; cache keys, captures, restores, and task execution
// remain in internal/jobs and internal/engine.
package cachecmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
	agentcontext "go.putnami.dev/protocol/agentcontext"
	cache "go.putnami.dev/protocol/cache"
	extensionproto "go.putnami.dev/protocol/extension"
	infra "go.putnami.dev/protocol/infra"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/store"
)

const ReportVersion = 1

const (
	ClassKeyedInput     = "keyed-input"
	ClassCapturedOutput = "captured-output"
	ClassEphemeral      = "ephemeral"
)

type Finding struct {
	Check    string `json:"check"`
	Project  string `json:"project,omitempty"`
	Task     string `json:"task,omitempty"`
	Path     string `json:"path,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type Task struct {
	Key           string `json:"key"`
	Project       string `json:"project"`
	CacheKey      string `json:"cacheKey,omitempty"`
	Ambient       bool   `json:"ambient,omitempty"`
	Deterministic bool   `json:"deterministic,omitempty"`
	DoubleRun     string `json:"doubleRun"`
	HitLive       string `json:"hitLive"`
	WriteClosure  string `json:"writeClosure"`
}

type GenPath struct {
	Project string `json:"project"`
	Path    string `json:"path"`
	Class   string `json:"class"`
}

type Summary struct {
	Tasks    int `json:"tasks"`
	Passed   int `json:"passed"`
	Ambient  int `json:"ambient"`
	Findings int `json:"findings"`
	Blocking int `json:"blocking"`
}

type Report struct {
	Version  int       `json:"version"`
	Status   string    `json:"status"`
	Commands []string  `json:"commands"`
	Projects []string  `json:"projects"`
	Tasks    []Task    `json:"tasks"`
	GenPaths []GenPath `json:"genPaths,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	Summary  Summary   `json:"summary"`
}

// Tree is a project-relative snapshot keyed by slash-form path. Values hash
// file bytes, executable mode, and symlink target; timestamps are absent.
type Tree map[string]string

type Run struct {
	Workspace *workspace.Workspace
	Plan      []*jobs.ScheduledJob
	Results   map[string]*jobs.JobResult
	Trees     map[string]Tree
	StoreRoot string
}

func SnapshotProjects(root string, projects []*workspace.Project) (map[string]Tree, error) {
	trees := make(map[string]Tree, len(projects))
	for _, project := range projects {
		if project == nil {
			continue
		}
		tree, err := snapshotTree(filepath.Join(root, filepath.FromSlash(project.Path)))
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", project.ID, err)
		}
		trees[project.ID] = tree
	}
	return trees, nil
}

func Analyze(commands []string, first, second, hit Run) Report {
	report := Report{Version: ReportVersion, Status: "passed", Commands: append([]string(nil), commands...)}
	for _, project := range projectsOfPlan(first.Plan) {
		report.Projects = append(report.Projects, project.ID)
	}
	sort.Strings(report.Projects)

	firstStore := store.NewLocalStore(first.StoreRoot)
	secondStore := store.NewLocalStore(second.StoreRoot)
	// One contract set per project, resolved once: write ownership is a
	// PROJECT-level surface (see writeAllowed), so every task of a project is
	// scored against the same declarations.
	projectContracts := make(map[string][]jobs.VerificationContract)
	for _, project := range projectsOfPlan(first.Plan) {
		projectContracts[project.ID] = contractsForProject(first, project.ID)
	}
	for _, job := range sortedPlan(first.Plan) {
		if job == nil || job.Project == nil {
			continue
		}
		report.Tasks = append(report.Tasks,
			analyzeTask(&report, job, first, second, hit, firstStore, secondStore, projectContracts[job.Project.ID]))
	}

	analyzeProjectTrees(&report, first, hit)

	sort.Slice(report.GenPaths, func(i, j int) bool {
		return report.GenPaths[i].Project+"\x00"+report.GenPaths[i].Path < report.GenPaths[j].Project+"\x00"+report.GenPaths[j].Path
	})
	sort.Slice(report.Findings, func(i, j int) bool {
		a, b := report.Findings[i], report.Findings[j]
		return strings.Join([]string{a.Check, a.Project, a.Task, a.Path, a.Message}, "\x00") <
			strings.Join([]string{b.Check, b.Project, b.Task, b.Path, b.Message}, "\x00")
	})
	report.Summary.Tasks = len(report.Tasks)
	for _, task := range report.Tasks {
		if task.DoubleRun == "passed" && task.HitLive == "passed" && task.WriteClosure == "passed" {
			report.Summary.Passed++
		}
	}
	if report.Summary.Blocking > 0 {
		report.Status = "failed"
	}
	return report
}

func analyzeTask(
	report *Report,
	job *jobs.ScheduledJob,
	first, second, hit Run,
	firstStore, secondStore *store.LocalStore,
	projectContracts []jobs.VerificationContract,
) Task {
	key := job.Key()
	a, b, h := first.Results[key], second.Results[key], hit.Results[key]
	contract, err := jobs.VerificationContractOf(first.Workspace, job, a)
	task := Task{Key: key, Project: job.Project.ID, Deterministic: contract.Deterministic,
		DoubleRun: "passed", HitLive: "passed", WriteClosure: "passed"}
	if a != nil {
		task.CacheKey = a.CacheKey
	}
	if err != nil {
		addFinding(report, Finding{Check: "write-closure", Project: job.Project.ID, Task: key,
			Severity: "error", Message: "resolve declared outputs: " + err.Error()})
		task.WriteClosure = "failed"
	}
	task.Ambient = contract.Ambient
	if contract.Ambient {
		report.Summary.Ambient++
	}
	if a == nil || b == nil || h == nil {
		addFinding(report, Finding{Check: "execution", Project: job.Project.ID, Task: key,
			Severity: "error", Message: "task result is missing from one or more verification runs"})
		task.DoubleRun, task.HitLive = "failed", "failed"
		return task
	}
	analyzeTaskKeysAndArtifacts(report, job, &task, contract, a, b, h, firstStore, secondStore)
	analyzeTaskWrites(report, job, &task, contract, projectContracts, a)
	return task
}

func analyzeTaskKeysAndArtifacts(
	report *Report,
	job *jobs.ScheduledJob,
	task *Task,
	contract jobs.VerificationContract,
	a, b, hit *jobs.JobResult,
	firstStore, secondStore *store.LocalStore,
) {
	key := job.Key()
	if a.CacheKey != b.CacheKey {
		severity := "error"
		task.DoubleRun = "failed"
		if contract.Ambient {
			severity, task.DoubleRun = "info", "ambient"
		}
		addFinding(report, Finding{Check: "double-run-key", Project: job.Project.ID, Task: key,
			Severity: severity, Message: "cache key changed between equivalent live runs"})
	}
	if a.CacheKey == "" || b.CacheKey == "" {
		return
	}
	opts, _ := overridesFor(job.Project)
	ad, aok, aerr := artifactDigest(firstStore, a.CacheKey, contract, opts)
	bd, bok, berr := artifactDigest(secondStore, b.CacheKey, contract, opts)
	if aerr != nil || berr != nil || !aok || !bok {
		addFinding(report, Finding{Check: "double-run-artifact", Project: job.Project.ID, Task: key,
			Severity: "error", Message: "one or both live runs did not publish a readable task entry"})
		task.DoubleRun = "failed"
	} else if ad != bd {
		addFinding(report, Finding{Check: "double-run-artifact", Project: job.Project.ID, Task: key,
			Severity: "error", Message: "captured artifact changed between equivalent live runs"})
		task.DoubleRun = "failed"
	}
	if !hit.CacheHit || hit.CacheKey != a.CacheKey {
		addFinding(report, Finding{Check: "hit-live", Project: job.Project.ID, Task: key,
			Severity: "error", Message: "verification cache entry was not restored as a hit"})
		task.HitLive = "failed"
	}
}

func analyzeTaskWrites(
	report *Report,
	job *jobs.ScheduledJob,
	task *Task,
	contract jobs.VerificationContract,
	projectContracts []jobs.VerificationContract,
	result *jobs.JobResult,
) {
	if result.VerificationError != "" {
		addFinding(report, Finding{Check: "write-closure", Project: job.Project.ID, Task: job.Key(),
			Severity: "error", Message: result.VerificationError})
		task.WriteClosure = "failed"
	}
	overrides, err := overridesFor(job.Project)
	if err != nil {
		addFinding(report, Finding{Check: "gen-classification", Project: job.Project.ID, Task: job.Key(),
			Severity: "error", Message: err.Error()})
	}
	for _, written := range result.VerificationWrites {
		if writeAllowed(job, contract, projectContracts, written, overrides) {
			continue
		}
		addFinding(report, Finding{Check: "write-closure", Project: job.Project.ID, Task: job.Key(),
			Path: written, Severity: "error", Message: "task wrote outside the project's declared outputs"})
		task.WriteClosure = "failed"
	}
}

func analyzeProjectTrees(report *Report, first, hit Run) {
	for _, project := range projectsOfPlan(first.Plan) {
		overrides, err := overridesFor(project)
		if err != nil {
			continue
		}
		contracts := contractsForProject(first, project.ID)
		for _, item := range unionPaths(first.Trees[project.ID], hit.Trees[project.ID]) {
			if !strings.HasPrefix(item, ".gen/") && item != ".gen" {
				continue
			}
			class := classify(item, contracts, overrides)
			if class == "" {
				addFinding(report, Finding{Check: "gen-classification", Project: project.ID, Path: item,
					Severity: "error", Message: ".gen path has no keyed-input, captured-output, or ephemeral classification"})
				continue
			}
			report.GenPaths = append(report.GenPaths, GenPath{Project: project.ID, Path: item, Class: class})
		}
		for _, item := range diffTrees(first.Trees[project.ID], hit.Trees[project.ID]) {
			if classify(item, contracts, overrides) == ClassEphemeral {
				continue
			}
			addFinding(report, Finding{Check: "hit-live", Project: project.ID, Path: item,
				Severity: "error", Message: "cache-hit tree differs from the equivalent live-run tree"})
		}
	}
}

func addFinding(report *Report, finding Finding) {
	report.Findings = append(report.Findings, finding)
	report.Summary.Findings++
	if finding.Severity == "error" {
		report.Summary.Blocking++
	}
}

func artifactDigest(
	local *store.LocalStore,
	key string,
	contract jobs.VerificationContract,
	opts overrides,
) (string, bool, error) {
	entry, err := local.LookupTaskEntry(key)
	if err != nil || entry == nil || entry.Result == nil {
		return "", false, err
	}
	result := *entry.Result
	result.Events = nil
	outputs, manifest := comparableArtifacts(entry, contract, opts)
	payload, err := json.Marshal(struct {
		Outputs  []store.TaskEntryOutput `json:"outputs"`
		Manifest any                     `json:"manifest"`
		Result   store.EntryResult       `json:"result"`
	}{Outputs: outputs, Manifest: manifest, Result: result})
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), true, nil
}

// comparableArtifacts removes files whose project-relative paths are
// explicitly ephemeral. Their bytes must not make two otherwise equivalent
// cache entries differ: the scheduler refreshes .gen/version.json on every
// live execution, and cache verify already classifies that stamp as ephemeral
// for the live-versus-hit tree check.
func comparableArtifacts(
	entry *store.TaskEntry,
	contract jobs.VerificationContract,
	opts overrides,
) ([]store.TaskEntryOutput, any) {
	outputs := append([]store.TaskEntryOutput(nil), entry.Outputs...)
	if entry.Manifest == nil {
		return outputs, nil
	}

	manifest := &cache.Manifest{Files: make([]cache.FileEntry, 0, len(entry.Manifest.Files))}
	for _, file := range entry.Manifest.Files {
		if projectPath, ok := projectArtifactPath(entry.Outputs, file.Path); ok &&
			classify(projectPath, []jobs.VerificationContract{contract}, opts) == ClassEphemeral {
			continue
		}
		manifest.Files = append(manifest.Files, file)
	}

	// Files and Size are a summary of the manifest. Recompute them from the
	// filtered manifest so a volatile ephemeral file cannot perturb a directory
	// output's otherwise stable descriptor.
	byID := make(map[string]int, len(outputs))
	for i := range outputs {
		outputs[i].Files, outputs[i].Size = 0, 0
		byID[outputs[i].ID] = i
	}
	for _, file := range manifest.Files {
		id, _, _ := strings.Cut(file.Path, "/")
		if index, ok := byID[id]; ok {
			outputs[index].Files++
			outputs[index].Size += file.Size
		}
	}
	return outputs, manifest
}

// projectArtifactPath maps a task-entry manifest file back to its
// project-relative output path. Task manifests use their declared output ID as
// the root ("<id>/<relative-file>"), not the output's filesystem path.
func projectArtifactPath(outputs []store.TaskEntryOutput, manifestPath string) (string, bool) {
	id, remainder, nested := strings.Cut(manifestPath, "/")
	for _, output := range outputs {
		if output.ID != id || output.Root != extensionproto.OutputRootProject {
			continue
		}
		if !nested {
			return output.Path, true
		}
		if output.Kind != extensionproto.OutputKindDirectory {
			return "", false
		}
		return path.Join(output.Path, remainder), true
	}
	return "", false
}

func projectsOfPlan(plan []*jobs.ScheduledJob) []*workspace.Project {
	byID := map[string]*workspace.Project{}
	for _, job := range plan {
		if job != nil && job.Project != nil {
			byID[job.Project.ID] = job.Project
		}
	}
	projects := make([]*workspace.Project, 0, len(byID))
	for _, project := range byID {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return projects
}

func sortedPlan(plan []*jobs.ScheduledJob) []*jobs.ScheduledJob {
	result := append([]*jobs.ScheduledJob(nil), plan...)
	sort.Slice(result, func(i, j int) bool { return result[i].Key() < result[j].Key() })
	return result
}

func snapshotTree(root string) (Tree, error) {
	tree := Tree{}
	err := filepath.WalkDir(root, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".putnami", "node_modules", ".venv":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, _ = fmt.Fprintf(hash, "%s\x00", info.Mode().String())
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(filename)
			if err != nil {
				return err
			}
			_, _ = hash.Write([]byte(target))
		} else if info.Mode().IsRegular() {
			data, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			_, _ = hash.Write(data)
		} else {
			return nil
		}
		tree[filepath.ToSlash(rel)] = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	return tree, err
}

func diffTrees(left, right Tree) []string {
	paths := unionPaths(left, right)
	result := paths[:0]
	for _, item := range paths {
		if left[item] != right[item] {
			result = append(result, item)
		}
	}
	return result
}

func unionPaths(left, right Tree) []string {
	set := make(map[string]bool, len(left)+len(right))
	for item := range left {
		set[item] = true
	}
	for item := range right {
		set[item] = true
	}
	result := make([]string, 0, len(set))
	for item := range set {
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}

type overrides struct {
	keyed, captured, ephemeral []string
}

func overridesFor(project *workspace.Project) (overrides, error) {
	var result overrides
	if project == nil || project.Config == nil {
		return result, nil
	}
	raw := project.Config.Options["cache-verify"]
	var err error
	if result.keyed, err = pathList(raw["keyed-inputs"]); err != nil {
		return result, fmt.Errorf("options.cache-verify.keyed-inputs: %w", err)
	}
	if result.captured, err = pathList(raw["captured-outputs"]); err != nil {
		return result, fmt.Errorf("options.cache-verify.captured-outputs: %w", err)
	}
	if result.ephemeral, err = pathList(raw["ephemeral"]); err != nil {
		return result, fmt.Errorf("options.cache-verify.ephemeral: %w", err)
	}
	return result, nil
}

func pathList(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	raw, ok := value.([]any)
	if ok {
		return validatePathList(raw)
	}
	typed, ok := value.([]string)
	if !ok {
		return nil, fmt.Errorf("must be an array of project-relative .gen paths")
	}
	raw = make([]any, len(typed))
	for i := range typed {
		raw[i] = typed[i]
	}
	return validatePathList(raw)
}

func validatePathList(raw []any) ([]string, error) {
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok || !safeGenPattern(text) {
			return nil, fmt.Errorf("%q is not a safe project-relative .gen path", item)
		}
		result = append(result, text)
	}
	sort.Strings(result)
	return result, nil
}

func safeGenPattern(value string) bool {
	return value == ".gen" || strings.HasPrefix(value, ".gen/") && !strings.Contains(value, "\\") &&
		!strings.Contains(value, "\x00") && !strings.Contains(value, "../") && !strings.HasSuffix(value, "/..")
}

func contractsForProject(run Run, projectID string) []jobs.VerificationContract {
	var result []jobs.VerificationContract
	for _, job := range run.Plan {
		if job == nil || job.Project == nil || job.Project.ID != projectID {
			continue
		}
		contract, err := jobs.VerificationContractOf(run.Workspace, job, run.Results[job.Key()])
		if err == nil {
			result = append(result, contract)
		}
	}
	return result
}

func classify(item string, contracts []jobs.VerificationContract, opts overrides) string {
	for _, pattern := range opts.ephemeral {
		if matchesPath(item, pattern) {
			return ClassEphemeral
		}
	}
	for _, pattern := range opts.captured {
		if matchesPath(item, pattern) {
			return ClassCapturedOutput
		}
	}
	for _, pattern := range opts.keyed {
		if matchesPath(item, pattern) {
			return ClassKeyedInput
		}
	}
	if item == path.Join(".gen", "version.json") ||
		item == path.Join(agentcontext.DocumentEmitDir, agentcontext.DocumentFilename) ||
		strings.HasPrefix(item, path.Join(infra.AggregatedManifestDir, infra.PerProjectManifestDir)+"/") {
		return ClassEphemeral
	}
	for _, contract := range contracts {
		for _, output := range contract.Outputs {
			if output.Root == extensionproto.OutputRootProject && matchesPath(item, output.Path) {
				return ClassCapturedOutput
			}
		}
	}
	for _, contract := range contracts {
		for _, pattern := range contract.ProjectInputs {
			if matchGlob(item, pattern) {
				return ClassKeyedInput
			}
		}
	}
	return ""
}

// writeAllowed decides whether one observed write is legitimate.
//
// The allowance is the PROJECT's declared output surface, not the writing
// task's own outputs, because that is the ownership model the manifests
// actually declare: one owner per output, and other tasks write into the
// owner's territory rather than claiming a slice of it. The Go extension
// states it at length — `build-generate` is the single producer of
// `<project>/.gen` and declares that subtree WHOLE minus the subpaths it cedes
// (to `build-describe`, and `.gen/conf` to the two `config-merge` tasks, which
// declare the merged file inside it), while `build-describe`'s staging,
// `config-extract`'s fallback and `build-infra`'s requirements all write inside
// it and declare nothing (go/extension/putnami.extension.json). Scoring each
// task against only its own outputs contradicted that and made this check fail
// by construction on every Go project.
//
// What it still catches, which is the point of the check: a write that lands
// outside every declared output of the project — an undeclared path nobody
// owns, or one project's task writing into another project's tree.
//
// projectContracts carries the contracts of every task planned for this
// project, and must include the writing task's own.
func writeAllowed(
	job *jobs.ScheduledJob,
	contract jobs.VerificationContract,
	projectContracts []jobs.VerificationContract,
	item string,
	opts overrides,
) bool {
	projectItem, inProject := projectRelativePath(job.Project.Path, item)
	if inProject && classify(projectItem, projectContracts, opts) == ClassEphemeral {
		return true
	}
	for _, owner := range projectContracts {
		for _, output := range owner.Outputs {
			switch output.Root {
			case extensionproto.OutputRootProject:
				if inProject && matchesPath(projectItem, output.Path) {
					return true
				}
			case extensionproto.OutputRootWorkspace:
				if matchesPath(item, output.Path) {
					return true
				}
			}
		}
	}
	// mutatesSources stays a property of the WRITER, not of the subtree owner:
	// it licenses a task to rewrite its own keyed inputs, which is a claim
	// about that task's declaration and cannot be inherited from a sibling.
	if contract.MutatesSources && inProject {
		for _, pattern := range contract.ProjectInputs {
			if matchGlob(projectItem, pattern) {
				return true
			}
		}
	}
	return false
}

func projectRelativePath(projectPath, item string) (string, bool) {
	prefix := strings.Trim(strings.TrimSpace(projectPath), "/")
	if prefix == "" || prefix == "." {
		return item, true
	}
	if item == prefix {
		return ".", true
	}
	if strings.HasPrefix(item, prefix+"/") {
		return strings.TrimPrefix(item, prefix+"/"), true
	}
	return "", false
}

func matchesPath(item, owner string) bool {
	owner = strings.TrimSuffix(owner, "/")
	return item == owner || strings.HasPrefix(item, owner+"/") || matchGlob(item, owner)
}

func matchGlob(item, pattern string) bool {
	return matchSegments(strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(item, "/"), "/"))
}

func matchSegments(patterns, items []string) bool {
	if len(patterns) == 0 {
		return len(items) == 0
	}
	if patterns[0] == "**" {
		return matchSegments(patterns[1:], items) || len(items) > 0 && matchSegments(patterns, items[1:])
	}
	if len(items) == 0 {
		return false
	}
	matched, err := path.Match(patterns[0], items[0])
	return err == nil && matched && matchSegments(patterns[1:], items[1:])
}
