package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	distribution "go.putnami.dev/protocol/distribution"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/catalog"
)

var npmMetadataClient = &http.Client{Timeout: 30 * time.Second}
var exactNPMVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-.+][0-9A-Za-z.-]+)?$`)

const cliUserAgentEnv = "PUTNAMI_CLI_USER_AGENT"

const upgradeReleaseSetParam = "releaseSet"

// The authority that produced the resolved versions, so the reported table says
// where they came from: a channel or branch tag is an npm dist-tag, an
// immutable snapshot is a release set, and an exact --version is neither.
const (
	npmDistTagSource    = "dist-tag"
	npmReleaseSetSource = "release-set"
	npmExactSource      = "exact"
)

func npmResolvedSource(setMode bool, target string) string {
	switch {
	case setMode:
		return npmReleaseSetSource
	case exactNPMVersion(target) != "":
		return npmExactSource
	default:
		return npmDistTagSource
	}
}

func runDepsUpgrade(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	bunBin, err := resolveBunBin()
	if err != nil {
		return "FAILED", nil, err
	}

	wsRoot := ctx.WorkspaceRoot
	target := upgradeTarget(ctx)
	dryRun := ctx.Params.Bool("dry-run", false, "dryRun")

	// Everything below this line can reach the private registry: the dist-tag
	// metadata request, then `bun install --force`. Refresh the credential once,
	// here, rather than at each of them.
	refreshPutnamiNpmCredential(ctx, emit)

	// Collect @putnami/* dependencies actually referenced by the workspace and
	// its members. Only referenced packages are managed — the framework set is
	// no longer force-pinned, so a workspace carries version policy for exactly
	// the packages it uses. @putnami/cloud is the exception: a workspace with the
	// cloud extension configured also needs the npm runtime package kept current.
	emit.PhaseStart("scan")
	packages := collectPutnamiDeps(wsRoot, emit)
	if len(packages) == 0 {
		emit.Log("info", "No @putnami/* dependencies found")
		emit.PhaseEnd("scan", "success")
		return "OK", nil, nil
	}
	emit.Log("info", fmt.Sprintf("Found %d @putnami/* package(s) to upgrade", len(packages)))
	emit.PhaseEnd("scan", "success")

	resolvedVersions, releaseRef, setMode, err := resolveUpgradeNPMVersions(ctx, wsRoot, packages, target, emit)
	if err != nil {
		emit.Diagnostic("error", "failed to resolve @putnami/* version: "+err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	resolvedSource := npmResolvedSource(setMode, target)
	if setMode {
		emit.Log("info", fmt.Sprintf("Resolved release set %s (%s)", releaseRef.ID, releaseRef.Digest))
	} else {
		// Legacy selector mode remains deliberately lenient per package. Release-
		// set mode is fail-closed instead: its full member map is the authority and
		// a referenced package missing from it invalidates the snapshot.
		packages = resolvedPackages(packages, resolvedVersions)
	}
	if len(packages) == 0 {
		emit.Log("info", "No @putnami/* packages resolved to a version")
		return "OK", nil, nil
	}
	logResolvedPutnamiVersions(emit, packages, resolvedVersions, resolvedSource)

	emit.PhaseStart("upgrade")
	var transaction *npmUpgradeTransaction
	if !dryRun {
		transaction, err = beginNPMUpgradeTransaction(wsRoot)
		if err != nil {
			emit.PhaseEnd("upgrade", "failed")
			return "FAILED", nil, fmt.Errorf("snapshot npm metadata: %w", err)
		}
	}
	if err := pinWorkspacePutnamiDepsWithVersions(wsRoot, packages, resolvedVersions, dryRun, emit); err != nil {
		emit.PhaseEnd("upgrade", "failed")
		return "FAILED", nil, rollbackNPMUpgrade(transaction, err)
	}
	if dryRun {
		emit.PhaseEnd("upgrade", "success")
		return "OK", nil, nil
	}
	emit.PhaseEnd("upgrade", "success")

	emit.PhaseStart("install")
	result, err := runBunWithTimeout("bun install", bunBin, []string{"install", "--force"}, wsRoot, bunNetworkTimeout)
	if err != nil {
		emit.PhaseEnd("install", "failed")
		return "FAILED", nil, rollbackNPMUpgrade(transaction, err)
	}

	if !result.Success {
		emit.PhaseEnd("install", "failed")
		emit.Log("error", "bun install failed: "+result.Stderr)
		if rollbackErr := rollbackNPMUpgrade(transaction, nil); rollbackErr != nil {
			return "FAILED", nil, rollbackErr
		}
		return "FAILED", nil, nil
	}

	emit.PhaseEnd("install", "success")
	transaction.commit()

	for _, pkg := range packages {
		emit.Log("info", fmt.Sprintf("✓ %s pinned to %s", pkg, resolvedVersions[pkg]))
	}

	return "OK", nil, nil
}

// npmEcosystem is the id of the ecosystem this extension owns. T10 promotes it
// to a declared profile in the manifest; here it is the value the probe writes
// and the deps upgrade reads back, in one place rather than four literals.
const npmEcosystem = "npm"

// resolveUpgradeNPMVersions consumes the orchestrator-resolved snapshot when
// present. It never queries npm dist-tags in set mode: one validated response
// supplies every package version, so a channel move during the job cannot mix
// releases. Absence preserves the legacy uniform/tag selector path.
func resolveUpgradeNPMVersions(ctx *pctx.Context, wsRoot string, packages []string, target string, emit *jsonl.Emitter) (map[string]string, distribution.ReleaseSetRef, bool, error) {
	raw, present := ctx.Params[upgradeReleaseSetParam]
	if !present {
		declared, err := npmRegistriesFrom(ctx.Params)
		if err != nil {
			return nil, distribution.ReleaseSetRef{}, false, err
		}
		versions, err := resolvePutnamiNPMVersions(context.Background(), wsRoot, declared, packages, target, emit)
		return versions, distribution.ReleaseSetRef{}, false, err
	}
	head, diagnostics := distribution.ParseAndValidateChannelHead(raw)
	if head == nil || diag.HasErrors(diagnostics) {
		return nil, distribution.ReleaseSetRef{}, true, fmt.Errorf("invalid release-set snapshot: %v", diagnostics)
	}
	if head.ReleaseSet == nil {
		return nil, distribution.ReleaseSetRef{}, true, fmt.Errorf("release-set snapshot %s carries no members", head.Ref.ID)
	}
	// The namespace is NOT compared to the workspace name. A consumer workspace
	// is not the publisher: a downstream consumer workspace consuming `putnami`
	// is the normal case, and requiring the two to match would keep every
	// consumer from upgrading.
	// The orchestrator already validated this response against the exact
	// request it made, namespace included.

	available := make(map[string]string)
	for _, member := range head.ReleaseSet.Members {
		if member.Ecosystem == npmEcosystem {
			available[member.Coordinate] = member.Version
		}
	}
	versions := make(map[string]string, len(packages))
	for _, name := range packages {
		version, ok := available[name]
		if !ok {
			return nil, distribution.ReleaseSetRef{}, true, fmt.Errorf("release set %s has no npm member for referenced package %q", head.Ref.ID, name)
		}
		versions[name] = version
	}
	return versions, head.Ref, true, nil
}

type npmUpgradeFileSnapshot struct {
	path    string
	data    []byte
	mode    os.FileMode
	existed bool
}

// npmUpgradeTransaction closes the manifest and both Bun lockfile spellings as
// one metadata unit. bun may create either lock format before failing, so an
// absent pre-upgrade file is as important to remember as an existing one.
type npmUpgradeTransaction struct {
	files  []npmUpgradeFileSnapshot
	active bool
}

func beginNPMUpgradeTransaction(wsRoot string) (*npmUpgradeTransaction, error) {
	transaction := &npmUpgradeTransaction{active: true}
	for _, name := range []string{"package.json", "bun.lock", "bun.lockb"} {
		path := filepath.Join(wsRoot, name)
		info, err := os.Lstat(path)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s is not a regular metadata file", path)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, readErr
			}
			transaction.files = append(transaction.files, npmUpgradeFileSnapshot{path: path, data: data, mode: info.Mode().Perm(), existed: true})
		case os.IsNotExist(err):
			transaction.files = append(transaction.files, npmUpgradeFileSnapshot{path: path})
		default:
			return nil, err
		}
	}
	return transaction, nil
}

func (t *npmUpgradeTransaction) rollback() error {
	if t == nil || !t.active {
		return nil
	}
	t.active = false
	var firstErr error
	for _, file := range t.files {
		if !file.existed {
			if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		temp, err := os.CreateTemp(filepath.Dir(file.path), ".putnami-upgrade-restore-*")
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		tempPath := temp.Name()
		if err = temp.Chmod(file.mode); err == nil {
			_, err = temp.Write(file.data)
		}
		if closeErr := temp.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(tempPath, file.path)
		}
		if err != nil {
			_ = os.Remove(tempPath)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (t *npmUpgradeTransaction) commit() {
	if t != nil {
		t.active = false
	}
}

func rollbackNPMUpgrade(transaction *npmUpgradeTransaction, cause error) error {
	if rollbackErr := transaction.rollback(); rollbackErr != nil {
		if cause != nil {
			return fmt.Errorf("%w (and restore npm metadata: %w)", cause, rollbackErr)
		}
		return fmt.Errorf("restore npm metadata: %w", rollbackErr)
	}
	return cause
}

func upgradeTarget(ctx *pctx.Context) string {
	target := ctx.Params.String("putnami-version", "putnamiVersion")
	if target == "" {
		target = ctx.Params.String("putnami-channel", "putnamiChannel")
	}
	if target == "" || target == "stable" {
		target = "latest"
	}
	return target
}

// resolvePutnamiNPMVersion reads one package's dist-tag from the registry the
// workspace .npmrc names. A dist-tag IS the npm-native spelling of a release
// channel, so this is the whole channel lookup for the npm ecosystem — no
// release set, no namespace.
//
// Every failure names the package, the channel, the registry host and the HTTP
// status, because those four facts pick the fix: publish the channel, fix the
// credential, or fix the registry.
func resolvePutnamiNPMVersion(ctx context.Context, wsRoot string, declared npmRegistries, packageName, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" || target == "stable" {
		target = "latest"
	}
	if v := exactNPMVersion(target); v != "" {
		return v, nil
	}

	registry := resolvePutnamiNPMRegistry(declared, packageName)
	host := npmRegistryHost(registry)
	fail := func(reason string, status int, cause error) error {
		message := fmt.Sprintf("dist-tag %q for %s: %s (registry %s", target, packageName, reason, host)
		if status != 0 {
			message += fmt.Sprintf(", HTTP %d", status)
		}
		message += ")"
		if cause != nil {
			return fmt.Errorf("%s: %w", message, cause)
		}
		return errors.New(message)
	}
	escapedName := strings.ReplaceAll(url.PathEscape(packageName), "%40", "@")
	metadataURL := strings.TrimRight(registry, "/") + "/" + escapedName

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return "", fail("cannot build the metadata request", 0, err)
	}
	setPutnamiUserAgent(req)
	// Anonymous metadata hides the versions a private registry only shows to an
	// authenticated client, which reports a published channel as missing. Present
	// the same _authToken npm itself would use.
	if token := npmRegistryAuthToken(wsRoot, registry); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// The registry is an explicit workspace .npmrc setting, not remote request
	// input. Private registries (including loopback in local development) are a
	// supported npm workflow and are pinned by the request tests.
	resp, err := npmMetadataClient.Do(req) //nolint:gosec // G704: trusted workspace registry
	if err != nil {
		return "", fail("registry is unreachable", 0, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fail("packument request failed", resp.StatusCode, nil)
	}

	var metadata struct {
		DistTags map[string]string `json:"dist-tags"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2*1024*1024)).Decode(&metadata); err != nil {
		return "", fail("packument is not readable", resp.StatusCode, err)
	}
	version := strings.TrimSpace(metadata.DistTags[target])
	if version == "" {
		return "", fail("no dist-tag with that name", resp.StatusCode, nil)
	}
	return version, nil
}

