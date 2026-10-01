package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"go.putnami.dev/errors"
	"go.putnami.dev/migration"
	protocaps "go.putnami.dev/protocol/capabilities"
	protofeatures "go.putnami.dev/protocol/features"
	protoinfra "go.putnami.dev/protocol/infra"
)

// This file holds the built-in "capabilities" describer: it walks every
// contribution kind the app package can observe through Collect[T] and the
// migration registry, folds them into a go.putnami.dev/protocol/capabilities
// ManifestV2, and emits the deterministic per-project capability and optional
// feature-evidence artifacts. Every collection is canonicalized before the
// protocol packages marshal it, giving follow-on language emitters the same
// byte-stable wire contract. Resolved dependency versions remain in the
// scheduler-owned .gen/version.json inventory and never enter this durable
// manifest.

// describerNameCapabilities is the built-in describer that emits the aggregated
// capability manifest. Filtered by "capabilities" / "all" via
// DescribeContext.Wants, matching how the sibling built-in describers gate
// themselves.
const describerNameCapabilities = "capabilities"

// capabilitiesSchemaURL is the published v2 JSON Schema $id stamped into the
// manifest. The protocol package does not export the URL as a constant, so it
// is duplicated here.
const capabilitiesSchemaURL = "https://putnami.dev/schemas/putnami-capabilities-v2.json"

// generatedVersionInfo is the subset of .gen/version.json that the capability
// emitter reads. The full shape lives above the app package in the dependency
// graph, so this package re-declares only the scheduler-owned project identity,
// workspace root, and source-bound package inventory it consumes. The volatile
// top-level build version must never enter the committed manifest.
type generatedVersionInfo struct {
	Name               string                       `json:"name"`
	CapabilityRoot     string                       `json:"capabilityRoot,omitempty"`
	CapabilityPackages []generatedCapabilityPackage `json:"capabilityPackages,omitempty"`
	legacyShape        bool
}

