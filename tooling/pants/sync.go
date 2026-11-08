package pants

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	detect "go.putnami.dev/tooling/observe/detect/pants"
	"go.putnami.dev/tooling/stacks"
)

// SyncJob is the job name of the workspace sync task.
const SyncJob = "workspace-sync"

// ProjectSchema is the schema a putnami.json the sync creates names.
const ProjectSchema = "https://putnami.dev/schemas/putnami-project.json"

// peekArgs are the arguments of the command that writes the export.
var peekArgs = []string{"peek", "::"}

// SyncResult is what one sync did.
type SyncResult struct {
	// Export names where the edges came from: the export file, or pants
	// peek ::. Empty when the sync had no export and wrote nothing.
	Export string
	// Written are the workspace-relative putnami.json files the sync
	// created or changed, sorted.
	Written []string
	// Unchanged counts the files whose content was already current.
	Unchanged int
	// Retracted are the workspace-relative putnami.json files of former
	// packages the sync removed the extension and the Pants edges from,
	// sorted.
	Retracted []string
	// EmptyExport reports an export that lists no package while packages
	// are marked: the sync wrote nothing, so a filtered export retracts none.
	EmptyExport bool
}

// Sync writes the package graph of the Pants repository at root into each
// component's putnami.json: the extension, the edges of the export as project
// IDs, to the other components and to the directories of the third-party
// targets it depends on, and test in disable.jobs when the component holds no
// test target, and as its filePatterns the lockfiles and the files that declare
// the third-party targets it uses, which no edge reaches when they lie outside
// a member directory. The
// edges come from the export pants-graph.json at the root, or from pants
// peek :: when there is none. Without either it writes nothing: the result's
// Export is empty. A root without pants.toml is no Pants repository and gets
// an empty result.
func Sync(ctx context.Context, root string, stderr io.Writer) (SyncResult, error) {
	return synchronize(ctx, root, stderr, false)
}

// Stale answers what Sync would change, and writes nothing: Written and
// Retracted name the putnami.json files that no longer match the export.
func Stale(ctx context.Context, root string, stderr io.Writer) (SyncResult, error) {
	return synchronize(ctx, root, stderr, true)
}

func synchronize(ctx context.Context, root string, stderr io.Writer, dry bool) (SyncResult, error) {
	var result SyncResult
	if _, err := os.Stat(filepath.Join(root, detect.Marker)); err != nil {
		return result, nil
	}
	opts := detect.Options{}
	result.Export = detect.DefaultGraphFile
	if _, err := os.Stat(filepath.Join(root, detect.DefaultGraphFile)); errors.Is(err, os.ErrNotExist) {
		if _, err := exec.LookPath("pants"); err != nil {
			return SyncResult{}, nil
		}
		file, cleanup, err := peek(ctx, root, stderr)
		if err != nil {
			return SyncResult{}, err
		}
		defer cleanup()
		opts.GraphFile = file
		result.Export = "pants " + strings.Join(peekArgs, " ")
	}
	detection := detect.Detect(root, opts)
	for _, c := range detection.Components {
		if c.Source != evidence.SourceOrchestrator {
			var reasons []string
			for _, d := range detection.Diagnostics {
				if d.Severity != diag.Info {
					reasons = append(reasons, d.Message)
				}
			}
			return SyncResult{}, fmt.Errorf("the Pants export cannot be read, so no edge is written: %s", strings.Join(reasons, "; "))
		}
	}
	current := map[string]bool{}
	for _, c := range detection.Components {
		if c.ID != wsproto.ProbeRootPath {
			current[c.ID] = true
		}
	}
	// The sync owns the edges between the projects it marks: the current
	// packages, and the former ones whose putnami.json still names the
	// extension.
	former, err := markedProjects(root)
	if err != nil {
		return SyncResult{}, err
	}
	if len(current) == 0 && len(former) > 0 {
		return SyncResult{Export: result.Export, EmptyExport: true}, nil
	}
	components := map[string]bool{}
	for dir := range current {
		components[wsproto.ProjectIDFromPath(dir)] = true
	}
	for _, dir := range former {
		components[wsproto.ProjectIDFromPath(dir)] = true
	}
	for _, c := range detection.Components {
		if c.ID == wsproto.ProbeRootPath {
			continue
		}
		var deps []string
		for _, d := range withoutRoot(append(slices.Clone(c.DependsOn), detection.Requires[c.ID]...)) {
			deps = append(deps, wsproto.ProjectIDFromPath(d))
		}
		file := filepath.Join(root, filepath.FromSlash(c.ID), wsproto.ConfigFilename)
		inputs := requiredInputs(root, c.ID, detection)
		changed, err := writeProject(root, file, deps, inputs, components, detection.IsTested(c.ID), dry)
		if err != nil {
			return SyncResult{}, fmt.Errorf("%s/%s: %w", c.ID, wsproto.ConfigFilename, err)
		}
		if changed {
			result.Written = append(result.Written, c.ID+"/"+wsproto.ConfigFilename)
		} else {
			result.Unchanged++
		}
	}
	for _, dir := range former {
		if current[dir] {
			continue
		}
		file := filepath.Join(root, filepath.FromSlash(dir), wsproto.ConfigFilename)
		changed, err := retractProject(root, file, components, dry)
		if err != nil {
			return SyncResult{}, fmt.Errorf("%s/%s: %w", dir, wsproto.ConfigFilename, err)
		}
		if changed {
			result.Retracted = append(result.Retracted, dir+"/"+wsproto.ConfigFilename)
		}
	}
	return result, nil
}