func npmRegistryHost(registry string) string {
	parsed, err := url.Parse(strings.TrimSpace(registry))
	if err != nil || parsed.Host == "" {
		return strings.TrimSpace(registry)
	}
	return parsed.Host
}

// npmRegistryAuthToken finds the credential npm would use for this registry:
// the workspace .npmrc first, then the user's. Absent is the normal case for a
// public registry.
func npmRegistryAuthToken(wsRoot, registry string) string {
	parsed, err := url.Parse(strings.TrimSpace(registry))
	if err != nil || parsed.Host == "" {
		return ""
	}
	key := "//" + parsed.Host + strings.TrimRight(parsed.Path, "/") + "/:_authToken="
	candidates := []string{filepath.Join(wsRoot, ".npmrc")}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".npmrc"))
	}
	for _, path := range candidates {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, key) {
				continue
			}
			token := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, key)), `"`)
			// An unexpanded ${NPM_TOKEN} placeholder is not a credential.
			if token != "" && !strings.Contains(token, "${") {
				return token
			}
		}
	}
	return ""
}

func resolvePutnamiNPMVersions(ctx context.Context, wsRoot string, declared npmRegistries, packages []string, target string, emit *jsonl.Emitter) (map[string]string, error) {
	versions := make(map[string]string, len(packages))
	for _, pkg := range packages {
		version, err := resolvePutnamiNPMVersion(ctx, wsRoot, declared, pkg, target)
		if err != nil {
			// A single unresolvable package (e.g. a stale catalog entry that was
			// never published) is warned and skipped rather than aborting the
			// whole upgrade — mirroring the Go upgrader's per-module leniency.
			emit.Log("warn", fmt.Sprintf("Skipping %s: %v", pkg, err))
			continue
		}
		versions[pkg] = version
	}
	// Nothing resolving at all points at the selector or the registry, not one
	// package — surface that as a hard failure.
	if len(versions) == 0 && len(packages) > 0 {
		return nil, fmt.Errorf("could not resolve %q for any @putnami/* package (registry unreachable or selector unknown)", target)
	}
	return versions, nil
}

