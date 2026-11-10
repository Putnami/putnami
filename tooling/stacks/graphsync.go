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
	// sync disables its test job. Ignored when the sync names its Jobs.
	Tested bool
	// Extension is the extension the package runs, one of the sync's
	// Extension and Extensions; empty is the sync's Extension.
	Extension string
	// Disabled are the jobs, among the sync's Jobs, the package runs none
	// of, such as a command its tool has no task for.
	Disabled []string
	// Edit, when set, writes the members of the package's putnami.json the
	// stack owns beyond the sync's, after them, and is called on every write.
	Edit func(doc map[string]any)
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
	// Extensions are the other extensions a package may run, such as the
	// language extensions a tool's projects are imported into: the sync
	// writes the package's own and removes the others.
	Extensions []string
	// Tag, when set, is a tag the sync writes on every package, for
	// selection; it marks nothing, since a team may tag its own projects so.
	Tag string
	// Marked, when set, makes the sync write MarkMember in the extension's
	// option layer of every package and read it as the mark of its packages,
	// whatever extension a package runs. Unset, a package is the sync's when
	// it names the extension.
	Marked bool
	// Jobs, when set, are the jobs the sync owns in disable.jobs, each
	// package naming those it disables in Disabled; unset, the sync owns test
	// alone, by Tested.
	Jobs []string
	// Retract, when set, removes from a former package's putnami.json the
	// members its packages' Edit wrote.
	Retract func(doc map[string]any)
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
	// Changes are, in a dry run, the content each file of Written and
	// Retracted holds and would hold, in the same order: Written's, then
	// Retracted's.
	Changes []FileChange
	// Warnings are what the tool's sync reports beside its summary, as
	// warnings so that a sync that passes still shows them, such as a
	// command it does not map or a cache the tool keeps off.
	Warnings []string
}

// FileChange is a file a sync changes: its workspace-relative path, its
// content before and after. A nil Before is a file the sync creates.
type FileChange struct {
	File          string
	Before, After []byte
}

