package rush

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	detect "go.putnami.dev/tooling/observe/detect/rush"
	"go.putnami.dev/tooling/stacks"
)

// SyncJob is the job name of the workspace sync task.
const SyncJob = "workspace-sync"

// CheckJob is the job name of the task that fails on a stale project graph.
const CheckJob = "rush-graph-check"

// profileID is the id of the Rush profile, the prefix of its diagnostic codes.
const profileID = "rush"

// Files under the workspace root the sync reads beside rush.json.
const (
	// CommandLineFile declares Rush's custom commands.
	CommandLineFile = "common/config/rush/command-line.json"
	// BuildCacheFile turns Rush's build cache on.
	BuildCacheFile = "common/config/rush/build-cache.json"
)

// Diagnostic codes of the sync.
const (
	// CodeSyncFailed is a sync that failed and wrote nothing.
	CodeSyncFailed = profileID + stacks.SyncFailedSuffix
	// CodeSyncStale is a project whose putnami.json no longer matches
	// rush.json and the project manifests.
	CodeSyncStale = profileID + stacks.SyncStaleSuffix
)

// mappedJobs are the Putnami jobs a Rush command or a project script maps
// to by name.
var mappedJobs = []string{stacks.TaskBuild, stacks.TaskTest, stacks.TaskLint}

// SyncResult is what one sync did.
type SyncResult = stacks.GraphResult

// Sync writes the Rush project graph of the workspace at root into each
// project's putnami.json, in the workspace's mode. A root without rush.json
// gets an empty result.
func Sync(ctx context.Context, root string, stderr io.Writer) (SyncResult, error) {
	return synchronize(ctx, root, stderr, false)
}

// Stale answers what Sync would change, and writes nothing.
func Stale(ctx context.Context, root string, stderr io.Writer) (SyncResult, error) {
	return synchronize(ctx, root, stderr, true)
}

func synchronize(ctx context.Context, root string, stderr io.Writer, dry bool) (SyncResult, error) {
	mode, err := Mode(root)
	if err != nil {
		return SyncResult{}, err
	}
	return synchronizeIn(ctx, root, stderr, mode, dry)
}

// synchronizeIn is the sync in the mode, which a rerun of putnami init
// --from may preview before the workspace records it.
func synchronizeIn(_ context.Context, root string, _ io.Writer, mode string, dry bool) (SyncResult, error) {
	if _, err := os.Stat(filepath.Join(root, detect.Config)); errors.Is(err, fs.ErrNotExist) {
		return SyncResult{}, nil
	}
	detection := detect.Detect(root)
	for _, d := range detection.Diagnostics {
		if d.Code == detect.CodeConfigInvalid && d.Field == detect.Config {
			return SyncResult{}, fmt.Errorf("%s", d.Message)
		}
	}
	commands, err := readCommands(root)
	if err != nil {
		return SyncResult{}, err
	}
	sync := graphSync(detection)
	packages := make([]stacks.GraphPackage, 0, len(detection.Projects))
	for _, p := range detection.Projects {
		pkg := stacks.GraphPackage{Dir: p.Folder, DependsOn: p.DependsOn, Extension: TypeScriptExtension}
		if mode == ModeDelegate {
			pkg.Inputs = delegateInputs(root, p.Folder, detection.Lockfile)
			for _, job := range []string{stacks.TaskTest, stacks.TaskLint} {
				if _, ok := commands[job]; !ok {
					pkg.Disabled = append(pkg.Disabled, job)
				}
			}
			pkg.Extension = ExtensionName
		}
		packages = append(packages, pkg)
	}
	result, err := sync.Apply(root, packages, dry)
	if err != nil {
		return SyncResult{}, err
	}
	result.Export = detect.Config
	result.Warnings = unmappedCommands(commands)
	if mode == ModeImport {
		writes, warnings, err := importConventions(root, detection.Projects)
		if err != nil {
			return SyncResult{}, err
		}
		if err := applyConventions(root, writes, &result, dry); err != nil {
			return SyncResult{}, err
		}
		result.Warnings = append(result.Warnings, warnings...)
		result.Warnings = append(result.Warnings, unmappedScripts(root, detection.Projects)...)
		return result, nil
	}
	if _, ok := commands[stacks.TaskTest]; ok {
		if note := unreportedProjects(root, detection.Projects); note != "" {
			result.Warnings = append(result.Warnings, note)
		}
	}
	if !buildCacheEnabled(root) {
		result.Warnings = append(result.Warnings, fmt.Sprintf("Rush's build cache is off (%s does not set buildCacheEnabled to true): each delegated task reruns the projects upstream of its own; turn the cache on to keep them cached", BuildCacheFile))
	}
	return result, nil
}

