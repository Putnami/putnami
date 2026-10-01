package capabilities

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Error codes for strict parsing and validation. Tooling and runtime
// compliance suites key off these — keep them in sync with ValidErrorCodes
// below.
const (
	ErrorCodeParseError                  = "capabilities.parse_error"
	ErrorCodeUnknownField                = "capabilities.unknown_field"
	ErrorCodeInvalidProtocolVersion      = "capabilities.invalid_protocol_version"
	ErrorCodeMissingProject              = "capabilities.missing_project"
	ErrorCodeInvalidName                 = "capabilities.invalid_name"
	ErrorCodeMissingProvenance           = "capabilities.missing_provenance"
	ErrorCodeInvalidSourceKind           = "capabilities.invalid_source_kind"
	ErrorCodeInvalidSchemaKind           = "capabilities.invalid_schema_kind"
	ErrorCodeInvalidDiscovererKind       = "capabilities.invalid_discoverer_kind"
	ErrorCodeInvalidInfraKind            = "capabilities.invalid_infra_kind"
	ErrorCodeInvalidProbe                = "capabilities.invalid_probe"
	ErrorCodeInvalidPhase                = "capabilities.invalid_phase"
	ErrorCodeInvalidCapabilityKind       = "capabilities.invalid_capability_kind"
	ErrorCodeMissingRequires             = "capabilities.missing_requires"
	ErrorCodeMissingRequiredProvider     = "capabilities.missing_required_provider"
	ErrorCodeDuplicateProvider           = "capabilities.duplicate_provider"
	ErrorCodeConflictingProvider         = "capabilities.conflicting_provider"
	ErrorCodeMissingContributionIdentity = "capabilities.missing_contribution_identity"
	ErrorCodeInvalidContributionIdentity = "capabilities.invalid_contribution_identity"
	ErrorCodeMissingOwnerProject         = "capabilities.missing_owner_project"
	ErrorCodeOwnerProjectMismatch        = "capabilities.owner_project_mismatch"
	ErrorCodeIdentityMismatch            = "capabilities.identity_mismatch"
	ErrorCodeDuplicateContribution       = "capabilities.duplicate_contribution"
	ErrorCodeConflictingContributionCopy = "capabilities.conflicting_contribution_copy"
	ErrorCodeMissingMigrationKind        = "capabilities.missing_migration_kind"
	ErrorCodeMissingDomainAccessMode     = "capabilities.missing_domain_access_mode"
	ErrorCodeMissingDomainAccessStatus   = "capabilities.missing_domain_access_status"
	ErrorCodeMissingDeclaration          = "capabilities.missing_declaration"
	ErrorCodeInvalidSourceBinding        = "capabilities.invalid_source_binding"
	ErrorCodeSourceBindingMismatch       = "capabilities.source_binding_mismatch"
	ErrorCodeSourceBindingUnavailable    = "capabilities.source_binding_unavailable"
	ErrorCodeInvalidPath                 = "capabilities.invalid_path"
	ErrorCodePathEscape                  = "capabilities.path_escape"
	ErrorCodeUnresolvedReference         = "capabilities.unresolved_reference"
	ErrorCodeAmbiguousReference          = "capabilities.ambiguous_reference"
	ErrorCodeV1Unreferenceable           = "capabilities.v1_unreferenceable"
)

// ValidErrorCodes enumerates the canonical capabilities-protocol error
// taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:                  true,
	ErrorCodeUnknownField:                true,
	ErrorCodeInvalidProtocolVersion:      true,
	ErrorCodeMissingProject:              true,
	ErrorCodeInvalidName:                 true,
	ErrorCodeMissingProvenance:           true,
	ErrorCodeInvalidSourceKind:           true,
	ErrorCodeInvalidSchemaKind:           true,
	ErrorCodeInvalidDiscovererKind:       true,
	ErrorCodeInvalidInfraKind:            true,
	ErrorCodeInvalidProbe:                true,
	ErrorCodeInvalidPhase:                true,
	ErrorCodeInvalidCapabilityKind:       true,
	ErrorCodeMissingRequires:             true,
	ErrorCodeMissingRequiredProvider:     true,
	ErrorCodeDuplicateProvider:           true,
	ErrorCodeConflictingProvider:         true,
	ErrorCodeMissingContributionIdentity: true,
	ErrorCodeInvalidContributionIdentity: true,
	ErrorCodeMissingOwnerProject:         true,
	ErrorCodeOwnerProjectMismatch:        true,
	ErrorCodeIdentityMismatch:            true,
	ErrorCodeDuplicateContribution:       true,
	ErrorCodeConflictingContributionCopy: true,
	ErrorCodeMissingMigrationKind:        true,
	ErrorCodeMissingDomainAccessMode:     true,
	ErrorCodeMissingDomainAccessStatus:   true,
	ErrorCodeMissingDeclaration:          true,
	ErrorCodeInvalidSourceBinding:        true,
	ErrorCodeSourceBindingMismatch:       true,
	ErrorCodeSourceBindingUnavailable:    true,
	ErrorCodeInvalidPath:                 true,
	ErrorCodePathEscape:                  true,
	ErrorCodeUnresolvedReference:         true,
	ErrorCodeAmbiguousReference:          true,
	ErrorCodeV1Unreferenceable:           true,
}

