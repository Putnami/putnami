package pkg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/agentartifact"

	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/catalog"
	"go.putnami.dev/typescript/extension/internal/git"
	"go.putnami.dev/typescript/extension/internal/project"
)

// resolveWorkspaceConfig returns the path to the workspace config file,
// preferring putnami.workspace.json and falling back to .putnamirc.json.
func resolveWorkspaceConfig(dir string) string {
	p := filepath.Join(dir, "putnami.workspace.json")
	if project.FileExists(p) {
		return p
	}
	legacy := filepath.Join(dir, ".putnamirc.json")
	if project.FileExists(legacy) {
		return legacy
	}
	return p
}

// resolveProjectConfig returns the path to the project config file,
// preferring putnami.json and falling back to .putnamirc.json.
func resolveProjectConfig(dir string) string {
	p := filepath.Join(dir, "putnami.json")
	if project.FileExists(p) {
		return p
	}
	legacy := filepath.Join(dir, ".putnamirc.json")
	if project.FileExists(legacy) {
		return legacy
	}
	return p
}

func findBuildOutput(workspaceRoot, projectPath string) string {
	// Package pipeline output
	pkgPath := filepath.Join(workspaceRoot, ".putnami", "out", projectPath, "package")
	if project.FileExists(pkgPath) {
		return pkgPath
	}
	// Standalone build output
	buildPath := filepath.Join(workspaceRoot, ".putnami", "out", projectPath, "build")
	if project.FileExists(buildPath) {
		return buildPath
	}
	// Fallback to cached artifact path
	safeProjectName := strings.ReplaceAll(projectPath, ":", "-")
	safeProjectName = strings.ReplaceAll(safeProjectName, "/", "-")
	safeProjectName = strings.ReplaceAll(safeProjectName, "\\", "-")
	cachePath := filepath.Join(
		workspaceRoot, ".putnami", "projects", safeProjectName,
		"@putnami-typescript", "build~transpile", "latest", "output",
	)
	if project.FileExists(cachePath) {
		return cachePath
	}
	return ""
}

// inheritWorkspaceFields copies shared metadata from the workspace root package.json
// into the project package.json when not already set.
func inheritWorkspaceFields(pkg, wsPkg *project.PackageJSON) {
	fields := []string{"author", "license", "repository", "bugs", "homepage", "funding", "engines", "packageManager"}
	for _, field := range fields {
		if pkg.Raw == nil || wsPkg.Raw == nil {
			continue
		}
		if _, exists := pkg.Raw[field]; !exists {
			if val, ok := wsPkg.Raw[field]; ok {
				pkg.Raw[field] = val
			}
		}
	}
}

// resolvePublishDeps rewrites the workspace:/catalog: specifiers across every
// publishable dependency map (dependencies, peerDependencies,
// optionalDependencies) in place, propagating the first resolution failure.
func resolvePublishDeps(pkg *project.PackageJSON, wsProjects map[string]bool, cat *catalog.Catalogs, versionSuffix string, release bool, wsVersion string) error {
	var err error
	if pkg.Dependencies, err = resolveWorkspaceDeps(pkg.Dependencies, wsProjects, cat, versionSuffix, release, wsVersion); err != nil {
		return err
	}
	if pkg.PeerDependencies, err = resolveWorkspaceDeps(pkg.PeerDependencies, wsProjects, cat, versionSuffix, release, wsVersion); err != nil {
		return err
	}
	if pkg.OptionalDependencies, err = resolveWorkspaceDeps(pkg.OptionalDependencies, wsProjects, cat, versionSuffix, release, wsVersion); err != nil {
		return err
	}
	return nil
}

// resolveSparsePublishDeps pins every published internal edge to the exact
// version in the next release-set snapshot. It never guesses from the current
// workspace version: that would republish unchanged upstreams logically even
// when the planner deliberately retained their older base-set version.
func resolveSparsePublishDeps(pkg *project.PackageJSON, wsProjects map[string]bool, cat *catalog.Catalogs, versions map[string]string) error {
	var err error
	if pkg.Dependencies, err = resolveSparseDependencies(pkg.Dependencies, wsProjects, cat, versions); err != nil {
		return fmt.Errorf("dependencies: %w", err)
	}
	if pkg.PeerDependencies, err = resolveSparseDependencies(pkg.PeerDependencies, wsProjects, cat, versions); err != nil {
		return fmt.Errorf("peerDependencies: %w", err)
	}
	if pkg.OptionalDependencies, err = resolveSparseDependencies(pkg.OptionalDependencies, wsProjects, cat, versions); err != nil {
		return fmt.Errorf("optionalDependencies: %w", err)
	}
	return nil
}