// Apply writes the packages' facts into their putnami.json under root, and
// retracts each former package: a directory whose putnami.json names the
// extension and that is no package any more. With dry set it writes nothing
// and answers what it would change.
func (g GraphSync) Apply(root string, packages []GraphPackage, dry bool) (GraphResult, error) {
	var result GraphResult
	var written, retracted []FileChange
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
		changed, change, err := g.writeProject(root, file, deps, p, components, dry)
		if err != nil {
			return GraphResult{}, fmt.Errorf("%s/%s: %w", p.Dir, wsproto.ConfigFilename, err)
		}
		if changed {
			result.Written = append(result.Written, p.Dir+"/"+wsproto.ConfigFilename)
			if change != nil {
				change.File = p.Dir + "/" + wsproto.ConfigFilename
				written = append(written, *change)
			}
		} else {
			result.Unchanged++
		}
	}
	slices.Sort(result.Written)
	slices.SortFunc(written, func(a, b FileChange) int { return strings.Compare(a.File, b.File) })
	for _, dir := range former {
		if current[dir] {
			continue
		}
		file := filepath.Join(root, filepath.FromSlash(dir), wsproto.ConfigFilename)
		changed, change, err := g.retractProject(root, file, components, dry)
		if err != nil {
			return GraphResult{}, fmt.Errorf("%s/%s: %w", dir, wsproto.ConfigFilename, err)
		}
		if changed {
			result.Retracted = append(result.Retracted, dir+"/"+wsproto.ConfigFilename)
			if change != nil {
				change.File = dir + "/" + wsproto.ConfigFilename
				retracted = append(retracted, *change)
			}
		}
	}
	if dry {
		result.Changes = append(slices.Clone(written), retracted...)
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
		if err != nil || !g.marks(doc) {
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

// marks reports whether a putnami.json is the sync's: it names the
// extension, or, when the sync is Marked, carries MarkMember in the
// extension's option layer.
func (g GraphSync) marks(doc map[string]any) bool {
	if slices.Contains(stringList(doc["extensions"]), g.Extension) {
		return true
	}
	if !g.Marked {
		return false
	}
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[g.Extension].(map[string]any)
	return layer[MarkMember] == true
}

// MarkMember is the member of a stack extension's option layer that marks a
// project as its graph sync's: the sync writes it, and a profile override
// ignores it.
const MarkMember = "graphSync"

// setMark writes or removes the sync's mark in the extension's option layer,
// and removes the layer and options when nothing is left in them.
func (g GraphSync) setMark(doc map[string]any, marked bool) {
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[g.Extension].(map[string]any)
	if marked {
		if options == nil {
			options = map[string]any{}
			doc["options"] = options
		}
		if layer == nil {
			layer = map[string]any{}
			options[g.Extension] = layer
		}
		layer[MarkMember] = true
		return
	}
	if layer == nil {
		return
	}
	delete(layer, MarkMember)
	if len(layer) == 0 {
		delete(options, g.Extension)
	}
	if len(options) == 0 {
		delete(doc, "options")
	}
}

// owns reports whether the sync writes the extension.
func (g GraphSync) owns(extension string) bool {
	return extension == g.Extension || slices.Contains(g.Extensions, extension)
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

// SyncedEdges are the edges the sync owns in the putnami.json of the package
// at dir, as workspace-relative directories: each project ID dependency on
// one of packages, the directories of the tool's current packages, or on
// another directory that holds a build file. A probe attributes them to the
// tool's graph (wsproto.DependencySourceBuildGraph): without the export, they
// are the edges the tool's own query reported to the last sync. An unreadable
// file has none: the core reports it.
func (g GraphSync) SyncedEdges(root, dir string, packages map[string]bool) []string {
	doc, _, err := readProject(filepath.Join(root, filepath.FromSlash(dir), wsproto.ConfigFilename))
	if err != nil {
		return nil
	}
	var out []string
	for _, dep := range stringList(doc["dependencies"]) {
		rel, ok := strings.CutPrefix(dep, "/")
		if !ok || rel == "" || rel == dir {
			continue
		}
		if packages[rel] || (g.HoldsBuildFile != nil && g.HoldsBuildFile(root, rel)) {
			out = append(out, rel)
		}
	}
	return sortedUnique(out)
}

// retractProject removes from the putnami.json of a former package the
// extension and the tool's edges, and writes it when its content changes.
func (g GraphSync) retractProject(root, file string, components map[string]bool, dry bool) (bool, *FileChange, error) {
	doc, existing, err := readProject(file)
	if err != nil {
		return false, nil, err
	}
	before := cloneDoc(doc)
	// A former package keeps a language extension it runs: the project
	// stays the team's, and only the stack's own extension leaves.
	setList(doc, "extensions", slices.DeleteFunc(stringList(doc["extensions"]), func(e string) bool { return e == g.Extension }))
	setList(doc, "dependencies", slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !g.keepDependency(root, d, components) }))
	if g.Retract != nil {
		g.Retract(doc)
	}
	g.setInputs(doc, nil)
	if g.Marked {
		g.setMark(doc, false)
	}
	if g.Tag != "" {
		setList(doc, "tags", slices.DeleteFunc(stringList(doc["tags"]), func(t string) bool { return t == g.Tag }))
	}
	if g.Jobs != nil {
		g.setDisabled(doc, nil)
	}
	if reflect.DeepEqual(cloneDoc(doc), before) {
		return false, nil, nil
	}
	return g.commit(file, doc, existing, dry)
}

// writeProject merges the package's facts into the putnami.json at file and
// writes it when a member the sync owns changes: the extension, the
// dependencies on the tool's packages, the inputs, the tag and the jobs it
// owns in disable.jobs, then what the package's Edit owns. Every other member
// is kept as it is.
func (g GraphSync) writeProject(root, file string, deps []string, p GraphPackage, components map[string]bool, dry bool) (bool, *FileChange, error) {
	doc, existing, err := readProject(file)
	if err != nil {
		return false, nil, err
	}
	if existing == nil {
		doc["$schema"] = ProjectSchema
	}
	before := cloneDoc(doc)

	extension := p.Extension
	if extension == "" {
		extension = g.Extension
	}
	extensions := slices.DeleteFunc(stringList(doc["extensions"]), func(e string) bool { return e != extension && g.owns(e) })
	if !slices.Contains(extensions, extension) {
		extensions = append(extensions, extension)
	}
	doc["extensions"] = extensions
	if g.Tag != "" {
		tags := stringList(doc["tags"])
		if !slices.Contains(tags, g.Tag) {
			tags = append(tags, g.Tag)
		}
		doc["tags"] = tags
	}

	kept := slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !g.keepDependency(root, d, components) })
	setList(doc, "dependencies", sortedUnique(append(kept, deps...)))

	disabled := p.Disabled
	if g.Jobs == nil && !p.Tested {
		disabled = []string{"test"}
	}
	g.setDisabled(doc, disabled)

	if p.Edit != nil {
		p.Edit(doc)
	}
	g.setInputs(doc, p.Inputs)
	if g.Marked {
		g.setMark(doc, true)
	}

	if existing != nil && reflect.DeepEqual(cloneDoc(doc), before) {
		return false, nil, nil
	}
	return g.commit(file, doc, existing, dry)
}

