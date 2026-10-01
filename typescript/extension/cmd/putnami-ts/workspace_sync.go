package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// The TypeScript workspace-sync task.
//
// Two mutations live here rather than in CLI core: the root package.json
// `workspaces` array, and per-project package.json `name` alignment — the two
// mutations that require knowing what npm is.
//
// Core keeps everything only it can own: Putnami membership and scopes,
// `--prune`, `--skip-install`, dry-run aggregation, and the canonical report.
// It passes the RESOLVED project selection in, so this task never re-derives
// which directories are projects — that would be a second discovery, disagreeing
// with the first the moment a scope's namePattern changed.
//
// Idempotence is a requirement, not a nicety: `projects sync` runs on every
// membership change and the tree must converge. A run that changes nothing
// writes nothing, so a no-op sync leaves file mtimes — and therefore every
// downstream cache key that observes them — untouched.

// workspaceSyncChange is one recorded mutation, in the form the caller reports.
type workspaceSyncChange struct {
	Path   string `json:"path"`
	Field  string `json:"field"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

func runWorkspaceSync(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	dryRun := ctx.Params.Bool("dryRun", false)

	// FAIL CLOSED on an empty selection. "No projects were named" and "this
	// workspace has no members" are indistinguishable from here, and treating
	// the first as the second would rewrite the root `workspaces` array to an
	// empty list — deleting the workspace's membership because the orchestrator
	// forgot to say what it was. The task declares `activation: workspace-once`
	// precisely so the resolved selection reaches it; if it did not, that is a
	// wiring bug and the right answer is to stop, loudly, having written nothing.
	if len(ctx.SelectedProjects) == 0 {
		return "FAILED", nil, errors.New(
			"workspace-sync received no resolved project selection; refusing to rewrite package workspaces " +
				"from an unknown membership (the task must run with activation: workspace-once)")
	}

	changes, err := syncTypeScriptWorkspace(ctx.WorkspaceRoot, ctx.SelectedProjects, dryRun)
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

// syncTypeScriptWorkspace performs both mutations and returns what it changed,
// in a deterministic order. Split from the job entry point so tests exercise the
// tree effects without a job context.
func syncTypeScriptWorkspace(root string, selected []pctx.ProjectRef, dryRun bool) ([]workspaceSyncChange, error) {
	members := typeScriptMembers(root, selected)

	var changes []workspaceSyncChange
	nameChanges, err := alignPackageNames(root, members, dryRun)
	if err != nil {
		return nil, err
	}
	changes = append(changes, nameChanges...)

	workspaceChange, err := syncRootWorkspaces(root, members, dryRun)
	if err != nil {
		return nil, err
	}
	if workspaceChange != nil {
		changes = append(changes, *workspaceChange)
	}
	return changes, nil
}

// typeScriptMember is one selected project this extension owns.
type typeScriptMember struct {
	Path string
	Name string
	// SourceName is the identity the project DECLARED, before a scope
	// namePattern override. It is what makes "this manifest is out of
	// alignment" distinguishable from "this manifest IS the alignment" — see
	// alignPackageNames.
	SourceName string
}

// typeScriptMembers narrows the resolved selection to the projects that carry a
// package.json, sorted by path.
//
// The workspace ROOT is excluded even when it carries one: an npm workspace root
// is not a member of its own workspaces array, and listing it makes bun resolve
// the workspace to itself.
func typeScriptMembers(root string, selected []pctx.ProjectRef) []typeScriptMember {
	members := make([]typeScriptMember, 0, len(selected))
	seen := make(map[string]bool, len(selected))
	for _, ref := range selected {
		rel := filepath.ToSlash(filepath.Clean(ref.Path))
		if rel == "." || rel == "" || rel == "/" || seen[rel] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel), workspaceMarkerFile)); err != nil {
			continue
		}
		seen[rel] = true
		members = append(members, typeScriptMember{Path: rel, Name: ref.Name, SourceName: ref.SourceName})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Path < members[j].Path })
	return members
}

// alignPackageNames writes each member's resolved project name into its
// package.json `name`, for the members whose DECLARED identity diverges from
// that resolved name.
//
// The resolved name is core's answer: it has already applied explicit
// putnami.json identity, the scope namePattern precedence, and the directory
// fallback. This task never re-derives it — a second derivation would silently
// rename a project the moment the two rules drifted.
//
// The divergence set is core's answer too, and honoring it is what keeps this
// task from renaming the whole workspace: a manifest whose SourceName equals
// the resolved name is never rewritten; an absent SourceName declines the
// rename.
func alignPackageNames(root string, members []typeScriptMember, dryRun bool) ([]workspaceSyncChange, error) {
	var changes []workspaceSyncChange
	for _, member := range members {
		if member.Name == "" {
			continue
		}
		if member.SourceName == "" || member.SourceName == member.Name {
			continue
		}
		manifestPath := filepath.Join(root, filepath.FromSlash(member.Path), workspaceMarkerFile)
		pkg, order, err := readRootManifest(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", member.Path, err)
		}
		current := ""
		if raw, ok := pkg["name"]; ok {
			if err := json.Unmarshal(raw, &current); err != nil {
				return nil, fmt.Errorf("%s: package.json name is not a string: %w", member.Path, err)
			}
		}
		if current == member.Name {
			continue
		}
		changes = append(changes, workspaceSyncChange{
			Path: member.Path + "/" + workspaceMarkerFile, Field: "name", Before: current, After: member.Name,
		})
		if dryRun {
			continue
		}
		encoded, err := json.Marshal(member.Name)
		if err != nil {
			return nil, fmt.Errorf("%s: encode name: %w", member.Path, err)
		}
		pkg["name"] = encoded
		if err := writeRootManifest(manifestPath, pkg, order); err != nil {
			return nil, fmt.Errorf("%s: %w", member.Path, err)
		}
	}
	return changes, nil
}

// syncRootWorkspaces rewrites the root package.json `workspaces` membership to
// exactly the members' paths.
//
// Both npm wire forms round-trip: the object form is written back AS an object
// with only its `packages` member replaced, so a workspace that configured
// `nohoist` (or any other sibling member) keeps it. Flattening the object to a
// bare array would delete that configuration on the first membership change,
// changing how bun and yarn hoist as a side effect of adding a project.
//
// A workspace with no root package.json is left alone: creating one would give
// a Go-only or Python-only workspace an npm identity nobody asked for. An
// unchanged membership is not rewritten, so a no-op sync does not touch the
// file.
func syncRootWorkspaces(root string, members []typeScriptMember, dryRun bool) (*workspaceSyncChange, error) {
	manifestPath := filepath.Join(root, workspaceMarkerFile)
	if _, err := os.Stat(manifestPath); err != nil {
		return nil, nil //nolint:nilerr // no root manifest is an ordinary state, not a failure
	}

	pkg, order, err := readRootManifest(manifestPath)
	if err != nil {
		return nil, err
	}

	desired := make([]string, 0, len(members))
	for _, member := range members {
		desired = append(desired, member.Path)
	}
	sort.Strings(desired)

	workspaces, err := readRootWorkspaces(pkg["workspaces"])
	if err != nil {
		return nil, err
	}
	if equalStringSlices(workspaces.packages, desired) {
		return nil, nil
	}

	change := &workspaceSyncChange{
		Path:   workspaceMarkerFile,
		Field:  "workspaces",
		Before: fmt.Sprintf("%d entries", len(workspaces.packages)),
		After:  fmt.Sprintf("%d entries", len(desired)),
	}
	if dryRun {
		return change, nil
	}

	encoded, err := workspaces.encode(desired)
	if err != nil {
		return nil, err
	}
	pkg["workspaces"] = encoded
	if err := writeRootManifest(manifestPath, pkg, order); err != nil {
		return nil, err
	}
	return change, nil
}

// rootWorkspaces is the root package.json `workspaces` member as it was
// authored, remembering WHICH of the two npm/yarn wire forms carried it.
type rootWorkspaces struct {
	// packages is the membership list, from either form.
	packages []string
	// object is the whole object form, sibling members included; nil for the
	// array form (and for an absent member, which writes back as an array).
	object map[string]json.RawMessage
	// order is the object form's authored key order, so a rewrite does not
	// scramble it into map iteration's alphabetical order — the same rule
	// writeRootManifest applies to the manifest's top level.
	order []string
}

// readRootWorkspaces parses the `workspaces` member, accepting both wire forms.
// An absent member reads as the empty array form, which is what lets a root
// manifest that never declared workspaces gain one.
func readRootWorkspaces(raw json.RawMessage) (rootWorkspaces, error) {
	var workspaces rootWorkspaces
	if len(raw) == 0 {
		return workspaces, nil
	}
	if err := json.Unmarshal(raw, &workspaces.packages); err == nil {
		return workspaces, nil
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return workspaces, fmt.Errorf("root package.json workspaces is neither an array nor an object: %w", err)
	}
	order, err := topLevelKeyOrder(raw)
	if err != nil {
		return workspaces, fmt.Errorf("parse root package.json workspaces key order: %w", err)
	}
	if packages, ok := object["packages"]; ok {
		if err := json.Unmarshal(packages, &workspaces.packages); err != nil {
			return workspaces, fmt.Errorf("root package.json workspaces.packages is not an array of strings: %w", err)
		}
	}
	workspaces.object, workspaces.order = object, order
	return workspaces, nil
}

// encode renders the desired membership back in the form it was read in: an
// object whose `packages` member is replaced and whose every other member —
// `nohoist`, and the `catalog`/`catalogs` bun accepts here too — survives in its
// authored position, or a bare array when that is what the manifest had.
func (w rootWorkspaces) encode(desired []string) (json.RawMessage, error) {
	packages, err := json.Marshal(desired)
	if err != nil {
		return nil, fmt.Errorf("encode workspaces: %w", err)
	}
	if w.object == nil {
		return packages, nil
	}

	// Copy rather than mutate: the caller's parsed view must not observe a
	// write that a later error abandons.
	object := make(map[string]json.RawMessage, len(w.object)+1)
	for key, value := range w.object {
		object[key] = value
	}
	object["packages"] = packages

	encoded, err := json.Marshal(orderedRawObject{keys: manifestKeyOrder(object, w.order), values: object})
	if err != nil {
		return nil, fmt.Errorf("encode workspaces object: %w", err)
	}
	return encoded, nil
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