func resolveSparseDependencies(deps map[string]string, wsProjects map[string]bool, cat *catalog.Catalogs, versions map[string]string) (map[string]string, error) {
	if deps == nil {
		return nil, nil
	}
	resolved := make(map[string]string, len(deps))
	for name, authored := range deps {
		version := authored
		if catalogName, ok := catalog.NameFromSpec(authored); ok {
			catalogVersion, found := "", false
			if cat != nil {
				catalogVersion, found = cat.LookupIn(catalogName, name)
			}
			if !found || catalogVersion == "" {
				return nil, fmt.Errorf("cannot resolve %q for %q: no matching entry in the workspace catalog", authored, name)
			}
			version = catalogVersion
		}

		exact, member := versions[name]
		if member {
			if exact == "" {
				return nil, fmt.Errorf("release-set member %q has an empty version", name)
			}
			resolved[name] = applyWorkspaceOperator(version, exact)
			continue
		}

		if wsProjects[name] || strings.HasPrefix(version, "workspace:") {
			return nil, fmt.Errorf("internal dependency %q is missing from the npm release-set plan", name)
		}
		if strings.HasPrefix(version, "catalog:") {
			return nil, fmt.Errorf("dependency %q retains unresolved catalog selector %q", name, version)
		}
		resolved[name] = version
	}
	return resolved, nil
}

func applyWorkspaceOperator(authored, exact string) string {
	if !strings.HasPrefix(authored, "workspace:") {
		return exact
	}
	switch strings.TrimPrefix(authored, "workspace:") {
	case "^":
		return "^" + exact
	case "~":
		return "~" + exact
	default:
		return exact
	}
}

// resolveWorkspaceDeps rewrites workspace: and catalog: specifiers to concrete
// versions for publishing. workspace: specs resolve to the workspace version
// (with the pre-release suffix for snapshots, stripped for stable releases);
// catalog: specs resolve to the version recorded in the publishing workspace's
// Bun catalog. A catalog: spec with no matching catalog entry is a hard error:
// a published tarball must never carry an unresolved catalog: specifier, since
// a consumer cannot resolve it against their own (typically absent) catalog.
func resolveWorkspaceDeps(deps map[string]string, wsProjects map[string]bool, cat *catalog.Catalogs, versionSuffix string, release bool, wsVersion string) (map[string]string, error) {
	if deps == nil {
		return nil, nil
	}
	resolved := make(map[string]string, len(deps))
	for name, version := range deps {
		// Bun catalog: protocol → the concrete version recorded in the
		// publishing workspace catalog. The resolved value may itself be a
		// workspace: spec (framework packages aligned through a catalog), so
		// fall through to the workspace handling below.
		if catalogName, ok := catalog.NameFromSpec(version); ok {
			catVersion, found := "", false
			if cat != nil {
				catVersion, found = cat.LookupIn(catalogName, name)
			}
			if !found || catVersion == "" {
				return nil, fmt.Errorf("cannot resolve %q for %q: no matching entry in the workspace catalog", version, name)
			}
			version = catVersion
		}

		switch {
		case strings.HasPrefix(version, "workspace:"):
			specifier := version[len("workspace:"):]
			workspaceVersion := wsVersion
			if workspaceVersion != "" && !release && versionSuffix != "" {
				workspaceVersion = workspaceVersion + "-" + versionSuffix
			}
			if workspaceVersion == "" {
				resolved[name] = version // keep original if can't resolve
				continue
			}
			switch specifier {
			case "*":
				resolved[name] = workspaceVersion
			case "~", "^":
				resolved[name] = specifier + workspaceVersion
			default:
				resolved[name] = workspaceVersion
			}
		case release && wsProjects[name]:
			// Stable release: strip pre-release suffix from workspace deps
			resolved[name] = project.StripPreReleaseSuffix(version)
		default:
			resolved[name] = version
		}
	}
	return resolved, nil
}