// describeCapabilities builds the capability manifest for the application and
// writes it to <OutputDir>/schema/capabilities.json — i.e.
// <project>/.gen/schema/capabilities.json (capabilities.EmitDir). Writing under
// .gen/schema/ (not the .gen/ root) is load-bearing: the codegen committer only
// promotes files under .gen/schema/ into the tracked tree, so a manifest at the
// .gen/ root would stay ephemeral and never ship.
//
// The write is atomic (temp file + rename) and the manifest is fully assembled
// in memory before any bytes touch disk, so a failed or partial describe never
// publishes a manifest — mirroring the sole-committer + atomic-finalize pattern
// the infra sidecar and describeMigrations use.
func (a *Application) describeCapabilities(dctx *DescribeContext) error {
	versionInfo, present, err := readGeneratedVersion(dctx.OutputDir)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "read capability scheduler metadata")
	}
	if !present {
		return nil
	}
	project := versionInfo.Name
	if project == "" || versionInfo.CapabilityPackages == nil {
		return errors.Newf(CodeConfigure, "capability scheduler metadata is missing project/package source-binding inventory")
	}
	// A complete source-unbound scheduler stamp clears prior capability artifacts
	// and publishes no v2 manifest. Partial stamps reach strict validation below.
	if versionInfo.legacyShape && legacyCapabilitySchedulerMetadata(project, versionInfo.CapabilityPackages) {
		if err := removeCapabilityArtifacts(dctx.OutputDir); err != nil {
			return errors.Wrapf(err, CodeConfigure, "remove capability artifacts for legacy scheduler metadata")
		}
		return nil
	}
	inventory, err := newCapabilityInventory(project, versionInfo.CapabilityPackages)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "validate capability scheduler metadata")
	}
	m := protocaps.ManifestV2{
		Schema:          capabilitiesSchemaURL,
		ProtocolVersion: protocaps.ProtocolVersionV2,
		Project:         project,
	}

	configDefs, err := a.collectCapabilityConfig(inventory)
	if err != nil {
		return err
	}
	m.ConfigDefinitions = configDefs

	m.Schemas, err = a.collectCapabilitySchemas(dctx.OutputDir, inventory)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "collect capability schemas")
	}
	m.Discoverers, err = a.collectCapabilityDiscoverers(inventory)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "collect capability discoverers")
	}

	var migrationFindings []capabilityValidationFinding
	m.Migrations, migrationFindings, err = a.collectCapabilityMigrations(inventory)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "collect capability migrations")
	}
	infraRequirements, err := a.collectCapabilityInfraSidecars(dctx.OutputDir, inventory)
	if err != nil {
		return err
	}
	if len(infraRequirements) == 0 {
		infraRequirements, err = a.collectCapabilityInfra(inventory)
		if err != nil {
			return errors.Wrapf(err, CodeConfigure, "collect capability infra requirements")
		}
	}
	m.InfraRequirements = infraRequirements
	m.HealthContributors, err = a.collectCapabilityHealth(inventory)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "collect capability health contributors")
	}
	m.LifecycleHooks, err = a.collectCapabilityLifecycle(inventory)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "collect capability lifecycle hooks")
	}
	m.RequiredCapabilities, err = a.collectRequiredCapabilities(inventory)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "collect required capabilities")
	}
	m.DomainAccess, err = a.collectDomainAccess(inventory)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "collect domain access contracts")
	}
	// Packages come last, after every other local contribution: they are scoped
	// to the capability surface those contributions define, so this call
	// must see the assembled manifest. Collecting them earlier would silently
	// drop a package whose only contribution is declared by a later collector.
	m.Packages, err = inventory.packageContributions(&m)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "collect capability packages")
	}
	if err := mergeDependencyCapabilityManifests(filepath.Dir(dctx.OutputDir), versionInfo.CapabilityRoot, &m, inventory); err != nil {
		return err
	}
	m = *protocaps.CanonicalManifestV2(&m)
	if diags := protocaps.ValidateManifestV2(&m); len(migrationFindings)+len(diags) > 0 {
		lines := make([]string, 0, len(migrationFindings)+len(diags))
		for _, finding := range migrationFindings {
			lines = append(lines, fmt.Sprintf("[%s] %s: %s", finding.Code, finding.Field, finding.Message))
		}
		for _, finding := range diags {
			lines = append(lines, fmt.Sprintf("[%s] %s: %s", finding.Code, finding.Field, finding.Message))
		}
		return errors.Newf(CodeConfigure, "capability manifest validation failed:\n%s", strings.Join(lines, "\n"))
	}
	evidence, err := buildFeatureEvidenceDocument(filepath.Dir(dctx.OutputDir), a.Module, &m, inventory)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "build feature evidence")
	}
	return writeCapabilityArtifacts(dctx.OutputDir, &m, evidence)
}

type capabilityValidationFinding struct {
	Code    string
	Field   string
	Message string
}

type capabilityMigrationSourceAssociation struct {
	contributor MigrationContributor
	source      migration.Source
	owner       *Module
}

func (a *Application) capabilityMigrationSources() ([]capabilityMigrationSourceAssociation, error) {
	associations := append([]capabilityMigrationSourceAssociation(nil), a.capabilityMigrationAssociations...)
	if a.migrationRegistry != nil {
		count := 0
		for _, kind := range a.migrationRegistry.Kinds() {
			count += len(a.migrationRegistry.Sources(kind))
		}
		if count != len(associations) {
			return nil, fmt.Errorf("migration registry contains %d sources but only %d retain precise MigrationContributor declarations", count, len(associations))
		}
	}
	return associations, nil
}

