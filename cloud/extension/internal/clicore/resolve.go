package clicore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CloudWorkspaceConfig is the resolved Cloud workspace link plus the
// @putnami/cloud manifest options it came from. Keeping both values together
// lets latency-sensitive callers reuse the already-decoded manifest instead of
// opening and parsing it again for a neighboring option block.
type CloudWorkspaceConfig struct {
	Link    map[string]any
	Options map[string]any
}

// ReadCloudManifestOptions returns only the root manifest's @putnami/cloud
// options. Unlike ReadCloudWorkspaceConfig it does not require an existing
// workspace link, which lets offline installers inspect explicit feature
// opt-ins before deciding whether setup may create that link.
func ReadCloudManifestOptions(workspaceRoot string) (map[string]any, error) {
	return readManifestCloudOptions(workspaceRoot)
}

// ReadCloudWorkspaceConfig resolves the workspace's Cloud link and returns the
// decoded @putnami/cloud options in the same pass. It prefers the link committed
// to putnami.workspace.json / putnami.json and falls back to the cached
// .putnami/cloud-link.json, erroring when neither is present or the cached link
// is missing required fields.
func ReadCloudWorkspaceConfig(workspaceRoot string) (CloudWorkspaceConfig, error) {
	options, err := readManifestCloudOptions(workspaceRoot)
	if err != nil {
		return CloudWorkspaceConfig{}, err
	}
	if link, ok, linkErr := manifestLink(options); ok || linkErr != nil {
		return CloudWorkspaceConfig{Link: link, Options: options}, linkErr
	}
	file := LinkPath(workspaceRoot)
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return CloudWorkspaceConfig{}, NewError("workspace not configured; run `putnami cloud setup --workspace <id>` first", ExitUsage)
		}
		return CloudWorkspaceConfig{}, err
	}
	var link map[string]any
	// The manifest link is primary; this file is a regenerable cache. An
	// empty/whitespace-only or otherwise malformed cache (e.g. a partial file
	// left by a killed `install`/`setup` on a pre-atomic CLI) is treated as
	// "no link" so the user gets the actionable not-configured message and
	// fails fast, rather than a cryptic parse error or a hang.
	if len(strings.TrimSpace(string(data))) == 0 {
		return CloudWorkspaceConfig{}, NewError("workspace not configured; run `putnami cloud setup --workspace <id>` first", ExitUsage)
	}
	if err := json.Unmarshal(data, &link); err != nil {
		return CloudWorkspaceConfig{}, NewError("workspace not configured; run `putnami cloud setup --workspace <id>` first", ExitUsage)
	}
	if StringValue(link["workspace_id"]) == "" {
		return CloudWorkspaceConfig{}, NewError("cloud-link.json missing workspace_id; run `putnami cloud setup --workspace <id>`", ExitUsage)
	}
	if StringValue(link["control_plane_url"]) == "" {
		return CloudWorkspaceConfig{}, NewError("cloud-link.json missing control_plane_url; run `putnami cloud setup --workspace <id>`", ExitUsage)
	}
	return CloudWorkspaceConfig{Link: link, Options: options}, nil
}

// ReadCloudLink resolves only the workspace link. Callers that also need
// @putnami/cloud options should use ReadCloudWorkspaceConfig so the manifest is
// not read and decoded twice.
func ReadCloudLink(workspaceRoot string) (map[string]any, error) {
	config, err := ReadCloudWorkspaceConfig(workspaceRoot)
	return config.Link, err
}

