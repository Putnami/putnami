package capabilities

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

var (
	sourceBindingPattern = regexp.MustCompile(`^source-v1:sha256:[0-9a-f]{64}$`)
	sha256Pattern        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	schemePattern        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// ParseManifestDocument dispatches strictly by the exact top-level protocol
// version. It never infers a version from document shape.
func ParseManifestDocument(data []byte) (*ManifestDocument, []diag.Diagnostic) {
	version, err := exactProtocolVersion(data)
	if err != nil {
		var versionErr protocolVersionError
		if errors.As(err, &versionErr) {
			return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "%s", versionErr.message)}
		}
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	switch version {
	case ProtocolVersion:
		m, diags := ParseManifest(data)
		if m == nil {
			return nil, diags
		}
		return &ManifestDocument{ProtocolVersion: version, V1: m}, diags
	case ProtocolVersionV2:
		m, diags := ParseManifestV2(data)
		if m == nil {
			return nil, diags
		}
		return &ManifestDocument{ProtocolVersion: version, V2: m}, diags
	default:
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocolVersion %d is not supported (want 1 or 2)", version)}
	}
}

type protocolVersionError struct{ message string }

func (e protocolVersionError) Error() string { return e.message }

// exactProtocolVersion walks only the top-level JSON object and retains the
// raw value token. JSON numeric equality is deliberately insufficient here:
// protocol versions 1 and 2 are accepted only as the exact integer tokens.
func exactProtocolVersion(data []byte) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return 0, fmt.Errorf("manifest must be a JSON object")
	}
	var raw json.RawMessage
	found := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return 0, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return 0, fmt.Errorf("manifest object key is not a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return 0, err
		}
		if key != "protocolVersion" {
			continue
		}
		if found {
			return 0, protocolVersionError{message: "protocolVersion must appear exactly once"}
		}
		found = true
		raw = bytes.TrimSpace(value)
	}
	if _, err := decoder.Token(); err != nil {
		return 0, err
	}
	if !found {
		return 0, protocolVersionError{message: "protocolVersion is required"}
	}
	switch string(raw) {
	case "1":
		return ProtocolVersion, nil
	case "2":
		return ProtocolVersionV2, nil
	default:
		return 0, protocolVersionError{message: fmt.Sprintf("protocolVersion token %q is not supported (want exact integer token 1 or 2)", raw)}
	}
}

// ParseManifestV2 decodes only the v2 wire shape and rejects unknown fields.
func ParseManifestV2(data []byte) (*ManifestV2, []diag.Diagnostic) {
	var m ManifestV2
	if err := decodeStrictJSON(data, &m, true); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &m, nil
}