// collectDomainAccess maps the runtime-enforced domain access contracts of the
// plugin tree into protocol rows, stamping the same provenance every other
// contribution carries.
//
// The rows are EVIDENCE. `putnami architecture validate` reads them beside the
// declared imports and reports a declared active contract nothing implements, or
// an implemented one nobody declared. A row never authorizes anything: emitting
// it cannot create a cross-domain permission, which is why this collector reads
// only what a plugin already stated and invents no member.
//
// Nothing here validates the ARC/DARC vocabulary. The contract was validated by
// `go.putnami.dev/protocol/architecture` when the component was constructed, and
// a second opinion here would be the drift the checker exists to catch.
func (a *Application) collectDomainAccess(inventory *capabilityInventory) ([]protocaps.DomainAccessV2, error) {
	var access []protocaps.DomainAccessV2
	for _, contributor := range Collect[DomainAccessContributor](a.Module) {
		for _, declaration := range contributor.DomainAccessContracts() {
			// The mode is the subkind, so one project enforcing two modes of one
			// import keeps two distinct identities instead of colliding.
			identity, err := inventory.identity(protocaps.ContributionKindDomainAccess, declaration.Mode, declaration.Import)
			if err != nil {
				return nil, err
			}
			if !inventory.local(identity) {
				continue
			}
			provenance, err := inventory.provenance(contributor, identity, "DomainAccessContracts")
			if err != nil {
				return nil, err
			}
			transports := make([]protocaps.DomainAccessTransportV2, 0, len(declaration.Transports))
			for _, transport := range declaration.Transports {
				transports = append(transports, protocaps.DomainAccessTransportV2{
					Role:         transport.Role,
					Kind:         transport.Kind,
					Contract:     transport.Contract,
					Availability: transport.Availability,
				})
			}
			sort.SliceStable(transports, func(i, j int) bool { return transports[i].Role < transports[j].Role })
			row := protocaps.DomainAccessV2{
				Identity:   identity,
				Import:     declaration.Import,
				Mode:       declaration.Mode,
				Status:     declaration.Status,
				Provenance: provenance,
			}
			if len(transports) > 0 {
				row.Transports = transports
			}
			// An all-empty enforcement block is omitted rather than emitted as an
			// object of empty strings: a reference contract enforces none of these
			// parameters, and saying so with an absent member is honest.
			if enforced := domainAccessEnforcement(declaration.Enforced); enforced != nil {
				row.Enforced = enforced
			}
			access = append(access, row)
		}
	}
	// Deterministic before canonicalization, which re-sorts every collection by
	// the protocol's contribution identity. Sorting here costs nothing and keeps
	// the collector's own output stable for a reader stepping through it.
	sort.SliceStable(access, func(i, j int) bool {
		if access[i].Import != access[j].Import {
			return access[i].Import < access[j].Import
		}
		return access[i].Mode < access[j].Mode
	})
	return access, nil
}

func domainAccessEnforcement(declared DomainAccessEnforcement) *protocaps.DomainAccessEnforcementV2 {
	if declared == (DomainAccessEnforcement{}) {
		return nil
	}
	return &protocaps.DomainAccessEnforcementV2{
		MaxStaleness: declared.MaxStaleness,
		OnMissing:    declared.OnMissing,
		OnStale:      declared.OnStale,
		Ordering:     declared.Ordering,
		LateEvents:   declared.LateEvents,
		Deletion:     declared.Deletion,
		Writer:       declared.Writer,
		Rebuild:      declared.Rebuild,
	}
}

// collectRequiredCapabilities maps requirement declarations from the plugin
// tree into protocol entries, stamping the same provenance as every other
// contribution and canonicalizing each requires set for deterministic output.
func (a *Application) collectRequiredCapabilities(inventory *capabilityInventory) ([]protocaps.RequiredCapabilityV2, error) {
	contributors := Collect[RequiredCapabilityContributor](a.Module)
	var required []protocaps.RequiredCapabilityV2
	for _, contributor := range contributors {
		for _, declaration := range contributor.RequiredCapabilities() {
			identity, err := inventory.identity(protocaps.ContributionKindRequiredCapability, "", declaration.Name)
			if err != nil {
				return nil, err
			}
			if !inventory.local(identity) {
				continue
			}
			provenance, err := inventory.provenance(contributor, identity, "RequiredCapabilities")
			if err != nil {
				return nil, err
			}
			requires := append([]protocaps.CapabilityKind(nil), declaration.Requires...)
			sort.SliceStable(requires, func(i, j int) bool { return requires[i] < requires[j] })
			required = append(required, protocaps.RequiredCapabilityV2{
				Identity:   identity,
				Name:       declaration.Name,
				Requires:   requires,
				Provenance: provenance,
			})
		}
	}
	sort.SliceStable(required, func(i, j int) bool {
		if required[i].Name != required[j].Name {
			return required[i].Name < required[j].Name
		}
		return required[i].Provenance.Package < required[j].Provenance.Package
	})
	return required, nil
}