// setGitInfoRelease sets gitInfo on the package.json raw data.
func setGitInfoRelease(pkg *project.PackageJSON, versionInfo *git.VersionInfo, release bool) {
	setGitInfo(pkg, versionInfo, release, true)
}

// setGitInfoReleaseSet omits invocation time so retrying one immutable planned
// version creates the same package bytes and can verify an existing upload.
func setGitInfoReleaseSet(pkg *project.PackageJSON, versionInfo *git.VersionInfo, release bool) {
	setGitInfo(pkg, versionInfo, release, false)
}

func setGitInfo(pkg *project.PackageJSON, versionInfo *git.VersionInfo, release, includeBuildTime bool) {
	if pkg.Raw == nil {
		pkg.Raw = make(map[string]json.RawMessage)
	}

	gitInfo := map[string]any{
		"release": release,
	}
	if includeBuildTime {
		gitInfo["buildTime"] = time.Now().UTC().Format(time.RFC3339)
	}

	if versionInfo != nil {
		gitInfo["branch"] = versionInfo.Branch
		gitInfo["sha"] = versionInfo.SHA
		gitInfo["isDirty"] = versionInfo.IsDirty
	}

	data, _ := json.Marshal(gitInfo)
	pkg.Raw["gitInfo"] = data
}

// deleteRawField removes a field from the package.json raw data.
func deleteRawField(pkg *project.PackageJSON, field string) {
	if pkg.Raw != nil {
		delete(pkg.Raw, field)
	}
}

// loadWorkspaceProjectNames returns a set of all project names in the workspace.
func loadWorkspaceProjectNames(workspaceRoot string) map[string]bool {
	names := make(map[string]bool)

	rcPath := resolveWorkspaceConfig(workspaceRoot)
	data, err := os.ReadFile(rcPath)
	if err != nil {
		return names
	}
	var wsConfig struct {
		Includes []string `json:"includes"`
		Projects []string `json:"projects"`
	}
	if json.Unmarshal(data, &wsConfig) != nil {
		return names
	}

	for _, projPath := range expandWorkspaceProjectPaths(workspaceRoot, "", workspaceConfigEntries(wsConfig), make(map[string]bool)) {
		// Validate path stays within workspace
		fullPath := filepath.Clean(filepath.Join(workspaceRoot, projPath))
		if !strings.HasPrefix(fullPath, filepath.Clean(workspaceRoot)+string(filepath.Separator)) {
			continue
		}
		// Try package.json first
		pkgJSONPath := filepath.Join(workspaceRoot, projPath, "package.json")
		if pkg := project.ReadPackageJSONSafe(pkgJSONPath); pkg != nil && pkg.Name != "" {
			names[pkg.Name] = true
			continue
		}
		// Try project config
		rcData, err := os.ReadFile(resolveProjectConfig(filepath.Join(workspaceRoot, projPath)))
		if err != nil {
			continue
		}
		var rc struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(rcData, &rc) == nil && rc.Name != "" {
			names[rc.Name] = true
		}
	}

	return names
}

func workspaceConfigEntries(config struct {
	Includes []string `json:"includes"`
	Projects []string `json:"projects"`
}) []string {
	seen := make(map[string]bool, len(config.Includes)+len(config.Projects))
	entries := make([]string, 0, len(config.Includes)+len(config.Projects))
	for _, entry := range append(config.Includes, config.Projects...) {
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		entries = append(entries, entry)
	}
	return entries
}

func expandWorkspaceProjectPaths(workspaceRoot, base string, entries []string, seen map[string]bool) []string {
	projects := make([]string, 0, len(entries))
	for _, entry := range entries {
		rel := filepath.Clean(filepath.Join(base, entry))
		if rel == "." || rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true

		scopeEntries := readScopeEntries(filepath.Join(workspaceRoot, rel, "putnami.json"))
		if len(scopeEntries) > 0 {
			projects = append(projects, expandWorkspaceProjectPaths(workspaceRoot, rel, scopeEntries, seen)...)
			continue
		}
		projects = append(projects, rel)
	}
	return projects
}