// graphSync writes the Rush project graph: each package names the extension
// of the mode, the Rush extension with its inputs in delegate mode, the
// TypeScript extension in import mode.
func graphSync(detection detect.Detection) stacks.GraphSync {
	folders := map[string]bool{}
	for _, p := range detection.Projects {
		folders[p.Folder] = true
	}
	return stacks.GraphSync{
		Extension: ExtensionName,
		// An imported project runs the TypeScript extension, a delegated one
		// the Rush extension: the sync writes the package's and removes the
		// other, so a mode switch leaves no project with both.
		Extensions:     []string{TypeScriptExtension},
		HoldsBuildFile: func(_, dir string) bool { return folders[dir] },
		SkippedDirs:    []string{"common", "temp"},
		// The sync owns test and lint in disable.jobs: a delegated project
		// lists those Rush has no command for, an imported one none.
		Jobs: []string{stacks.TaskTest, stacks.TaskLint},
	}
}

// delegateInputs are the file patterns of a delegated project, relative to
// its directory: rush.json, command-line.json and the Rush lockfile, which
// every task reads, so a lockfile-only change selects every project.
func delegateInputs(root, folder, lockfile string) []string {
	up := strings.Repeat("../", strings.Count(folder, "/")+1)
	var out []string
	for _, f := range []string{detect.Config, CommandLineFile, lockfile} {
		if f == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err == nil && info.Mode().IsRegular() {
			out = append(out, up+f)
		}
	}
	return out
}

// command is one custom command of command-line.json.
type command struct {
	Kind string `json:"commandKind"`
	Name string `json:"name"`
}

// readCommands reads the custom commands of command-line.json by name. A
// missing file declares none.
func readCommands(root string) (map[string]command, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(CommandLineFile))) //nolint:gosec // a file under the workspace root
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]command{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Commands []command `json:"commands"`
	}
	if err := json.Unmarshal(detect.StripComments(data), &file); err != nil {
		return nil, fmt.Errorf("%s cannot be parsed: %w", CommandLineFile, err)
	}
	out := map[string]command{}
	for _, c := range file.Commands {
		if c.Name != "" {
			out[c.Name] = c
		}
	}
	return out, nil
}

// runsPerProject reports whether a command kind runs on each selected
// project, so rush <name> --to <package> means something.
func runsPerProject(kind string) bool {
	return kind == "bulk" || kind == "phased"
}

// unmappedCommands lists, sorted, a note for each command of command-line.json
// that maps to no Putnami job: a per-project command whose name is no build,
// test or lint, and a global command.
func unmappedCommands(commands map[string]command) []string {
	var notes []string
	for name, c := range commands {
		switch {
		case !runsPerProject(c.Kind):
			notes = append(notes, fmt.Sprintf("not mapped: rush %s, a %s command, runs once for the repository and keeps running through Rush", name, c.Kind))
		case !slices.Contains(mappedJobs, name):
			notes = append(notes, fmt.Sprintf("not mapped: rush %s, a %s command, has no Putnami job of that name and keeps running through Rush", name, c.Kind))
		}
	}
	sort.Strings(notes)
	return notes
}

// unmappedScripts lists, sorted by name, a note for each package.json script
// of an imported project that maps to no Putnami job, with the projects that
// declare it.
func unmappedScripts(root string, projects []detect.Project) []string {
	byScript := map[string][]string{}
	for _, p := range projects {
		for name := range scripts(root, p.Folder) {
			if !slices.Contains(mappedJobs, name) {
				byScript[name] = append(byScript[name], p.Name)
			}
		}
	}
	names := make([]string, 0, len(byScript))
	for name := range byScript {
		names = append(names, name)
	}
	sort.Strings(names)
	notes := make([]string, 0, len(names))
	for _, name := range names {
		owners := byScript[name]
		sort.Strings(owners)
		notes = append(notes, fmt.Sprintf("not mapped: the %s script of %s has no Putnami job of that name; run it with rushx %s", name, strings.Join(owners, ", "), name))
	}
	return notes
}

