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
	"go.putnami.dev/typescript/extension/internal/pnpmworkspace"
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
	// A Rush repository lists its projects and their package names in
	// rush.json, which rush install checks against each package.json: the sync
	// renames no package and writes no member list there.
	manager, _ := detectPackageManager(root)
	if manager.Name == managerRush {
		return nil, nil
	}
	members := typeScriptMembers(root, selected)

	var changes []workspaceSyncChange
	nameChanges, err := alignPackageNames(root, members, dryRun)
	if err != nil {
		return nil, err
	}
	changes = append(changes, nameChanges...)

	// The membership goes where the workspace's package manager reads it. A
	// declaration the install refuses still has its members in package.json.
	workspaceChange, err := syncWorkspaceMembers(root, manager, members, dryRun)
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
		encoded, err := marshalManifestValue(member.Name)
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

// syncWorkspaceMembers writes the members where manager reads them: the
// packages of pnpm-workspace.yaml for pnpm, nothing for Rush, whose rush.json
// the repository writes, and the root package.json workspaces otherwise.
func syncWorkspaceMembers(root string, manager packageManager, members []typeScriptMember, dryRun bool) (*workspaceSyncChange, error) {
	if manager.listsMembersInPNPMWorkspace() {
		return syncPNPMWorkspacePackages(root, members, dryRun)
	}
	return syncRootWorkspaces(root, members, dryRun)
}

// syncPNPMWorkspacePackages lists the members in the `packages` list of
// pnpm-workspace.yaml, keeping every other line of the file and every entry
// the repository wrote (planMemberList). A file without `packages` is left
// alone: pnpm then takes every package under the root, so it lists every
// member already. An adopted repository without the file is left alone too:
// it keeps no pnpm workspace. A workspace with no root package.json is left
// alone, as syncRootWorkspaces leaves it, and so is one with no member and no
// pnpm-workspace.yaml. An unchanged list is not rewritten.
func syncPNPMWorkspacePackages(root string, members []typeScriptMember, dryRun bool) (*workspaceSyncChange, error) {
	if _, err := os.Stat(filepath.Join(root, workspaceMarkerFile)); err != nil {
		return nil, nil //nolint:nilerr // no root manifest is an ordinary state, not a failure
	}
	path := pnpmWorkspaceFile(root)
	data, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", pnpmworkspace.File, err)
	}

	current, found, err := pnpmworkspace.Packages(string(data))
	if err != nil {
		return nil, err
	}
	if exists && !found {
		return nil, nil
	}
	if !exists && adoptedRepository(root) {
		return nil, nil
	}
	edit := planMemberList(root, current, members)
	if !edit.changes(current) {
		return nil, nil
	}
	updated, err := pnpmworkspace.EditPackages(string(data), edit.drop, edit.added)
	if err != nil {
		return nil, err
	}
	after, _, err := pnpmworkspace.Packages(updated)
	if err != nil {
		return nil, err
	}

	change := &workspaceSyncChange{
		Path:   pnpmworkspace.File,
		Field:  "packages",
		Before: fmt.Sprintf("%d entries", len(current)),
		After:  fmt.Sprintf("%d entries", len(after)),
	}
	if dryRun {
		return change, nil
	}
	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return nil, fmt.Errorf("write %s: %w", pnpmworkspace.File, err)
	}
	return change, nil
}

// syncRootWorkspaces lists the members in the root package.json
// `workspaces`, keeping every entry the repository wrote (planMemberList).
//
// Both npm wire forms round-trip: the object form is written back AS an object
// with only its `packages` member replaced, so a workspace that configured
// `nohoist` (or any other sibling member) keeps it. Flattening the object to a
// bare array would delete that configuration on the first membership change,
// changing how bun and yarn hoist as a side effect of adding a project.
//
// A workspace with no root package.json is left alone: creating one would give
// a Go-only or Python-only workspace an npm identity nobody asked for. So is
// the root package.json of an adopted repository that has no workspaces: the
// repository installs no member there. An
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

	if _, listed := pkg["workspaces"]; !listed && adoptedRepository(root) {
		return nil, nil
	}
	workspaces, err := readRootWorkspaces(pkg["workspaces"])
	if err != nil {
		return nil, err
	}
	edit := planMemberList(root, workspaces.packages, members)
	if !edit.changes(workspaces.packages) {
		return nil, nil
	}
	var kept []string
	for _, entry := range workspaces.packages {
		if !edit.drop(entry) {
			kept = append(kept, entry)
		}
	}
	desired := pnpmworkspace.InsertSorted(kept, edit.added)

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
	packages, err := marshalManifestValue(desired)
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

	encoded, err := marshalManifestValue(orderedRawObject{keys: manifestKeyOrder(object, w.order), values: object})
	if err != nil {
		return nil, fmt.Errorf("encode workspaces object: %w", err)
	}
	return encoded, nil
}
