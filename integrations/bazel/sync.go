package bazel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	detect "go.putnami.dev/tooling/observe/detect/bazel"
	"go.putnami.dev/tooling/stacks"
)

// SyncJob is the job name of the workspace sync task.
const SyncJob = "workspace-sync"

// CheckJob is the job name of the task that fails on a stale package graph.
const CheckJob = "bazel-graph-check"

// profileID is the id of the Bazel profile, which names its stack-root lock.
const profileID = "bazel"

// queryArgs are the arguments of the query the sync runs without an export:
// every rule of the main repository, with the labels it reads, which give
// the same edges as the CI's deps(//...) export.
var queryArgs = []string{"query", "//...", "--output=streamed_jsonproto"}

// moduleFiles are the files at the workspace root that every package reads:
// the module and workspace files, the lockfile, the flags and the version.
var moduleFiles = []string{"MODULE.bazel", "MODULE.bazel.lock", "WORKSPACE", "WORKSPACE.bazel", ".bazelrc", ".bazelversion"}

// Diagnostic codes of the sync.
const (
	// CodeSyncFailed is a sync that failed and wrote nothing.
	CodeSyncFailed = profileID + stacks.SyncFailedSuffix
	// CodeSyncNoExport is a sync with neither the export nor bazel, which
	// wrote nothing.
	CodeSyncNoExport = profileID + stacks.SyncNoExportSuffix
	// CodeSyncStale is a package whose putnami.json no longer matches the
	// export.
	CodeSyncStale = profileID + stacks.SyncStaleSuffix
)

// SyncResult is what one sync did.
type SyncResult = stacks.GraphResult

// Sync writes the package graph of the Bazel workspace at root into each
// package's putnami.json: the extension, the packages it depends on as
// project IDs, the module files at the root as its filePatterns, and test in
// disable.jobs when it holds no test rule. The edges come from the export
// bazel-graph.json at the root, or from bazel query //... when there is none.
// Without either it writes nothing: the result's Export is empty. A root that
// is no Bazel workspace gets an empty result.
func Sync(ctx context.Context, root string, stderr io.Writer) (SyncResult, error) {
	return synchronize(ctx, root, stderr, false)
}

// Stale answers what Sync would change, and writes nothing.
func Stale(ctx context.Context, root string, stderr io.Writer) (SyncResult, error) {
	return synchronize(ctx, root, stderr, true)
}

func synchronize(ctx context.Context, root string, stderr io.Writer, dry bool) (SyncResult, error) {
	if !detect.IsWorkspace(root) {
		return SyncResult{}, nil
	}
	export, graph := detect.GraphFile, ""
	if _, err := os.Stat(filepath.Join(root, detect.GraphFile)); errors.Is(err, os.ErrNotExist) {
		if _, err := exec.LookPath("bazel"); err != nil {
			return SyncResult{}, nil
		}
		file, cleanup, err := stacks.ExportGraph(ctx, root, profileID, "bazel", queryArgs, detect.GraphFile, stderr)
		if err != nil {
			return SyncResult{}, err
		}
		defer cleanup()
		graph, export = file, "bazel "+strings.Join(queryArgs, " ")
	}
	detection := detect.Detect(root, graph)
	if !detection.FromExport {
		var reasons []string
		for _, d := range detection.Diagnostics {
			if d.Severity != diag.Info {
				reasons = append(reasons, d.Message)
			}
		}
		return SyncResult{}, fmt.Errorf("the Bazel query export cannot be read, so no edge is written: %s", strings.Join(reasons, "; "))
	}
	modules := rootFiles(root)
	packages := make([]stacks.GraphPackage, 0, len(detection.Packages))
	for _, p := range detection.Packages {
		if p.Dir == wsproto.ProbeRootPath {
			continue
		}
		packages = append(packages, stacks.GraphPackage{
			Dir:       p.Dir,
			DependsOn: p.DependsOn,
			Inputs:    inputs(root, p, modules),
			Tested:    p.Tested,
		})
	}
	result, err := graphSync.Apply(root, packages, dry)
	if err != nil {
		return SyncResult{}, err
	}
	result.Export = export
	return result, nil
}

// rootFiles lists the module files the workspace root holds.
func rootFiles(root string) []string {
	var out []string
	for _, name := range moduleFiles {
		if info, err := os.Stat(filepath.Join(root, name)); err == nil && info.Mode().IsRegular() {
			out = append(out, name)
		}
	}
	return out
}

// inputs are the file patterns of a package, relative to its directory: the
// module files of the root, and the root BUILD file when it depends on a
// target of the root package, which no edge reaches.
func inputs(root string, p detect.Package, modules []string) []string {
	files := append([]string(nil), modules...)
	for _, d := range p.DependsOn {
		if d == wsproto.ProbeRootPath {
			if build := detect.BuildFile(root, wsproto.ProbeRootPath); build != "" {
				files = append(files, build)
			}
		}
	}
	up := strings.Repeat("../", strings.Count(p.Dir, "/")+1)
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = up + path.Clean(f)
	}
	return out
}

// graphSync writes the Bazel package graph: a directory that holds a BUILD
// file is a Bazel package, whose edges the sync owns.
var graphSync = stacks.GraphSync{
	Extension:      ExtensionName,
	HoldsBuildFile: func(root, dir string) bool { return detect.BuildFile(root, dir) != "" },
	SkippedDirs:    []string{"bazel-bin", "bazel-out", "bazel-testlogs"},
}

// graphJobs are the workspace sync task and the validate task of the Bazel
// package graph.
var graphJobs = stacks.GraphJobs{
	Tool:          profileID,
	GraphFile:     detect.GraphFile,
	ExportCommand: detect.ExportCommand,
	Sync:          synchronize,
}

// RunSync is the job of the workspace sync task: Sync on the workspace root,
// reported line by line.
func RunSync(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return graphJobs.RunSync(ctx, emit, args)
}

// RunCheck is the job of the validate task: it fails when a sync would change
// a putnami.json, so a BUILD change that altered the edges cannot pass a gate
// that selected the projects on the old ones.
func RunCheck(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	return graphJobs.RunCheck(ctx, emit, args)
}