func decodeStrictJSON(data []byte, out any, disallowUnknown bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if disallowUnknown {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func parseDiagnostic(err error) diag.Diagnostic {
	message := err.Error()
	if strings.HasPrefix(message, "json: unknown field ") {
		field := strings.Trim(strings.TrimPrefix(message, "json: unknown field "), `"`)
		return diag.Errorf(ErrorCodeUnknownField, field, "%s", message)
	}
	return diag.Errorf(ErrorCodeParseError, "", "%s", message)
}

// ValidateManifestV2 checks owner-scoped identity, contribution-field
// agreement, precise provenance, and intra-manifest uniqueness.
func ValidateManifestV2(input *ManifestV2) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}
	// Validation observes the document as the producer wrote it: no legacy field
	// is stripped and no package contribution is scoped away, so every entry that
	// is actually on the wire gets its diagnostics. Dropping content here would
	// hide an invalid entry behind the emission projection.
	m := canonicalManifestV2(input, false, false, false)
	var diags []diag.Diagnostic
	if m.ProtocolVersion != ProtocolVersionV2 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocolVersion %d is not supported by the v2 parser (want 2)", m.ProtocolVersion))
	}
	if strings.TrimSpace(m.Project) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProject, "project", "manifest is missing the project identifier"))
	}

	type identityAt struct {
		identity ContributionIdentity
		field    string
	}
	identities := make([]identityAt, 0)
	check := func(field string, identity ContributionIdentity, provenance ProvenanceV2, wantKind ContributionKind, wantSubkind, wantKey string, subkindRequired bool) {
		diags = append(diags, validateIdentityV2(field+".identity", identity, provenance, wantKind, wantSubkind, wantKey, subkindRequired)...)
		diags = append(diags, validateProvenanceV2(field+".provenance", provenance)...)
		if identity.OwnerProject != "" || identity.Kind != "" || identity.Subkind != "" || identity.Key != "" {
			identities = append(identities, identityAt{identity: identity, field: field})
		}
	}
	for i, c := range m.ConfigDefinitions {
		field := fmt.Sprintf("configDefinitions[%d]", i)
		diags = append(diags, validateName(field+".path", c.Path)...)
		for j, f := range c.Fields {
			diags = append(diags, validateName(fmt.Sprintf("%s.fields[%d].name", field, j), f.Name)...)
		}
		check(field, c.Identity, c.Provenance, ContributionKindConfig, "", c.Path, false)
	}
	for i, s := range m.Schemas {
		field := fmt.Sprintf("schemas[%d]", i)
		diags = append(diags, validateName(field+".name", s.Name)...)
		if !ValidSchemaKinds[s.Kind] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSchemaKind, field+".kind", "schema kind %q is not in the v2 set", s.Kind))
		}
		check(field, s.Identity, s.Provenance, ContributionKindSchema, string(s.Kind), s.Name, true)
	}
	for i, d := range m.Discoverers {
		field := fmt.Sprintf("discoverers[%d]", i)
		diags = append(diags, validateName(field+".name", d.Name)...)
		if !ValidDiscovererKinds[d.Kind] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidDiscovererKind, field+".kind", "discoverer kind %q is not in the v2 set", d.Kind))
		}
		check(field, d.Identity, d.Provenance, ContributionKindDiscoverer, string(d.Kind), d.Name, true)
	}
	for i, migration := range m.Migrations {
		field := fmt.Sprintf("migrations[%d]", i)
		diags = append(diags, validateName(field+".name", migration.Name)...)
		if strings.TrimSpace(migration.Kind) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingMigrationKind, field+".kind", "migration kind is required in v2"))
		}
		check(field, migration.Identity, migration.Provenance, ContributionKindMigration, migration.Kind, migration.Name, true)
	}
	for i, r := range m.InfraRequirements {
		field := fmt.Sprintf("infraRequirements[%d]", i)
		diags = append(diags, validateName(field+".name", r.Name)...)
		if !ValidInfraKinds[r.Kind] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidInfraKind, field+".kind", "infra kind %q is not in the v2 set", r.Kind))
		}
		check(field, r.Identity, r.Provenance, ContributionKindInfra, string(r.Kind), r.Name, true)
	}
	for i, h := range m.HealthContributors {
		field := fmt.Sprintf("healthContributors[%d]", i)
		diags = append(diags, validateName(field+".name", h.Name)...)
		if !ValidProbeKinds[h.Probe] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidProbe, field+".probe", "probe %q is not in the v2 set", h.Probe))
		}
		check(field, h.Identity, h.Provenance, ContributionKindHealth, string(h.Probe), h.Name, true)
	}
	for i, hook := range m.LifecycleHooks {
		field := fmt.Sprintf("lifecycleHooks[%d]", i)
		diags = append(diags, validateName(field+".name", hook.Name)...)
		if !ValidLifecyclePhases[hook.Phase] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPhase, field+".phase", "lifecycle phase %q is not in the v2 set", hook.Phase))
		}
		check(field, hook.Identity, hook.Provenance, ContributionKindLifecycle, string(hook.Phase), hook.Name, true)
	}
	for i, p := range m.Packages {
		field := fmt.Sprintf("packages[%d]", i)
		diags = append(diags, validateName(field+".package", p.Package)...)
		check(field, p.Identity, p.Provenance, ContributionKindPackage, "", p.Package, false)
	}
	for i, p := range m.PackageVersions {
		field := fmt.Sprintf("packageVersions[%d]", i)
		diags = append(diags, validateName(field+".package", p.Package)...)
		check(field, p.Identity, p.Provenance, ContributionKindPackage, "", p.Package, false)
	}
	for i, r := range m.RequiredCapabilities {
		field := fmt.Sprintf("requiredCapabilities[%d]", i)
		diags = append(diags, validateName(field+".name", r.Name)...)
		if len(r.Requires) == 0 {
			diags = append(diags, diag.Errorf(ErrorCodeMissingRequires, field+".requires", "requiredCapability %q must depend on at least one capability kind", r.Name))
		}
		for j, kind := range r.Requires {
			if !ValidCapabilityKinds[kind] {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidCapabilityKind, fmt.Sprintf("%s.requires[%d]", field, j), "capability kind %q is not in the v2 set", kind))
			}
		}
		check(field, r.Identity, r.Provenance, ContributionKindRequiredCapability, "", r.Name, false)
	}
	for i, access := range m.DomainAccess {
		field := fmt.Sprintf("domainAccess[%d]", i)
		diags = append(diags, validateName(field+".import", access.Import)...)
		// The mode is the subkind, so an implementation that enforces one mode of
		// an import cannot collide with an implementation of another. What a mode
		// MEANS stays in protocols/architecture: this document carries the value
		// and the checker that joins the two documents reports a disagreement.
		if strings.TrimSpace(access.Mode) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingDomainAccessMode, field+".mode", "domainAccess %q must carry the access mode it enforces", access.Import))
		}
		if strings.TrimSpace(access.Status) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeMissingDomainAccessStatus, field+".status", "domainAccess %q must carry the declared lifecycle status", access.Import))
		}
		for j, transport := range access.Transports {
			transportField := fmt.Sprintf("%s.transports[%d]", field, j)
			diags = append(diags, validateName(transportField+".role", transport.Role)...)
			diags = append(diags, validateName(transportField+".kind", transport.Kind)...)
			diags = append(diags, validateName(transportField+".availability", transport.Availability)...)
		}
		check(field, access.Identity, access.Provenance, ContributionKindDomainAccess, access.Mode, access.Import, true)
	}

	seen := make(map[ContributionIdentity]string, len(identities))
	for _, item := range identities {
		if first, exists := seen[item.identity]; exists {
			diags = append(diags, diag.Errorf(ErrorCodeDuplicateContribution, item.field+".identity", "contribution identity is duplicated within the manifest (%s and %s)", first, item.field))
		} else {
			seen[item.identity] = item.field
		}
	}
	diags = append(diags, validateRequiredProvidersV2(m)...)
	sortDiagnosticsV2(diags)
	return diags
}