// ParseManifest decodes a capability manifest from JSON in strict mode
// (unknown fields rejected). It returns a non-nil manifest only when parsing
// produced no errors.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		code := ErrorCodeParseError
		field := ""
		msg := err.Error()
		if strings.HasPrefix(msg, "json: unknown field ") {
			code = ErrorCodeUnknownField
			field = strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
		}
		return nil, []diag.Diagnostic{diag.Errorf(code, field, "%s", msg)}
	}
	return &m, nil
}

// ValidateManifest checks structural invariants on a parsed manifest. It
// returns one diagnostic per finding in a stable, source-order sequence.
func ValidateManifest(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}

	var diags []diag.Diagnostic
	diags = append(diags, validateProtocolVersion(m.ProtocolVersion)...)

	if strings.TrimSpace(m.Project) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProject, "project",
			"manifest is missing the project identifier"))
	}

	for i, c := range m.ConfigDefinitions {
		field := fmt.Sprintf("configDefinitions[%d]", i)
		diags = append(diags, validateName(field+".path", c.Path)...)
		for j, f := range c.Fields {
			diags = append(diags, validateName(fmt.Sprintf("%s.fields[%d].name", field, j), f.Name)...)
		}
		diags = append(diags, validateProvenance(field, c.Provenance)...)
	}
	for i, s := range m.Schemas {
		field := fmt.Sprintf("schemas[%d]", i)
		diags = append(diags, validateName(field+".name", s.Name)...)
		if !ValidSchemaKinds[s.Kind] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchemaKind, field+".kind",
				"schema kind %q is not in the v1 set: route, openapi, proto", s.Kind))
		}
		diags = append(diags, validateProvenance(field, s.Provenance)...)
	}
	for i, d := range m.Discoverers {
		field := fmt.Sprintf("discoverers[%d]", i)
		diags = append(diags, validateName(field+".name", d.Name)...)
		if !ValidDiscovererKinds[d.Kind] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidDiscovererKind, field+".kind",
				"discoverer kind %q is not in the v1 set: source, config", d.Kind))
		}
		diags = append(diags, validateProvenance(field, d.Provenance)...)
	}
	for i, mb := range m.Migrations {
		field := fmt.Sprintf("migrations[%d]", i)
		diags = append(diags, validateName(field+".name", mb.Name)...)
		diags = append(diags, validateProvenance(field, mb.Provenance)...)
	}
	for i, r := range m.InfraRequirements {
		field := fmt.Sprintf("infraRequirements[%d]", i)
		diags = append(diags, validateName(field+".name", r.Name)...)
		if !ValidInfraKinds[r.Kind] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidInfraKind, field+".kind",
				"infra kind %q is not in the v1 set: database, events, storage, secret, scheduledJob", r.Kind))
		}
		diags = append(diags, validateProvenance(field, r.Provenance)...)
	}
	for i, h := range m.HealthContributors {
		field := fmt.Sprintf("healthContributors[%d]", i)
		diags = append(diags, validateName(field+".name", h.Name)...)
		if !ValidProbeKinds[h.Probe] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProbe, field+".probe",
				"probe %q is not in the v1 set: health, liveness, readiness", h.Probe))
		}
		diags = append(diags, validateProvenance(field, h.Provenance)...)
	}
	for i, l := range m.LifecycleHooks {
		field := fmt.Sprintf("lifecycleHooks[%d]", i)
		diags = append(diags, validateName(field+".name", l.Name)...)
		if !ValidLifecyclePhases[l.Phase] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPhase, field+".phase",
				"lifecycle phase %q is not in the v1 set: starter, stopper", l.Phase))
		}
		diags = append(diags, validateProvenance(field, l.Provenance)...)
	}
	for i, p := range m.PackageVersions {
		field := fmt.Sprintf("packageVersions[%d]", i)
		diags = append(diags, validateName(field+".package", p.Package)...)
		diags = append(diags, validateProvenance(field, p.Provenance)...)
	}
	for i, rc := range m.RequiredCapabilities {
		field := fmt.Sprintf("requiredCapabilities[%d]", i)
		diags = append(diags, validateName(field+".name", rc.Name)...)
		if len(rc.Requires) == 0 {
			diags = append(diags, diag.Errorf(ErrorCodeMissingRequires, field+".requires",
				"requiredCapability %q must depend on at least one capability kind", rc.Name))
		}
		for j, k := range rc.Requires {
			if !ValidCapabilityKinds[k] {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidCapabilityKind,
					fmt.Sprintf("%s.requires[%d]", field, j),
					"capability kind %q is not in the v1 set", k))
			}
		}
		diags = append(diags, validateProvenance(field, rc.Provenance)...)
	}

	// Semantic validation runs after the source-order structural pass so callers
	// always see malformed entries first, followed by deterministic provider
	// collisions and unsatisfied requirements.
	diags = append(diags, validateProviders(m)...)
	diags = append(diags, validateRequiredProviders(m)...)
	return diags
}

