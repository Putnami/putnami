package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"unicode"

	"go.putnami.dev/migration"
	protocaps "go.putnami.dev/protocol/capabilities"
	protocfg "go.putnami.dev/protocol/config"
	protoinfra "go.putnami.dev/protocol/infra"
)

// generatedCapabilityPackage is one entry of the scheduler stamp written to
// .gen/version.json (tooling/cli/internal/jobs.CapabilityPackageStamp).
//
// The stamp covers WORKSPACE projects only — the CLI reaches them through
// putnami.json dependencies — so an inventory entry always describes source the
// scheduler could bind. A contributor that lives in a published module the
// workload consumes has no such entry and never will; capability_modules.go
// resolves it from this binary's own module graph and marks the synthetic entry
// external, which is the only state in which SourceRoot and EvidencePath are
// legitimately empty.
//
// SourceBindingUnavailable with an empty SourceBinding is the scheduler saying
// Git does not manage the workspace root: the build makes no source claim. The
// manifest is the one a bound stamp produces, byte for byte, and no feature
// evidence is written. The marker is a fact about the root, so every entry of
// one stamp carries it or none does.
type generatedCapabilityPackage struct {
	Package                  string `json:"package"`
	Version                  string `json:"version"`
	EvidencePath             string `json:"evidencePath"`
	SourceRoot               string `json:"sourceRoot"`
	SourceBinding            string `json:"sourceBinding"`
	SourceBindingUnavailable bool   `json:"sourceBindingUnavailable,omitempty"`
	CapabilityManifestPath   string `json:"capabilityManifestPath,omitempty"`

	// external marks a synthetic entry for a published module dependency. It is
	// unexported so it can never be decoded from a stamp: only this package
	// mints external entries, and only from the linked build info.
	external bool
	// externalImportPath is the module path source imports this published owner
	// by, kept only when a versioned `replace` makes it differ from Package.
	// Vendored source is filed under it, so resolving a vendored declaration
	// needs the requirement as well as the module that replaced it.
	externalImportPath string
}

type capabilityInventory struct {
	project  string
	packages map[string]generatedCapabilityPackage
	ordered  []generatedCapabilityPackage
	// modules is the published module graph of this binary, snapshotted once so
	// every lookup in one describe run answers from the same table.
	modules []publishedModule
}

