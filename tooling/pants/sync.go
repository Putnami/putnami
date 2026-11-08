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
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	detect "go.putnami.dev/tooling/observe/detect/pants"
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
}

// Sync writes the package graph of the Pants repository at root into each
// component's putnami.json: the extension, the edges of the export as project
// IDs, and test in disable.jobs when the component holds no test target. The
// edges come from the export pants-graph.json at the root, or from pants
// peek :: when there is none. Without either it writes nothing: the result's
// Export is empty. A root without pants.toml is no Pants repository and gets
// an empty result.
func Sync(ctx context.Context, root string, stderr io.Writer) (SyncResult, error) {
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
	components := map[string]bool{}
	for _, c := range detection.Components {
		components[wsproto.ProjectIDFromPath(c.ID)] = true
	}
	for _, c := range detection.Components {
		if c.ID == wsproto.ProbeRootPath {
			continue
		}
		var deps []string
		for _, d := range withoutRoot(c.DependsOn) {
			deps = append(deps, wsproto.ProjectIDFromPath(d))
		}
		file := filepath.Join(root, filepath.FromSlash(c.ID), wsproto.ConfigFilename)
		changed, err := writeProject(file, deps, components, detection.IsTested(c.ID))
		if err != nil {
			return SyncResult{}, fmt.Errorf("%s/%s: %w", c.ID, wsproto.ConfigFilename, err)
		}
		if changed {
			result.Written = append(result.Written, c.ID+"/"+wsproto.ConfigFilename)
		} else {
			result.Unchanged++
		}
	}
	return result, nil
}

// peek runs pants peek :: in root and writes its output to a temporary file.
func peek(ctx context.Context, root string, stderr io.Writer) (string, func(), error) {
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
// and writes it when its content changes. Every member the sync does not own
// is kept: dependencies on a project that is no Pants component, other
// extensions, other disabled jobs.
func writeProject(file string, deps []string, components map[string]bool, tested bool) (bool, error) {
	doc := map[string]any{}
	existing, err := os.ReadFile(file) //nolint:gosec // the component's own putnami.json
	switch {
	case errors.Is(err, os.ErrNotExist):
		doc["$schema"] = ProjectSchema
	case err != nil:
		return false, err
	default:
		if err := json.Unmarshal(existing, &doc); err != nil {
			return false, fmt.Errorf("not a JSON object: %w", err)
		}
	}

	extensions := stringList(doc["extensions"])
	if !slices.Contains(extensions, ExtensionName) {
		extensions = append(extensions, ExtensionName)
	}
	doc["extensions"] = extensions

	kept := slices.DeleteFunc(stringList(doc["dependencies"]), func(d string) bool { return components[d] })
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
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil { //nolint:gosec // a tracked project file
		return false, err
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
	emit.Info(syncSummary(result))
	return "OK", map[string]any{"written": result.Written}, nil
}

// CodeSyncFailed is the diagnostic code of a sync that wrote nothing.
const CodeSyncFailed = "pants.sync_failed"

func syncSummary(r SyncResult) string {
	if r.Export == "" {
		return fmt.Sprintf("no %s at the workspace root and no pants on PATH: the putnami.json files keep their edges; write the export with `%s`, then run putnami projects sync", detect.DefaultGraphFile, detect.ExportCommand)
	}
	return fmt.Sprintf("the package graph of %s: %d putnami.json written, %d current", r.Export, len(r.Written), r.Unchanged)
}