// validateProviders rejects two contributions that claim the same logical
// provider identity. Equal definitions are duplicates; definitions whose
// provider-specific payload differs are conflicts. Provenance is deliberately
// excluded from payload equality: two packages declaring the same provider are
// still duplicate providers, not conflicting definitions.
func validateProviders(m *Manifest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, findProviderCollisions("configDefinitions", m.ConfigDefinitions,
		func(v ConfigDefinition) string { return v.Path },
		func(a, b ConfigDefinition) bool { return equalConfigFields(a.Fields, b.Fields) },
		func(v ConfigDefinition) Provenance { return v.Provenance })...)
	diags = append(diags, findProviderCollisions("schemas", m.Schemas,
		func(v SchemaContribution) string { return string(v.Kind) + ":" + v.Name },
		func(a, b SchemaContribution) bool { return a.Path == b.Path },
		func(v SchemaContribution) Provenance { return v.Provenance })...)
	diags = append(diags, findProviderCollisions("discoverers", m.Discoverers,
		func(v Discoverer) string { return string(v.Kind) + ":" + v.Name },
		func(_, _ Discoverer) bool { return true },
		func(v Discoverer) Provenance { return v.Provenance })...)
	// MigrationBundle deliberately has no migration-kind field in protocol v1,
	// while the migration registry scopes namespaces by (kind, namespace).
	// Consequently a wire manifest cannot distinguish two valid cross-kind
	// sources that share a namespace. Emitters validate migration collisions
	// against their typed source values before projecting them into v1; doing it
	// here by Name alone would reject supported applications.
	diags = append(diags, findProviderCollisions("infraRequirements", m.InfraRequirements,
		func(v InfraRequirement) string { return string(v.Kind) + ":" + v.Name },
		func(_, _ InfraRequirement) bool { return true },
		func(v InfraRequirement) Provenance { return v.Provenance })...)
	diags = append(diags, findProviderCollisions("healthContributors", m.HealthContributors,
		func(v HealthContributor) string { return string(v.Probe) + ":" + v.Name },
		func(_, _ HealthContributor) bool { return true },
		func(v HealthContributor) Provenance { return v.Provenance })...)
	diags = append(diags, findProviderCollisions("lifecycleHooks", m.LifecycleHooks,
		func(v LifecycleHook) string { return string(v.Phase) + ":" + v.Name },
		func(_, _ LifecycleHook) bool { return true },
		func(v LifecycleHook) Provenance { return v.Provenance })...)
	diags = append(diags, findProviderCollisions("packageVersions", m.PackageVersions,
		func(v PackageVersion) string { return v.Package },
		func(a, b PackageVersion) bool { return a.Version == b.Version },
		func(v PackageVersion) Provenance { return v.Provenance })...)
	diags = append(diags, findProviderCollisions("requiredCapabilities", m.RequiredCapabilities,
		func(v RequiredCapability) string { return v.Name },
		func(a, b RequiredCapability) bool { return equalCapabilityKinds(a.Requires, b.Requires) },
		func(v RequiredCapability) Provenance { return v.Provenance })...)
	return diags
}

func findProviderCollisions[T any](
	field string,
	items []T,
	identity func(T) string,
	equalPayload func(T, T) bool,
	provenance func(T) Provenance,
) []diag.Diagnostic {
	seen := make(map[string]int, len(items))
	var diags []diag.Diagnostic
	for i, item := range items {
		key := identity(item)
		if first, ok := seen[key]; ok {
			code := ErrorCodeDuplicateProvider
			problem := "is declared more than once"
			remediation := "remove one duplicate declaration or give each provider a unique name"
			if !equalPayload(items[first], item) {
				code = ErrorCodeConflictingProvider
				problem = "has conflicting declarations"
				remediation = "reconcile the definitions or give each provider a unique name"
			}
			diags = append(diags, diag.Errorf(code, fmt.Sprintf("%s[%d]", field, i),
				"provider %q %s (%s and %s); %s",
				key, problem, provenanceLabel(provenance(items[first])), provenanceLabel(provenance(item)), remediation))
			continue
		}
		seen[key] = i
	}
	return diags
}