// ResolveApp resolves the target project name. An explicit --app/--appName wins
// (normalized to the canonical putnami.json `name`); otherwise it walks up from
// cwd to the nearest workload putnami.json the same way the native putnami CLI
// resolves projects, then falls back to a lone putnami.json at the workspace
// root for single-project layouts.
func ResolveApp(params map[string]any, workspaceRoot string) (string, error) {
	if app := StringParam(params, "app", "appName"); app != "" {
		// Accept the unqualified tail (`api`) as well as the full
		// `name` (`apps/api`), normalizing to the canonical
		// name the control plane keys on. A token that resolves to no single
		// local project is returned verbatim so the precise not-found/ambiguous
		// error surfaces at the point of use (e.g. readGenManifest) rather than
		// here — preserving behavior for callers that legitimately name an app
		// with no local putnami.json.
		if canon, err := CanonicalProjectName(workspaceRoot, app); err == nil {
			return canon, nil
		}
		return app, nil
	}
	// Walk up from cwd looking for the nearest workload putnami.json — same
	// resolution model the native `putnami` CLI uses, so `cd <workload> &&
	// putnami cloud …` behaves consistently across all subcommands. The
	// walk is constrained to descendants of workspaceRoot so test
	// sandboxes (which set PUTNAMI_WORKSPACE_ROOT to a tempdir while the
	// process cwd is still the test package directory) don't pull in
	// putnami.json files from unrelated trees.
	if cwd, err := os.Getwd(); err == nil {
		if name, ok := FindActiveProject(cwd, workspaceRoot); ok {
			return name, nil
		}
	}
	// Legacy fallback for single-project workspaces that keep their lone
	// putnami.json at the workspace root. This repo uses putnami.workspace.json
	// at the root so the fallback no-ops here, but other repos may still rely
	// on it.
	rootProject := filepath.Join(workspaceRoot, "putnami.json")
	if data, err := os.ReadFile(rootProject); err == nil {
		var project map[string]any
		if err := json.Unmarshal(data, &project); err == nil {
			if name := StringValue(project["name"]); name != "" {
				return name, nil
			}
		}
	}
	return "", NewError("could not determine app; pass it as the first positional (`putnami cloud <command> <app>`) or cd into a workload directory", ExitUsage)
}

