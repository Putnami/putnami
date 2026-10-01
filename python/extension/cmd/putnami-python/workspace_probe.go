package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	pyworkspace "go.putnami.dev/python/extension/internal/workspace"
)

// The Python workspace probe.
//
// This is where "what is a Python project, and what is it called?" moves out of
// the orchestrator. It is the successor to core's `readPyProjectName`, and it is
// the third and last of the three probes whose simultaneous existence is what
// makes deleting the core parsers legal.
//
// The same four properties the Go and TypeScript probes hold apply here:
//
//   - PURE. The answer is a function of the tree alone: no clock, no absolute
//     path, no environment lookup, no network, no `uv` invocation. The answer's
//     digest keys core's workspace snapshot and, from this slice on, every
//     project's metadata digest — so a byte that varies between two runs over
//     one tree is a cache-correctness bug.
//
//   - PATHS, NOT NAMES. Nothing here reports a dependency edge, and that is a
//     deliberate scope decision rather than an omission: the epic assigns
//     Python "pyproject.toml identity updates" and nothing else. Core never
//     derived Python dependency edges either, so inventing them in the slice
//     that DELETES the comparison guard would ship an unreviewed graph change
//     under the cover of a migration. A follow-up that adds them is a graph
//     change with its own evidence.
//
//   - THE PROJECT ROOT IS NOT ASSUMED TO BE THE DISTRIBUTION ROOT. Every path
//     reported here is derived from where the pyproject.toml actually is.
//
//   - REPORT, NEVER GUESS. A pyproject.toml that exists but declares no
//     `[project] name` is reported as a diagnostic and skipped. Inventing an
//     identity would rename a distribution; dropping it silently is a known
//     failure this repository already paid for once.

// pythonExtensionName is the identity this extension answers under. It keys the
// provider-owned metadata bucket in core's merged view and must match the name
// the manifest declares.
const pythonExtensionName = "@putnami/python"

// pythonWorkspaceMarker is the manifest whose presence marks a Python project.
// It matches the `workspace.markers` entry in putnami.extension.json; the
// manifest contract test pins the two together.
const pythonWorkspaceMarker = "pyproject.toml"

// pythonWorkspaceRootFiles are uv's workspace manifest and resolved lock state.
// They ride the existing per-project watchedFiles member so newer providers
// stay readable by strict v1 cores while core keeps Python/uv vocabulary out of
// impacted selection.
var pythonWorkspaceRootFiles = []string{pythonWorkspaceMarker, "uv.lock"}

// handleWorkspaceProbe answers core's reserved probe invocation. It is called
// before ordinary subcommand dispatch, exactly like the runtime handshake, and
// reports handled=false for every other argv.
func handleWorkspaceProbe(args []string, stdin io.Reader, stdout io.Writer) (bool, error) {
	return wsproto.ServeProbe(args, stdin, stdout, func(request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.ProbeResult{}, fmt.Errorf("resolve workspace root: %w", err)
		}
		return probePythonWorkspace(root, request)
	})
}

// probePythonWorkspace answers one request against the tree rooted at root.
//
// Core spawns the probe with its working directory set to the workspace root
// and every path in the exchange repo-relative, so root is the only absolute
// value in play and it never reaches the answer.
func probePythonWorkspace(root string, request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	result := wsproto.ProbeResult{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: pythonExtensionName,
	}

	// Candidates are walked in sorted order so the answer — including which
	// diagnostic is reported first — never depends on the request's ordering.
	ordered := append([]string(nil), request.Paths...)
	sort.Strings(ordered)

	for _, candidate := range ordered {
		normalized, ok := wsproto.NormalizeProbePath(candidate)
		if !ok {
			continue
		}
		marker := pythonMarkerPathFor(normalized)
		absolute := filepath.Join(root, filepath.FromSlash(marker))
		data, readErr := os.ReadFile(absolute) //nolint:gosec // repo-relative path joined under the workspace root
		if readErr != nil {
			// No pyproject.toml — the ordinary "not a Python project" answer.
			continue
		}
		if normalized == wsproto.ProbeRootPath && declaresUVWorkspace(string(data)) {
			// A root manifest that declares `[tool.uv.workspace]` is the
			// workspace's OWN manifest, not a member. Claiming it as a project
			// would give the workspace root a distribution identity it does not
			// have — the same rule the TypeScript probe applies to a root
			// package.json that declares `workspaces`.
			continue
		}
		name, err := pyworkspace.ParsePyprojectName(absolute)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics,
				diag.Warningf("invalid-manifest", marker, "pyproject.toml declares no [project] name: %v", err))
			continue
		}
		result.Projects = append(result.Projects, wsproto.ProbeProject{
			Path:       normalized,
			SourceName: name,
			SourceFile: marker,
			// A project's own answer changes when its own pyproject.toml
			// changes; declaring it keeps core's watch and snapshot machinery
			// from having to know that pyproject.toml is what this provider
			// reads.
			WatchedFiles: append([]string{marker}, pythonWorkspaceRootFiles...),
		})
	}

	wsproto.NormalizeProbeResult(&result)
	return result, nil
}

// declaresUVWorkspace reports whether a pyproject.toml carries the
// `[tool.uv.workspace]` table that marks it as a uv workspace ROOT.
func declaresUVWorkspace(content string) bool {
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if idx := strings.Index(line, "#"); idx == 0 {
			continue
		}
		if line == "[tool.uv.workspace]" {
			return true
		}
	}
	return false
}

// pythonMarkerPathFor is the repo-relative pyproject.toml path for a candidate
// directory.
func pythonMarkerPathFor(candidate string) string {
	if candidate == "" || candidate == wsproto.ProbeRootPath {
		return pythonWorkspaceMarker
	}
	return path.Join(candidate, pythonWorkspaceMarker)
}