// setDisabled writes the jobs the sync owns in disable.jobs: those of
// disabled, after every job the sync does not own, kept in its place.
func (g GraphSync) setDisabled(doc map[string]any, disabled []string) {
	owned := g.Jobs
	if owned == nil {
		owned = []string{"test"}
	}
	disable, _ := doc["disable"].(map[string]any)
	if disable == nil {
		disable = map[string]any{}
	}
	jobs := slices.DeleteFunc(stringList(disable["jobs"]), func(j string) bool { return slices.Contains(owned, j) })
	for _, j := range owned {
		if slices.Contains(disabled, j) {
			jobs = append(jobs, j)
		}
	}
	setList(disable, "jobs", jobs)
	if len(disable) == 0 {
		delete(doc, "disable")
	} else {
		doc["disable"] = disable
	}
}

// commit writes doc at file, or in a dry run answers the change it would
// make, with the content the file would hold.
func (g GraphSync) commit(file string, doc map[string]any, existing []byte, dry bool) (bool, *FileChange, error) {
	if !dry {
		changed, err := writeJSON(file, doc, existing)
		return changed, nil, err
	}
	after, err := renderJSON(doc)
	if err != nil {
		return false, nil, err
	}
	if bytes.Equal(after, existing) {
		return false, nil, nil
	}
	return true, &FileChange{Before: existing, After: after}, nil
}

// renderJSON is doc as the sync writes it: two-space indented, with sorted
// keys and a final newline.
func renderJSON(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeJSON writes doc at file, two-space indented with sorted keys, when its
// bytes differ from existing; it reports whether it wrote. The file is
// replaced through a temporary file beside it.
func writeJSON(file string, doc map[string]any, existing []byte) (bool, error) {
	data, err := renderJSON(doc)
	if err != nil {
		return false, err
	}
	buf := bytes.NewBuffer(data)
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
	// SyncPreviewSuffix is a putnami.json a dry run of the sync would change.
	SyncPreviewSuffix = ".sync_preview"
)

// RunSync is the job of the workspace sync task, reported line by line.
// With the dryRun parameter it answers what a sync would change, names each
// file, and writes nothing.
func (j GraphJobs) RunSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	dry := ctx.Params.Bool("dryRun", false)
	result, err := j.Sync(context.Background(), ctx.WorkspaceRoot, os.Stderr, dry)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, j.Tool+SyncFailedSuffix)
		return statusFailed, nil, errors.New(j.Tool + SyncFailedSuffix)
	}
	if result.Export == "" || result.EmptyExport {
		emit.DiagnosticWithCode("warning", j.Summary(result), "", 0, 0, j.Tool+SyncNoExportSuffix)
	} else {
		emit.Info(j.Summary(result))
	}
	for _, warning := range result.Warnings {
		emit.Diagnostic("warning", warning, "", 0)
	}
	if dry {
		EmitPreview(emit, j.Tool, result)
	}
	return statusOK, map[string]any{"written": result.Written, "retracted": result.Retracted, "dryRun": dry}, nil
}

// EmitPreview reports each file a dry run of a sync would change as a
// <tool>.sync_preview warning that carries its diff, so the run's output
// shows it.
func EmitPreview(emit *jsonl.Emitter, tool string, result GraphResult) {
	if len(result.Changes) > 0 {
		for _, c := range result.Changes {
			emit.DiagnosticWithCode("warning", fmt.Sprintf("%s would change:\n%s", c.File, strings.TrimSuffix(Diff(c.File, c.Before, c.After), "\n")), c.File, 0, 0, tool+SyncPreviewSuffix)
		}
		return
	}
	for _, file := range append(slices.Clone(result.Written), result.Retracted...) {
		emit.DiagnosticWithCode("warning", fmt.Sprintf("%s would change", file), file, 0, 0, tool+SyncPreviewSuffix)
	}
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