// resolvedPackages returns the subset of names that resolved to a concrete
// version, preserving the input order.
func resolvedPackages(packages []string, versions map[string]string) []string {
	out := make([]string, 0, len(packages))
	for _, name := range packages {
		if versions[name] != "" {
			out = append(out, name)
		}
	}
	return out
}

// logResolvedPutnamiVersions prints the resolved table. source names the
// protocol that answered ("dist-tag" for a channel, "release-set" for an
// immutable snapshot) so a reader knows which authority produced the versions
// before anything is written.
//
// The REVISION column is the source revision the version carries: versions are
// derived from git now, so an ordered pre-release ends in the commit it was cut
// from and that suffix is the only way to tell two builds of one base version
// apart. A stable version carries no revision and shows "-".
func logResolvedPutnamiVersions(emit *jsonl.Emitter, packages []string, versions map[string]string, source string) {
	for _, line := range renderUpgradeTable(packages, versions, source) {
		emit.Log("info", line)
	}
}

// upgradeTableColumns are the table's headers, in order.
var upgradeTableColumns = []string{"PACKAGE", "VERSION", "REVISION", "SOURCE"}

// renderUpgradeTable builds the resolved-version rows, header first, padded so
// the columns line up. It is separated from emission so the shape is testable
// without an emitter.
func renderUpgradeTable(packages []string, versions map[string]string, source string) []string {
	if len(packages) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(packages)+1)
	rows = append(rows, upgradeTableColumns)
	for _, pkg := range packages {
		version := versions[pkg]
		rows = append(rows, []string{pkg, valueOrDash(version), npmVersionRevision(version), source})
	}

	widths := make([]int, len(upgradeTableColumns))
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(cell)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len(cell)))
			}
		}
		lines = append(lines, b.String())
	}
	return lines
}