// removeCapabilityManifest invalidates the publishable manifest and any fixed
// temp file left by an interrupted prior run. It is called before the describe
// lifecycle starts, so a failure in Configure or any earlier describer cannot
// leave a stale capability manifest that appears current.
func removeCapabilityManifest(outputDir string) error {
	dir := filepath.Join(outputDir, filepath.Base(protocaps.EmitDir))
	for _, path := range []string{
		filepath.Join(dir, protocaps.ManifestFilename),
		filepath.Join(dir, protocaps.ManifestFilename+".tmp"),
		filepath.Join(dir, "feature-evidence", featureEvidenceFilename),
		filepath.Join(dir, "feature-evidence", featureEvidenceFilename+".tmp"),
	} {
		//nolint:gosec // outputDir is framework-controlled (describe CLI / PUTNAMI_DESCRIBE_OUT), not attacker-supplied
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return errors.Wrapf(err, CodeConfigure, "invalidate capability manifest")
		}
	}
	return nil
}

// collectCapabilityConfig maps every ConfigContributor's config blocks to
// capability ConfigDefinitions, reusing buildConfigBlocks (the same reflection
// the config describer uses) so the field set — and its sensitive flags —
// matches the published config schema. Only top-level fields are emitted, which
// is all the flat ConfigField shape the protocol models.
func (a *Application) collectCapabilityConfig(inventory *capabilityInventory) ([]protocaps.ConfigDefinitionV2, error) {
	contributors := Collect[ConfigContributor](a.Module)
	if len(contributors) == 0 {
		return nil, nil
	}
	var defs []protocaps.ConfigDefinitionV2
	for _, c := range contributors {
		blocks, err := buildConfigBlocks(c.ConfigDefinitions())
		if err != nil {
			return nil, err
		}
		for _, b := range blocks {
			identity, err := inventory.identity(protocaps.ContributionKindConfig, "", b.Path)
			if err != nil {
				return nil, err
			}
			if !inventory.local(identity) {
				continue
			}
			provenance, err := inventory.provenance(c, identity, "ConfigDefinitions")
			if err != nil {
				return nil, err
			}
			def := protocaps.ConfigDefinitionV2{Identity: identity, Path: b.Path, Provenance: provenance}
			for _, f := range b.Fields {
				def.Fields = append(def.Fields, protocaps.ConfigField{
					Name:      f.Name,
					Type:      f.Type,
					Sensitive: f.Sensitive,
				})
			}
			defs = append(defs, def)
		}
	}
	sort.SliceStable(defs, func(i, j int) bool {
		if defs[i].Path != defs[j].Path {
			return defs[i].Path < defs[j].Path
		}
		return defs[i].Provenance.Package < defs[j].Provenance.Package
	})
	return defs, nil
}

