package moon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	detect "go.putnami.dev/tooling/observe/detect/moon"
	"go.putnami.dev/tooling/stacks"
)

// SyncJob is the job name of the workspace sync task.
const SyncJob = "workspace-sync"

// CheckJob is the job name of the task that fails on a stale project graph.
const CheckJob = "moon-graph-check"

// Diagnostic codes of the sync.
const (
	// CodeSyncFailed is a sync that failed and wrote nothing.
	CodeSyncFailed = profileID + stacks.SyncFailedSuffix
	// CodeSyncNoExport is a sync outside a moon workspace, which wrote
	// nothing.
	CodeSyncNoExport = profileID + stacks.SyncNoExportSuffix
	// CodeSyncStale is a project whose putnami.json no longer matches the
	// moon project graph.
	CodeSyncStale = profileID + stacks.SyncStaleSuffix
	// CodeTaskUnmapped is a moon task the sync maps to no Putnami command.
	CodeTaskUnmapped = "moon.task_unmapped"
	// CodeReportsUnnamed is a delegated project whose test reads no report.
	CodeReportsUnnamed = "moon.reports_unnamed"
)

// Unmapped is a moon task the sync maps to no Putnami command: it keeps
// running through moon.
type Unmapped struct {
	// Project is the moon project id, Task the moon task.
	Project, Task string
	// Reason says why no command runs it.
	Reason string
}

// SyncResult is what one sync did.
type SyncResult struct {
	stacks.GraphResult
	// Mode is the adoption mode the sync wrote.
	Mode string
	// Extensions are the language extensions the sync declared in the
	// workspace, sorted; empty in delegate mode.
	Extensions []string
	// Unmapped are the moon tasks no Putnami command runs, by project then
	// task.
	Unmapped []Unmapped
	// Unreported are the moon ids of the delegated projects whose test task
	// reads no report: its result is moon's exit code alone.
	Unreported []string
}

// Sync writes the moon project graph of the workspace at root into each
// project's putnami.json, in the workspace's mode: the tag moon, the
// extension the mode maps the project to, its dependencies on other moon
// projects as project IDs, build, lint and test in disable.jobs when the
// project has no moon task for them, and the step of a task moon tags
// rather than names. In import mode it declares in the workspace config the
// language extensions it maps projects to. A root that is no moon workspace
// gets an empty result.
func Sync(ctx context.Context, root string) (SyncResult, error) {
	return synchronize(ctx, root, false)
}

// Stale answers what Sync would change, and writes nothing.
func Stale(ctx context.Context, root string) (SyncResult, error) {
	return synchronize(ctx, root, true)
}

func synchronize(_ context.Context, root string, dry bool) (SyncResult, error) {
	fromExport := fileExists(root, detect.DefaultGraphFile)
	if !detect.IsWorkspace(root) && !fromExport {
		return SyncResult{}, nil
	}
	detection := detect.Detect(root, detect.Options{})
	mode := Mode(root)
	result := SyncResult{Mode: mode}
	extensions := map[string]bool{}
	var packages []stacks.GraphPackage
	for _, p := range detection.Projects {
		if p.Dir == wsproto.ProbeRootPath {
			continue
		}
		extension := ExtensionFor(mode, p)
		mapped := map[string]string{}
		var disabled []string
		for _, command := range Commands {
			if task := TaskFor(p, command); task != "" {
				mapped[command] = task
			} else {
				disabled = append(disabled, command)
			}
		}
		for _, t := range p.Tasks {
			if !slices.Contains(slices.Collect(maps.Values(mapped)), t.Name) {
				result.Unmapped = append(result.Unmapped, Unmapped{Project: p.ID, Task: t.Name, Reason: fmt.Sprintf("neither named nor tagged %s; it keeps running through moon run %s:%s", strings.Join(Commands, ", "), p.ID, t.Name)})
			}
		}
		delegated := extension == ExtensionName
		if delegated && mapped["test"] != "" && !namesReports(root, p.Dir) {
			result.Unreported = append(result.Unreported, p.ID)
		}
		if !delegated {
			extensions[extension] = true
		}
		packages = append(packages, stacks.GraphPackage{
			Dir:       p.Dir,
			DependsOn: p.DependsOn,
			Extension: extension,
			Disabled:  disabled,
			Edit:      editSteps(mapped, delegated),
		})
	}
	sortUnmapped(result.Unmapped)
	graph, err := graphSync.Apply(root, packages, dry)
	if err != nil {
		return SyncResult{}, err
	}
	result.GraphResult = graph
	result.Export = detect.WorkspaceFile
	if detection.FromExport {
		result.Export = detect.DefaultGraphFile
	}
	result.Extensions = slices.Sorted(maps.Keys(extensions))
	if !graph.EmptyExport {
		changed, err := declareExtensions(root, result.Extensions, dry)
		if err != nil {
			return SyncResult{}, fmt.Errorf("%s: %w", wsproto.WorkspaceConfigFilename, err)
		}
		if changed {
			result.Written = append(result.Written, wsproto.WorkspaceConfigFilename)
		}
	}
	return result, nil
}