// npmVersionRevision returns the source revision a resolved version carries:
// the trailing segment of its pre-release suffix, which is the commit the build
// was cut from. A stable version has no pre-release and yields "-".
func npmVersionRevision(version string) string {
	version = strings.TrimSpace(version)
	if build := strings.IndexByte(version, '+'); build >= 0 {
		version = version[:build]
	}
	dash := strings.LastIndexByte(version, '-')
	if dash < 0 || dash == len(version)-1 {
		return "-"
	}
	return version[dash+1:]
}

func setPutnamiUserAgent(req *http.Request) {
	if req.Header.Get("User-Agent") != "" {
		return
	}
	ua := strings.TrimSpace(os.Getenv(cliUserAgentEnv))
	if ua == "" {
		ua = "putnami-cli/dev"
	}
	req.Header.Set("User-Agent", ua)
}

func exactNPMVersion(target string) string {
	target = strings.TrimSpace(target)
	if !exactNPMVersionPattern.MatchString(target) {
		return ""
	}
	return strings.TrimPrefix(target, "v")
}

// resolvePutnamiNPMRegistry returns the registry that serves one package: the
// workspace `registries.npm.scopes` entry for the package's scope, falling back
// to npm's own default registry.
//
// It reads the DECLARATION rather than scanning the .npmrc the install writes
// from it. Two readers of one generated file drift the moment one of them runs
// before the generator; the workspace document does not.
func resolvePutnamiNPMRegistry(declared npmRegistries, packageName string) string {
	if registry := declared.registryForPackage(packageName); registry != "" {
		return registry
	}
	return defaultNPMRegistry
}