// legacyCapabilitySchedulerStampShape accepts only source-unbound scheduler
// fields. Present empty, null, and unknown fields fail recognition.
func legacyCapabilitySchedulerStampShape(data []byte) bool {
	var stamp struct {
		CapabilityPackages []map[string]json.RawMessage `json:"capabilityPackages"`
	}
	if err := json.Unmarshal(data, &stamp); err != nil || len(stamp.CapabilityPackages) == 0 {
		return false
	}
	for _, pkg := range stamp.CapabilityPackages {
		for key, raw := range pkg {
			switch key {
			case "package", "version", "evidencePath", "capabilityManifestPath":
				var value string
				if err := json.Unmarshal(raw, &value); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) ||
					(key == "capabilityManifestPath" && strings.TrimSpace(value) == "") {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

// legacyCapabilitySchedulerMetadata recognizes a complete source-unbound
// scheduler stamp. Partial bindings, malformed or duplicate package identities,
// and a missing workload package fail recognition.
func legacyCapabilitySchedulerMetadata(project string, packages []generatedCapabilityPackage) bool {
	if strings.TrimSpace(project) == "" || len(packages) == 0 {
		return false
	}
	seen := make(map[string]bool, len(packages))
	workloadPresent := false
	for _, pkg := range packages {
		if pkg.SourceRoot != "" || pkg.SourceBinding != "" || pkg.SourceBindingUnavailable {
			return false
		}
		if strings.TrimSpace(pkg.Package) == "" || !isResolvedCapabilityVersion(pkg.Version) ||
			!canonicalWorkspaceRelativePath(pkg.EvidencePath, false) || seen[pkg.Package] {
			return false
		}
		seen[pkg.Package] = true
		workloadPresent = workloadPresent || pkg.Package == project
	}
	return workloadPresent
}

func newCapabilityInventory(project string, packages []generatedCapabilityPackage) (*capabilityInventory, error) {
	inv := &capabilityInventory{
		project:  project,
		packages: make(map[string]generatedCapabilityPackage),
		modules:  capabilityPublishedModules(),
	}
	if packages != nil && len(packages) == 0 {
		return nil, fmt.Errorf("capabilityPackages must be non-empty when scheduler metadata is present")
	}
	unavailable := 0
	for index, pkg := range packages {
		if strings.TrimSpace(pkg.Package) == "" || strings.TrimSpace(pkg.Version) == "" || strings.TrimSpace(pkg.EvidencePath) == "" || strings.TrimSpace(pkg.SourceRoot) == "" {
			return nil, fmt.Errorf("capabilityPackages[%d] is incomplete or malformed", index)
		}
		if !canonicalWorkspaceRelativePath(pkg.SourceRoot, true) || !canonicalWorkspaceRelativePath(pkg.EvidencePath, false) {
			return nil, fmt.Errorf("capabilityPackages[%d] sourceRoot/evidencePath is not a canonical contained workspace-relative path", index)
		}
		switch {
		case pkg.SourceBindingUnavailable && pkg.SourceBinding != "":
			return nil, fmt.Errorf("capability package %q carries a source-v1 binding it marks unavailable", pkg.Package)
		case pkg.SourceBindingUnavailable:
			unavailable++
		case !validCapabilitySourceBinding(pkg.SourceBinding):
			return nil, fmt.Errorf("capability package %q has no available source-v1 binding", pkg.Package)
		}
		if !isResolvedCapabilityVersion(pkg.Version) {
			return nil, fmt.Errorf("capability package %q has unresolved version %q", pkg.Package, pkg.Version)
		}
		pkg.EvidencePath = filepath.ToSlash(pkg.EvidencePath)
		pkg.SourceRoot = filepath.ToSlash(pkg.SourceRoot)
		pkg.CapabilityManifestPath = filepath.ToSlash(pkg.CapabilityManifestPath)
		if existing, ok := inv.packages[pkg.Package]; ok {
			if existing != pkg {
				return nil, fmt.Errorf("capability package %q has conflicting scheduler metadata", pkg.Package)
			}
			continue
		}
		inv.packages[pkg.Package] = pkg
	}
	if unavailable != 0 && unavailable != len(packages) {
		return nil, fmt.Errorf("capabilityPackages mixes available and unavailable source-v1 bindings")
	}
	if packages != nil {
		if _, ok := inv.packages[project]; !ok {
			return nil, fmt.Errorf("capabilityPackages is missing the workload package %q", project)
		}
	}
	for _, pkg := range inv.packages {
		inv.ordered = append(inv.ordered, pkg)
	}
	sort.Slice(inv.ordered, func(i, j int) bool { return inv.ordered[i].Package < inv.ordered[j].Package })
	return inv, nil
}

func canonicalWorkspaceRelativePath(value string, allowRoot bool) bool {
	if value == "." && allowRoot {
		return true
	}
	first := strings.SplitN(value, "/", 2)[0]
	if value == "" || strings.Contains(value, "\\") || strings.Contains(first, ":") || filepath.IsAbs(value) || filepath.ToSlash(filepath.Clean(value)) != value {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validCapabilitySourceBinding(binding string) bool {
	const prefix = "source-v1:sha256:"
	if len(binding) != len(prefix)+64 || !strings.HasPrefix(binding, prefix) {
		return false
	}
	for _, value := range binding[len(prefix):] {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return false
		}
	}
	return true
}

func isResolvedCapabilityVersion(version string) bool {
	value := strings.TrimSpace(version)
	return value != "" && !strings.HasPrefix(value, "workspace:") && !strings.ContainsAny(value, "~^*<>=|")
}

func (inv *capabilityInventory) ownerFor(value any) (generatedCapabilityPackage, error) {
	if hint, ok := value.(interface{ capabilityDeclarationHint() (string, string, bool) }); ok {
		if file, _, present := hint.capabilityDeclarationHint(); present {
			if owner, err := inv.ownerForSourceFile(file); err == nil {
				return owner, nil
			}
		}
	}
	packageName := pkgPathOf(value)
	if packageName == "" {
		packageName = inv.project
	}
	if metadata, ok := inv.packages[packageName]; ok {
		return metadata, nil
	}
	var matches []generatedCapabilityPackage
	for name, metadata := range inv.packages {
		if strings.HasPrefix(packageName, name+"/") {
			matches = append(matches, metadata)
		}
	}
	if len(matches) == 0 {
		if owner, ok := inv.externalOwnerForPackage(packageName); ok {
			return owner, nil
		}
		return generatedCapabilityPackage{}, fmt.Errorf("contributor package %q has no scheduler-stamped owner project and is not provided by a published module of this build", packageName)
	}
	sort.Slice(matches, func(i, j int) bool { return len(matches[i].Package) > len(matches[j].Package) })
	return matches[0], nil
}

func (inv *capabilityInventory) ownerForMethod(value any, methodName string) (generatedCapabilityPackage, error) {
	file, _, err := contributionMethodSource(value, methodName)
	if err == nil {
		if owner, ownerErr := inv.ownerForSourceFile(file); ownerErr == nil {
			return owner, nil
		}
	}
	return inv.ownerFor(value)
}

func (inv *capabilityInventory) ownerForSourceFile(file string) (generatedCapabilityPackage, error) {
	// A file compiled from a published module belongs to that module, and to
	// nothing the scheduler stamped. Resolve it before any stamped source root
	// gets a chance: a single-project workspace stamps sourceRoot "." and would
	// otherwise walk up from the module cache, find the dependency's own go.mod,
	// and claim the file for the workload.
	if owner, ok := inv.externalOwnerForFile(file); ok {
		return owner, nil
	}
	var matches []generatedCapabilityPackage
	for _, metadata := range inv.packages {
		if _, err := declarationPathForPackage(file, metadata); err == nil {
			matches = append(matches, metadata)
		}
	}
	if len(matches) == 0 {
		return generatedCapabilityPackage{}, fmt.Errorf("source file %q has no scheduler-stamped producer", file)
	}
	sort.Slice(matches, func(i, j int) bool {
		if len(matches[i].Package) != len(matches[j].Package) {
			return len(matches[i].Package) > len(matches[j].Package)
		}
		return len(matches[i].SourceRoot) > len(matches[j].SourceRoot)
	})
	return matches[0], nil
}

func (inv *capabilityInventory) identity(kind protocaps.ContributionKind, subkind, key string) (protocaps.ContributionIdentity, error) {
	if _, ok := inv.packages[inv.project]; !ok {
		return protocaps.ContributionIdentity{}, fmt.Errorf("contribution (%q,%q,%q) cannot use workload %q because it has no scheduler stamp", kind, subkind, key, inv.project)
	}
	return protocaps.ContributionIdentity{OwnerProject: inv.project, Kind: kind, Subkind: subkind, Key: key}, nil
}

func (inv *capabilityInventory) provenance(value any, identity protocaps.ContributionIdentity, methodName string) (protocaps.ProvenanceV2, error) {
	_, ok := inv.packages[identity.OwnerProject]
	if !ok {
		return protocaps.ProvenanceV2{}, fmt.Errorf("contribution %s owner has no scheduler-stamped source root", capabilityIdentityLabel(identity))
	}
	producer, err := inv.ownerForMethod(value, methodName)
	if err != nil {
		return protocaps.ProvenanceV2{}, fmt.Errorf("resolve concrete producer for %s: %w", capabilityIdentityLabel(identity), err)
	}
	declaration, producerPackage, err := automaticContributionDeclaration(value, methodName, identity.OwnerProject, producer)
	if err != nil {
		return protocaps.ProvenanceV2{}, fmt.Errorf("automatic declaration for contribution %s failed: %w", capabilityIdentityLabel(identity), err)
	}
	return protocaps.ProvenanceV2{
		Project:     identity.OwnerProject,
		Package:     producerPackage,
		SourceKind:  protocaps.SourceKindFramework,
		Declaration: declaration,
	}, nil
}

func (inv *capabilityInventory) ownerForPackageName(packageName string) (generatedCapabilityPackage, error) {
	if metadata, ok := inv.packages[packageName]; ok {
		return metadata, nil
	}
	var matches []generatedCapabilityPackage
	for name, metadata := range inv.packages {
		if strings.HasPrefix(packageName, name+"/") {
			matches = append(matches, metadata)
		}
	}
	if len(matches) == 0 {
		if owner, ok := inv.externalOwnerForPackage(packageName); ok {
			return owner, nil
		}
		return generatedCapabilityPackage{}, fmt.Errorf("producer package %q has no scheduler-stamped source root and is not provided by a published module of this build", packageName)
	}
	sort.Slice(matches, func(i, j int) bool { return len(matches[i].Package) > len(matches[j].Package) })
	return matches[0], nil
}

func automaticContributionDeclaration(value any, methodName, semanticOwner string, owner generatedCapabilityPackage) (protocaps.DeclarationLocation, string, error) {
	file, symbol, err := contributionMethodSource(value, methodName)
	if err != nil {
		return protocaps.DeclarationLocation{}, "", err
	}
	path, err := declarationPathForPackage(file, owner)
	if err != nil {
		return protocaps.DeclarationLocation{}, "", err
	}
	root := protocaps.LocationRootProject
	if owner.Package != semanticOwner {
		root = protocaps.LocationRootPackage
	}
	return protocaps.DeclarationLocation{Root: root, Path: path, Symbol: symbol}, owner.Package, nil
}

func contributionMethodSource(value any, methodName string) (string, string, error) {
	if hint, ok := value.(interface{ capabilityDeclarationHint() (string, string, bool) }); ok {
		if file, symbol, present := hint.capabilityDeclarationHint(); present {
			return file, symbol, nil
		}
	}
	typeOf := reflect.TypeOf(value)
	if typeOf == nil {
		return "", "", fmt.Errorf("nil contributor")
	}
	method, ok := typeOf.MethodByName(methodName)
	if !ok && typeOf.Kind() != reflect.Pointer {
		method, ok = reflect.PointerTo(typeOf).MethodByName(methodName)
	}
	if !ok {
		return "", "", fmt.Errorf("contributor %T has no %s method", value, methodName)
	}
	fn := runtime.FuncForPC(method.Func.Pointer())
	if fn == nil {
		return "", "", fmt.Errorf("resolve %T.%s source", value, methodName)
	}
	file, _ := fn.FileLine(method.Func.Pointer())
	return file, fn.Name(), nil
}

func declarationPathForPackage(file string, owner generatedCapabilityPackage) (string, error) {
	if owner.external {
		// The stamped source roots below are workspace-relative; a published
		// module is addressed relative to its own module root instead, which is
		// the same reduction on every machine and in every build mode.
		rel, ok := moduleRelativeSourcePath(file, publishedModule{Path: owner.Package, Version: owner.Version, ImportPath: owner.externalImportPath})
		if !ok {
			return "", fmt.Errorf("source file %q is not contained by published module %s@%s", file, owner.Package, owner.Version)
		}
		return rel, nil
	}
	file = filepath.ToSlash(filepath.Clean(file))
	if file == owner.Package {
		return "", fmt.Errorf("source file %q has no declaration path", file)
	}
	if strings.HasPrefix(file, owner.Package+"/") {
		rel := strings.TrimPrefix(file, owner.Package+"/")
		if canonicalWorkspaceRelativePath(rel, false) {
			return rel, nil
		}
	}
	sourceRoot := strings.Trim(filepath.ToSlash(filepath.Clean(owner.SourceRoot)), "/")
	if sourceRoot == "." {
		// A single-project repository has no workspace-relative prefix. Walk up
		// from the exact method source to its closest stamped descriptor.
		for dir := filepath.Dir(filepath.FromSlash(file)); ; dir = filepath.Dir(dir) {
			for _, descriptor := range []string{"go.mod", "package.json", "pyproject.toml", "putnami.json"} {
				if _, err := os.Stat(filepath.Join(dir, descriptor)); err == nil {
					rel, relErr := filepath.Rel(dir, filepath.FromSlash(file))
					if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						return filepath.ToSlash(rel), nil
					}
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
		return "", fmt.Errorf("source file %q is not contained by a project descriptor root", file)
	}
	marker := "/" + sourceRoot + "/"
	index := strings.LastIndex(file, marker)
	if index < 0 {
		return "", fmt.Errorf("source file %q is not contained by stamped source root %q", file, sourceRoot)
	}
	rel := file[index+len(marker):]
	if !canonicalWorkspaceRelativePath(rel, false) {
		return "", fmt.Errorf("derived declaration path %q is not canonical", rel)
	}
	return rel, nil
}

// makesNoSourceClaim reports whether the scheduler stamped owner with an
// unavailable binding. Evidence binds a record to source, so there is none to
// write for it.
func (inv *capabilityInventory) makesNoSourceClaim(owner string) bool {
	return inv.packages[owner].SourceBindingUnavailable
}

func (inv *capabilityInventory) bindingForOwner(owner string) (string, error) {
	metadata, ok := inv.packages[owner]
	if !ok || !validCapabilitySourceBinding(metadata.SourceBinding) {
		return "", fmt.Errorf("owner project %q has no available source-v1 binding", owner)
	}
	return metadata.SourceBinding, nil
}

func (inv *capabilityInventory) local(identity protocaps.ContributionIdentity) bool {
	return identity.OwnerProject == inv.project
}

// packageContributions emits one package contribution per stamped package that
// is part of manifest's capability surface: the workload itself and the packages
// that actually declared one of the contributions already collected into
// manifest.
//
// It is deliberately NOT the scheduler's full package inventory. That inventory
// is the workload's reachable module closure, so emitting it made this
// project's COMMITTED manifest a function of the whole dependency graph: adding
// a protocol dependency to any transitively-reachable project re-stamped every
// app's schema/capabilities.json, surfacing on CI as an unrelated project's
// "worktree mutated (gate stage)" failure. The closure remains available
// as build evidence in the scheduler stamp (.gen/version.json
// capabilityPackages), which is never committed.
//
// The protocol owns the surface rule (protocaps.CapabilitySurfaceV2) and
// canonical emission enforces it, so this filter is the producer honoring the
// same contract rather than a second, drifting definition of it.
func (inv *capabilityInventory) packageContributions(manifest *protocaps.ManifestV2) ([]protocaps.PackageV2, error) {
	surface := protocaps.CapabilitySurfaceV2(manifest)
	packages := make([]protocaps.PackageV2, 0, len(inv.ordered))
	for _, pkg := range inv.ordered {
		if !surface[pkg.Package] {
			continue
		}
		declaration, err := filepath.Rel(filepath.FromSlash(pkg.SourceRoot), filepath.FromSlash(pkg.EvidencePath))
		if err != nil || declaration == ".." || strings.HasPrefix(declaration, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("capability package %q descriptor is outside its source root", pkg.Package)
		}
		packages = append(packages, protocaps.PackageV2{
			Identity: protocaps.ContributionIdentity{OwnerProject: pkg.Package, Kind: protocaps.ContributionKindPackage, Key: pkg.Package},
			Package:  pkg.Package,
			Provenance: protocaps.ProvenanceV2{
				Project: pkg.Package, Package: pkg.Package,
				SourceKind:  protocaps.SourceKindFramework,
				Declaration: protocaps.DeclarationLocation{Root: protocaps.LocationRootProject, Path: filepath.ToSlash(declaration)},
			},
		})
	}
	return packages, nil
}

func (a *Application) collectCapabilitySchemas(outputDir string, inv *capabilityInventory) ([]protocaps.SchemaContributionV2, error) {
	contributors := Collect[CapabilityInventoryContributor](a.Module)
	var schemas []protocaps.SchemaContributionV2
	for _, contributor := range contributors {
		for _, contribution := range contributor.CapabilitySchemas() {
			if contribution.Kind != protocaps.SchemaKindRoute {
				artifactPath := filepath.Join(outputDir, filepath.FromSlash(contribution.Path))
				if info, err := os.Stat(artifactPath); err != nil || info.IsDir() {
					continue
				}
			}
			identity, err := inv.identity(protocaps.ContributionKindSchema, string(contribution.Kind), contribution.Name)
			if err != nil {
				return nil, err
			}
			if !inv.local(identity) {
				continue
			}
			provenance, err := inv.provenance(contributor, identity, "CapabilitySchemas")
			if err != nil {
				return nil, err
			}
			// A non-route schema path is already the native declaration of its
			// generated artifact. Preserve it in v2 provenance automatically.
			if contribution.Kind != protocaps.SchemaKindRoute && contribution.Path != "" {
				artifact := protocaps.ArtifactLocation{Root: protocaps.LocationRootProject, Path: filepath.ToSlash(contribution.Path)}
				present := false
				for _, existing := range provenance.Artifacts {
					if existing.Root == artifact.Root && existing.Path == artifact.Path {
						present = true
						break
					}
				}
				if !present {
					provenance.Artifacts = append(provenance.Artifacts, artifact)
				}
			}
			schemas = append(schemas, protocaps.SchemaContributionV2{
				Identity: identity, Name: contribution.Name, Kind: contribution.Kind, Path: filepath.ToSlash(contribution.Path), Provenance: provenance,
			})
		}
	}
	return schemas, nil
}

func (a *Application) collectCapabilityDiscoverers(inv *capabilityInventory) ([]protocaps.DiscovererV2, error) {
	var discoverers []protocaps.DiscovererV2
	for _, contributor := range Collect[CapabilityInventoryContributor](a.Module) {
		for _, contribution := range contributor.CapabilityDiscoverers() {
			identity, err := inv.identity(protocaps.ContributionKindDiscoverer, string(contribution.Kind), contribution.Name)
			if err != nil {
				return nil, err
			}
			if !inv.local(identity) {
				continue
			}
			provenance, err := inv.provenance(contributor, identity, "CapabilityDiscoverers")
			if err != nil {
				return nil, err
			}
			discoverers = append(discoverers, protocaps.DiscovererV2{
				Identity: identity, Name: contribution.Name, Kind: contribution.Kind, Provenance: provenance,
			})
		}
	}
	for _, contributor := range Collect[ConfigContributor](a.Module) {
		for _, definition := range contributor.ConfigDefinitions() {
			identity, err := inv.identity(protocaps.ContributionKindDiscoverer, string(protocaps.DiscovererKindConfig), definition.Path)
			if err != nil {
				return nil, err
			}
			if !inv.local(identity) {
				continue
			}
			provenance, err := inv.provenance(contributor, identity, "ConfigDefinitions")
			if err != nil {
				return nil, err
			}
			discoverers = append(discoverers, protocaps.DiscovererV2{
				Identity: identity, Name: definition.Path, Kind: protocaps.DiscovererKindConfig, Provenance: provenance,
			})
		}
	}
	if a.migrationRegistry != nil {
		// A SQL migration discoverer identifies the source namespace, not an
		// individual SQLSource value. SQL sources can merge multiple
		// contributions for one (kind, namespace, datasource); emit their
		// single discoverer representation once so validation does not
		// mistake compatible source fragments for duplicate providers. Custom
		// migration kinds retain one discoverer per source for validation.
		seenMigrationDiscoverers := make(map[string]struct{})
		associations, err := a.capabilityMigrationSources()
		if err != nil {
			return nil, err
		}
		for _, association := range associations {
			source, kind := association.source, association.source.Kind()
			name := string(kind) + ":" + source.Namespace()
			if kind == migration.KindSQL {
				if _, ok := seenMigrationDiscoverers[name]; ok {
					continue
				}
				seenMigrationDiscoverers[name] = struct{}{}
			}
			identity, err := inv.identity(protocaps.ContributionKindDiscoverer, string(protocaps.DiscovererKindSource), name)
			if err != nil {
				return nil, err
			}
			if !inv.local(identity) {
				continue
			}
			provenance, err := inv.provenance(association.contributor, identity, "MigrationSources")
			if err != nil {
				return nil, err
			}
			discoverers = append(discoverers, protocaps.DiscovererV2{
				Identity: identity, Name: name,
				Kind: protocaps.DiscovererKindSource, Provenance: provenance,
			})
		}
	}
	return discoverers, nil
}

func (a *Application) collectCapabilityInfraSidecars(outputDir string, inv *capabilityInventory) ([]protocaps.InfraRequirementV2, error) {
	paths, err := filepath.Glob(filepath.Join(outputDir, protoinfra.PerProjectManifestDir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("discover capability infra sidecars: %w", err)
	}
	sort.Strings(paths)
	var requirements []protocaps.InfraRequirementV2
	seen := make(map[protocaps.ContributionIdentity]protocaps.ProvenanceV2)
	seenContainer := make(map[protocaps.ContributionIdentity]string)
	describers := make(map[string]Describer)
	for _, describer := range a.describers() {
		if _, duplicate := describers[describer.Name()]; duplicate {
			return nil, fmt.Errorf("infra sidecar producer name %q is ambiguous", describer.Name())
		}
		describers[describer.Name()] = describer
	}
	migrationProducers := make(map[string]map[string]MigrationContributor)
	for _, association := range a.capabilityMigrationAssociations {
		if schemaContributor, ok := association.source.(migration.SchemaContributor); ok {
			for _, database := range schemaContributor.InfraDatabases() {
				key := capabilityDeclarationKey(association.contributor, "MigrationSources")
				if migrationProducers[database.Name] == nil {
					migrationProducers[database.Name] = make(map[string]MigrationContributor)
				}
				migrationProducers[database.Name][key] = association.contributor
			}
		}
	}
	secretProducers := make(map[string]map[string]ConfigContributor)
	for _, contributor := range Collect[ConfigContributor](a.Module) {
		blocks, blockErr := buildConfigBlocks(contributor.ConfigDefinitions())
		if blockErr != nil {
			return nil, blockErr
		}
		for _, block := range blocks {
			for _, field := range block.Fields {
				walkCapabilitySensitiveField(block.Path, field, func(secret string) {
					key := capabilityDeclarationKey(contributor, "ConfigDefinitions")
					if secretProducers[secret] == nil {
						secretProducers[secret] = make(map[string]ConfigContributor)
					}
					secretProducers[secret][key] = contributor
				})
			}
		}
	}
	appendRequirement := func(container, name string, kind protocaps.InfraKind, producer any, method string) error {
		identity, err := inv.identity(protocaps.ContributionKindInfra, string(kind), name)
		if err != nil {
			return err
		}
		if !inv.local(identity) {
			return nil
		}
		provenance, err := inv.provenance(producer, identity, method)
		if err != nil {
			return err
		}
		if first, exists := seen[identity]; exists {
			firstWire, err := canonicalInfraRequirementWire(protocaps.InfraRequirementV2{
				Identity: identity, Name: name, Kind: kind, Provenance: first,
			})
			if err != nil {
				return fmt.Errorf("canonicalize existing infra contribution %s: %w", capabilityIdentityLabel(identity), err)
			}
			currentWire, err := canonicalInfraRequirementWire(protocaps.InfraRequirementV2{
				Identity: identity, Name: name, Kind: kind, Provenance: provenance,
			})
			if err != nil {
				return fmt.Errorf("canonicalize incoming infra contribution %s: %w", capabilityIdentityLabel(identity), err)
			}
			if bytes.Equal(firstWire, currentWire) {
				return nil
			}
			// A database adapter owns the infrastructure resource declaration.
			// Every other sidecar declaring the same database — migration source
			// metadata, or a workload's own describe-only sidecar that supplies
			// schemas without owning migrations (a read-only workload against a
			// datasource whose migrations another workload owns) — is a fallback,
			// not a second competing declaration of the same database. Two
			// non-adapter sidecars remain ambiguous: neither is canonical.
			if kind == protocaps.InfraKindDatabase {
				switch {
				case seenContainer[identity] == "database":
					return nil
				case container == "database":
					for index := range requirements {
						if requirements[index].Identity == identity {
							requirements[index].Provenance = provenance
							break
						}
					}
					seen[identity], seenContainer[identity] = provenance, container
					return nil
				}
			}
			return fmt.Errorf("infra contribution %s has ambiguous declarations in sidecars %q and %q", capabilityIdentityLabel(identity), seenContainer[identity], container)
		}
		seen[identity], seenContainer[identity] = provenance, container
		requirements = append(requirements, protocaps.InfraRequirementV2{Identity: identity, Name: name, Kind: kind, Provenance: provenance})
		return nil
	}
	for _, path := range paths {
		// The aggregator's workload runtime defaults live in the same directory
		// (`.gen/infra/runtime.json`, README "Workload runtime defaults") but are
		// runtime intent, not a library manifest — the strict per-project schema
		// rejects their `ingress`/`scaling` members. Same rule as
		// protoinfra's generated-manifest collection.
		if filepath.Base(path) == "runtime.json" {
			continue
		}
		slug := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		var producer any = describers[slug]
		method := "Describe"
		data, readErr := os.ReadFile(path) //nolint:gosec // path comes from the framework-owned output directory glob
		if readErr != nil {
			return nil, fmt.Errorf("read capability infra sidecar %s: %w", path, readErr)
		}
		manifest, diags := protoinfra.ParseAndValidatePerProjectManifest(data)
		if len(diags) > 0 {
			lines := make([]string, 0, len(diags))
			for _, diag := range diags {
				lines = append(lines, fmt.Sprintf("[%s] %s: %s", diag.Code, diag.Field, diag.Message))
			}
			return nil, fmt.Errorf("invalid capability infra sidecar %s: %s", path, strings.Join(lines, "; "))
		}
		for _, database := range manifest.Databases {
			value := producer
			valueMethod := method
			if slug == "migration" {
				// The migration sidecar is a FALLBACK declaration: when the
				// database adapter has already declared this database (the
				// "database" sidecar sorts before "migration", so it is always
				// seen first), the fallback's provenance is discarded by
				// appendRequirement — so a database fed by several migration
				// sources (namespaced schemas of one datasource) must not fail
				// producer resolution for a declaration that cannot win.
				identity, identityErr := inv.identity(protocaps.ContributionKindInfra, string(protocaps.InfraKindDatabase), database.Name)
				if identityErr != nil {
					return nil, identityErr
				}
				if seenContainer[identity] == "database" {
					continue
				}
				resolved, resolveErr := uniqueCapabilityProducer(migrationProducers[database.Name], "migration database", database.Name)
				if resolveErr != nil {
					return nil, resolveErr
				}
				value, valueMethod = resolved, "MigrationSources"
			}
			if err := appendRequirement(slug, database.Name, protocaps.InfraKindDatabase, value, valueMethod); err != nil {
				return nil, err
			}
		}
		if manifest.Events != nil {
			for _, topic := range manifest.Events.Publishes {
				if err := appendRequirement(slug, topic, protocaps.InfraKindEvents, producer, method); err != nil {
					return nil, err
				}
			}
			for _, subscription := range manifest.Events.Subscribes {
				if err := appendRequirement(slug, subscription.Topic, protocaps.InfraKindEvents, producer, method); err != nil {
					return nil, err
				}
			}
		}
		for _, storage := range manifest.Storage {
			if err := appendRequirement(slug, storage.Name, protocaps.InfraKindStorage, producer, method); err != nil {
				return nil, err
			}
		}
		for _, secret := range manifest.Secrets {
			value, valueMethod := producer, method
			if slug == "secrets" {
				resolved, resolveErr := uniqueCapabilityProducer(secretProducers[secret], "secret", secret)
				if resolveErr != nil {
					return nil, resolveErr
				}
				value, valueMethod = resolved, "ConfigDefinitions"
			}
			if err := appendRequirement(slug, secret, protocaps.InfraKindSecret, value, valueMethod); err != nil {
				return nil, err
			}
		}
		for _, job := range manifest.ScheduledJobs {
			if err := appendRequirement(slug, job.Name, protocaps.InfraKindScheduledJob, producer, method); err != nil {
				return nil, err
			}
		}
	}
	return requirements, nil
}

func canonicalInfraRequirementWire(requirement protocaps.InfraRequirementV2) ([]byte, error) {
	canonical := protocaps.CanonicalManifestV2(&protocaps.ManifestV2{
		InfraRequirements: []protocaps.InfraRequirementV2{requirement},
	})
	return json.Marshal(canonical.InfraRequirements[0])
}

func capabilityDeclarationKey(value any, method string) string {
	_, symbol, err := contributionMethodSource(value, method)
	if err != nil {
		return fmt.Sprintf("%T.%s", value, method)
	}
	producerPackage := pkgPathOf(value)
	if producerPackage == "" {
		producerPackage = fmt.Sprintf("%T", value)
	}
	return producerPackage + ":" + symbol
}

func uniqueCapabilityProducer[T any](values map[string]T, kind, name string) (T, error) {
	var zero T
	if len(values) == 0 {
		return zero, fmt.Errorf("%s %q has no precise producer declaration", kind, name)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 1 {
		return zero, fmt.Errorf("%s %q has ambiguous producer declarations: %s", kind, name, strings.Join(keys, ", "))
	}
	return values[keys[0]], nil
}

func walkCapabilitySensitiveField(prefix string, field protocfg.FieldSchema, emit func(string)) {
	path := prefix
	if field.Name != "" {
		if path == "" {
			path = field.Name
		} else {
			path += "." + field.Name
		}
	}
	if field.Sensitive {
		name := field.Env
		if name == "" {
			name = path
		}
		emit(canonicalCapabilitySecretName(name))
	}
	switch field.Type {
	case protocfg.FieldTypeObject:
		for _, nested := range field.Fields {
			walkCapabilitySensitiveField(path, nested, emit)
		}
	case protocfg.FieldTypeArray:
		if field.Items != nil {
			walkCapabilitySensitiveField(path, *field.Items, emit)
		}
	case protocfg.FieldTypeMap:
		if field.Values != nil {
			walkCapabilitySensitiveField(path, *field.Values, emit)
		}
	}
}

func canonicalCapabilitySecretName(value string) string {
	var split strings.Builder
	var previous rune
	for index, current := range value {
		if index > 0 && unicode.IsUpper(current) && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
			split.WriteByte('_')
		}
		split.WriteRune(current)
		previous = current
	}
	var canonical strings.Builder
	for _, current := range strings.ToLower(split.String()) {
		switch {
		case current >= 'a' && current <= 'z', current >= '0' && current <= '9', current == '_', current == '.', current == '/', current == '-':
			canonical.WriteRune(current)
		default:
			canonical.WriteByte('_')
		}
	}
	out := strings.TrimLeft(canonical.String(), "_./-")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func mergeDependencyCapabilityManifests(projectRoot, capabilityRoot string, target *protocaps.ManifestV2, inv *capabilityInventory) error {
	*target = *protocaps.CanonicalManifestV2(target)
	configDefinitions, err := newCapabilityMergeState(target.Project, &target.ConfigDefinitions, func(value protocaps.ConfigDefinitionV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		return err
	}
	schemas, err := newCapabilityMergeState(target.Project, &target.Schemas, func(value protocaps.SchemaContributionV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		return err
	}
	discoverers, err := newCapabilityMergeState(target.Project, &target.Discoverers, func(value protocaps.DiscovererV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		return err
	}
	migrations, err := newCapabilityMergeState(target.Project, &target.Migrations, func(value protocaps.MigrationBundleV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		return err
	}
	infraRequirements, err := newCapabilityMergeState(target.Project, &target.InfraRequirements, func(value protocaps.InfraRequirementV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		return err
	}
	healthContributors, err := newCapabilityMergeState(target.Project, &target.HealthContributors, func(value protocaps.HealthContributorV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		return err
	}
	lifecycleHooks, err := newCapabilityMergeState(target.Project, &target.LifecycleHooks, func(value protocaps.LifecycleHookV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		return err
	}
	packages, err := newCapabilityMergeState(target.Project, &target.Packages, func(value protocaps.PackageV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		return err
	}
	requiredCapabilities, err := newCapabilityMergeState(target.Project, &target.RequiredCapabilities, func(value protocaps.RequiredCapabilityV2) protocaps.ContributionIdentity { return value.Identity })
	if err != nil {
		return err
	}
	workspaceRoot := projectRoot
	if capabilityRoot != "" {
		workspaceRoot = filepath.Join(projectRoot, filepath.FromSlash(capabilityRoot))
	}
	absWorkspaceRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return fmt.Errorf("resolve capability workspace root: %w", err)
	}
	for _, pkg := range inv.ordered {
		if pkg.CapabilityManifestPath == "" {
			continue
		}
		path := pkg.CapabilityManifestPath
		if !filepath.IsAbs(path) {
			path = filepath.Join(projectRoot, filepath.FromSlash(path))
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolve dependency capability manifest %s: %w", pkg.Package, err)
		}
		relToRoot, err := filepath.Rel(absWorkspaceRoot, absPath)
		if err != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
			return fmt.Errorf("dependency capability manifest %s escapes the stamped workspace root", pkg.Package)
		}
		if filepath.ToSlash(filepath.Join(filepath.Dir(relToRoot), filepath.Base(relToRoot))) != filepath.ToSlash(relToRoot) ||
			filepath.Base(absPath) != protocaps.ManifestFilename || filepath.Base(filepath.Dir(absPath)) != "schema" {
			return fmt.Errorf("dependency capability manifest %s must target schema/%s", pkg.Package, protocaps.ManifestFilename)
		}
		_, err = os.Stat(absPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat dependency capability manifest %s: %w", pkg.Package, err)
		}
		realWorkspaceRoot, err := filepath.EvalSymlinks(absWorkspaceRoot)
		if err != nil {
			return fmt.Errorf("resolve stamped workspace root symlinks: %w", err)
		}
		realPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return fmt.Errorf("resolve dependency capability manifest symlinks %s: %w", pkg.Package, err)
		}
		realRel, err := filepath.Rel(realWorkspaceRoot, realPath)
		if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("dependency capability manifest %s escapes the stamped workspace root", pkg.Package)
		}
		data, err := os.ReadFile(absPath) //nolint:gosec // lexical and symlink-resolved paths are constrained under the scheduler-stamped workspace root
		if err != nil {
			return fmt.Errorf("read dependency capability manifest %s: %w", pkg.Package, err)
		}
		document, diags := protocaps.ParseAndValidateManifestDocument(data)
		if len(diags) > 0 {
			lines := make([]string, 0, len(diags))
			for _, diag := range diags {
				lines = append(lines, fmt.Sprintf("[%s] %s: %s", diag.Code, diag.Field, diag.Message))
			}
			return fmt.Errorf("invalid dependency capability manifest %s: %s", pkg.Package, strings.Join(lines, "; "))
		}
		if document.V2 == nil {
			return fmt.Errorf("dependency capability manifest %s uses protocol v1 and cannot be aggregated into v2 without precise identity, migration kind, and declaration", pkg.Package)
		}
		manifest := protocaps.CanonicalManifestV2(document.V2)
		if manifest.Project != pkg.Package {
			return fmt.Errorf("dependency capability manifest %s declares container project %q", pkg.Package, manifest.Project)
		}
		if err := validateDependencyProvenance(manifest, inv); err != nil {
			return fmt.Errorf("dependency capability manifest %s: %w", pkg.Package, err)
		}
		if err := configDefinitions.merge(pkg.Package, manifest.ConfigDefinitions); err != nil {
			return err
		}
		if err := schemas.merge(pkg.Package, manifest.Schemas); err != nil {
			return err
		}
		if err := discoverers.merge(pkg.Package, manifest.Discoverers); err != nil {
			return err
		}
		if err := migrations.merge(pkg.Package, manifest.Migrations); err != nil {
			return err
		}
		if err := infraRequirements.merge(pkg.Package, manifest.InfraRequirements); err != nil {
			return err
		}
		if err := healthContributors.merge(pkg.Package, manifest.HealthContributors); err != nil {
			return err
		}
		if err := lifecycleHooks.merge(pkg.Package, manifest.LifecycleHooks); err != nil {
			return err
		}
		if err := packages.merge(pkg.Package, manifest.Packages); err != nil {
			return err
		}
		if err := requiredCapabilities.merge(pkg.Package, manifest.RequiredCapabilities); err != nil {
			return err
		}
	}
	return nil
}

func validateDependencyProvenance(manifest *protocaps.ManifestV2, inv *capabilityInventory) error {
	check := func(identity protocaps.ContributionIdentity, provenance protocaps.ProvenanceV2) error {
		switch provenance.Declaration.Root {
		case protocaps.LocationRootProject:
			if _, ok := inv.packages[provenance.Project]; !ok {
				return fmt.Errorf("contribution %s owner project %q has no scheduler-stamped source root", capabilityIdentityLabel(identity), provenance.Project)
			}
		case protocaps.LocationRootPackage:
			if _, err := inv.ownerForPackageName(provenance.Package); err != nil {
				return err
			}
		default:
			return fmt.Errorf("contribution %s uses unsupported workspace-root provenance for project-scoped dependency aggregation", capabilityIdentityLabel(identity))
		}
		if provenance.Package != "" {
			if _, err := inv.ownerForPackageName(provenance.Package); err != nil {
				return err
			}
		}
		return nil
	}
	for _, value := range manifest.ConfigDefinitions {
		if err := check(value.Identity, value.Provenance); err != nil {
			return err
		}
	}
	for _, value := range manifest.Schemas {
		if err := check(value.Identity, value.Provenance); err != nil {
			return err
		}
	}
	for _, value := range manifest.Discoverers {
		if err := check(value.Identity, value.Provenance); err != nil {
			return err
		}
	}
	for _, value := range manifest.Migrations {
		if err := check(value.Identity, value.Provenance); err != nil {
			return err
		}
	}
	for _, value := range manifest.InfraRequirements {
		if err := check(value.Identity, value.Provenance); err != nil {
			return err
		}
	}
	for _, value := range manifest.HealthContributors {
		if err := check(value.Identity, value.Provenance); err != nil {
			return err
		}
	}
	for _, value := range manifest.LifecycleHooks {
		if err := check(value.Identity, value.Provenance); err != nil {
			return err
		}
	}
	for _, value := range manifest.Packages {
		if err := check(value.Identity, value.Provenance); err != nil {
			return err
		}
	}
	for _, value := range manifest.RequiredCapabilities {
		if err := check(value.Identity, value.Provenance); err != nil {
			return err
		}
	}
	return nil
}

type capabilityMergeState[T any] struct {
	target   *[]T
	identity func(T) protocaps.ContributionIdentity
	wire     map[protocaps.ContributionIdentity][]byte
	origins  map[protocaps.ContributionIdentity][]string
}

func newCapabilityMergeState[T any](localContainer string, target *[]T, identity func(T) protocaps.ContributionIdentity) (*capabilityMergeState[T], error) {
	state := &capabilityMergeState[T]{
		target: target, identity: identity,
		wire:    make(map[protocaps.ContributionIdentity][]byte, len(*target)),
		origins: make(map[protocaps.ContributionIdentity][]string, len(*target)),
	}
	for _, value := range *target {
		wire, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("marshal existing contribution %s: %w", capabilityIdentityLabel(identity(value)), err)
		}
		key := identity(value)
		state.wire[key] = wire
		if localContainer != "" {
			state.origins[key] = appendCapabilityOrigin(state.origins[key], localContainer)
		}
	}
	return state, nil
}

func (state *capabilityMergeState[T]) merge(container string, incoming []T) error {
	for _, value := range incoming {
		key := state.identity(value)
		wire, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal dependency contribution %s: %w", capabilityIdentityLabel(key), err)
		}
		if existing, ok := state.wire[key]; ok {
			if !bytes.Equal(existing, wire) {
				origins := appendCapabilityOrigin(append([]string(nil), state.origins[key]...), container)
				sort.Strings(origins)
				return fmt.Errorf("dependency containers %q contribution %s have conflicting canonical wire copies", strings.Join(origins, ", "), capabilityIdentityLabel(key))
			}
			state.origins[key] = appendCapabilityOrigin(state.origins[key], container)
			continue
		}
		state.wire[key] = wire
		state.origins[key] = appendCapabilityOrigin(state.origins[key], container)
		*state.target = append(*state.target, value)
	}
	return nil
}

func appendCapabilityOrigin(origins []string, origin string) []string {
	for _, existing := range origins {
		if existing == origin {
			return origins
		}
	}
	return append(origins, origin)
}