// AvailableProviderKindsV2 projects every contribution in one manifest
// container onto the provider kinds visible to that workload. Provider
// completeness is intentionally container-scoped, preserving v1 semantics:
// dependency-owned copies in the same aggregate may satisfy a requirement,
// while contributions found only in another manifest cannot.
func AvailableProviderKindsV2(m *ManifestV2) map[CapabilityKind]bool {
	available := map[CapabilityKind]bool{
		CapabilityKindConfig:     len(m.ConfigDefinitions) > 0,
		CapabilityKindSchema:     len(m.Schemas) > 0,
		CapabilityKindDiscoverer: len(m.Discoverers) > 0,
		CapabilityKindMigration:  len(m.Migrations) > 0,
		CapabilityKindInfra:      len(m.InfraRequirements) > 0,
		CapabilityKindLifecycle:  len(m.LifecycleHooks) > 0,
		CapabilityKindPackage:    len(m.Packages) > 0 || len(m.PackageVersions) > 0,
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

func validateRequiredProvidersV2(m *ManifestV2) []diag.Diagnostic {
	available := AvailableProviderKindsV2(m)
	var diags []diag.Diagnostic
	for i, required := range m.RequiredCapabilities {
		for j, kind := range required.Requires {
			if !ValidCapabilityKinds[kind] || available[kind] {
				continue
			}
			diags = append(diags, diag.Errorf(ErrorCodeMissingRequiredProvider,
				fmt.Sprintf("requiredCapabilities[%d].requires[%d]", i, j),
				"project %q required capability %q declared by %s needs a %q provider, but the manifest has none; contribute a %q provider to project %q or remove %q from requires",
				m.Project, required.Name, provenanceLabelV2(required.Provenance), kind, kind, m.Project, kind))
		}
	}
	return diags
}

func provenanceLabelV2(provenance ProvenanceV2) string {
	switch {
	case strings.TrimSpace(provenance.Declaration.Path) != "":
		return provenance.Declaration.Path
	case strings.TrimSpace(provenance.Package) != "":
		return provenance.Package
	case strings.TrimSpace(provenance.Project) != "":
		return provenance.Project
	default:
		return "unknown source"
	}
}

func sortDiagnosticsV2(diags []diag.Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		left, right := diags[i], diags[j]
		if left.Severity != right.Severity {
			return left.Severity < right.Severity
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		return left.Message < right.Message
	})
}

func validateIdentityV2(field string, identity ContributionIdentity, provenance ProvenanceV2, wantKind ContributionKind, wantSubkind, wantKey string, subkindRequired bool) []diag.Diagnostic {
	if identity == (ContributionIdentity{}) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingContributionIdentity, field, "v2 contribution identity is required")}
	}
	var diags []diag.Diagnostic
	if strings.TrimSpace(identity.OwnerProject) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingOwnerProject, field+".ownerProject", "ownerProject is required"))
	}
	if !ValidContributionKinds[identity.Kind] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidContributionIdentity, field+".kind", "contribution kind %q is not in the v2 set", identity.Kind))
	}
	if strings.TrimSpace(identity.Key) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidContributionIdentity, field+".key", "contribution key is required"))
	}
	if subkindRequired && strings.TrimSpace(identity.Subkind) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidContributionIdentity, field+".subkind", "subkind is required for contribution kind %q", wantKind))
	}
	if !subkindRequired && identity.Subkind != "" {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidContributionIdentity, field+".subkind", "subkind must be absent for contribution kind %q", wantKind))
	}
	if identity.OwnerProject != provenance.Project && identity.OwnerProject != "" && provenance.Project != "" {
		diags = append(diags, diag.Errorf(ErrorCodeOwnerProjectMismatch, field+".ownerProject", "ownerProject %q must equal provenance.project %q", identity.OwnerProject, provenance.Project))
	}
	if identity.Kind != wantKind || identity.Subkind != wantSubkind || identity.Key != wantKey {
		diags = append(diags, diag.Errorf(ErrorCodeIdentityMismatch, field, "identity must equal (%q, %q, %q) for this contribution", wantKind, wantSubkind, wantKey))
	}
	return diags
}