// pinWorkspacePutnamiDeps writes the resolved version for every referenced
// @putnami/* package into the workspace root package.json.
//
// When the root declares a Bun catalog, the catalog is the single source of
// truth: versions are written there (and any catalog:-referenced package missing
// from the catalog is seeded so `bun install` does not fail), while the
// duplicated @putnami/* dependencies/overrides are stripped. Otherwise the
// legacy shape — exact-pinned root dependencies plus overrides — is maintained.
func pinWorkspacePutnamiDeps(wsRoot string, packages []string, version string, dryRun bool, emit *jsonl.Emitter) error {
	return pinWorkspacePutnamiDepsWithVersions(wsRoot, packages, packageVersionMap(packages, version), dryRun, emit)
}

func packageVersionMap(packages []string, version string) map[string]string {
	versions := make(map[string]string, len(packages))
	for _, name := range packages {
		versions[name] = version
	}
	return versions
}

func pinWorkspacePutnamiDepsWithVersions(wsRoot string, packages []string, versions map[string]string, dryRun bool, emit *jsonl.Emitter) error {
	pkgPath := filepath.Join(wsRoot, "package.json")
	pkg, order, err := readRootManifest(pkgPath)
	if err != nil {
		return err
	}

	if cats := parseCatalogModel(pkg); cats.present() {
		return pinViaCatalog(pkgPath, pkg, order, cats, packages, versions, dryRun, emit)
	}
	return pinViaDependencies(pkgPath, pkg, order, packages, versions, dryRun, emit)
}