func readScopeEntries(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var config struct {
		Includes []string `json:"includes"`
		Projects []string `json:"projects"`
	}
	if json.Unmarshal(data, &config) != nil {
		return nil
	}
	return workspaceConfigEntries(config)
}

// copyReadme copies README.md from project root to output directory.
func copyReadme(projectRoot, outputPath string) {
	for _, name := range []string{"README.md", "readme.md"} {
		src := filepath.Join(projectRoot, name)
		if project.FileExists(src) {
			_ = build.CopyFile(src, filepath.Join(outputPath, name))
			return
		}
	}
}

// copyDocFiles copies AI.md and doc/ directory from the project root to the
// output directory. These files provide AI-friendly package documentation and
// technical reference that consumers can use alongside the published package.
func copyDocFiles(projectRoot, outputPath string) {
	src := filepath.Join(projectRoot, "AI.md")
	if project.FileExists(src) {
		_ = build.CopyFile(src, filepath.Join(outputPath, "AI.md"))
	}

	// Copy doc/ directory
	docDir := filepath.Join(projectRoot, "doc")
	if !project.FileExists(docDir) {
		return
	}
	_ = filepath.WalkDir(docDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(projectRoot, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(outputPath, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0755)
		}
		return build.CopyFile(path, dest)
	})
}

// copyLicenseFiles copies LICENSE files from workspace root to output directory.
func copyLicenseFiles(workspaceRoot, outputPath string) {
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "LICENSE" || strings.HasPrefix(name, "LICENSE.") {
			_ = build.CopyFile(filepath.Join(workspaceRoot, name), filepath.Join(outputPath, name))
		}
	}
}

// rewriteTSPathsToJS replaces .ts/.tsx extensions with .js in the package.json
// main, exports, bin and browser fields. The build step transpiles TypeScript to
// JavaScript, but the source package.json references .ts paths. Published
// packages only contain .js files, so these paths must be updated.
func rewriteTSPathsToJS(pkg *project.PackageJSON) {
	// Rewrite main field
	pkg.Main = tsPathToJS(pkg.Main)

	// Rewrite exports field
	if pkg.Exports != nil {
		var raw any
		if json.Unmarshal(pkg.Exports, &raw) == nil {
			rewritten := rewriteExportsValue(raw)
			if data, err := json.Marshal(rewritten); err == nil {
				pkg.Exports = data
			}
		}
	}

	// Rewrite bin field
	if pkg.Bin != nil {
		var raw any
		if json.Unmarshal(pkg.Bin, &raw) == nil {
			rewritten := rewriteExportsValue(raw)
			if data, err := json.Marshal(rewritten); err == nil {
				pkg.Bin = data
			}
		}
	}

	rewriteBrowserFieldToJS(pkg)
}

// rewriteBrowserFieldToJS rewrites the top-level `browser` field, which
// PackageJSON only carries through the Raw round-trip map. Both the keys (the
// module being replaced) and the values (its browser replacement) are source
// paths, so both must move from .ts/.tsx to .js — otherwise the published map
// points at files that do not exist in the tarball and is silently inert.
//
// The map is a per-module redirect for bundlers, not the publication boundary:
// SSR containment is enforced by transpiling browser entrypoints in their own
// build graph (see resolveEntrypointPlan in internal/build/transpile.go).
// A `false` value (module stubbed out for browsers) is preserved verbatim.
func rewriteBrowserFieldToJS(pkg *project.PackageJSON) {
	if pkg.Raw == nil {
		return
	}
	raw, ok := pkg.Raw["browser"]
	if !ok {
		return
	}
	var parsed any
	if json.Unmarshal(raw, &parsed) != nil {
		return
	}

	var rewritten any
	switch v := parsed.(type) {
	case string:
		rewritten = tsPathToJS(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[tsPathToJS(k)] = rewriteExportsValue(val)
		}
		rewritten = out
	default:
		return
	}

	if data, err := json.Marshal(rewritten); err == nil {
		pkg.Raw["browser"] = data
	}
}