// collectCapabilityMigrations records one MigrationBundle per logical migration
// provider. SQL sources deliberately merge definitions contributed by multiple
// plugins under one (kind, namespace, datasource), so capability emission must
// mirror that runtime identity rather than treating each source as a separate
// provider. Sources targeting different datasources remain distinct bundles.
// This compatibility rule is deliberately SQL-only: custom migration kinds
// preserve their existing one-source-per-(kind, namespace) validation.
func (a *Application) collectCapabilityMigrations(inventory *capabilityInventory) ([]protocaps.MigrationBundleV2, []capabilityValidationFinding, error) {
	if a.migrationRegistry == nil {
		return nil, nil, nil
	}
	var bundles []protocaps.MigrationBundleV2
	var findings []capabilityValidationFinding
	sqlProviders := make(map[protocaps.ContributionIdentity]protocaps.MigrationBundleV2)
	sqlSchemas := make(map[string]capabilitySQLSchema)
	associations, err := a.capabilityMigrationSources()
	if err != nil {
		return nil, nil, err
	}
	for _, association := range associations {
		s, k := association.source, association.source.Kind()
		identity, err := inventory.identity(protocaps.ContributionKindMigration, string(k), s.Namespace())
		if err != nil {
			return nil, nil, err
		}
		if !inventory.local(identity) {
			continue
		}
		provenance, err := inventory.provenance(association.contributor, identity, "MigrationSources")
		if err != nil {
			return nil, nil, err
		}
		mb := protocaps.MigrationBundleV2{
			Identity:   identity,
			Name:       s.Namespace(),
			Kind:       string(k),
			Provenance: provenance,
		}
		var schema string
		if sc, ok := s.(migration.SchemaContributor); ok {
			databases := sc.InfraDatabases()
			mb.Datasource = smallestDatabaseName(databases)
			schema = capabilityDatasourceSchema(databases, mb.Datasource)
		}

		if k == migration.KindSQL {
			if first, ok := sqlSchemas[mb.Datasource]; ok && schema != "" && first.schema != "" && first.schema != schema {
				findings = append(findings, capabilityValidationFinding{
					Code:  protocaps.ErrorCodeConflictingProvider,
					Field: fmt.Sprintf("migrations[%d]", len(bundles)),
					Message: fmt.Sprintf("SQL migration datasource %q has conflicting schemas %q and %q (%s and %s); reconcile the schema declarations or target separate datasources",
						mb.Datasource, first.schema, schema, capabilityProvenanceLabel(first.provenance), capabilityProvenanceLabel(mb.Provenance)),
				})
				continue
			}
			if schema != "" {
				sqlSchemas[mb.Datasource] = capabilitySQLSchema{schema: schema, provenance: mb.Provenance}
			}

			if first, ok := sqlProviders[identity]; ok {
				firstWire, err := canonicalMigrationBundleWire(first)
				if err != nil {
					return nil, nil, fmt.Errorf("canonicalize existing SQL migration %s: %w", capabilityIdentityLabel(identity), err)
				}
				currentWire, err := canonicalMigrationBundleWire(mb)
				if err != nil {
					return nil, nil, fmt.Errorf("canonicalize incoming SQL migration %s: %w", capabilityIdentityLabel(identity), err)
				}
				if !bytes.Equal(firstWire, currentWire) {
					findings = append(findings, capabilityValidationFinding{
						Code:  protocaps.ErrorCodeConflictingContributionCopy,
						Field: fmt.Sprintf("migrations[%d]", len(bundles)),
						Message: fmt.Sprintf("SQL migration contribution %s has conflicting declarations (%s and %s)",
							capabilityIdentityLabel(identity), capabilityProvenanceLabel(first.Provenance), capabilityProvenanceLabel(mb.Provenance)),
					})
				}
				continue
			}
			sqlProviders[identity] = mb
			bundles = append(bundles, mb)
			continue
		}
		bundles = append(bundles, mb)
	}
	sort.SliceStable(bundles, func(i, j int) bool {
		if bundles[i].Name != bundles[j].Name {
			return bundles[i].Name < bundles[j].Name
		}
		if bundles[i].Datasource != bundles[j].Datasource {
			return bundles[i].Datasource < bundles[j].Datasource
		}
		return bundles[i].Provenance.Package < bundles[j].Provenance.Package
	})
	return bundles, findings, nil
}

func canonicalMigrationBundleWire(bundle protocaps.MigrationBundleV2) ([]byte, error) {
	canonical := protocaps.CanonicalManifestV2(&protocaps.ManifestV2{
		Migrations: []protocaps.MigrationBundleV2{bundle},
	})
	return json.Marshal(canonical.Migrations[0])
}

type capabilitySQLSchema struct {
	schema     string
	provenance protocaps.ProvenanceV2
}

