package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/typescript/extension/internal/catalog"
	"go.putnami.dev/typescript/extension/internal/pnpmworkspace"
)

// The deps-edit job is what `putnami deps add`, `deps remove` and `deps
// prune` run for a TypeScript project: it edits the project's package.json,
// keeps the workspace catalog the package manager reads consistent with it
// (ADR 0002), then installs with the workspace's package manager. A failed
// install puts every file the job wrote back as it was.
//
// The CLI sends the same params it sends the Go extension's deps-edit job:
// action (add, remove or prune), edits (one {project, path, modules} per
// project) and dry-run.

const (
	depsEditAdd    = "add"
	depsEditRemove = "remove"
	depsEditPrune  = "prune"
)

// depsEditFields are the dependency fields of a package.json, in the order an
// add looks for an existing entry. peerDependencies is not one: an add never
// turns a peer range into an installed dependency, but remove drops it too.
var depsEditFields = []string{"dependencies", "devDependencies", "optionalDependencies"}

// depsEditRemoveFields are the fields remove and prune drop a package from.
var depsEditRemoveFields = []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"}

// npmPackageName matches a valid npm package name, scoped or not.
var npmPackageName = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9][a-z0-9._~-]*$`)

// npmDistTag matches a version spec that is a dist-tag, such as latest or
// next, rather than a version, a range or a protocol.
var npmDistTag = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)

// npmVersionPrefix matches a v-prefixed version or range, such as v1 or v1.2.
var npmVersionPrefix = regexp.MustCompile(`^v[0-9]`)

// depsEditLatestTag is the dist-tag an add resolves when the spec names no
// version.
const depsEditLatestTag = "latest"

// lookupDepsEditVersion reads the version a dist-tag names for a package on
// the registry the workspace declares for it. Tests replace it.
var lookupDepsEditVersion = func(ctx *pctx.Context, name, tag string) (string, error) {
	declared, err := npmRegistriesFrom(ctx.Params)
	if err != nil {
		return "", err
	}
	return resolvePutnamiNPMVersion(context.Background(), ctx.WorkspaceRoot, declared, name, tag)
}

// depsEditEntry is one element of params.edits.
type depsEditEntry struct {
	// Project is the name of the workspace project, for the messages.
	Project string `json:"project"`
	// Path is the project directory, relative to the workspace root.
	Path string `json:"path"`
	// Modules are package specs (add), package names (remove) or the
	// workspace-relative directories of the projects whose package is dropped
	// (prune).
	Modules []string `json:"modules"`
}

func runDepsEdit(ctx *pctx.Context, emit *jsonl.Emitter, _ []string) (string, map[string]any, error) {
	action, edits, problem := parseDepsEditParams(ctx.Params)
	if problem != "" {
		emit.Diagnostic("error", problem, "", 0)
		return "FAILED", nil, nil
	}
	if action == depsEditAdd {
		if problem := refuseDropOnDepsAdd(edits); problem != "" {
			emit.Diagnostic("error", problem, "", 0)
			return "FAILED", nil, nil
		}
	}
	// An edit resolves versions on the registry and the install downloads.
	if registrycred.OfflineDependencies() {
		emit.Diagnostic("error", "deps "+action+" reaches the network and rewrites package.json, "+
			"so it does not run when dependencies are offline (a hosted run)", "", 0)
		return "FAILED", nil, nil
	}
	wsRoot := ctx.WorkspaceRoot
	manager, err := detectPackageManager(wsRoot)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	if manager.Name == managerRush {
		emit.Diagnostic("error", "a Rush repository adds and removes dependencies with rush add and rush remove, which @putnami/typescript does not run", "", 0)
		return "FAILED", nil, nil
	}
	dryRun := ctx.Params.Bool("dry-run", false, "dryRun")

	state, err := newDepsEditState(ctx, emit, manager)
	if err != nil {
		emit.Diagnostic("error", err.Error(), "", 0)
		return "FAILED", nil, nil
	}
	for _, e := range edits {
		if err := state.apply(action, e, dryRun); err != nil {
			message := err.Error()
			if dryRun {
				message += "; the dry run changed no file"
			} else {
				message += "; no file was changed"
			}
			emit.Diagnostic("error", message, "", 0)
			return "FAILED", nil, nil
		}
	}
	if err := state.dropUnreferencedCatalogEntries(); err != nil {
		emit.Diagnostic("error", err.Error()+"; no file was changed", "", 0)
		return "FAILED", nil, nil
	}

	if dryRun {
		state.report = append(state.report, "Dry run complete; no TypeScript files were changed")
		emit.Log("info", state.report[len(state.report)-1])
		emit.SummaryWithData(strings.Join(state.report, "\n"), map[string]any{"visibility": "always"})
		return "OK", nil, nil
	}
	written := state.changedFiles()
	if len(written) == 0 {
		state.report = append(state.report, "No file changed")
		emit.SummaryWithData(strings.Join(state.report, "\n"), map[string]any{"visibility": "always"})
		return "OK", nil, nil
	}

	managerBin, err := resolveManagerBin(manager)
	if err != nil {
		emit.Diagnostic("error", err.Error()+"; no file was changed", "", 0)
		return "FAILED", nil, nil
	}
	transaction, err := beginNPMUpgradeTransaction(wsRoot, manager, written...)
	if err != nil {
		emit.Diagnostic("error", fmt.Sprintf("snapshot npm metadata: %v; no file was changed", err), "", 0)
		return "FAILED", nil, nil
	}
	// restored puts every file back and reports cause; the job then fails.
	restored := func(cause string) string {
		message := cause + "; every file the command edited is back as it was"
		if rollbackErr := transaction.rollback(); rollbackErr != nil {
			message = cause + "\nrestore npm metadata: " + rollbackErr.Error()
		}
		emit.Diagnostic("error", message, "", 0)
		return "FAILED"
	}
	// A SIGINT or a SIGTERM, such as the task timeout, stops the install and
	// puts the files back too.
	stopped, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := state.write(); err != nil {
		return restored(err.Error()), nil, nil
	}

	emit.PhaseStart("install")
	install := workspaceInstallInvocation(manager, false, false)
	label := manager.Name + " install"
	var opts []exec.Option
	if len(install.env) > 0 {
		opts = append(opts, exec.Env(install.env))
	}
	result, err := runBunWithin(stopped, label, managerBin, install.args, wsRoot, bunNetworkTimeout, opts...)
	if err != nil {
		emit.PhaseEnd("install", "failed")
		return restored(err.Error()), nil, nil
	}
	if !result.Success {
		emit.PhaseEnd("install", "failed")
		reportRegistryRefusal(emit, result.Stderr)
		return restored(label + " failed: " + managerFailureOutput(result)), nil, nil
	}
	emit.PhaseEnd("install", "success")
	transaction.commit()
	emit.SummaryWithData(strings.Join(state.report, "\n"), map[string]any{"visibility": "always"})
	return "OK", nil, nil
}

// parseDepsEditParams reads the action and the edits, or names what is wrong
// with them.
func parseDepsEditParams(params pctx.Params) (string, []depsEditEntry, string) {
	action := params.String("action")
	if action != depsEditAdd && action != depsEditRemove && action != depsEditPrune {
		return "", nil, fmt.Sprintf("deps-edit needs params.action to be add, remove or prune, not %q", action)
	}
	var edits []depsEditEntry
	if err := json.Unmarshal(params["edits"], &edits); err != nil || len(edits) == 0 {
		return "", nil, "deps-edit needs params.edits: a non-empty array of {project, path, modules}"
	}
	for _, e := range edits {
		if e.Path == "" || !filepath.IsLocal(filepath.FromSlash(e.Path)) && e.Path != "." {
			return "", nil, fmt.Sprintf("deps-edit needs the directory of %q relative to the workspace root, not %q", e.Project, e.Path)
		}
		if len(e.Modules) == 0 {
			return "", nil, fmt.Sprintf("deps %s requires at least one package (e.g. `putnami deps %s @putnami/database`)", action, action)
		}
	}
	return action, edits, ""
}

// refuseDropOnDepsAdd names the specs of an add whose version is none, and the
// deps remove that drops them.
func refuseDropOnDepsAdd(edits []depsEditEntry) string {
	var dropped []string
	for _, e := range edits {
		for _, spec := range e.Modules {
			if name, version := splitPackageSpec(spec); version == "none" {
				dropped = append(dropped, name)
			}
		}
	}
	if len(dropped) == 0 {
		return ""
	}
	return fmt.Sprintf("deps add does not drop a package: %s names the version none; deps remove drops it: putnami deps remove %s",
		strings.Join(dropped, ", "), strings.Join(dropped, " "))
}

// splitPackageSpec splits a package spec at its last "@" that is not its
// first character, so @scope/pkg has no version and @scope/pkg@^1 has ^1.
func splitPackageSpec(spec string) (name, version string) {
	spec = strings.TrimSpace(spec)
	i := strings.LastIndex(spec, "@")
	if i <= 0 {
		return spec, ""
	}
	return spec[:i], spec[i+1:]
}

// manifestFile is one package.json the job reads and may write, with the
// top-level key order it had on disk.
type manifestFile struct {
	path    string
	pkg     map[string]json.RawMessage
	order   []string
	changed bool
}

// depsEditState holds every file a deps edit reads, in memory, until the
// whole edit is planned: nothing is written before every edit succeeded.
type depsEditState struct {
	ctx     *pctx.Context
	emit    *jsonl.Emitter
	manager packageManager
	// manifests are the package.json files read so far, by absolute path, so
	// a project at the workspace root shares its manifest with the catalog.
	manifests map[string]*manifestFile
	// bunCatalog is the catalog of the root package.json, for Bun.
	bunCatalog *catalogModel
	// pnpmText is pnpm-workspace.yaml, for pnpm; pnpmChanged says it changed.
	pnpmText    string
	pnpmChanged bool
	// emptiedDefault and emptiedNamed record that a remove left the Bun
	// default catalog, or every named catalog, empty: write drops the key.
	emptiedDefault, emptiedNamed bool
	// members are the packages the workspace members publish, by name, with
	// their version.
	members map[string]string
	// removed are the catalog entries a remove or prune stopped referencing,
	// by catalog and name.
	removed map[[2]string]bool
	// credentialRefreshed records that the registry credential was refreshed.
	credentialRefreshed bool
	report              []string
}

func newDepsEditState(ctx *pctx.Context, emit *jsonl.Emitter, manager packageManager) (*depsEditState, error) {
	s := &depsEditState{
		ctx:       ctx,
		emit:      emit,
		manager:   manager,
		manifests: map[string]*manifestFile{},
		members:   workspacePackageVersions(ctx.WorkspaceRoot),
		removed:   map[[2]string]bool{},
	}
	switch manager.Name {
	case managerBun:
		root, err := s.manifest(filepath.Join(ctx.WorkspaceRoot, "package.json"))
		if err != nil {
			return nil, err
		}
		s.bunCatalog = parseCatalogModel(root.pkg)
	case managerPNPM:
		data, err := os.ReadFile(pnpmWorkspaceFile(ctx.WorkspaceRoot))
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", pnpmworkspace.File, err)
		}
		s.pnpmText = string(data)
		if _, err := pnpmworkspace.ReadCatalogs(s.pnpmText); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// readsCatalog reports whether the workspace's package manager reads a
// catalog the job writes shared versions to.
func (s *depsEditState) readsCatalog() bool {
	return s.bunCatalog != nil || s.manager.Name == managerPNPM
}

// manifest reads the package.json at path once.
func (s *depsEditState) manifest(path string) (*manifestFile, error) {
	if m, ok := s.manifests[path]; ok {
		return m, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	pkg, order, err := readRootManifest(path)
	if err != nil {
		return nil, err
	}
	m := &manifestFile{path: path, pkg: pkg, order: order}
	s.manifests[path] = m
	return m, nil
}

// apply plans one project's edit in memory.
func (s *depsEditState) apply(action string, e depsEditEntry, dryRun bool) error {
	dir := filepath.Join(s.ctx.WorkspaceRoot, filepath.FromSlash(e.Path))
	m, err := s.manifest(filepath.Join(dir, "package.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is not a TypeScript package: %s has no package.json", e.Project, e.Path)
		}
		return err
	}
	verb := map[string]string{depsEditAdd: "Adding", depsEditRemove: "Removing", depsEditPrune: "Removing"}[action]
	if dryRun {
		verb = map[string]string{depsEditAdd: "Planning to add", depsEditRemove: "Planning to remove", depsEditPrune: "Planning to remove"}[action]
	}
	s.emit.Log("info", fmt.Sprintf("%s %s in %s...", verb, strings.Join(e.Modules, " "), e.Project))
	for _, module := range e.Modules {
		switch action {
		case depsEditAdd:
			err = s.add(e.Project, m, module)
		case depsEditRemove:
			name, _ := splitPackageSpec(module)
			err = s.remove(action, e.Project, m, name, []string{name})
		case depsEditPrune:
			var name string
			if name, err = s.packageAt(module); err == nil {
				// The putnami section may name the target by package name or
				// by its directory.
				dir := filepath.ToSlash(filepath.Clean(filepath.FromSlash(module)))
				err = s.remove(action, e.Project, m, name, []string{name, dir, "/" + dir, "./" + dir})
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// packageAt is the name of the package the project at dir publishes.
func (s *depsEditState) packageAt(dir string) (string, error) {
	if !filepath.IsLocal(filepath.FromSlash(dir)) {
		return "", fmt.Errorf("deps prune needs the directory of the target relative to the workspace root, not %q", dir)
	}
	path := filepath.Join(s.ctx.WorkspaceRoot, filepath.FromSlash(dir), "package.json")
	m, err := s.manifest(path)
	if err != nil {
		return "", fmt.Errorf("deps prune cannot name the package of %s: %w", dir, err)
	}
	var name string
	if json.Unmarshal(m.pkg["name"], &name) != nil || name == "" {
		return "", fmt.Errorf("deps prune cannot name the package of %s: its package.json has no name", dir)
	}
	return name, nil
}

// add plans one package spec into the project's manifest m.
func (s *depsEditState) add(project string, m *manifestFile, spec string) error {
	name, version := splitPackageSpec(spec)
	if !npmPackageName.MatchString(name) {
		return fmt.Errorf("deps add: %q is not an npm package name", name)
	}
	var own string
	_ = json.Unmarshal(m.pkg["name"], &own)
	if own == name {
		return fmt.Errorf("deps add: %s is the package of %s itself", name, project)
	}
	field, current := "dependencies", ""
	for _, f := range depsEditFields {
		if value, ok := catalog.UnmarshalStringMap(m.pkg[f])[name]; ok {
			field, current = f, value
			break
		}
	}

	var want string
	memberVersion, member := s.members[name]
	switch {
	case member:
		want = s.memberSpec(memberVersion)
		if version != "" {
			s.note(fmt.Sprintf("%s is a workspace package: %s depends on it as %s, not %s", name, project, want, version))
		}
	case version == "" && current != "" && !strings.HasPrefix(current, catalog.Protocol) && !strings.HasPrefix(current, "workspace:"):
		// The spec names no version and the project already pins one: keep it
		// rather than move the project to whatever latest is now.
		s.note(fmt.Sprintf("%s already lists %s: %s in %s; name a version to change it", project, name, current, field))
		return nil
	case s.readsCatalog():
		selected := ""
		if c, ok := catalog.NameFromSpec(current); ok {
			selected = c
		} else if holders := s.catalogHolders(name); len(holders) > 0 {
			selected = holders[0]
		}
		entry, held := s.catalogLookup(selected, name)
		switch {
		case version != "" || !held:
			resolved, err := s.resolve(name, version)
			if err != nil {
				return err
			}
			if !held || entry != resolved {
				if err := s.catalogPut(selected, name, resolved); err != nil {
					return err
				}
				s.note(fmt.Sprintf("%s %s: %s → %s", catalogLabel(s.catalogFile(), selected), name, valueOrDash(entry), resolved))
			}
		default:
			s.note(fmt.Sprintf("%s %s keeps %s", catalogLabel(s.catalogFile(), selected), name, entry))
		}
		want = catalog.Protocol + selected
	default:
		resolved, err := s.resolve(name, version)
		if err != nil {
			return err
		}
		want = resolved
	}
	if current == want {
		s.note(fmt.Sprintf("%s already lists %s: %s in %s", project, name, want, field))
		return nil
	}
	setDependency(m, field, name, want)
	s.note(fmt.Sprintf("%s %s %s: %s → %s", project, field, name, valueOrDash(current), want))
	return nil
}

// remove plans dropping name from every dependency field of m. aliases are
// the entries of the package.json putnami section's dependencies that name
// the same project, by package name and, for a prune, by directory; they are
// dropped too.
func (s *depsEditState) remove(action, project string, m *manifestFile, name string, aliases []string) error {
	found := s.removePutnamiDependency(project, m, aliases)
	for _, field := range depsEditRemoveFields {
		deps := catalog.UnmarshalStringMap(m.pkg[field])
		value, ok := deps[name]
		if !ok {
			continue
		}
		found = true
		if c, isCatalog := catalog.NameFromSpec(value); isCatalog {
			s.removed[[2]string{c, name}] = true
		}
		delete(deps, name)
		if len(deps) == 0 {
			delete(m.pkg, field)
		} else {
			raw, _ := marshalManifestValue(deps)
			m.pkg[field] = raw
		}
		m.changed = true
		s.note(fmt.Sprintf("%s %s %s: %s → -", project, field, name, value))
	}
	if !found {
		return fmt.Errorf("deps %s: %s lists no dependency on %s", action, project, name)
	}
	return nil
}

// removePutnamiDependency drops each alias from the dependencies of the
// putnami section of m, and reports whether it dropped one.
func (s *depsEditState) removePutnamiDependency(project string, m *manifestFile, aliases []string) bool {
	if len(aliases) == 0 || len(m.pkg["putnami"]) == 0 {
		return false
	}
	var section map[string]json.RawMessage
	if json.Unmarshal(m.pkg["putnami"], &section) != nil {
		return false
	}
	var deps []string
	if json.Unmarshal(section["dependencies"], &deps) != nil {
		return false
	}
	kept := deps[:0:0]
	for _, dep := range deps {
		if slices.Contains(aliases, dep) {
			s.note(fmt.Sprintf("%s putnami.dependencies %s: dropped", project, dep))
			continue
		}
		kept = append(kept, dep)
	}
	if len(kept) == len(deps) {
		return false
	}
	if len(kept) == 0 {
		delete(section, "dependencies")
	} else {
		section["dependencies"], _ = marshalManifestValue(kept)
	}
	m.pkg["putnami"], _ = marshalManifestValue(section)
	m.changed = true
	return true
}

// memberSpec is the spec a project depends on a workspace package with:
// workspace:* where the package manager reads that protocol, and ^ the
// package's version, or *, with npm and Yarn 1, which do not: a range keeps
// matching the local copy after a patch or minor bump.
func (s *depsEditState) memberSpec(version string) string {
	if s.manager.Name == managerNPM || (s.manager.Name == managerYarn && !s.manager.Berry) {
		if version == "" {
			return "*"
		}
		return "^" + version
	}
	return "workspace:*"
}

// workspacePackageVersions returns the packages the workspace members
// publish, by name, with the version each declares.
func workspacePackageVersions(wsRoot string) map[string]string {
	versions := map[string]string{}
	for _, dir := range workspaceMemberDirs(wsRoot) {
		data, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			continue
		}
		var pkg struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if json.Unmarshal(data, &pkg) == nil && pkg.Name != "" {
			versions[pkg.Name] = pkg.Version
		}
	}
	return versions
}

// setDependency writes name: spec into the field of m.
func setDependency(m *manifestFile, field, name, spec string) {
	deps := catalog.UnmarshalStringMap(m.pkg[field])
	if deps == nil {
		deps = map[string]string{}
	}
	deps[name] = spec
	raw, _ := marshalManifestValue(deps)
	m.pkg[field] = raw
	m.changed = true
}

// resolve turns the version a spec names into the one the job writes: a
// dist-tag, latest when the spec names none, becomes ^ and the version the
// registry names for it; any other version, range or protocol is kept as
// written.
func (s *depsEditState) resolve(name, version string) (string, error) {
	if version == "" {
		version = depsEditLatestTag
	}
	if exact := exactNPMVersion(version); exact != "" {
		return exact, nil
	}
	if !npmDistTag.MatchString(version) || version == "x" || version == "X" || npmVersionPrefix.MatchString(version) {
		return version, nil
	}
	if !s.credentialRefreshed {
		refreshPutnamiNpmCredential(s.ctx, s.emit)
		s.credentialRefreshed = true
	}
	resolved, err := lookupDepsEditVersion(s.ctx, name, version)
	if err != nil {
		reportRegistryRefusal(s.emit, err.Error())
		return "", fmt.Errorf("deps add cannot resolve %s@%s: %w", name, version, err)
	}
	return "^" + resolved, nil
}

// catalogHolders returns the catalogs that hold name: the default one first,
// then the named ones in name order.
func (s *depsEditState) catalogHolders(name string) []string {
	var defaults map[string]string
	var named map[string]map[string]string
	if s.bunCatalog != nil {
		defaults, named = s.bunCatalog.defaultCat, s.bunCatalog.named
	} else {
		cats, _ := pnpmworkspace.ReadCatalogs(s.pnpmText)
		defaults, named = cats.Default, cats.Named
	}
	var holders []string
	if _, ok := defaults[name]; ok {
		holders = append(holders, "")
	}
	var names []string
	for catalogName, entries := range named {
		if _, ok := entries[name]; ok {
			names = append(names, catalogName)
		}
	}
	sort.Strings(names)
	return append(holders, names...)
}

func (s *depsEditState) catalogLookup(catalogName, name string) (string, bool) {
	if s.bunCatalog != nil {
		return s.bunCatalog.lookupIn(catalogName, name)
	}
	cats, _ := pnpmworkspace.ReadCatalogs(s.pnpmText)
	if catalogName == "" {
		v, ok := cats.Default[name]
		return v, ok
	}
	v, ok := cats.Named[catalogName][name]
	return v, ok
}

func (s *depsEditState) catalogPut(catalogName, name, version string) error {
	if s.bunCatalog != nil {
		s.bunCatalog.addToCatalog(catalogName, name, version)
		return s.markRootChanged()
	}
	text, found, err := pnpmworkspace.SetCatalogEntry(s.pnpmText, catalogName, name, version)
	if err != nil {
		return err
	}
	if !found {
		if text, err = pnpmworkspace.AddCatalogEntries(s.pnpmText, []pnpmworkspace.Seed{{Catalog: catalogName, Name: name, Version: version}}); err != nil {
			return err
		}
	}
	s.pnpmText, s.pnpmChanged = text, true
	return nil
}

func (s *depsEditState) catalogRemove(catalogName, name string) error {
	if s.bunCatalog != nil {
		if catalogName == "" {
			delete(s.bunCatalog.defaultCat, name)
			s.emptiedDefault = len(s.bunCatalog.defaultCat) == 0
		} else {
			delete(s.bunCatalog.named[catalogName], name)
			if len(s.bunCatalog.named[catalogName]) == 0 {
				delete(s.bunCatalog.named, catalogName)
				s.emptiedNamed = len(s.bunCatalog.named) == 0
			}
		}
		return s.markRootChanged()
	}
	text, found, err := pnpmworkspace.RemoveCatalogEntry(s.pnpmText, catalogName, name)
	if err != nil {
		return err
	}
	if found {
		s.pnpmText, s.pnpmChanged = text, true
	}
	return nil
}

func (s *depsEditState) markRootChanged() error {
	root, err := s.manifest(filepath.Join(s.ctx.WorkspaceRoot, "package.json"))
	if err != nil {
		return err
	}
	root.changed = true
	return nil
}

// catalogFile names the file that holds the catalog.
func (s *depsEditState) catalogFile() string {
	if s.bunCatalog != nil {
		return "package.json"
	}
	return pnpmworkspace.File
}

// catalogLabel names a catalog in a report line.
func catalogLabel(file, catalogName string) string {
	if catalogName == "" {
		return file + " catalog"
	}
	return file + " catalogs." + catalogName
}

// dropUnreferencedCatalogEntries removes each catalog entry a remove or prune
// stopped referencing when no workspace package references it any more.
func (s *depsEditState) dropUnreferencedCatalogEntries() error {
	if len(s.removed) == 0 || !s.readsCatalog() {
		return nil
	}
	referenced := map[[2]string]bool{}
	paths := []string{filepath.Join(s.ctx.WorkspaceRoot, "package.json")}
	for _, dir := range workspaceMemberDirs(s.ctx.WorkspaceRoot) {
		paths = append(paths, filepath.Join(dir, "package.json"))
	}
	for i, path := range paths {
		m, err := s.manifest(path)
		if err != nil {
			continue
		}
		for _, field := range depsEditRemoveFields {
			for name, value := range catalog.UnmarshalStringMap(m.pkg[field]) {
				if c, ok := catalog.NameFromSpec(value); ok {
					referenced[[2]string{c, name}] = true
				}
			}
		}
		if i == 0 {
			// The root's overrides and resolutions may reference the catalog
			// too, in package.json and, for pnpm, in pnpm-workspace.yaml.
			var pnpm map[string]json.RawMessage
			_ = json.Unmarshal(m.pkg["pnpm"], &pnpm)
			for _, raw := range []json.RawMessage{m.pkg["overrides"], m.pkg["resolutions"], pnpm["overrides"]} {
				collectCatalogReferences(raw, "", referenced)
			}
		}
	}
	if s.manager.Name == managerPNPM {
		overrides, err := pnpmworkspace.Mapping(s.pnpmText, "overrides")
		if err != nil {
			// Unreadable overrides may reference any entry: drop none.
			s.note("pnpm-workspace.yaml overrides are unreadable, so no catalog entry is dropped: " + err.Error())
			return nil
		}
		for name, value := range overrides {
			if c, ok := catalog.NameFromSpec(value); ok {
				for _, target := range overrideTargets(name) {
					referenced[[2]string{c, target}] = true
				}
			}
		}
	}
	keys := make([][2]string, 0, len(s.removed))
	for key := range s.removed {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, key := range keys {
		if referenced[key] {
			continue
		}
		entry, held := s.catalogLookup(key[0], key[1])
		if !held {
			continue
		}
		if err := s.catalogRemove(key[0], key[1]); err != nil {
			return err
		}
		s.note(fmt.Sprintf("%s %s: %s → - (no package references it)", catalogLabel(s.catalogFile(), key[0]), key[1], entry))
	}
	return nil
}

// collectCatalogReferences records every catalog: value of an overrides or
// resolutions object, which may nest, under the package its key names.
func collectCatalogReferences(raw json.RawMessage, key string, referenced map[[2]string]bool) {
	if len(raw) == 0 {
		return
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		if c, ok := catalog.NameFromSpec(value); ok && key != "" {
			for _, target := range overrideTargets(key) {
				referenced[[2]string{c, target}] = true
			}
		}
		return
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return
	}
	for k, v := range object {
		if k == "." {
			k = key
		}
		collectCatalogReferences(v, k, referenced)
	}
}

// overrideTargets are the packages an override or resolution key may name:
// the last package of a path such as a>b (pnpm), a/b or **/b (Yarn), without
// a version. A key whose split is ambiguous, such as @scope/a, yields every
// reading, so a catalog entry it may reference is never dropped.
func overrideTargets(key string) []string {
	if i := strings.LastIndex(key, ">"); i >= 0 {
		key = key[i+1:]
	}
	if i := strings.LastIndex(key, "**/"); i >= 0 {
		key = key[i+3:]
	}
	// A version follows the last @ that does not start a scope.
	name := key
	if i := strings.LastIndex(key, "@"); i > 0 && key[i-1] != '/' {
		name = key[:i]
	}
	targets := []string{name}
	parts := strings.Split(name, "/")
	if n := len(parts); n >= 2 {
		targets = append(targets, parts[n-1])
		if strings.HasPrefix(parts[n-2], "@") {
			targets = append(targets, parts[n-2]+"/"+parts[n-1])
		}
		if n >= 3 && strings.HasPrefix(parts[n-3], "@") {
			targets = append(targets, parts[n-3]+"/"+parts[n-2]+"/"+parts[n-1])
		}
	}
	return targets
}

// changedFiles lists the files the edit writes, by absolute path.
func (s *depsEditState) changedFiles() []string {
	var paths []string
	for path, m := range s.manifests {
		if m.changed {
			paths = append(paths, path)
		}
	}
	if s.pnpmChanged {
		paths = append(paths, pnpmWorkspaceFile(s.ctx.WorkspaceRoot))
	}
	sort.Strings(paths)
	return paths
}

// write writes every changed file.
func (s *depsEditState) write() error {
	root := filepath.Join(s.ctx.WorkspaceRoot, "package.json")
	if s.bunCatalog != nil {
		if m := s.manifests[root]; m != nil && m.changed {
			if err := s.bunCatalog.writeBack(m.pkg); err != nil {
				return err
			}
			if err := s.dropEmptiedCatalogs(m.pkg); err != nil {
				return err
			}
		}
	}
	for _, path := range s.changedFiles() {
		if m, ok := s.manifests[path]; ok {
			if err := writeRootManifest(path, m.pkg, m.order); err != nil {
				return err
			}
			continue
		}
		if err := os.WriteFile(path, []byte(s.pnpmText), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", pnpmworkspace.File, err)
		}
	}
	return nil
}

// dropEmptiedCatalogs removes the catalog keys a remove left empty, at the
// placement the catalog was read from.
func (s *depsEditState) dropEmptiedCatalogs(pkg map[string]json.RawMessage) error {
	c := s.bunCatalog
	drop := func(key string, atTop bool) error {
		if atTop {
			delete(pkg, key)
			return nil
		}
		if c.wsObj == nil {
			return nil
		}
		delete(c.wsObj, key)
		raw, err := marshalManifestValue(c.wsObj)
		if err != nil {
			return fmt.Errorf("marshal workspaces: %w", err)
		}
		pkg["workspaces"] = raw
		return nil
	}
	if s.emptiedDefault && len(c.defaultCat) == 0 {
		if err := drop("catalog", c.defaultAtTop); err != nil {
			return err
		}
	}
	if s.emptiedNamed && len(c.named) == 0 {
		return drop("catalogs", c.namedAtTop)
	}
	return nil
}

// note logs one planned change and keeps it for the summary.
func (s *depsEditState) note(line string) {
	s.emit.Log("info", line)
	s.report = append(s.report, line)
}