// rewriteExportsValue recursively rewrites .ts/.tsx paths to .js in an exports value.
func rewriteExportsValue(v any) any {
	switch val := v.(type) {
	case string:
		return tsPathToJS(val)
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, v := range val {
			out[k] = rewriteExportsValue(v)
		}
		return out
	default:
		return v
	}
}

// tsPathToJS replaces .ts or .tsx extension with .js in a file path.
func tsPathToJS(p string) string {
	if strings.HasSuffix(p, ".ts") {
		return p[:len(p)-3] + ".js"
	}
	if strings.HasSuffix(p, ".tsx") {
		return p[:len(p)-4] + ".js"
	}
	return p
}

// injectExportTypes declares the published package's type entry points so that
// offline type resolution is deterministic and does not depend on TypeScript's
// fragile "swap .js → .d.ts sibling" fallback. It runs AFTER rewriteTSPathsToJS
// (so export paths are already .js) and only ever references a .d.ts that was
// actually emitted under packageDir — never inventing a types entry for an
// export whose declaration was not produced (e.g. generated serve entrypoints).
//
// For each exports entry:
//   - a bare string "./x.js" whose sibling "./x.d.ts" exists becomes the object
//     {"types":"./x.d.ts","default":"./x.js"} (types first, TS requirement);
//   - a condition object gains a leading "types" key computed from its "default"
//     (fallback "browser", or any single .js leaf) when that .d.ts exists and no
//     "types" condition is already present.
//
// It also sets the top-level pkg.Types from the "." export's (or main's) .d.ts
// when emitted, aiding legacy node/node10 resolution. The bin field is left
// untouched — executables carry no meaningful published types.
//
// Ordering is deterministic: exports keys are iterated in sorted order and the
// resulting object is serialized with types first, so the published artifact is
// byte-stable (it is cache-relevant).
func injectExportTypes(pkg *project.PackageJSON, packageDir string) {
	dtsExists := func(dtsRel string) bool {
		if dtsRel == "" {
			return false
		}
		return project.FileExists(filepath.Join(packageDir, strings.TrimPrefix(dtsRel, "./")))
	}

	// Top-level types: from the "." export default (or main) when emitted.
	if pkg.Types == "" {
		if dts := jsPathToDTS(exportDefaultPath(pkg.Exports, ".")); dtsExists(dts) {
			pkg.Types = dts
		} else if dts := jsPathToDTS(pkg.Main); dts != "" && dtsExists(dts) {
			pkg.Types = dts
		}
	}

	if pkg.Exports == nil {
		return
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(pkg.Exports, &raw) != nil {
		return // exports is not an object (e.g. a bare string) — leave as-is
	}

	// Sorted key iteration keeps the rebuilt exports object byte-stable.
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out orderedJSON
	for _, key := range keys {
		out.add(key, exportEntryWithTypes(raw[key], dtsExists))
	}

	if data := out.marshal(); data != nil {
		pkg.Exports = data
	}
}

// exportEntryWithTypes returns the exports entry value with a "types" condition
// added (types first) when the corresponding .d.ts exists under packageDir and
// no types condition is already present. Bare-string entries are upgraded to
// object form. Entries whose .d.ts is absent are returned unchanged.
func exportEntryWithTypes(rawEntry json.RawMessage, dtsExists func(string) bool) json.RawMessage {
	// Bare string entry: "./x.js"
	var s string
	if json.Unmarshal(rawEntry, &s) == nil {
		dts := jsPathToDTS(s)
		if dts == "" || !dtsExists(dts) {
			return rawEntry
		}
		var obj orderedJSON
		obj.addString("types", dts)
		obj.addString("default", s)
		if data := obj.marshal(); data != nil {
			return data
		}
		return rawEntry
	}

	// Condition object entry: {"browser":...,"default":"./x.js"}
	var conds map[string]string
	if json.Unmarshal(rawEntry, &conds) != nil {
		return rawEntry // nested/unknown shape — leave untouched
	}
	if _, has := conds["types"]; has {
		return rawEntry // already declares types
	}
	target := conds["default"]
	if target == "" {
		target = conds["browser"]
	}
	if target == "" {
		target = singleJSLeaf(conds)
	}
	dts := jsPathToDTS(target)
	if dts == "" || !dtsExists(dts) {
		return rawEntry
	}

	// Rebuild with "types" first, remaining conditions in sorted order for
	// determinism.
	var obj orderedJSON
	obj.addString("types", dts)
	condKeys := make([]string, 0, len(conds))
	for k := range conds {
		condKeys = append(condKeys, k)
	}
	sort.Strings(condKeys)
	for _, k := range condKeys {
		obj.addString(k, conds[k])
	}
	if data := obj.marshal(); data != nil {
		return data
	}
	return rawEntry
}

// exportDefaultPath resolves the "default" (fallback "browser", then any single
// .js leaf) path of the named export key from an exports map. Returns "" when
// absent or when exports is not an object.
func exportDefaultPath(exports json.RawMessage, key string) string {
	if exports == nil {
		return ""
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(exports, &raw) != nil {
		return ""
	}
	entry, ok := raw[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(entry, &s) == nil {
		return s
	}
	var conds map[string]string
	if json.Unmarshal(entry, &conds) != nil {
		return ""
	}
	if v := conds["default"]; v != "" {
		return v
	}
	if v := conds["browser"]; v != "" {
		return v
	}
	return singleJSLeaf(conds)
}

// singleJSLeaf returns the sole .js value in a condition map, or "" when there
// is not exactly one .js leaf (ambiguous — do not guess a types target).
func singleJSLeaf(conds map[string]string) string {
	found := ""
	for _, v := range conds {
		if strings.HasSuffix(v, ".js") {
			if found != "" && found != v {
				return "" // ambiguous
			}
			found = v
		}
	}
	return found
}

// jsPathToDTS swaps a trailing ".js" for ".d.ts". Returns "" for non-.js paths.
func jsPathToDTS(p string) string {
	if !strings.HasSuffix(p, ".js") {
		return ""
	}
	return p[:len(p)-len(".js")] + ".d.ts"
}

// orderedJSON builds a JSON object with insertion-ordered keys, so a "types"
// condition can be serialized FIRST as TypeScript requires (Go maps would
// otherwise re-sort keys on marshal).
type orderedJSON struct {
	keys   []string
	values []json.RawMessage
}

func (o *orderedJSON) add(key string, value json.RawMessage) {
	o.keys = append(o.keys, key)
	o.values = append(o.values, value)
}

func (o *orderedJSON) addString(key, value string) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	o.add(key, data)
}

// marshal serializes the ordered object to compact JSON. Returns nil on error.
func (o *orderedJSON) marshal() json.RawMessage {
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(key)
		if err != nil {
			return nil
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.values[i])
	}
	b.WriteByte('}')
	// Validate the assembled bytes are well-formed JSON.
	if !json.Valid([]byte(b.String())) {
		return nil
	}
	return json.RawMessage(b.String())
}

// copyHookFiles copies putnami.extension.json from the project root to the
// package output directory and stamps the publish version into the manifest.
// Framework packages (e.g. @putnami/application, @putnami/web) declare
// preBuild hooks in putnami.extension.json that the generate phase discovers
// at build time. Without this file, hook discovery fails and code generation
// (serve entrypoints, route loaders, SSR bundles) is skipped.
//
// For manifests that declare a command or MCP tool contract surface it also
// runs the package-time contract gate: the staged manifest is validated
// strictly at the current CLI contract and, on success, stamped with the
// earned cliContract. A non-conforming manifest returns an error that fails
// the package job — it must never reach the registry, because the loader's
// tolerance for older contracts is only safe when stamped contracts are
// verified. Hook-only manifests (empty commands) have no flag or agent-facing
// tool surface for the contract to govern and keep the historical best-effort
// behavior.
//
// A manifest that declares agentContent has its content built into the package
// before the gate runs (agentartifact.StageExtensionContent): the built tree
// under agentContent.path, bound by the digest the staged manifest then
// carries, under the package's own name and version. The gate verifies those
// bytes, and a package.json `files` allowlist that would leave them out of the
// published package fails packaging.
//
// Note: bin/*.ts files are NOT copied here. They are included as transpile
// entrypoints (see resolveEntrypointPlan in transpile.go) and appear in the
// lib output as compiled .js files alongside their dependencies.
func copyHookFiles(projectRoot, outputPath, version string) error {
	manifestSrc := filepath.Join(projectRoot, "putnami.extension.json")
	data, err := os.ReadFile(manifestSrc)
	if err != nil {
		return nil // no manifest — nothing to stage
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		// A malformed manifest must never reach the registry: consumers can't
		// unmarshal it and would silently skip the whole extension (the tolerant-loading
		// failure mode). Fail the package job here instead of copying it as-is
		// past the contract gate.
		return fmt.Errorf("parse extension manifest %s: %w", manifestSrc, err)
	}
	manifest["version"] = version
	rewritten, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("rewrite extension manifest %s: %w", manifestSrc, err)
	}
	stagedPath := filepath.Join(outputPath, "putnami.extension.json")
	if err := os.WriteFile(stagedPath, append(rewritten, '\n'), 0o644); err != nil {
		return fmt.Errorf("stage extension manifest %s: %w", stagedPath, err)
	}
	content, err := agentartifact.StageExtensionContent(projectRoot, outputPath, stagedPackageName(outputPath), version)
	if err != nil {
		return err
	}
	if content != nil {
		if err := requireFilesAllowlistCoversAgentContent(outputPath, stagedPath); err != nil {
			return err
		}
	}
	// Gate + stamp on the staged copy only; the source manifest is untouched.
	// NPM outputs are platform-neutral source/library packages, not installable
	// extension archives, so they deliberately do not require a host executable
	// here. The Go-owned archive packager stages the matching executable for
	// every target platform and calls validateStagedRuntimeExecutable there.
	return gateAndStampManifestContract(outputPath)
}