// FindAppDir locates the workspace project that owns appName by walking the
// workspace for a putnami.json with a matching `name`. If the workspace
// root *is* the app, we use it directly. The walk is shallow (we stop
// descending into .gen, node_modules, hidden dirs) so a busy workspace
// doesn't slow publish to a crawl.
func FindAppDir(workspaceRoot, appName string) (string, error) {
	// filepath.WalkDir does not descend a root that is itself a symlink, and
	// Conductor exposes branch-named symlink aliases pointing at the real
	// worktree (e.g. 257-greenfield-registry -> .../seoul). Run from such an
	// alias and the walk would visit nothing, failing with a misleading "no
	// workspace project named …". Resolve the root to its real path first.
	requestedRoot := workspaceRoot
	if resolved, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		workspaceRoot = resolved
	}
	if dir, ok := workingProjectDir(requestedRoot, workspaceRoot, appName); ok {
		return dir, nil
	}
	if name, _ := ProjectNameAt(workspaceRoot); name == appName {
		return workspaceRoot, nil
	}
	var exact string
	// dirs whose project `name`'s unqualified tail (the segment after the last
	// "/") equals appName — the fallback when no exact name matches.
	var tailMatches []string
	walkErr := filepath.WalkDir(workspaceRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		base := d.Name()
		if d.IsDir() {
			if path == workspaceRoot {
				return nil
			}
			// Prune noisy descents that never contain workloads.
			// .claude/worktrees/<name>/ and .context/ hold full repo copies
			// for assistant sessions — each contains duplicate putnami.json files
			// for every workload, and without this skip FindAppDir would
			// match a worktree copy instead of the real workload.
			if skipProjectWalkDir(base) {
				return filepath.SkipDir
			}
			return nil
		}
		if base != "putnami.json" {
			return nil
		}
		dir := filepath.Dir(path)
		name, _ := ProjectNameAt(dir)
		if name == "" {
			return nil
		}
		// An exact putnami.json `name` match always wins outright.
		if name == appName {
			exact = dir
			return filepath.SkipAll
		}
		// Otherwise accept the unqualified tail: `api` resolves
		// `apps/api`, the same way `services/put-server` is also
		// reachable as `put-server`. Tail matches are collected (not short-circuited)
		// so an ambiguous tail shared by two namespaces is reported rather than
		// silently resolved to whichever the walk reached first.
		if ProjectNameTail(name) == appName {
			tailMatches = append(tailMatches, dir)
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	if exact != "" {
		return exact, nil
	}
	switch len(tailMatches) {
	case 1:
		return tailMatches[0], nil
	case 0:
		return "", NewError(fmt.Sprintf("no workspace project named %q under %s — pass --app <name> matching a putnami.json `name` (its full name or its unqualified tail)", appName, workspaceRoot), ExitUsage)
	default:
		names := make([]string, 0, len(tailMatches))
		for _, dir := range tailMatches {
			if name, _ := ProjectNameAt(dir); name != "" {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		return "", NewError(fmt.Sprintf("ambiguous app %q matches multiple projects (%s) — pass the full putnami.json `name`", appName, strings.Join(names, ", ")), ExitUsage)
	}
}

// skipProjectWalkDir names the directories a project walk never enters:
// generated and installed trees, and the full repository copies assistant
// sessions keep under .claude/worktrees/ and .context/.
func skipProjectWalkDir(base string) bool {
	switch base {
	case ".gen", "node_modules", ".putnami", ".git", ".claude", ".context":
		return true
	}
	return false
}

// WorkspaceProjectKeys is every key a project of the checkout answers to: the
// putnami.json `name` of each project and its directory relative to the
// workspace root. A server-side record keyed by anything else was left by an
// earlier project name. The set is empty when the walk finds no project.
func WorkspaceProjectKeys(workspaceRoot string) map[string]bool {
	keys := map[string]bool{}
	if resolved, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		workspaceRoot = resolved
	}
	_ = filepath.WalkDir(workspaceRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != workspaceRoot && skipProjectWalkDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "putnami.json" {
			return nil
		}
		dir := filepath.Dir(path)
		if name, _ := ProjectNameAt(dir); name != "" {
			keys[name] = true
			if relative, err := filepath.Rel(workspaceRoot, dir); err == nil && relative != "." {
				keys[filepath.ToSlash(relative)] = true
			}
		}
		return nil
	})
	return keys
}

// ProjectNameTail returns the unqualified tail of a putnami.json `name` — the
// segment after the last "/". For "apps/api" it returns
// "api"; for an already-unqualified "events" it returns "events".
func ProjectNameTail(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// workingProjectDir returns the working directory when it is a project of
// the workspace named exactly appName. That project wins over a same-name
// match elsewhere in the tree: a task runs from its project root, and a
// result cached for that root must not depend on the other manifests a walk
// would visit.
func workingProjectDir(requestedRoot, resolvedRoot, appName string) (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	if !IsUnder(cwd, resolvedRoot) && !IsUnder(cwd, requestedRoot) {
		return "", false
	}
	if name, _ := ProjectNameAt(cwd); name == "" || name != appName {
		return "", false
	}
	return cwd, true
}

// CanonicalProjectName resolves a user-supplied app token (full name or
// unqualified tail) to the canonical putnami.json `name` of the matching
// project. The control plane keys config-bootstrap, workspace_projects, and the
// runtime service account on this canonical name, so every server-bound payload
// must carry it rather than whatever shorthand the user typed. Returns the
// FindAppDir error (not-found / ambiguous) when the token resolves to no single
// project.
func CanonicalProjectName(workspaceRoot, appName string) (string, error) {
	dir, err := FindAppDir(workspaceRoot, appName)
	if err != nil {
		return "", err
	}
	name, err := ProjectNameAt(dir)
	if err != nil {
		return "", err
	}
	if name == "" {
		return appName, nil
	}
	return name, nil
}

// ProjectNameAt reads the `name` field from the putnami.json in dir.
func ProjectNameAt(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "putnami.json"))
	if err != nil {
		return "", err
	}
	var project map[string]any
	if err := json.Unmarshal(data, &project); err != nil {
		return "", err
	}
	return StringValue(project["name"]), nil
}

