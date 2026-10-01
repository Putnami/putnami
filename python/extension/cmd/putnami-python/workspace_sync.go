package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// The Python workspace-sync task.
//
// One mutation moves here from CLI core: aligning a project's `[project] name`
// in pyproject.toml to the project's resolved name. Core owned it in
// project_update.go, as one arm of a three-way switch over package.json /
// go.mod / pyproject.toml; this slice deletes that switch, so the write has to
// live where the format is understood.
//
// Core keeps everything only it can own: Putnami membership and scopes,
// `--prune`, `--skip-install`, dry-run aggregation, and the canonical report. It
// passes the RESOLVED project selection in, so this task never re-derives which
// directories are projects — that would be a second discovery, disagreeing with
// the first the moment a scope's namePattern changed.
//
// The uv workspace member list is NOT written here. It is the `workspace-install`
// job's business (SyncUVWorkspace), which owns the root pyproject.toml's
// `[tool.uv.workspace]`/`[tool.uv.sources]` tables and then locks against them;
// splitting one file's tables across two tasks is how two writers start flipping
// it on alternate runs.
//
// Idempotence is a requirement, not a nicety: `projects sync` runs on every
// membership change and the tree must converge. A run that changes nothing
// writes nothing, so a no-op sync leaves file mtimes — and therefore every
// downstream cache key that observes them — untouched.

// nameAssignment matches the `name = "value"` form ParsePyprojectName reads, so
// the writer and the reader agree on exactly one spelling. A manifest whose name
// is authored some other way (single quotes, a multi-line string) is left alone
// rather than rewritten into a form the reader would then disagree with.
var nameAssignment = regexp.MustCompile(`^name\s*=\s*"([^"]*)"\s*$`)

// workspaceSyncChange is one recorded mutation, in the form the caller reports.
type workspaceSyncChange struct {
	// Path is the repo-relative file that changed.
	Path string `json:"path"`
	// Field names what changed in it.
	Field string `json:"field"`
	// Before is the value the file carried.
	Before string `json:"before,omitempty"`
	// After is the value written.
	After string `json:"after,omitempty"`
}

// runWorkspaceSync is the `workspace-sync` job entry point.
func runWorkspaceSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	dryRun := ctx.Params.Bool("dryRun", false)

	// FAIL CLOSED on an empty selection. The task declares
	// `activation: workspace-once` precisely so the RESOLVED project selection
	// reaches it; if it did not, that is a wiring bug. Renaming distributions
	// from a membership nobody supplied is not a repair, and stopping loudly
	// having written nothing is the only safe answer.
	if len(ctx.SelectedProjects) == 0 {
		return "FAILED", nil, errors.New(
			"workspace-sync received no resolved project selection; refusing to rewrite pyproject.toml " +
				"identities from an unknown membership (the task must run with activation: workspace-once)")
	}

	changes, err := syncPythonWorkspace(ctx.WorkspaceRoot, ctx.SelectedProjects, dryRun)
	if err != nil {
		return "FAILED", nil, err
	}
	for _, change := range changes {
		emit.Log("info", fmt.Sprintf("%s: %s %q → %q", change.Path, change.Field, change.Before, change.After))
	}

	encoded := make([]any, 0, len(changes))
	for _, change := range changes {
		encoded = append(encoded, change)
	}
	return "OK", map[string]any{"changes": encoded, "dryRun": dryRun}, nil
}

// pythonMember is one selected project this extension owns.
type pythonMember struct {
	Path string
	Name string
	// SourceName is the identity the project DECLARED, before a scope
	// namePattern override — see syncPythonWorkspace for why the rename is
	// gated on it.
	SourceName string
}

// pythonMembers narrows the resolved selection to the projects that carry a
// pyproject.toml, sorted by path.
//
// The workspace ROOT is excluded even when it carries one: the root manifest is
// the uv workspace's own, and rewriting its `[project] name` would rename the
// workspace after whichever project happened to resolve to path ".".
func pythonMembers(root string, selected []pctx.ProjectRef) []pythonMember {
	members := make([]pythonMember, 0, len(selected))
	seen := make(map[string]bool, len(selected))
	for _, ref := range selected {
		rel := filepath.ToSlash(filepath.Clean(ref.Path))
		if rel == "." || rel == "" || rel == "/" || seen[rel] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel), pythonWorkspaceMarker)); err != nil {
			continue
		}
		seen[rel] = true
		members = append(members, pythonMember{Path: rel, Name: ref.Name, SourceName: ref.SourceName})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Path < members[j].Path })
	return members
}

// syncPythonWorkspace aligns the `[project] name` of each member whose DECLARED
// identity diverges from its resolved name, and returns what it changed, in
// path order. Split from the job entry point so tests exercise the tree effects
// without a job context.
//
// The divergence set is core's, not this task's. A manifest whose
// SourceName already equals the resolved name is the manifest the identity was
// resolved FROM: it is not out of alignment, and rewriting its `[project] name`
// to something else — a path-shaped putnami.json name, say — orphans every
// sibling that still depends on the distribution by its published name. Nothing
// here rewrites those references, so the rename lands half-applied and
// resolution breaks workspace-wide. This is the guard core's deleted writer
// (SyncProjectName) carried, moved to where the write now lives.
//
// An ABSENT SourceName is the deliberate fail-safe for an orchestrator older
// than the protocol member: decline the rename. Not renaming leaves a manifest
// a later sync can still fix; renaming is what cannot be undone from here.
func syncPythonWorkspace(root string, selected []pctx.ProjectRef, dryRun bool) ([]workspaceSyncChange, error) {
	var changes []workspaceSyncChange
	for _, member := range pythonMembers(root, selected) {
		if member.Name == "" {
			continue
		}
		if member.SourceName == "" || member.SourceName == member.Name {
			continue
		}
		manifestPath := filepath.Join(root, filepath.FromSlash(member.Path), pythonWorkspaceMarker)
		data, err := os.ReadFile(manifestPath) //nolint:gosec // repo-relative path joined under the workspace root
		if err != nil {
			return nil, fmt.Errorf("%s: %w", member.Path, err)
		}
		updated, before, changed := replaceProjectName(string(data), member.Name)
		if !changed {
			continue
		}
		changes = append(changes, workspaceSyncChange{
			Path: member.Path + "/" + pythonWorkspaceMarker, Field: "name", Before: before, After: member.Name,
		})
		if dryRun {
			continue
		}
		if err := os.WriteFile(manifestPath, []byte(updated), 0o644); err != nil { //nolint:gosec // manifests are world-readable source
			return nil, fmt.Errorf("write %s: %w", member.Path+"/"+pythonWorkspaceMarker, err)
		}
	}
	return changes, nil
}

// replaceProjectName rewrites the `name = "..."` assignment inside the
// `[project]` table and returns the new content, the value it replaced, and
// whether anything changed.
//
// Section gating is the point: a `name = ...` under `[tool.poetry]` or
// `[tool.uv.sources]` is a different key entirely, and rewriting it would
// corrupt the file. Only the FIRST assignment in `[project]` is touched, and a
// manifest with no `[project] name` is left alone — that manifest is the one the
// probe already reported as invalid, and inventing the key here would create an
// identity the probe refused to guess at.
func replaceProjectName(content, name string) (string, string, bool) {
	lines := strings.Split(content, "\n")
	inProject := false
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inProject = line == "[project]"
			continue
		}
		if !inProject {
			continue
		}
		match := nameAssignment.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		if match[1] == name {
			return content, match[1], false
		}
		lines[i] = `name = "` + name + `"`
		return strings.Join(lines, "\n"), match[1], true
	}
	return content, "", false
}