// capabilityDatasourceSchema returns the canonical schemas a source declares
// for one datasource. SQLSource declares at most one, but normalizing the full
// set keeps the projection deterministic for other SchemaContributor values.
func capabilityDatasourceSchema(databases []protoinfra.Database, datasource string) string {
	schemas := make(map[string]struct{})
	for _, database := range databases {
		if database.Name != datasource {
			continue
		}
		for _, schema := range database.Schemas {
			if schema != "" {
				schemas[schema] = struct{}{}
			}
		}
	}
	if len(schemas) == 0 {
		return ""
	}
	ordered := make([]string, 0, len(schemas))
	for schema := range schemas {
		ordered = append(ordered, schema)
	}
	sort.Strings(ordered)
	return strings.Join(ordered, "\x00")
}

func capabilityProvenanceLabel(p protocaps.ProvenanceV2) string {
	switch {
	case strings.TrimSpace(p.Declaration.Path) != "":
		return p.Declaration.Path
	case strings.TrimSpace(p.Package) != "":
		return p.Package
	case strings.TrimSpace(p.Project) != "":
		return p.Project
	default:
		return "unknown source"
	}
}

// collectCapabilityInfra projects the databases the migration registry declares
// (the authoritative source for the schemas each database owns) into infra
// requirements. Other infra kinds (events, storage, secret, scheduledJob) come
// from their own framework sidecars, which the app package does not collect, so
// only database requirements are emitted here.
func (a *Application) collectCapabilityInfra(inventory *capabilityInventory) ([]protocaps.InfraRequirementV2, error) {
	if a.migrationRegistry == nil {
		return nil, nil
	}
	var reqs []protocaps.InfraRequirementV2
	seen := make(map[protocaps.ContributionIdentity]bool)
	associations, err := a.capabilityMigrationSources()
	if err != nil {
		return nil, err
	}
	for _, association := range associations {
		schemaContributor, ok := association.source.(migration.SchemaContributor)
		if !ok {
			continue
		}
		for _, db := range schemaContributor.InfraDatabases() {
			identity, err := inventory.identity(protocaps.ContributionKindInfra, string(protocaps.InfraKindDatabase), db.Name)
			if err != nil {
				return nil, err
			}
			if !inventory.local(identity) {
				continue
			}
			if seen[identity] {
				continue
			}
			seen[identity] = true
			provenance, err := inventory.provenance(association.contributor, identity, "MigrationSources")
			if err != nil {
				return nil, err
			}
			reqs = append(reqs, protocaps.InfraRequirementV2{Identity: identity, Name: db.Name, Kind: protocaps.InfraKindDatabase, Provenance: provenance})
		}
	}
	// InfraRequirements already returns databases sorted by (name, engine); sort
	// again by name so the capability list is deterministic regardless.
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].Name < reqs[j].Name })
	return reqs, nil
}

// collectCapabilityHealth records every health and readiness probe the module
// tree contributes. HealthChecker feeds /healthz (probe "health");
// ReadinessChecker feeds /readyz (probe "readiness"). The framework has no
// dedicated liveness (/livez) probe interface yet, so ProbeKindLiveness is
// unused here.
func (a *Application) collectCapabilityHealth(inventory *capabilityInventory) ([]protocaps.HealthContributorV2, error) {
	healthCheckers := Collect[HealthChecker](a.Module)
	readinessCheckers := Collect[ReadinessChecker](a.Module)
	hcs := make([]protocaps.HealthContributorV2, 0, len(healthCheckers)+len(readinessCheckers))
	for _, h := range healthCheckers {
		identity, err := inventory.identity(protocaps.ContributionKindHealth, string(protocaps.ProbeKindHealth), h.Name())
		if err != nil {
			return nil, err
		}
		if !inventory.local(identity) {
			continue
		}
		provenance, err := inventory.provenance(h, identity, "CheckHealth")
		if err != nil {
			return nil, err
		}
		hcs = append(hcs, protocaps.HealthContributorV2{Identity: identity, Name: h.Name(), Probe: protocaps.ProbeKindHealth, Provenance: provenance})
	}
	for _, r := range readinessCheckers {
		identity, err := inventory.identity(protocaps.ContributionKindHealth, string(protocaps.ProbeKindReadiness), r.Name())
		if err != nil {
			return nil, err
		}
		if !inventory.local(identity) {
			continue
		}
		provenance, err := inventory.provenance(r, identity, "CheckReadiness")
		if err != nil {
			return nil, err
		}
		hcs = append(hcs, protocaps.HealthContributorV2{Identity: identity, Name: r.Name(), Probe: protocaps.ProbeKindReadiness, Provenance: provenance})
	}
	sort.SliceStable(hcs, func(i, j int) bool {
		if hcs[i].Probe != hcs[j].Probe {
			return hcs[i].Probe < hcs[j].Probe
		}
		if hcs[i].Name != hcs[j].Name {
			return hcs[i].Name < hcs[j].Name
		}
		return hcs[i].Provenance.Package < hcs[j].Provenance.Package
	})
	return hcs, nil
}