// stagedPackageName is the name of the package.json the npm packager staged in
// outputPath: the name the package is published under, and the one the CLI
// gives an extension whose manifest names none. It is empty when there is no
// readable package.json.
func stagedPackageName(outputPath string) string {
	data, err := os.ReadFile(filepath.Join(outputPath, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Name)
}

// requireFilesAllowlistCoversAgentContent refuses a staged package.json whose
// `files` allowlist leaves agentContent.path out of the published package. The
// allowlist is applied by npm publish, after the contract gate, so it must
// carry every byte the packaged manifest binds.
//
// An entry covers the content when it names the content path, one of its
// parent directories, or everything (`*`, `**`); a trailing `/`, `/*` or `/**`
// names the same directory. A negated entry (`!…`) that names the content
// path, a parent or anything inside it refuses the package. Without a `files`
// field npm publishes the whole directory.
func requireFilesAllowlistCoversAgentContent(outputPath, stagedManifestPath string) error {
	manifestData, err := os.ReadFile(stagedManifestPath)
	if err != nil {
		return fmt.Errorf("read staged extension manifest: %w", err)
	}
	var manifest struct {
		AgentContent struct {
			Path string `json:"path"`
		} `json:"agentContent"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parse staged extension manifest: %w", err)
	}
	contentPath := manifest.AgentContent.Path
	pkgData, err := os.ReadFile(filepath.Join(outputPath, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Files *[]string `json:"files"`
	}
	if err := json.Unmarshal(pkgData, &pkg); err != nil || pkg.Files == nil {
		return nil
	}
	covered := false
	for _, entry := range *pkg.Files {
		negated := strings.HasPrefix(entry, "!")
		name := strings.TrimPrefix(strings.TrimPrefix(entry, "!"), "./")
		for _, suffix := range []string{"/**", "/*", "/"} {
			name = strings.TrimSuffix(name, suffix)
		}
		everything := name == "*" || name == "**"
		coversContent := everything || name == contentPath || strings.HasPrefix(contentPath, name+"/")
		if negated && (coversContent || strings.HasPrefix(name, contentPath+"/")) {
			return fmt.Errorf("package.json files excludes %q, which holds the agent content the extension manifest binds; npm would publish the manifest without it", entry)
		}
		if !negated && coversContent {
			covered = true
		}
	}
	if !covered {
		return fmt.Errorf("package.json files does not include agentContent.path %q; npm would publish the manifest without the agent content it binds: add %q to files", contentPath, contentPath)
	}
	return nil
}
