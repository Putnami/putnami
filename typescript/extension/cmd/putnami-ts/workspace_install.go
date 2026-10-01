package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
)

func runWorkspaceInstall(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	force := ctx.Params.Bool("force", false)
	hosted := registrycred.OfflineDependencies()

	// A hosted run installs the committed manifests as they are: it writes no
	// file the repository commits, and workspace-fetch downloaded for these.
	if !hosted {
		if err := shapeWorkspaceManifests(ctx, emit, bunBin); err != nil {
			return "FAILED", nil, err
		}
	}

	// Refresh the user's credential for the private registries the scope
	// lines name, before bun goes and reads from them. A hosted run installs
	// from the cache workspace-fetch filled, and a credential file in the
	// user's home is what a lifecycle script could read.
	if !hosted {
		refreshPutnamiNpmCredential(ctx, emit)
	}

	installArgs := workspaceInstallArgs(force, hosted)

	emit.PhaseStart("install")
	result, err := runBunWithTimeout("bun install", bunBin, installArgs, ctx.WorkspaceRoot, bunNetworkTimeout)
	if err != nil {
		emit.PhaseEnd("install", "failed")
		return "FAILED", nil, err
	}

	if !result.Success {
		emit.PhaseEnd("install", "failed")
		emit.Log("error", "bun install failed: "+result.Stderr)
		return "FAILED", nil, nil
	}

	emit.PhaseEnd("install", "success")
	emit.Log("info", "Workspace dependencies installed")

	return "OK", nil, nil
}

// workspaceInstallArgs is the `bun install` argv. On a hosted run bun installs
// the committed bun.lock unchanged from the cache workspace-fetch filled:
// --frozen-lockfile fails instead of resolving anything the lock does not
// hold. Bun 1.4 has no offline mode and accepts an unknown flag such as
// `--offline` without effect, so the complete cache is what keeps the install
// off the registry; a package missing from it fails on a private registry for
// want of a credential.
func workspaceInstallArgs(force, hosted bool) []string {
	args := []string{"install"}
	if hosted {
		args = append(args, "--frozen-lockfile")
	}
	if force {
		args = append(args, "--force")
	}
	return args
}

// shapeWorkspaceManifests brings every workspace file bun reads to the state
// an install uses: the extension's devDependencies, the member list, the
// catalog entries, the packageManager declaration, tsconfig.json, biome.json
// and the .npmrc scope lines. workspace-fetch and workspace-install both call
// it, so the fetch downloads for exactly the manifests the install reads. Each
// step writes only what is missing or stale, so a second call rewrites
// nothing. A failure to list the members or to materialize biome.json is
// returned; the other steps log a warning and continue.
func shapeWorkspaceManifests(ctx *pctx.Context, emit *jsonl.Emitter, bunBin string) error {
	// Ensure workspace devDependencies declared by this extension are present.
	if err := ensureWorkspaceDevDeps(ctx, emit); err != nil {
		emit.Log("warn", "failed to ensure workspace devDependencies: "+err.Error())
	}

	// List every member in the root workspaces before bun reads it, so a
	// project created since the last `projects sync` is installed too. The
	// catalog seeding below reads the members from the same list.
	if err := ensureWorkspaceMembership(ctx, emit); err != nil {
		return err
	}

	// Seed catalog entries for any catalog:-referenced @putnami/* package so a
	// freshly scaffolded catalog-first workspace installs before its first
	// `putnami upgrade --deps`. A seed on a channel the caller chose fails the
	// install when it fails.
	if err := ensureWorkspaceCatalog(ctx, emit); err != nil {
		if catalogSeedChannel(ctx) != defaultCatalogSeedChannel {
			return fmt.Errorf("seed the workspace catalog: %w", err)
		}
		emit.Log("warn", "failed to ensure workspace catalog: "+err.Error())
	}

	// Declare the bun this install runs with, so the lock written after the
	// installers pins the bun the task runtime probes.
	if err := ensureWorkspacePackageManager(ctx, emit, bunBin); err != nil {
		emit.Log("warn", "failed to declare the workspace packageManager: "+err.Error())
	}

	// Ensure workspace tsconfig.json exists.
	if err := ensureWorkspaceTsConfig(ctx, emit); err != nil {
		emit.Log("warn", "failed to ensure workspace tsconfig.json: "+err.Error())
	}

	// Materialize the formatter rules owned by this resolved extension with the
	// workspace-root role, so every project observes one stable configuration.
	if err := ensureWorkspaceBiomeConfig(ctx, emit); err != nil {
		return err
	}

	// Ensure workspace .npmrc maps every declared scope to its registry.
	if err := ensureWorkspaceNpmrc(ctx, emit); err != nil {
		emit.Log("warn", "failed to ensure workspace .npmrc: "+err.Error())
	}
	return nil
}