// collectCapabilityLifecycle records every Starter and Stopper the module tree
// contributes as a lifecycle hook.
func (a *Application) collectCapabilityLifecycle(inventory *capabilityInventory) ([]protocaps.LifecycleHookV2, error) {
	starters := Collect[Starter](a.Module)
	stoppers := Collect[Stopper](a.Module)
	hooks := make([]protocaps.LifecycleHookV2, 0, len(starters)+len(stoppers))
	for _, s := range starters {
		identity, err := inventory.identity(protocaps.ContributionKindLifecycle, string(protocaps.LifecyclePhaseStarter), s.Name())
		if err != nil {
			return nil, err
		}
		if !inventory.local(identity) {
			continue
		}
		provenance, err := inventory.provenance(s, identity, "Start")
		if err != nil {
			return nil, err
		}
		hooks = append(hooks, protocaps.LifecycleHookV2{Identity: identity, Name: s.Name(), Phase: protocaps.LifecyclePhaseStarter, Provenance: provenance})
	}
	for _, s := range stoppers {
		identity, err := inventory.identity(protocaps.ContributionKindLifecycle, string(protocaps.LifecyclePhaseStopper), s.Name())
		if err != nil {
			return nil, err
		}
		if !inventory.local(identity) {
			continue
		}
		provenance, err := inventory.provenance(s, identity, "Stop")
		if err != nil {
			return nil, err
		}
		hooks = append(hooks, protocaps.LifecycleHookV2{Identity: identity, Name: s.Name(), Phase: protocaps.LifecyclePhaseStopper, Provenance: provenance})
	}
	sort.SliceStable(hooks, func(i, j int) bool {
		if hooks[i].Phase != hooks[j].Phase {
			return hooks[i].Phase < hooks[j].Phase
		}
		if hooks[i].Name != hooks[j].Name {
			return hooks[i].Name < hooks[j].Name
		}
		return hooks[i].Provenance.Package < hooks[j].Provenance.Package
	})
	return hooks, nil
}

// smallestDatabaseName returns the lexicographically smallest database name in
// dbs, or "" when dbs is empty, whatever order dbs is in. An empty database
// name is a real (and smallest) value.
func smallestDatabaseName(dbs []protoinfra.Database) string {
	name := ""
	found := false
	for _, db := range dbs {
		if !found || db.Name < name {
			name = db.Name
			found = true
		}
	}
	return name
}

// pkgPathOf returns the import path of v's concrete type, dereferencing
// pointers. Empty for types without a package path (e.g. builtins), which
// omitempty drops.
func pkgPathOf(v any) string {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return ""
	}
	return t.PkgPath()
}

// readGeneratedVersion reads <outputDir>/version.json — the .gen/version.json
// the build pipeline stamps before describe runs. A missing file yields the
// zero value for direct unit calls. A present but malformed scheduler stamp
// fails closed so graph-owned contributions cannot silently disappear.
func readGeneratedVersion(outputDir string) (generatedVersionInfo, bool, error) {
	//nolint:gosec // outputDir is framework-controlled (describe CLI / PUTNAMI_DESCRIBE_OUT), not attacker-supplied
	data, err := os.ReadFile(filepath.Join(outputDir, "version.json"))
	if os.IsNotExist(err) {
		return generatedVersionInfo{}, false, nil
	}
	if err != nil {
		return generatedVersionInfo{}, false, err
	}
	var v generatedVersionInfo
	if err := json.Unmarshal(data, &v); err != nil {
		return generatedVersionInfo{}, true, err
	}
	v.legacyShape = legacyCapabilitySchedulerStampShape(data)
	return v, true, nil
}