func validateProvenanceV2(field string, p ProvenanceV2) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if strings.TrimSpace(p.Project) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProvenance, field+".project", "entry must carry a provenance project"))
	}
	if !ValidSourceKinds[p.SourceKind] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSourceKind, field+".sourceKind", "sourceKind %q is not in the v2 set", p.SourceKind))
	}
	if p.LegacySourceBinding != "" && !sourceBindingPattern.MatchString(p.LegacySourceBinding) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSourceBinding, field+".sourceBinding", "legacy sourceBinding must use source-v1:sha256:<64-lower-hex>"))
	}
	if p.Declaration.Root == "" && p.Declaration.Path == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingDeclaration, field+".declaration", "a precise declaration is required"))
	} else {
		diags = append(diags, validateLocation(field+".declaration", p.Declaration.Root, p.Declaration.Path, p.Package)...)
	}
	for i, a := range p.Artifacts {
		afield := fmt.Sprintf("%s.artifacts[%d]", field, i)
		diags = append(diags, validateLocation(afield, a.Root, a.Path, p.Package)...)
		if a.Digest != "" && !sha256Pattern.MatchString(a.Digest) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSourceBinding, afield+".digest", "artifact digest must use sha256:<64-lower-hex>"))
		}
		if a.Root == p.Declaration.Root && a.Path == p.Declaration.Path {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPath, afield+".path", "artifact must not duplicate the declaration location"))
		}
	}
	return diags
}

func validateLocation(field string, root LocationRoot, value, packageName string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if !ValidLocationRoots[root] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidPath, field+".root", "root %q is not one of workspace, project, package", root))
	}
	if root == LocationRootPackage && strings.TrimSpace(packageName) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingProvenance, field+".root", "package locations require provenance package"))
	}
	if code, message := validateProtocolPath(value); code != "" {
		diags = append(diags, diag.Errorf(code, field+".path", "%s", message))
	}
	return diags
}

func validateProtocolPath(value string) (string, string) {
	if value == "" {
		return ErrorCodeInvalidPath, "path is required"
	}
	if strings.ContainsAny(value, "\\\x00") {
		return ErrorCodeInvalidPath, "path must use slash separators and contain no NUL"
	}
	if strings.HasPrefix(value, "/") || schemePattern.MatchString(value) {
		return ErrorCodePathEscape, "path must be relative to its declared root"
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == ".." {
			return ErrorCodePathEscape, "path must not escape its declared root"
		}
		if part == "" || part == "." {
			return ErrorCodeInvalidPath, "path must be its non-empty lexical clean form"
		}
	}
	if path.Clean(value) != value {
		return ErrorCodeInvalidPath, "path must be its lexical clean form"
	}
	return "", ""
}

// ParseAndValidateManifestDocument parses either supported version and applies
// that version's semantic validator.
func ParseAndValidateManifestDocument(data []byte) (*ManifestDocument, []diag.Diagnostic) {
	doc, diags := ParseManifestDocument(data)
	if doc == nil || diag.HasErrors(diags) {
		return nil, diags
	}
	if doc.V1 != nil {
		diags = append(diags, ValidateManifest(doc.V1)...)
	} else {
		diags = append(diags, ValidateManifestV2(doc.V2)...)
	}
	return doc, diags
}

// ParseAndValidateManifestV2 strictly parses and semantically validates a v2 manifest.
func ParseAndValidateManifestV2(data []byte) (*ManifestV2, []diag.Diagnostic) {
	m, diags := ParseManifestV2(data)
	if m == nil || diag.HasErrors(diags) {
		return nil, diags
	}
	return m, append(diags, ValidateManifestV2(m)...)
}
