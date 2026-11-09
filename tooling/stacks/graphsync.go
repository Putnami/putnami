package stacks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// ProjectSchema is the schema a putnami.json the graph sync creates names.
const ProjectSchema = "https://putnami.dev/schemas/putnami-project.json"

// GraphPackage is one package of a delegated tool's export.
type GraphPackage struct {
	// Dir is the package's workspace-relative directory.
	Dir string
	// DependsOn lists the workspace-relative directories of the packages and
	// the other directories it depends on. The workspace root is no edge.
	DependsOn []string
	// Inputs are file patterns, relative to Dir, that select the package
	// though no edge reaches them, such as a lockfile outside every member.
	Inputs []string
	// Tested reports that the package holds a test target: without one, the
	// sync disables its test job.
	Tested bool
}

// GraphSync writes the package graph a delegated tool's export states into
// each package's putnami.json (ADR 0008): the extension, the edges as project
// IDs, the inputs as the filePatterns of the extension's option layer, and
// test in disable.jobs without a test target. It owns those members and keeps
// every other one.
type GraphSync struct {
	// Extension is the stack extension's name, which marks the packages the
	// sync owns.
	Extension string
	// HoldsBuildFile reports whether a workspace-relative directory holds a
	// build file of the tool: a dependency on such a directory is the sync's
	// own, so it removes one the export no longer states.
	HoldsBuildFile func(root, dir string) bool
	// SkippedDirs are directory names the walk for marked packages never
	// enters, besides hidden directories: the tool's outputs and caches.
	SkippedDirs []string
}

// GraphResult is what one graph sync did.
type GraphResult struct {
	// Export names where the edges came from: the export file, or the
	// command that printed it. Empty when the sync had no export and wrote
	// nothing.
	Export string
	// Written are the workspace-relative putnami.json files the sync created
	// or changed, sorted.
	Written []string
	// Unchanged counts the files whose content was already current.
	Unchanged int
	// Retracted are the workspace-relative putnami.json files of former
	// packages the sync removed the extension and the tool's edges from,
	// sorted.
	Retracted []string
	// EmptyExport reports an export that lists no package while packages
	// are marked: the sync wrote nothing, so a filtered export retracts none.
	EmptyExport bool
}

// Apply writes the packages' facts into their putnami.json under root, and
// retracts each former package: a directory whose putnami.json names the
// extension and that is no package any more. With dry set it writes nothing
// and answers what it would change.
func (g GraphSync) Apply(root string, packages []GraphPackage, dry bool) (GraphResult, error) {
	var result GraphResult
	current := map[string]bool{}
	for _, p := range packages {
		if p.Dir != wsproto.ProbeRootPath {
			current[p.Dir] = true
		}
	}
	// The sync owns the edges between the projects it marks: the current
	// packages, and the former ones whose putnami.json still names the
	// extension.
	former, err := g.markedProjects(root)
	if err != nil {
		return GraphResult{}, err
	}
	if len(current) == 0 && len(former) > 0 {
		return GraphResult{EmptyExport: true}, nil
	}
	components := map[string]bool{}
	for dir := range current {
		components[wsproto.ProjectIDFromPath(dir)] = true
	}
	for _, dir := range former {
		components[wsproto.ProjectIDFromPath(dir)] = true
	}
	for _, p := range packages {
		if p.Dir == wsproto.ProbeRootPath {
			continue
		}
		var deps []string
		for _, d := range p.DependsOn {
			if d != wsproto.ProbeRootPath {
				deps = append(deps, wsproto.ProjectIDFromPath(d))
			}
		}
		file := filepath.Join(root, filepath.FromSlash(p.Dir), wsproto.ConfigFilename)
		changed, err := g.writeProject(root, file, deps, p.Inputs, components, p.Tested, dry)
		if err != nil {
			return GraphResult{}, fmt.Errorf("%s/%s: %w", p.Dir, wsproto.ConfigFilename, err)
		}
		if changed {
			result.Written = append(result.Written, p.Dir+"/"+wsproto.ConfigFilename)
		} else {
			result.Unchanged++
		}
	}
	slices.Sort(result.Written)
	for _, dir := range former {
		if current[dir] {
			continue
		}
		file := filepath.Join(root, filepath.FromSlash(dir), wsproto.ConfigFilename)
		changed, err := g.retractProject(root, file, components, dry)
		if err != nil {
			return GraphResult{}, fmt.Errorf("%s/%s: %w", dir, wsproto.ConfigFilename, err)
		}
		if changed {
			result.Retracted = append(result.Retracted, dir+"/"+wsproto.ConfigFilename)
		}
	}
	return result, nil
}

// filePatternsKey is the member of an option layer that names the files that
// select the project.
const filePatternsKey = "filePatterns"