func equalConfigFields(a, b []ConfigField) bool {
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

func equalCapabilityKinds(a, b []CapabilityKind) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[CapabilityKind]int, len(a))
	for _, kind := range a {
		counts[kind]++
	}
	for _, kind := range b {
		counts[kind]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func provenanceLabel(p Provenance) string {
	switch {
	case strings.TrimSpace(p.EvidencePath) != "":
		return p.EvidencePath
	case strings.TrimSpace(p.Package) != "":
		return p.Package
	case strings.TrimSpace(p.Project) != "":
		return p.Project
	default:
		return "unknown source"
	}
}

// validateRequiredProviders checks completeness against the contribution kinds
// that are actually represented in v1. Datasource is projected from a
// migration's non-empty datasource binding; probe requirements match the
// corresponding health-contributor probe rather than any health entry.
func validateRequiredProviders(m *Manifest) []diag.Diagnostic {
	available := AvailableProviderKinds(m)
	var diags []diag.Diagnostic
	for i, required := range m.RequiredCapabilities {
		for j, kind := range required.Requires {
			if !ValidCapabilityKinds[kind] || available[kind] {
				continue
			}
			diags = append(diags, diag.Errorf(ErrorCodeMissingRequiredProvider,
				fmt.Sprintf("requiredCapabilities[%d].requires[%d]", i, j),
				"project %q required capability %q declared by %s needs a %q provider, but the manifest has none; contribute a %q provider to project %q or remove %q from requires",
				m.Project, required.Name, provenanceLabel(required.Provenance), kind, kind, m.Project, kind))
		}
	}
	return diags
}

// AvailableProviderKinds projects a manifest onto the set of capability kinds it
// represents: a populated contribution section implies its kind; the datasource
// kind is implied by a migration bound to a datasource; and each health
// contributor's probe implies health / liveness / readiness. It is the canonical
// manifest→kind projection — the required-provider validator (above) and any
// out-of-module consumer (e.g. the CLI's conformance-pack advisory) share it so
// their notion of "which kinds a manifest provides" can never drift.
func AvailableProviderKinds(m *Manifest) map[CapabilityKind]bool {
	available := map[CapabilityKind]bool{
		CapabilityKindConfig:     len(m.ConfigDefinitions) > 0,
		CapabilityKindSchema:     len(m.Schemas) > 0,
		CapabilityKindDiscoverer: len(m.Discoverers) > 0,
		CapabilityKindMigration:  len(m.Migrations) > 0,
		CapabilityKindInfra:      len(m.InfraRequirements) > 0,
		CapabilityKindLifecycle:  len(m.LifecycleHooks) > 0,
		CapabilityKindPackage:    len(m.PackageVersions) > 0,
	}
	for _, migration := range m.Migrations {
		if strings.TrimSpace(migration.Datasource) != "" {
			available[CapabilityKindDatasource] = true
		}
	}
	for _, health := range m.HealthContributors {
		switch health.Probe {
		case ProbeKindHealth:
			available[CapabilityKindHealth] = true
		case ProbeKindLiveness:
			available[CapabilityKindLiveness] = true
		case ProbeKindReadiness:
			available[CapabilityKindReadiness] = true
		}
	}
	return available
}

// validateProtocolVersion checks that v is the supported protocol version.
func validateProtocolVersion(v int) []diag.Diagnostic {
	if v != ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported by this parser (want %d)", v, ProtocolVersion)}
	}
	return nil
}

// validateName checks that a required identifier field is non-empty. Entry
// names range from logical capability names to route paths and config block
// paths, so no single character pattern applies — the check is presence only.
func validateName(field, name string) []diag.Diagnostic {
	if strings.TrimSpace(name) == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, field, "name is required")}
	}
	return nil
}

// validateProvenance checks that an entry carries traceable provenance: a
// non-empty Project and a canonical SourceKind. The remaining provenance
// fields are optional context.
func validateProvenance(field string, p Provenance) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if strings.TrimSpace(p.Project) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProvenance, field+".provenance.project",
			"entry must carry a provenance project"))
	}
	if !ValidSourceKinds[p.SourceKind] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSourceKind, field+".provenance.sourceKind",
			"sourceKind %q is not in the v1 set: framework, manual, generated", p.SourceKind))
	}
	return diags
}

// ParseAndValidateManifest is a convenience that runs strict parsing followed
// by structural validation.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return m, append(diags, ValidateManifest(m)...)
}
