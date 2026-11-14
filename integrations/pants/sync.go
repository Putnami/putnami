package pants

import (
	"context"
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
	"go.putnami.dev/tooling/stacks"
)

// SyncJob is the job name of the workspace sync task.
const SyncJob = "workspace-sync"

// ProjectSchema is the schema a putnami.json the sync creates names.
const ProjectSchema = stacks.ProjectSchema

// SyncResult is what one sync did.
type SyncResult = stacks.GraphResult

// Sync writes the package graph of the Pants repository at root into each
// component's putnami.json: the extension, the edges of the export as project
// IDs, to the other components and to the directories of the third-party
// targets it depends on, and test in disable.jobs when the component holds no
// test target, and as its filePatterns the lockfiles and the files that declare
// the third-party targets it uses, which no edge reaches when they lie outside
// a member directory. The edges come from the export pants-graph.json at the
// root, or, when there is none, from pants peek on the targets pants list ::
// names, in batches. Without either it writes nothing: the result's Export is
// empty. A root without pants.toml is no Pants repository and gets
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
		file, cleanup, err := exportGraph(ctx, root, stderr)
		if err != nil {
			return SyncResult{}, err
		}
		defer cleanup()
		opts.GraphFile = file
		result.Export = exportName
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
	packages := make([]stacks.GraphPackage, 0, len(detection.Components))
	for _, c := range detection.Components {
		if c.ID == wsproto.ProbeRootPath {
			continue
		}
		packages = append(packages, stacks.GraphPackage{
			Dir:       c.ID,
			DependsOn: append(slices.Clone(c.DependsOn), detection.Requires[c.ID]...),
			Inputs:    requiredInputs(root, c.ID, detection),
			Tested:    detection.IsTested(c.ID),
		})
	}
	applied, err := graphSync.Apply(root, packages, dry)
	if err != nil {
		return SyncResult{}, err
	}
	applied.Export = result.Export
	for _, d := range detection.Diagnostics {
		if d.Severity == diag.Warning {
			applied.Warnings = append(applied.Warnings, d.Message)
		}
	}
	return applied, nil
}

// graphSync writes the Pants package graph: a directory that holds a BUILD
// file is a Pants directory, whose edges the sync owns.
var graphSync = stacks.GraphSync{
	Extension:      ExtensionName,
	HoldsBuildFile: func(root, dir string) bool { return buildFile(root, dir) != "" },
	SkippedDirs:    []string{"dist", "__pycache__"},
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
	slices.Sort(out)
	return slices.Compact(out)
}

// profileID is the id of the Pants profile, which names its stack-root lock.
const profileID = "pants"

// graphJobs are the workspace sync task and the validate task of the Pants
// package graph.
var graphJobs = stacks.GraphJobs{
	Tool:          "pants",
	GraphFile:     detect.DefaultGraphFile,
	ExportCommand: detect.ExportCommand,
	Sync:          synchronize,
}

// RunSync is the job of the workspace sync task: Sync on the workspace root,
// reported line by line.
func RunSync(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return graphJobs.RunSync(ctx, emit, args)
}

// Diagnostic codes of the sync.
const (
	// CodeSyncFailed is a sync that failed and wrote nothing.
	CodeSyncFailed = "pants" + stacks.SyncFailedSuffix
	// CodeSyncNoExport is a sync with neither the export nor pants, which
	// wrote nothing.
	CodeSyncNoExport = "pants" + stacks.SyncNoExportSuffix
	// CodeSyncStale is a package whose putnami.json no longer matches the
	// export.
	CodeSyncStale = "pants" + stacks.SyncStaleSuffix
)

// CheckJob is the job name of the task that fails on a stale package graph.
const CheckJob = "pants-graph-check"

// RunCheck is the job of the validate task: it fails when a sync would change
// a putnami.json, so a source change that altered the edges Pants infers
// cannot pass a gate that selected the projects on the old ones.
func RunCheck(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return graphJobs.RunCheck(ctx, emit, args)
}

func syncSummary(r SyncResult) string {
	return graphJobs.Summary(r)
}
