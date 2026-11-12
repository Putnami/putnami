// Package bazel is @putnami/bazel, a delegated stack extension: it runs the
// packages of a Bazel repository as Putnami projects, each task running bazel
// on one package, with the stack core of go.putnami.dev/tooling/stacks and
// the Bazel detector of putnami observe. Bazel keeps its own cache and remote
// execution: the manifest disables Putnami's cache.
package bazel

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	detect "go.putnami.dev/tooling/observe/detect/bazel"
	"go.putnami.dev/tooling/stacks"
)

// ExtensionName is the extension's name.
const ExtensionName = "@putnami/bazel"

//go:embed profile.json
var profileJSON []byte

// Stack returns the Bazel stack: the embedded profile, the Bazel detector,
// and the runtime command that collects the reports of a test run.
func Stack() (stacks.Stack, error) {
	profile, err := stacks.Decode(profileJSON)
	if err != nil {
		return stacks.Stack{}, fmt.Errorf("profile.json: %w", err)
	}
	return stacks.Stack{
		Extension: ExtensionName,
		Profile:   profile,
		Detect:    Detect,
		Commands:  map[string]stacks.Command{CollectReportsCommand: CollectReports},
	}, nil
}

// Detect names the Bazel packages among the candidate paths with the Bazel
// detector: each directory that holds a BUILD or BUILD.bazel file and a rule
// of the export bazel-graph.json, or without the export each such directory
// whose putnami.json the sync wrote for this extension, named by its
// directory, with the workspace root for stack root, and as edges the
// export's and those the sync wrote in its putnami.json, each attributed to the
// Bazel graph.
// Without the export, a BUILD file is not parsed: the sync, which reads the
// rules from bazel query, tells a package from a directory with no rule.
// Every other candidate that holds a BUILD file, the root package included,
// is bare: it is named by its directory, the root by the workspace, and runs
// no task. A directory Bazel loads no package from, such as one an
// ignore_directories glob of REPO.bazel or .bazelignore names, is none.
func Detect(root string, request wsproto.ProbeRequest) ([]stacks.Component, []diag.Diagnostic) {
	if !detect.IsWorkspace(root) {
		return nil, nil
	}
	detection := detect.Detect(root, "")
	packages := make(map[string]detect.Package, len(detection.Packages))
	dirs := make(map[string]bool, len(detection.Packages))
	for _, p := range detection.Packages {
		packages[p.Dir] = p
		dirs[p.Dir] = true
	}
	var out []stacks.Component
	for _, dir := range request.Paths {
		p, ok := packages[dir]
		if !detection.FromExport && ok {
			ok = synced(root, dir)
		}
		if !ok || dir == wsproto.ProbeRootPath {
			file := detect.BuildFile(root, dir)
			if file == "" || detect.Ignored(root, dir) {
				continue
			}
			name := dir
			if dir == wsproto.ProbeRootPath {
				name = ""
			}
			out = append(out, stacks.Component{Path: dir, Name: name, SourceFile: file, Bare: true})
			continue
		}
		deps := graphSync.SyncedEdges(root, p.Dir, dirs)
		for _, d := range p.DependsOn {
			if d != wsproto.ProbeRootPath {
				deps = append(deps, d)
			}
		}
		slices.Sort(deps)
		out = append(out, stacks.Component{
			Path:         p.Dir,
			Name:         p.Dir,
			SourceFile:   detect.BuildFile(root, p.Dir),
			Root:         wsproto.ProbeRootPath,
			Dependencies: slices.Compact(deps),
			EdgeSource:   wsproto.DependencySourceBuildGraph,
		})
	}
	// Without an export the edges come from each package's putnami.json,
	// which the sync task writes: a missing export is no finding here.
	var diags []diag.Diagnostic
	for _, d := range detection.Diagnostics {
		if d.Code != detect.CodeGraphMissing {
			diags = append(diags, d)
		}
	}
	return out, diags
}

// synced answers whether the putnami.json of the directory dir names the
// extension, as the sync writes it for each package of the query.
func synced(root, dir string) bool {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), wsproto.ConfigFilename)) //nolint:gosec // a project file under the workspace root
	if err != nil {
		return false
	}
	var project struct {
		Extensions []string `json:"extensions"`
	}
	return json.Unmarshal(data, &project) == nil && slices.Contains(project.Extensions, ExtensionName)
}