// ReadManifestLink reads the Cloud workspace link embedded in the manifest
// (options.@putnami/cloud.workspace of putnami.workspace.json or putnami.json).
// The bool reports whether a link block was found; a found-but-incomplete link
// is an error.
func ReadManifestLink(workspaceRoot string) (map[string]any, bool, error) {
	options, err := readManifestCloudOptions(workspaceRoot)
	if err != nil {
		return nil, false, err
	}
	return manifestLink(options)
}

// ReadManifestCloudOptions returns the options.@putnami/cloud block of the
// workspace manifest (putnami.workspace.json, else putnami.json), or nil when
// neither file declares one. Callers read workspace-authored cloud settings
// that are not the link itself, such as the release-set owner a consumer
// workspace tracks.
func ReadManifestCloudOptions(workspaceRoot string) (map[string]any, error) {
	return readManifestCloudOptions(workspaceRoot)
}

func readManifestCloudOptions(workspaceRoot string) (map[string]any, error) {
	for _, name := range []string{"putnami.workspace.json", "putnami.json"} {
		file := filepath.Join(workspaceRoot, name)
		data, err := os.ReadFile(file)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		var manifest map[string]any
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		options, _ := manifest["options"].(map[string]any)
		cloud, _ := options["@putnami/cloud"].(map[string]any)
		return cloud, nil
	}
	return nil, nil
}

func manifestLink(options map[string]any) (map[string]any, bool, error) {
	link, _ := options["workspace"].(map[string]any)
	if link == nil {
		return nil, false, nil
	}
	if StringValue(link["workspace_id"]) == "" {
		return nil, false, NewError("Putnami Cloud workspace config missing workspace_id; run `putnami cloud setup --workspace <id>`", ExitUsage)
	}
	if StringValue(link["control_plane_url"]) == "" {
		link["control_plane_url"] = DefaultControlPlaneURL
	}
	return link, true, nil
}

// FindActiveProject walks up from start looking for the nearest workload
// putnami.json. Returns the project's `name` field + true on hit. The
// native putnami CLI uses an equivalent walk for project resolution; this
// is the cloud extension catching up so `cd <workload> && putnami cloud
// publish-config` doesn't need an explicit --app flag.
//
// The walk is constrained to descendants of workspaceRoot so it doesn't
// cross workspace boundaries (matters for tests that set
// PUTNAMI_WORKSPACE_ROOT to a sandboxed tempdir while the test process
// itself runs from inside the cloud repo). An empty workspaceRoot
// disables the constraint. The walk also halts at any
// putnami.workspace.json — that file marks a workspace root and isn't a
// workload project.
func FindActiveProject(start, workspaceRoot string) (string, bool) {
	if start == "" {
		return "", false
	}
	if !IsUnder(start, workspaceRoot) {
		return "", false
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return "", false
		}
		if data, err := os.ReadFile(filepath.Join(dir, "putnami.json")); err == nil {
			var project map[string]any
			if err := json.Unmarshal(data, &project); err == nil {
				if name := StringValue(project["name"]); name != "" {
					return name, true
				}
			}
		}
		// Don't climb past the workspace root even if no marker file is
		// present there (some workspaces use a different layout).
		if workspaceRoot != "" && SamePath(dir, workspaceRoot) {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// IsUnder reports whether path equals or sits beneath root. Absolute-path
// normalized; symlink-blind (matches Go stdlib conventions for filesystem
// containment checks elsewhere in the cloud CLI).
func IsUnder(path, root string) bool {
	if root == "" {
		return true
	}
	absRoot, errR := filepath.Abs(root)
	absPath, errP := filepath.Abs(path)
	if errR != nil || errP != nil {
		return false
	}
	if absPath == absRoot {
		return true
	}
	return strings.HasPrefix(absPath, absRoot+string(filepath.Separator))
}

// SamePath reports whether two paths refer to the same absolute location.
func SamePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return aa == bb
}

// LinkPath returns the path of the cached Cloud link file under workspaceRoot.
func LinkPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, LinkFileRelative)
}