// scripts reads the scripts of the package.json in folder.
func scripts(root, folder string) map[string]string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(folder), "package.json")) //nolint:gosec // a project manifest under the workspace root
	if err != nil {
		return nil
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	_ = json.Unmarshal(data, &manifest)
	return manifest.Scripts
}

// unreportedProjects names, in one note, the delegated projects whose
// putnami.json names no JUnit or coverage report for the test task: their
// test result is the exit code alone.
func unreportedProjects(root string, projects []detect.Project) string {
	var names []string
	for _, p := range projects {
		cfg := wsproto.LoadProjectConfig(filepath.Join(root, filepath.FromSlash(p.Folder)))
		if cfg == nil || !namesReports(cfg.Options[ExtensionName]) {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return fmt.Sprintf("no test report: the test task of %s reads no JUnit or coverage file, so its result is the exit code alone; name them under options.%s.tasks.test.reports of the project's putnami.json", strings.Join(names, ", "), ExtensionName)
}

// namesReports reports whether an option layer names the reports of the test
// task.
func namesReports(layer map[string]any) bool {
	tasks, _ := layer["tasks"].(map[string]any)
	test, _ := tasks[stacks.TaskTest].(map[string]any)
	reports, _ := test["reports"].(map[string]any)
	return len(reports) > 0
}

// buildCacheEnabled reports whether build-cache.json turns Rush's build cache
// on.
func buildCacheEnabled(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(BuildCacheFile))) //nolint:gosec // a file under the workspace root
	if err != nil {
		return false
	}
	var cfg struct {
		Enabled bool `json:"buildCacheEnabled"`
	}
	return json.Unmarshal(detect.StripComments(data), &cfg) == nil && cfg.Enabled
}

// syncJobs is the workspace sync task of the Rush project graph.
var syncJobs = stacks.GraphJobs{
	Tool:          profileID,
	GraphFile:     detect.Config,
	ExportCommand: "rush init",
	Sync:          synchronize,
}

// checkJobs is the validate task: in import mode Putnami owns the graph, so
// there is nothing to compare.
var checkJobs = stacks.GraphJobs{
	Tool:          profileID,
	GraphFile:     detect.Config,
	ExportCommand: "rush init",
	Sync: func(ctx context.Context, root string, stderr io.Writer, dry bool) (stacks.GraphResult, error) {
		if mode, err := Mode(root); err == nil && mode == ModeImport {
			if _, err := os.Stat(filepath.Join(root, detect.Config)); err == nil {
				return stacks.GraphResult{Export: detect.Config}, nil
			}
		}
		return synchronize(ctx, root, stderr, dry)
	},
}

// ModeParam is the param of the sync job that names the mode to sync in,
// which outranks the mode the workspace records, as a rerun of putnami init
// --from --mode does.
const ModeParam = "mode"

// RunSync is the job of the workspace sync task: Sync on the workspace root,
// reported line by line, in the mode of the mode param or else the recorded
// one, or with the dryRun param the diff it would write.
func RunSync(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	jobs := syncJobs
	if mode := ctx.Params.String(ModeParam); mode == ModeImport || mode == ModeDelegate {
		jobs.Sync = func(c context.Context, root string, stderr io.Writer, dry bool) (stacks.GraphResult, error) {
			return synchronizeIn(c, root, stderr, mode, dry)
		}
	}
	return jobs.RunSync(ctx, emit, args)
}

// RunCheck is the job of the validate task: in delegate mode it fails with
// rush.sync_stale when a sync would change a putnami.json, so a change to
// rush.json or a project manifest that altered the edges cannot pass a gate
// that selected the projects on the old ones.
func RunCheck(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return checkJobs.RunCheck(ctx, emit, args)
}