// TaskFor is the moon task of the project that the command runs: the task
// named after the command, else the first, by name, tagged with it; empty
// when there is none.
func TaskFor(p detect.Project, command string) string {
	for _, t := range p.Tasks {
		if t.Name == command {
			return t.Name
		}
	}
	for _, t := range p.Tasks {
		if slices.Contains(t.Tags, command) {
			return t.Name
		}
	}
	return ""
}

// stepsKey is the member of a task override that holds its steps.
const stepsKey = "steps"

// editSteps writes, for a delegated project, the steps of each command whose
// moon task is not named after it, under the project's override of the moon
// profile, and removes every other steps override of the commands: the
// profile's own step runs a task of the command's name.
func editSteps(mapped map[string]string, delegated bool) func(map[string]any) {
	return func(doc map[string]any) {
		steps := map[string][][]string{}
		if delegated {
			for command, task := range mapped {
				if task != command {
					steps[command] = [][]string{{"moon", "run", "{component}:" + task}}
				}
			}
		}
		setSteps(doc, steps)
	}
}

// retractSteps removes the steps overrides the sync wrote from a former
// project's putnami.json.
func retractSteps(doc map[string]any) {
	setSteps(doc, nil)
}

// setSteps writes the steps of options["@putnami/moon"].tasks.<command> for
// each command, removes those of the other commands, and removes each object
// left empty, so the project's own members, such as its reports, stay.
func setSteps(doc map[string]any, steps map[string][][]string) {
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[ExtensionName].(map[string]any)
	tasks, _ := layer["tasks"].(map[string]any)
	if len(steps) == 0 && tasks == nil {
		return
	}
	if options == nil {
		options = map[string]any{}
	}
	if layer == nil {
		layer = map[string]any{}
	}
	if tasks == nil {
		tasks = map[string]any{}
	}
	for _, command := range Commands {
		task, _ := tasks[command].(map[string]any)
		if s, ok := steps[command]; ok {
			if task == nil {
				task = map[string]any{}
			}
			list := make([]any, len(s))
			for i, step := range s {
				args := make([]any, len(step))
				for j, a := range step {
					args[j] = a
				}
				list[i] = args
			}
			task[stepsKey] = list
			tasks[command] = task
			continue
		}
		if task == nil {
			continue
		}
		delete(task, stepsKey)
		if len(task) == 0 {
			delete(tasks, command)
		}
	}
	setMember(layer, "tasks", tasks)
	setMember(options, ExtensionName, layer)
	setMember(doc, "options", options)
}

// setMember sets the object at key, or removes it when it is empty.
func setMember(doc map[string]any, key string, value map[string]any) {
	if len(value) == 0 {
		delete(doc, key)
		return
	}
	doc[key] = value
}

// namesReports reports whether the project's putnami.json names a report
// of its test task under its override of the moon profile.
func namesReports(root, dir string) bool {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), wsproto.ConfigFilename)) //nolint:gosec // a project file under the workspace root
	if err != nil {
		return false
	}
	var project struct {
		Options map[string]struct {
			Tasks map[string]struct {
				Reports json.RawMessage `json:"reports"`
			} `json:"tasks"`
		} `json:"options"`
	}
	if json.Unmarshal(data, &project) != nil {
		return false
	}
	reports := bytes.TrimSpace(project.Options[ExtensionName].Tasks["test"].Reports)
	return len(reports) > 0 && !bytes.Equal(reports, []byte("null"))
}

// declareExtensions adds to the workspace config's extensions each of the
// language extensions it does not declare yet, and reports whether it changed
// the file. It never removes one: a workspace may run a language extension
// for its own projects.
func declareExtensions(root string, extensions []string, dry bool) (bool, error) {
	if len(extensions) == 0 {
		return false, nil
	}
	file := filepath.Join(root, wsproto.WorkspaceConfigFilename)
	data, err := os.ReadFile(file) //nolint:gosec // the workspace config under the root
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, fmt.Errorf("not a JSON object: %w", err)
	}
	var declared []string
	if raw, ok := cfg["extensions"]; ok {
		if err := json.Unmarshal(raw, &declared); err != nil {
			return false, fmt.Errorf("extensions is not a list of names: %w", err)
		}
	}
	missing := slices.DeleteFunc(slices.Clone(extensions), func(e string) bool { return slices.Contains(declared, e) })
	if len(missing) == 0 {
		return false, nil
	}
	if dry {
		return true, nil
	}
	return true, writeExtensions(file, data, append(declared, missing...))
}