// setInputs writes the file patterns of the extension's option layer, and
// removes the layer and options when nothing is left in them.
func (g GraphSync) setInputs(doc map[string]any, inputs []string) {
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[g.Extension].(map[string]any)
	if len(inputs) > 0 {
		if options == nil {
			options = map[string]any{}
			doc["options"] = options
		}
		if layer == nil {
			layer = map[string]any{}
			options[g.Extension] = layer
		}
		layer[filePatternsKey] = inputs
		return
	}
	if layer == nil {
		return
	}
	delete(layer, filePatternsKey)
	if len(layer) == 0 {
		delete(options, g.Extension)
	}
	if len(options) == 0 {
		delete(doc, "options")
	}
}

// markedProjects lists, sorted, the workspace-relative directories under root
// whose putnami.json names the extension. It skips hidden directories,
// node_modules and the sync's skipped directories.
func (g GraphSync) markedProjects(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			name := entry.Name()
			if p != root && (strings.HasPrefix(name, ".") || name == "node_modules" || slices.Contains(g.SkippedDirs, name)) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != wsproto.ConfigFilename {
			return nil
		}
		doc, _, err := readProject(p)
		if err != nil || !slices.Contains(stringList(doc["extensions"]), g.Extension) {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil || rel == "." {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(out)
	return out, err
}

// readProject reads the putnami.json at file as a JSON object, with its
// bytes; a missing file reads as an empty object and nil bytes.
func readProject(file string) (map[string]any, []byte, error) {
	doc := map[string]any{}
	existing, err := os.ReadFile(file) //nolint:gosec // a project file under the workspace root
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(existing))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("not a JSON object: %w", err)
	}
	return doc, existing, nil
}

// cloneDoc deep-copies a decoded JSON object, so the members the sync owns
// can be compared before and after it edits them.
func cloneDoc(doc map[string]any) map[string]any {
	data, _ := json.Marshal(doc)
	out := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

// keepDependency reports whether the sync keeps a dependency it does not
// write: one on a project that is no package of the tool, current or former,
// and, for a project ID, whose directory exists and holds no build file.
func (g GraphSync) keepDependency(root, dep string, components map[string]bool) bool {
	if components[dep] {
		return false
	}
	if rel, ok := strings.CutPrefix(dep, "/"); ok {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return false
		}
		if g.HoldsBuildFile != nil && g.HoldsBuildFile(root, rel) {
			return false
		}
	}
	return true
}

// retractProject removes from the putnami.json of a former package the
// extension and the tool's edges, and writes it when its content changes.
func (g GraphSync) retractProject(root, file string, components map[string]bool, dry bool) (bool, error) {
	doc, existing, err := readProject(file)
	if err != nil {
		return false, err
	}
	before := cloneDoc(doc)
	setList(doc, "extensions", slices.DeleteFunc(stringList(doc["extensions"]), func(e string) bool { return e == g.Extension }))
	setList(doc, "dependencies", slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !g.keepDependency(root, d, components) }))
	g.setInputs(doc, nil)
	if reflect.DeepEqual(cloneDoc(doc), before) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	return writeJSON(file, doc, existing)
}

// writeProject merges the package's facts into the putnami.json at file and
// writes it when a member the sync owns changes: the extension, the
// dependencies on the tool's packages, the inputs and test in disable.jobs.
// Every other member is kept as it is.
func (g GraphSync) writeProject(root, file string, deps, inputs []string, components map[string]bool, tested, dry bool) (bool, error) {
	doc, existing, err := readProject(file)
	if err != nil {
		return false, err
	}
	if existing == nil {
		doc["$schema"] = ProjectSchema
	}
	before := cloneDoc(doc)

	extensions := stringList(doc["extensions"])
	if !slices.Contains(extensions, g.Extension) {
		extensions = append(extensions, g.Extension)
	}
	doc["extensions"] = extensions

	kept := slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !g.keepDependency(root, d, components) })
	setList(doc, "dependencies", sortedUnique(append(kept, deps...)))

	disable, _ := doc["disable"].(map[string]any)
	if disable == nil {
		disable = map[string]any{}
	}
	jobs := slices.DeleteFunc(stringList(disable["jobs"]), func(j string) bool { return j == "test" })
	if !tested {
		jobs = append(jobs, "test")
	}
	setList(disable, "jobs", jobs)
	if len(disable) == 0 {
		delete(doc, "disable")
	} else {
		doc["disable"] = disable
	}

	g.setInputs(doc, inputs)

	if existing != nil && reflect.DeepEqual(cloneDoc(doc), before) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	return writeJSON(file, doc, existing)
}