// ensureWorkspaceMembership writes the root package.json `workspaces` from the
// complete workspace membership in the job context, with the workspace-sync
// task's own code, so it writes the list `projects sync` writes. A context
// without the membership leaves the manifest alone: an unknown membership
// never rewrites the list. An unchanged list is not rewritten.
func ensureWorkspaceMembership(ctx *pctx.Context, emit *jsonl.Emitter) error {
	if len(ctx.WorkspaceProjects) == 0 {
		return nil
	}
	change, err := syncRootWorkspaces(ctx.WorkspaceRoot, typeScriptMembers(ctx.WorkspaceRoot, ctx.WorkspaceProjects), false)
	if err != nil {
		return fmt.Errorf("write the workspace members to package.json: %w", err)
	}
	if change != nil {
		emit.Log("info", fmt.Sprintf("%s: %s %s → %s", change.Path, change.Field, change.Before, change.After))
	}
	return nil
}

// bunVersionTimeout bounds the `bun --version` probe that names the bun a
// workspace declares.
const bunVersionTimeout = 30 * time.Second

// ensureWorkspacePackageManager declares bunBin as the workspace root
// package.json's packageManager, as bun@<version>, when that manifest exists
// and declares none. The CLI pins the lock's bun toolchain from this field, and
// the task runtime runs only with a bun whose version equals that pin. An
// existing declaration is never changed, the root manifest keeps its key
// order, and a bun whose version is not a plain release (a canary build) is
// not declared, since no published release carries that version.
func ensureWorkspacePackageManager(ctx *pctx.Context, emit *jsonl.Emitter, bunBin string) error {
	pkgPath := filepath.Join(ctx.WorkspaceRoot, "package.json")
	if _, err := os.Stat(pkgPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat package.json: %w", err)
	}
	pkg, order, err := readRootManifest(pkgPath)
	if err != nil {
		return err
	}
	if _, declared := pkg["packageManager"]; declared {
		return nil
	}
	result, err := runBunWithTimeout("bun --version", bunBin, []string{"--version"}, ctx.WorkspaceRoot, bunVersionTimeout)
	if err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("bun --version exited with code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	version := strings.TrimSpace(result.Stdout)
	if !isPlainReleaseVersion(version) {
		emit.Log("warn", fmt.Sprintf("bun %q is not a release version: package.json declares no packageManager, so the lock pins no bun", version))
		return nil
	}
	declaration, err := json.Marshal("bun@" + version)
	if err != nil {
		return fmt.Errorf("marshal packageManager: %w", err)
	}
	pkg["packageManager"] = declaration
	if err := writeRootManifest(pkgPath, pkg, order); err != nil {
		return err
	}
	emit.Log("info", "declared packageManager bun@"+version+" in package.json")
	return nil
}

// isPlainReleaseVersion reports whether version is MAJOR.MINOR.PATCH with
// decimal parts and no prerelease or build suffix.
func isPlainReleaseVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// ensureWorkspaceDevDeps reads workspaceDevDependencies from the extension
// manifest and adds any missing entries to the workspace root package.json.
func ensureWorkspaceDevDeps(ctx *pctx.Context, emit *jsonl.Emitter) error {
	// Read extension manifest.
	manifestPath := filepath.Join(ctx.Extension.Root, "putnami.extension.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}

	var manifest struct {
		WorkspaceDevDeps map[string]string `json:"workspaceDevDependencies"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	if len(manifest.WorkspaceDevDeps) == 0 {
		return nil
	}

	// Read workspace package.json (create a minimal one if it doesn't exist).
	pkgPath := filepath.Join(ctx.WorkspaceRoot, "package.json")
	pkgData, err := os.ReadFile(pkgPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read package.json: %w", err)
		}
		pkgData = []byte(`{"private": true}`)
	}

	var pkg map[string]json.RawMessage
	if err := json.Unmarshal(pkgData, &pkg); err != nil {
		return fmt.Errorf("parse package.json: %w", err)
	}

	// Parse existing devDependencies.
	devDeps := make(map[string]string)
	if raw, ok := pkg["devDependencies"]; ok {
		if err := json.Unmarshal(raw, &devDeps); err != nil {
			return fmt.Errorf("parse devDependencies: %w", err)
		}
	}

	// Add missing dependencies.
	var added []string
	for name, version := range manifest.WorkspaceDevDeps {
		if _, exists := devDeps[name]; !exists {
			devDeps[name] = version
			added = append(added, name)
		}
	}

	if len(added) == 0 {
		return nil
	}

	sort.Strings(added)
	for _, name := range added {
		emit.Log("info", "adding workspace devDependency: "+name+"@"+devDeps[name])
	}

	// Write back devDependencies.
	devDepsJSON, err := json.Marshal(devDeps)
	if err != nil {
		return fmt.Errorf("marshal devDependencies: %w", err)
	}
	pkg["devDependencies"] = devDepsJSON

	// Write package.json with 2-space indent.
	out, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal package.json: %w", err)
	}
	out = append(out, '\n')

	if err := os.WriteFile(pkgPath, out, 0644); err != nil {
		return fmt.Errorf("write package.json: %w", err)
	}

	return nil
}

// ensureWorkspaceNpmrc renders the workspace .npmrc scope lines from the
// workspace `registries.npm.scopes` declaration — one `@<scope>:registry=<url>`
// line per declared scope.
//
// The declaration is the source of truth, so a scope line that points somewhere
// else is REPLACED rather than left alone: the workspace document is where a
// mirror is selected now, and a stale generated line silently sending installs
// to the previous registry is exactly the drift this section exists to remove.
// Lines the declaration says nothing about — a credential line, another scope,
// a bun setting — are preserved verbatim, in place.
func ensureWorkspaceNpmrc(ctx *pctx.Context, emit *jsonl.Emitter) error {
	declared, err := npmRegistriesFrom(ctx.Params)
	if err != nil {
		return err
	}
	scopes := declared.scopeNames()
	if len(scopes) == 0 {
		return nil
	}

	npmrcPath := filepath.Join(ctx.WorkspaceRoot, ".npmrc")
	existing, err := os.ReadFile(npmrcPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read .npmrc: %w", err)
	}

	updated, changed := applyNpmrcScopeLines(existing, declared, scopes)
	if !changed {
		return nil
	}
	if err := os.WriteFile(npmrcPath, updated, 0644); err != nil {
		return fmt.Errorf("write .npmrc: %w", err)
	}
	for _, scope := range scopes {
		emit.Log("info", "configured "+scope+" scope in workspace .npmrc → "+declared.Scopes[scope])
	}
	return nil
}

// applyNpmrcScopeLines rewrites the managed scope lines of an .npmrc, in place
// where one already exists and appended in declared order otherwise, and
// reports whether anything moved.
func applyNpmrcScopeLines(existing []byte, declared npmRegistries, scopes []string) ([]byte, bool) {
	wanted := make(map[string]string, len(scopes))
	for _, scope := range scopes {
		wanted[npmScopeRegistryPrefix(scope)] = npmScopeRegistryLine(scope, declared.Scopes[scope])
	}

	var lines [][]byte
	if len(existing) > 0 {
		lines = bytes.Split(existing, []byte{'\n'})
		// Match line-oriented file semantics: a final newline terminates the
		// preceding line; it does not introduce another empty line.
		if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
			lines = lines[:len(lines)-1]
		}
	}

	var out [][]byte
	written := make(map[string]bool, len(wanted))
	changed := false
	for _, line := range lines {
		prefix := managedScopePrefix(strings.TrimSpace(string(line)), wanted)
		if prefix == "" {
			out = append(out, append([]byte(nil), line...))
			continue
		}
		if written[prefix] {
			// A second line for the same scope is a duplicate the generator
			// left behind; one declaration yields exactly one line.
			changed = true
			continue
		}
		written[prefix] = true
		if string(line) != wanted[prefix] {
			changed = true
		}
		out = append(out, []byte(wanted[prefix]))
	}
	for _, scope := range scopes {
		prefix := npmScopeRegistryPrefix(scope)
		if written[prefix] {
			continue
		}
		changed = true
		out = append(out, []byte(wanted[prefix]))
	}

	var buf bytes.Buffer
	for _, line := range out {
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), changed
}

// managedScopePrefix returns the managed prefix a line carries, or "" when the
// line is none of this extension's business.
func managedScopePrefix(line string, wanted map[string]string) string {
	for prefix := range wanted {
		if strings.HasPrefix(line, prefix) {
			return prefix
		}
	}
	return ""
}

// ensureWorkspaceTsConfig creates a workspace tsconfig.json that extends the
// extension's default config if one does not already exist.
func ensureWorkspaceTsConfig(ctx *pctx.Context, emit *jsonl.Emitter) error {
	tsconfigPath := filepath.Join(ctx.WorkspaceRoot, "tsconfig.json")
	if _, err := os.Stat(tsconfigPath); err == nil {
		return nil
	}

	// Compute the relative path from workspace root to extension config.
	relPath, err := filepath.Rel(ctx.WorkspaceRoot, filepath.Join(ctx.Extension.Root, "config", "tsconfig.json"))
	if err != nil {
		return fmt.Errorf("compute relative path: %w", err)
	}

	content := fmt.Sprintf("{\n  \"extends\": \"./%s\"\n}\n", filepath.ToSlash(relPath))
	if err := os.WriteFile(tsconfigPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write tsconfig.json: %w", err)
	}

	emit.Log("info", "created workspace tsconfig.json extending extension defaults")
	return nil
}

// ensureWorkspaceBiomeConfig projects the extension-owned nested default into
// a workspace-root configuration. A workspace-authored configuration is always
// preserved.
func ensureWorkspaceBiomeConfig(ctx *pctx.Context, emit *jsonl.Emitter) error {
	workspaceConfig := filepath.Join(ctx.WorkspaceRoot, "biome.json")
	if _, err := os.Stat(workspaceConfig); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat workspace biome.json: %w", err)
	}
	extensionConfig := filepath.Join(ctx.Extension.Root, "config", "biome.json")
	content, err := os.ReadFile(extensionConfig)
	if err != nil {
		return fmt.Errorf("read extension biome.json: %w", err)
	}
	workspaceContent, err := workspaceBiomeConfig(content)
	if err != nil {
		return fmt.Errorf("parse extension biome.json: %w", err)
	}
	if err := os.WriteFile(workspaceConfig, workspaceContent, 0644); err != nil {
		return fmt.Errorf("write workspace biome.json: %w", err)
	}
	emit.Log("info", "created workspace biome.json from extension defaults")
	return nil
}

// workspaceBiomeConfig keeps the resolved extension's formatter and lint rules
// intact while changing only the role of the config. The shipped default is a
// nested fallback and therefore declares root:false; the materialized file is
// the workspace root and must declare root:true.
func workspaceBiomeConfig(extensionContent []byte) ([]byte, error) {
	var config map[string]json.RawMessage
	if err := json.Unmarshal(extensionContent, &config); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, fmt.Errorf("configuration must be a JSON object")
	}
	config["root"] = json.RawMessage("true")
	content, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

// defaultCatalogSeedChannel is the channel a catalog seed reads when the
// caller chose none.
const defaultCatalogSeedChannel = "latest"

// catalogSeedChannel is the channel the caller chose for this install through
// the putnami-channel job option, the option deps-upgrade reads its channel
// from. `putnami init` sets it when it resolves on a channel other than
// latest. Absent, empty and stable all read latest.
func catalogSeedChannel(ctx *pctx.Context) string {
	channel := strings.TrimSpace(ctx.Params.String("putnami-channel", "putnamiChannel"))
	if channel == "" || channel == "stable" {
		return defaultCatalogSeedChannel
	}
	return channel
}

// ensureWorkspaceCatalog seeds the workspace catalog with any @putnami/* package
// that a member references through the "catalog:" protocol but that is missing
// from the referenced catalog. Bun fails the install when a catalog dependency
// has no catalog entry, so this keeps freshly scaffolded catalog-first packages
// installable before the first `putnami upgrade --deps`. Existing catalog
// entries (including non-Putnami ones such as react) are never touched.
//
// On latest, a missing entry is seeded with the "latest" dist-tag; deps-upgrade
// later pins it to the resolved release. On any other channel
// (catalogSeedChannel), a missing entry is seeded with the exact version the
// channel's dist-tag names for that package on the registry the workspace
// declares, never with the channel name: the workspace then installs that
// release and follows no channel. A package the channel does not name fails
// the seed, and nothing is written; the seed never reads latest instead.
func ensureWorkspaceCatalog(ctx *pctx.Context, emit *jsonl.Emitter) error {
	refs := collectCatalogPutnamiRefs(ctx.WorkspaceRoot)
	if len(refs) == 0 {
		return nil
	}

	pkgPath := filepath.Join(ctx.WorkspaceRoot, "package.json")
	pkg, order, err := readRootManifest(pkgPath)
	if err != nil {
		return err
	}
	cats := parseCatalogModel(pkg)

	var missing []catalogPutnamiRef
	for _, ref := range refs {
		if _, ok := cats.lookupIn(ref.catalog, ref.name); !ok {
			missing = append(missing, ref)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	channel := catalogSeedChannel(ctx)
	seeds, err := catalogSeedVersions(ctx, emit, missing, channel)
	if err != nil {
		return err
	}

	added := make([]string, 0, len(missing))
	for _, ref := range missing {
		cats.addToCatalog(ref.catalog, ref.name, seeds[ref.name])
		added = append(added, ref.label()+"@"+seeds[ref.name])
	}
	sort.Strings(added)
	for _, entry := range added {
		emit.Log("info", "seeding workspace catalog entry: "+entry)
	}
	if err := cats.writeBack(pkg); err != nil {
		return err
	}
	return writeRootManifest(pkgPath, pkg, order)
}

// catalogSeedVersions returns the catalog entry to seed for each missing
// package: the "latest" dist-tag on latest, and on any other channel the exact
// version the channel's dist-tag names for the package
// (resolvePutnamiNPMVersion). The channel lookup reads the registry before the
// install refreshes the user's credential, so it refreshes it first, as
// deps-upgrade does before its own lookup.
func catalogSeedVersions(ctx *pctx.Context, emit *jsonl.Emitter, missing []catalogPutnamiRef, channel string) (map[string]string, error) {
	seeds := make(map[string]string, len(missing))
	if channel == defaultCatalogSeedChannel {
		for _, ref := range missing {
			seeds[ref.name] = defaultCatalogSeedChannel
		}
		return seeds, nil
	}

	declared, err := npmRegistriesFrom(ctx.Params)
	if err != nil {
		return nil, err
	}
	refreshPutnamiNpmCredential(ctx, emit)
	for _, ref := range missing {
		if _, ok := seeds[ref.name]; ok {
			continue
		}
		version, err := resolvePutnamiNPMVersion(context.Background(), ctx.WorkspaceRoot, declared, ref.name, channel)
		if err != nil {
			return nil, err
		}
		seeds[ref.name] = version
	}
	return seeds, nil
}

type catalogPutnamiRef struct {
	name    string
	catalog string
}

func (r catalogPutnamiRef) key() string {
	return r.catalog + "\x00" + r.name
}

func (r catalogPutnamiRef) label() string {
	if r.catalog == "" {
		return r.name
	}
	return r.name + " (catalog:" + r.catalog + ")"
}

// collectCatalogPutnamiRefs returns the sorted set of @putnami/* package names
// that workspace members reference through the "catalog:" protocol, preserving
// the selected catalog for named refs such as "catalog:framework".
func collectCatalogPutnamiRefs(wsRoot string) []catalogPutnamiRef {
	seen := make(map[string]catalogPutnamiRef)
	for _, dir := range workspaceMemberDirs(wsRoot) {
		if strings.Contains(dir, "node_modules") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			continue
		}
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if json.Unmarshal(data, &pkg) != nil {
			continue
		}
		for _, set := range []map[string]string{pkg.Dependencies, pkg.DevDependencies} {
			for name, spec := range set {
				catalogName, ok := catalogNameFromSpec(spec)
				if strings.HasPrefix(name, putnamiScopePrefix) && ok {
					ref := catalogPutnamiRef{name: name, catalog: catalogName}
					seen[ref.key()] = ref
				}
			}
		}
	}

	out := make([]catalogPutnamiRef, 0, len(seen))
	for _, ref := range seen {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].catalog != out[j].catalog {
			return out[i].catalog < out[j].catalog
		}
		return out[i].name < out[j].name
	})
	return out
}

func catalogNameFromSpec(spec string) (string, bool) {
	if !strings.HasPrefix(spec, catalogProtocol) {
		return "", false
	}
	return strings.TrimPrefix(spec, catalogProtocol), true
}