// requiredInputs are the file patterns of a component, relative to its
// directory: the resolve lockfiles and the files that declare the third-party
// targets it uses, and the root BUILD file when it uses a target of the root.
// A component that uses no third-party target has none.
func requiredInputs(root, dir string, detection detect.Detection) []string {
	declared, uses := detection.RequiredFiles[dir]
	if !uses {
		return nil
	}
	files := append(slices.Clone(detection.Lockfiles), declared...)
	if slices.Contains(detection.Requires[dir], wsproto.ProbeRootPath) {
		if build := buildFile(root, wsproto.ProbeRootPath); build != "" {
			files = append(files, build)
		}
	}
	var out []string
	for _, file := range files {
		rel, err := filepath.Rel(filepath.FromSlash(dir), filepath.FromSlash(file))
		if err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return sortedUnique(out)
}

// setInputs writes the file patterns of the extension's option layer, and
// removes the layer and options when nothing is left in them.
func setInputs(doc map[string]any, inputs []string) {
	options, _ := doc["options"].(map[string]any)
	layer, _ := options[ExtensionName].(map[string]any)
	if len(inputs) > 0 {
		if options == nil {
			options = map[string]any{}
			doc["options"] = options
		}
		if layer == nil {
			layer = map[string]any{}
			options[ExtensionName] = layer
		}
		layer[filePatternsKey] = inputs
		return
	}
	if layer == nil {
		return
	}
	delete(layer, filePatternsKey)
	if len(layer) == 0 {
		delete(options, ExtensionName)
	}
	if len(options) == 0 {
		delete(doc, "options")
	}
}

// profileID is the id of the Pants profile, which names its stack-root lock.
const profileID = "pants"

// filePatternsKey is the member of an option layer that names the files that
// select the project.
const filePatternsKey = "filePatterns"

// skippedDirs are never walked for project files: build outputs, installed
// packages and caches.
var skippedDirs = map[string]bool{"dist": true, "node_modules": true, "__pycache__": true}

// markedProjects lists, sorted, the workspace-relative directories under root
// whose putnami.json names the extension. It skips hidden directories, dist,
// node_modules and __pycache__.
func markedProjects(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if p != root && (strings.HasPrefix(entry.Name(), ".") || skippedDirs[entry.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != wsproto.ConfigFilename {
			return nil
		}
		doc, _, err := readProject(p)
		if err != nil || !slices.Contains(stringList(doc["extensions"]), ExtensionName) {
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

// clone deep-copies a decoded JSON object, so the members the sync owns can be
// compared before and after it edits them.
func clone(doc map[string]any) map[string]any {
	data, _ := json.Marshal(doc)
	out := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

// keepDependency reports whether the sync keeps a dependency it does not
// write: one on a project that is no Pants package, current or former, and,
// for a project ID, whose directory exists and holds no BUILD file.
func keepDependency(root, dep string, components map[string]bool) bool {
	if components[dep] {
		return false
	}
	if rel, ok := strings.CutPrefix(dep, "/"); ok {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return false
		}
		if buildFile(root, rel) != "" {
			return false
		}
	}
	return true
}

// retractProject removes from the putnami.json of a former package the
// extension and its Pants edges, and writes it when its content changes.
func retractProject(root, file string, components map[string]bool, dry bool) (bool, error) {
	doc, existing, err := readProject(file)
	if err != nil {
		return false, err
	}
	before := clone(doc)
	setList(doc, "extensions", slices.DeleteFunc(stringList(doc["extensions"]), func(e string) bool { return e == ExtensionName }))
	setList(doc, "dependencies", slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !keepDependency(root, d, components) }))
	setInputs(doc, nil)
	if reflect.DeepEqual(clone(doc), before) {
		return false, nil
	}
	if dry {
		return true, nil
	}
	return writeJSON(file, doc, existing)
}

// peek runs pants peek :: in root and writes its output to a temporary file.
func peek(ctx context.Context, root string, stderr io.Writer) (string, func(), error) {
	// Pants serves one run at a time per repository, so peek waits for the
	// tasks of the stack root, under the lock they hold.
	lock, err := stacks.LockRoot(root, profileID, wsproto.ProbeRootPath)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = lock.Release() }()
	dir, err := os.MkdirTemp("", "putnami-pants-peek-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	file := filepath.Join(dir, detect.DefaultGraphFile)
	out, err := os.Create(file) //nolint:gosec // a file in a directory this call created
	if err != nil {
		cleanup()
		return "", nil, err
	}
	cmd := exec.CommandContext(ctx, "pants", peekArgs...)
	cmd.Dir = root
	cmd.Stdout = out
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if err := out.Close(); err != nil && runErr == nil {
		runErr = err
	}
	if runErr != nil {
		cleanup()
		return "", nil, fmt.Errorf("pants %s: %w", strings.Join(peekArgs, " "), runErr)
	}
	return file, cleanup, nil
}

// writeProject merges the package's Pants facts into the putnami.json at file
// and writes it when a member the sync owns changes: the extension, the
// dependencies on Pants packages, and test in disable.jobs. Every other member
// is kept as it is.
func writeProject(root, file string, deps, inputs []string, components map[string]bool, tested, dry bool) (bool, error) {
	doc, existing, err := readProject(file)
	if err != nil {
		return false, err
	}
	if existing == nil {
		doc["$schema"] = ProjectSchema
	}
	before := clone(doc)

	extensions := stringList(doc["extensions"])
	if !slices.Contains(extensions, ExtensionName) {
		extensions = append(extensions, ExtensionName)
	}
	doc["extensions"] = extensions

	kept := slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return !keepDependency(root, d, components) })
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

	setInputs(doc, inputs)

	if existing != nil && reflect.DeepEqual(clone(doc), before) {
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

// RunSync is the job of the workspace sync task: Sync on the workspace root,
// reported line by line.
func RunSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	result, err := Sync(context.Background(), ctx.WorkspaceRoot, os.Stderr)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	if result.Export == "" || result.EmptyExport {
		emit.DiagnosticWithCode("warning", syncSummary(result), "", 0, 0, CodeSyncNoExport)
	} else {
		emit.Info(syncSummary(result))
	}
	return "OK", map[string]any{"written": result.Written, "retracted": result.Retracted}, nil
}

// Diagnostic codes of the sync.
const (
	// CodeSyncFailed is a sync that failed and wrote nothing.
	CodeSyncFailed = "pants.sync_failed"
	// CodeSyncNoExport is a sync with neither the export nor pants, which
	// wrote nothing.
	CodeSyncNoExport = "pants.sync_no_export"
	// CodeSyncStale is a package whose putnami.json no longer matches the
	// export.
	CodeSyncStale = "pants.sync_stale"
)

// CheckJob is the job name of the task that fails on a stale package graph.
const CheckJob = "pants-graph-check"

// RunCheck is the job of the validate task: it fails when a sync would change
// a putnami.json, so a source change that altered the edges Pants infers
// cannot pass a gate that selected the projects on the old ones.
func RunCheck(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	result, err := Stale(context.Background(), ctx.WorkspaceRoot, os.Stderr)
	if err != nil {
		emit.DiagnosticWithCode("error", err.Error(), "", 0, 0, CodeSyncFailed)
		return "FAILED", nil, errors.New(CodeSyncFailed)
	}
	if result.Export == "" || result.EmptyExport {
		emit.DiagnosticWithCode("warning", "the package graph cannot be checked: "+syncSummary(result), "", 0, 0, CodeSyncNoExport)
		return "OK", nil, nil
	}
	stale := append(slices.Clone(result.Written), result.Retracted...)
	if len(stale) == 0 {
		return "OK", nil, nil
	}
	for _, file := range stale {
		emit.DiagnosticWithCode("error", fmt.Sprintf("%s no longer matches %s: run putnami projects sync and commit the result", file, result.Export), file, 0, 0, CodeSyncStale)
	}
	return "FAILED", nil, errors.New(CodeSyncStale)
}

func syncSummary(r SyncResult) string {
	if r.EmptyExport {
		return fmt.Sprintf("%s lists no package, so no putnami.json changes: check the target filters of the repository, then run putnami projects sync", r.Export)
	}
	if r.Export == "" {
		return fmt.Sprintf("no %s at the workspace root and no pants on PATH: the putnami.json files keep their edges; write the export with `%s`, then run putnami projects sync", detect.DefaultGraphFile, detect.ExportCommand)
	}
	return fmt.Sprintf("the package graph of %s: %d putnami.json written, %d current, %d former packages retracted", r.Export, len(r.Written), r.Unchanged, len(r.Retracted))
}