// writeJSON writes doc at file, two-space indented with sorted keys, when its
// bytes differ from existing; it reports whether it wrote. The file is
// replaced through a temporary file beside it.
func writeJSON(file string, doc map[string]any, existing []byte) (bool, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return false, err
	}
	if bytes.Equal(buf.Bytes(), existing) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+wsproto.ConfigFilename+".*")
	if err != nil {
		return false, err
	}
	_, werr := tmp.Write(buf.Bytes())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644) //nolint:gosec // a tracked project file
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), file)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return false, werr
	}
	return true, nil
}

// stringList reads a JSON array of strings; any other value reads as empty.
func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func setList(doc map[string]any, key string, list []string) {
	if len(list) == 0 {
		delete(doc, key)
		return
	}
	doc[key] = list
}

func sortedUnique(list []string) []string {
	slices.Sort(list)
	return slices.Compact(list)
}

// GraphJobs are the workspace jobs of a delegated stack's graph sync: the sync
// task that writes the package graph, and the validate task that fails on a
// stale one. Their diagnostic codes are <Tool>.sync_failed,
// <Tool>.sync_no_export and <Tool>.sync_stale.
type GraphJobs struct {
	// Tool is the build tool's command and the prefix of the diagnostic
	// codes, such as pants.
	Tool string
	// GraphFile is the export the sync reads at the workspace root.
	GraphFile string
	// ExportCommand is the command that writes GraphFile.
	ExportCommand string
	// Sync writes the package graph under root, or with dry set answers what
	// it would change. A result with no Export had neither the export nor
	// the tool.
	Sync func(ctx context.Context, root string, stderr io.Writer, dry bool) (GraphResult, error)
}

// Diagnostic code suffixes of the graph jobs.
const (
	// SyncFailedSuffix is a sync that failed and wrote nothing.
	SyncFailedSuffix = ".sync_failed"
	// SyncNoExportSuffix is a sync with neither the export nor the tool,
	// which wrote nothing.
	SyncNoExportSuffix = ".sync_no_export"
	// SyncStaleSuffix is a package whose putnami.json no longer matches the
	// export.
	SyncStaleSuffix = ".sync_stale"
)

// RunSync is the job of the workspace sync task, reported line by line.
func (j GraphJobs) RunSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	result, err := j.Sync(context.Background(), ctx.WorkspaceRoot, os.Stderr, false)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, j.Tool+SyncFailedSuffix)
		return statusFailed, nil, errors.New(j.Tool + SyncFailedSuffix)
	}
	if result.Export == "" || result.EmptyExport {
		emit.DiagnosticWithCode("warning", j.Summary(result), "", 0, 0, j.Tool+SyncNoExportSuffix)
	} else {
		emit.Info(j.Summary(result))
	}
	return statusOK, map[string]any{"written": result.Written, "retracted": result.Retracted}, nil
}

// RunCheck is the job of the validate task: it fails when a sync would change
// a putnami.json, so a change that altered the tool's edges cannot pass a gate
// that selected the projects on the old ones.
func (j GraphJobs) RunCheck(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	result, err := j.Sync(context.Background(), ctx.WorkspaceRoot, os.Stderr, true)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, j.Tool+SyncFailedSuffix)
		return statusFailed, nil, errors.New(j.Tool + SyncFailedSuffix)
	}
	if result.Export == "" || result.EmptyExport {
		emit.DiagnosticWithCode("warning", "the package graph cannot be checked: "+j.Summary(result), "", 0, 0, j.Tool+SyncNoExportSuffix)
		return statusOK, nil, nil
	}
	stale := append(slices.Clone(result.Written), result.Retracted...)
	if len(stale) == 0 {
		return statusOK, nil, nil
	}
	for _, file := range stale {
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s no longer matches %s: run putnami projects sync and commit the result", file, result.Export), file, 0, 0, j.Tool+SyncStaleSuffix)
	}
	return statusFailed, nil, errors.New(j.Tool + SyncStaleSuffix)
}

// Summary is the line a sync reports: what it wrote, or why it wrote nothing.
func (j GraphJobs) Summary(r GraphResult) string {
	if r.EmptyExport {
		return fmt.Sprintf("%s lists no package, so no putnami.json changes: check the target filters of the repository, then run putnami projects sync", r.Export)
	}
	if r.Export == "" {
		return fmt.Sprintf("no %s at the workspace root and no %s on PATH: the putnami.json files keep their edges; write the export with `%s`, then run putnami projects sync", j.GraphFile, j.Tool, j.ExportCommand)
	}
	return fmt.Sprintf("the package graph of %s: %d putnami.json written, %d current, %d former packages retracted", r.Export, len(r.Written), r.Unchanged, len(r.Retracted))
}