// writeExtensions rewrites the extensions member of the workspace config,
// keeping every other member, in its order, two-space indented.
func writeExtensions(file string, data []byte, extensions []string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	var keys []string
	members := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if _, seen := members[key]; !seen {
			keys = append(keys, key)
		}
		members[key] = raw
	}
	list, err := json.Marshal(extensions)
	if err != nil {
		return err
	}
	if _, ok := members["extensions"]; !ok {
		keys = append(keys, "extensions")
	}
	members["extensions"] = list
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, key := range keys {
		name, _ := json.Marshal(key)
		var value bytes.Buffer
		if err := json.Indent(&value, members[key], "  ", "  "); err != nil {
			return err
		}
		fmt.Fprintf(&buf, "  %s: %s", name, value.Bytes())
		if i < len(keys)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	return os.WriteFile(file, buf.Bytes(), info.Mode().Perm()) //nolint:gosec // the workspace config under the root
}

// graphSync writes the moon project graph: a directory that holds a moon.yml
// is a moon project, whose edges the sync owns, and the tag moon marks every
// project the sync wrote, whatever extension it runs.
var graphSync = stacks.GraphSync{
	Extension:      ExtensionName,
	Extensions:     slices.Sorted(maps.Values(LanguageExtensions)),
	Tag:            profileID,
	Jobs:           Commands,
	HoldsBuildFile: func(root, dir string) bool { return fileExists(root, path.Join(dir, detect.ProjectFile)) },
	Retract:        retractSteps,
}

func fileExists(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil && info.Mode().IsRegular()
}

// graphJobs words the summary of a sync of the moon project graph.
var graphJobs = stacks.GraphJobs{
	Tool:          profileID,
	GraphFile:     detect.DefaultGraphFile,
	ExportCommand: detect.ExportCommand,
}

// RunSync is the job of the workspace sync task: Sync on the workspace root,
// reported line by line, with each moon task it maps to no command and each
// delegated project whose test reads no report.
func RunSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	result, err := Sync(context.Background(), ctx.WorkspaceRoot)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	if result.Export == "" || result.EmptyExport {
		emit.DiagnosticWithCode("warning", summary(result), "", 0, 0, CodeSyncNoExport)
	} else {
		emit.Info(summary(result))
	}
	for _, u := range result.Unmapped {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("moon task %s:%s is %s", u.Project, u.Task, u.Reason), "", 0, 0, CodeTaskUnmapped)
	}
	if len(result.Unreported) > 0 {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("the test task of %s names no report under options[%q].tasks.test.reports: its result is moon's exit code alone", strings.Join(result.Unreported, ", "), ExtensionName), "", 0, 0, CodeReportsUnnamed)
	}
	unmapped := make([]string, 0, len(result.Unmapped))
	for _, u := range result.Unmapped {
		unmapped = append(unmapped, u.Project+":"+u.Task)
	}
	return "OK", map[string]any{
		"mode":       result.Mode,
		"written":    result.Written,
		"retracted":  result.Retracted,
		"extensions": result.Extensions,
		"unmapped":   unmapped,
		"unreported": result.Unreported,
	}, nil
}

// summary is the line a sync reports.
func summary(r SyncResult) string {
	if r.Export == "" {
		return fmt.Sprintf("no %s and no %s at the workspace root: no putnami.json changes", detect.WorkspaceFile, detect.DefaultGraphFile)
	}
	line := graphJobs.Summary(r.GraphResult)
	if r.EmptyExport {
		return line
	}
	line += fmt.Sprintf(", in %s mode", r.Mode)
	if len(r.Extensions) > 0 {
		line += ", importing into " + strings.Join(r.Extensions, ", ")
	}
	return line
}

// RunCheck is the job of the validate task: it fails when a sync would change
// a putnami.json, so a change that altered moon's edges cannot pass a gate
// that selected the projects on the old ones.
func RunCheck(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	result, err := Stale(context.Background(), ctx.WorkspaceRoot)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	if result.Export == "" || result.EmptyExport {
		emit.DiagnosticWithCode("warning", "the project graph cannot be checked: "+summary(result), "", 0, 0, CodeSyncNoExport)
		return "OK", nil, nil
	}
	stale := append(slices.Clone(result.Written), result.Retracted...)
	if len(stale) == 0 {
		return "OK", nil, nil
	}
	for _, file := range stale {
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s no longer matches the moon project graph of %s: run putnami projects sync and commit the result", file, result.Export), file, 0, 0, CodeSyncStale)
	}
	return "FAILED", nil, errors.New(CodeSyncStale)
}

// sortUnmapped orders the unmapped tasks by project, then task.
func sortUnmapped(list []Unmapped) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Project != list[j].Project {
			return list[i].Project < list[j].Project
		}
		return list[i].Task < list[j].Task
	})
}