// pinViaCatalog updates @putnami/* versions inside the workspace catalog and
// removes the redundant root dependencies/overrides so the catalog stays the
// only version policy.
func pinViaCatalog(pkgPath string, pkg map[string]json.RawMessage, order []string, cats *catalogModel, packages []string, versions map[string]string, dryRun bool, emit *jsonl.Emitter) error {
	changed := false
	for _, name := range packages {
		version := versions[name]
		current, inCatalog := cats.lookup(name)
		switch {
		case !inCatalog:
			changed = true
			logVersionChange(emit, dryRun, "catalog", name, "", version)
			if !dryRun {
				cats.addToDefault(name, version)
			}
		case current != version:
			changed = true
			logVersionChange(emit, dryRun, "catalog", name, current, version)
			if !dryRun {
				cats.set(name, version)
			}
		}
	}

	depsChanged, err := stripPutnamiEntries(pkg, "dependencies", dryRun, emit)
	if err != nil {
		return err
	}
	overridesChanged, err := stripPutnamiEntries(pkg, "overrides", dryRun, emit)
	if err != nil {
		return err
	}
	changed = changed || depsChanged || overridesChanged

	if !changed {
		emit.Log("info", "Root catalog already pins @putnami/* dependencies")
		return nil
	}
	if dryRun {
		return nil
	}

	if err := cats.writeBack(pkg); err != nil {
		return err
	}
	return writeRootManifest(pkgPath, pkg, order)
}

// pinViaDependencies maintains the legacy (catalog-free) shape: each referenced
// @putnami/* package is exact-pinned in root dependencies, and that direct pin
// is the sole version policy. An override that duplicates a direct dependency at
// the same version is pure npm EOVERRIDE bait, so any redundant
// @putnami/* override is stripped rather than written — matching pinViaCatalog.
// Non-@putnami overrides are left untouched.
func pinViaDependencies(pkgPath string, pkg map[string]json.RawMessage, order []string, packages []string, versions map[string]string, dryRun bool, emit *jsonl.Emitter) error {
	deps := catalog.UnmarshalStringMap(pkg["dependencies"])
	if deps == nil {
		deps = make(map[string]string)
	}

	depsChanged := false
	for _, name := range packages {
		version := versions[name]
		if deps[name] != version {
			depsChanged = true
			logVersionChange(emit, dryRun, "dependency", name, deps[name], version)
			if !dryRun {
				deps[name] = version
			}
		}
	}

	// Drop redundant @putnami/* overrides (the EOVERRIDE trigger); the direct
	// dependency pin above already fixes the version.
	overridesChanged, err := stripPutnamiEntries(pkg, "overrides", dryRun, emit)
	if err != nil {
		return err
	}

	if !depsChanged && !overridesChanged {
		emit.Log("info", "Root package.json already pins @putnami/* dependencies")
		return nil
	}
	if dryRun {
		return nil
	}

	if depsChanged {
		depsJSON, err := json.Marshal(deps)
		if err != nil {
			return fmt.Errorf("marshal dependencies: %w", err)
		}
		pkg["dependencies"] = depsJSON
	}

	return writeRootManifest(pkgPath, pkg, order)
}