// writeCapabilityArtifacts marshals the capability and optional evidence
// documents canonically and writes them under <outputDir>/schema/.
// outputDir is the workload's ".gen" directory, so the manifest's committed
// subpath is joined under it: EmitDir is ".gen/schema" but describers receive
// the ".gen" directory itself, so only its trailing "schema" segment
// (filepath.Base(EmitDir)) is appended — mirroring how infra.SidecarPathIn
// nests a producer file under the ".gen" OutputDir.
//
// The write is temp-file-plus-rename so a crash mid-write never leaves a partial
// manifest on the promotion path.
func writeCapabilityArtifacts(outputDir string, manifest *protocaps.ManifestV2, evidence *protofeatures.EvidenceDocument) error {
	manifestData, err := protocaps.MarshalManifestV2(manifest)
	if err != nil {
		return errors.Wrapf(err, CodeConfigure, "marshal capability manifest")
	}
	var evidenceData []byte
	if evidence != nil && len(evidence.Evidence) > 0 {
		evidenceData, err = protofeatures.MarshalEvidenceDocument(evidence)
		if err != nil {
			return errors.Wrapf(err, CodeConfigure, "marshal feature evidence")
		}
	}

	dir := filepath.Join(outputDir, filepath.Base(protocaps.EmitDir))
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // OutputDir is framework-controlled
		return errors.Wrapf(err, CodeConfigure, "prepare capability manifest dir")
	}
	final := filepath.Join(dir, protocaps.ManifestFilename)
	tmp := final + ".tmp"
	evidenceDir := filepath.Join(dir, "feature-evidence")
	evidenceFinal := filepath.Join(evidenceDir, featureEvidenceFilename)
	evidenceTmp := evidenceFinal + ".tmp"
	cleanup := func() {
		_ = removeCapabilityArtifacts(outputDir) //nolint:errcheck // best-effort rollback after a more useful publication error
	}
	if len(evidenceData) > 0 {
		if err := os.MkdirAll(evidenceDir, 0o750); err != nil {
			cleanup()
			return errors.Wrapf(err, CodeConfigure, "prepare feature evidence dir")
		}
		if err := os.WriteFile(evidenceTmp, evidenceData, 0o600); err != nil {
			cleanup()
			return errors.Wrapf(err, CodeConfigure, "write feature evidence")
		}
	} else {
		for _, path := range []string{evidenceTmp, evidenceFinal} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				cleanup()
				return errors.Wrapf(err, CodeConfigure, "remove stale feature evidence")
			}
		}
	}
	if err := os.WriteFile(tmp, manifestData, 0o600); err != nil {
		cleanup()
		return errors.Wrapf(err, CodeConfigure, "write capability manifest")
	}
	if len(evidenceData) > 0 {
		if err := os.Rename(evidenceTmp, evidenceFinal); err != nil {
			cleanup()
			return errors.Wrapf(err, CodeConfigure, "finalize feature evidence")
		}
	}
	// The capability manifest is the commit marker and is renamed last. A
	// crash can expose evidence without a manifest, never a manifest that
	// references evidence which was not fully finalized.
	if err := os.Rename(tmp, final); err != nil {
		cleanup()
		return errors.Wrapf(err, CodeConfigure, "finalize capability manifest")
	}
	return nil
}

// removeCapabilityArtifacts clears only the Go framework's generated
// capability outputs. Other language/framework evidence files may share the
// feature-evidence directory and are deliberately left untouched.
func removeCapabilityArtifacts(outputDir string) error {
	dir := filepath.Join(outputDir, filepath.Base(protocaps.EmitDir))
	final := filepath.Join(dir, protocaps.ManifestFilename)
	evidenceFinal := filepath.Join(dir, "feature-evidence", featureEvidenceFilename)
	for _, path := range []string{final + ".tmp", final, evidenceFinal + ".tmp", evidenceFinal} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