// logVersionChange emits a uniform "Pinned"/"Would set" line for a version
// transition in a given location (catalog, dependency, override).
func logVersionChange(emit *jsonl.Emitter, dryRun bool, where, name, current, version string) {
	verb := "Pinned"
	if dryRun {
		verb = "Would set"
	}
	emit.Log("info", fmt.Sprintf("%s %s %s: %s → %s", verb, where, name, valueOrDash(current), version))
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// collectPutnamiDeps scans the root and every workspace member package.json for
// @putnami/* dependencies, excluding packages that are local workspace projects
// (which are resolved from source rather than the registry). Members are read
// from the "workspaces" field, so catalog:-referenced packages are detected
// regardless of directory depth.
func collectPutnamiDeps(wsRoot string, emit *jsonl.Emitter) []string {
	seen := make(map[string]bool)

	rootManifest := filepath.Join(wsRoot, "package.json")
	scanPackageJSON(rootManifest, seen)
	for _, dir := range workspaceMemberDirs(wsRoot) {
		if strings.Contains(dir, "node_modules") {
			continue
		}
		scanPackageJSON(filepath.Join(dir, "package.json"), seen)
	}

	// Manage every @putnami/* entry already declared in the workspace catalog,
	// even when no member imports it directly. A catalog entry is version policy
	// the workspace committed to; leaving an unreferenced one untouched lets it
	// drift stale on upgrade (the exact symptom that stranded catalog-only
	// packages at an old version). This mirrors the Go upgrader, which re-pins
	// every existing go.work replace rather than only referenced modules.
	scanCatalogPutnamiDeps(rootManifest, seen)

	// Exclude local workspace packages — they should not be fetched from the registry.
	for name := range collectWorkspacePackageNames(wsRoot) {
		if seen[name] {
			emit.Log("info", fmt.Sprintf("Skipping local workspace package %s", name))
			delete(seen, name)
		}
	}

	packages := make([]string, 0, len(seen))
	for name := range seen {
		packages = append(packages, name)
	}
	sort.Strings(packages)
	return packages
}

// scanCatalogPutnamiDeps records every @putnami/* package declared in the root
// package.json Bun catalog(s) — the default and any named catalogs, whether
// placed at the top level or nested under the workspaces object. Catalog entries
// are the workspace's committed version policy, so upgrade keeps them current
// regardless of whether a member currently imports them.
func scanCatalogPutnamiDeps(rootManifest string, seen map[string]bool) {
	pkg, _, err := readRootManifest(rootManifest)
	if err != nil {
		return
	}
	cats := parseCatalogModel(pkg)
	if !cats.present() {
		return
	}
	for _, name := range cats.scopedNames(putnamiScopePrefix) {
		seen[name] = true
	}
}

// collectWorkspacePackageNames returns the set of package names declared by the
// local workspace members.
func collectWorkspacePackageNames(wsRoot string) map[string]bool {
	names := make(map[string]bool)
	for _, dir := range workspaceMemberDirs(wsRoot) {
		pkgData, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			continue
		}
		var pkg struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(pkgData, &pkg) == nil && pkg.Name != "" {
			names[pkg.Name] = true
		}
	}
	return names
}

// workspaceMemberDirs resolves the directories of the workspace members listed
// in the root package.json "workspaces" field. It accepts both the array form
// (["packages/*"]) and the object form ({"packages": [...]}), and expands glob
// patterns the way Bun does.
func workspaceMemberDirs(wsRoot string) []string {
	data, err := os.ReadFile(filepath.Join(wsRoot, "package.json"))
	if err != nil {
		return nil
	}
	var root struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if json.Unmarshal(data, &root) != nil || len(root.Workspaces) == 0 {
		return nil
	}

	var dirs []string
	for _, pattern := range parseWorkspacePatterns(root.Workspaces) {
		pattern = filepath.FromSlash(pattern)
		full := filepath.Join(wsRoot, pattern)
		if strings.ContainsAny(pattern, "*?[") {
			matches, _ := filepath.Glob(full)
			dirs = append(dirs, matches...)
			continue
		}
		dirs = append(dirs, full)
	}
	return dirs
}

// parseWorkspacePatterns extracts the member glob patterns from a "workspaces"
// value in either the array or object ({"packages": [...]}) form.
func parseWorkspacePatterns(raw json.RawMessage) []string {
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	var obj struct {
		Packages []string `json:"packages"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Packages
	}
	return nil
}

func scanPackageJSON(path string, seen map[string]bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return
	}

	for name := range pkg.Dependencies {
		if strings.HasPrefix(name, "@putnami/") {
			seen[name] = true
		}
	}
	for name := range pkg.DevDependencies {
		if strings.HasPrefix(name, "@putnami/") {
			seen[name] = true
		}
	}
}
