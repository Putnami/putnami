package architecture

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	diag "go.putnami.dev/protocol/diagnostic"
)

var (
	domainPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]*(/[a-z][a-z0-9-]*)*$`)
	semanticIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9_-]*)+$`)
	contractIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9_-]*)+\.v([1-9][0-9]*)$`)
	factPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
)

// ValidateManifest validates the local, context-free invariants of one domain declaration.
func ValidateManifest(manifest *Manifest) []diag.Diagnostic {
	if manifest == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}
	var diagnostics []diag.Diagnostic
	if manifest.ProtocolVersion != ProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocol version %d is not supported", manifest.ProtocolVersion))
	}
	if !validDomain(manifest.Domain) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDomain, "domain", "domain must be a lower-case slash-separated semantic ID"))
	}
	if !validText(manifest.Owner, 256) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDomain, "owner", "owner is required and must be bounded text"))
	}
	if manifest.Projects == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "projects", "projects is required; use an empty array when the domain maps no project yet"))
	}
	if manifest.Exports == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "exports", "exports is required; use an empty array when the domain publishes nothing"))
	}
	if manifest.Imports == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "imports", "imports is required; use an empty array when the domain consumes nothing"))
	}

	seenProjects := make(map[string]int, len(manifest.Projects))
	for index, project := range manifest.Projects {
		field := fmt.Sprintf("projects[%d]", index)
		if !validProjectID(project) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProject, field, "project must be one canonical absolute Putnami project ID"))
		}
		if previous, exists := seenProjects[project]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateProject, field, "project %q is repeated (also projects[%d])", project, previous))
		} else {
			seenProjects[project] = index
		}
	}

	seenOwned := make(map[string]int, len(manifest.Owns))
	for index, owned := range manifest.Owns {
		field := fmt.Sprintf("owns[%d]", index)
		if !validSemanticID(owned.ID) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".id", "owned concept ID must be a lower-case dotted semantic ID"))
		}
		if !validOwnershipKind(owned.Kind) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".kind", "unknown ownership kind %q", owned.Kind))
		}
		if !validText(owned.Description, 2048) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".description", "owned concept description is required"))
		}
		if previous, exists := seenOwned[owned.ID]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".id", "owned concept %q is repeated (also owns[%d])", owned.ID, previous))
		} else {
			seenOwned[owned.ID] = index
		}
	}

	seenExports := make(map[string]int, len(manifest.Exports))
	for index := range manifest.Exports {
		field := fmt.Sprintf("exports[%d]", index)
		diagnostics = append(diagnostics, validateExport(&manifest.Exports[index], field)...)
		id := manifest.Exports[index].ID
		if previous, exists := seenExports[id]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateExport, field+".id", "export %q is repeated (also exports[%d])", id, previous))
		} else {
			seenExports[id] = index
		}
	}

	seenImports := make(map[string]int, len(manifest.Imports))
	for index := range manifest.Imports {
		field := fmt.Sprintf("imports[%d]", index)
		diagnostics = append(diagnostics, validateImport(&manifest.Imports[index], field)...)
		id := manifest.Imports[index].ID
		if previous, exists := seenImports[id]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateImport, field+".id", "import %q is repeated (also imports[%d])", id, previous))
		} else {
			seenImports[id] = index
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateExport(export *Export, field string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !validContractID(export.ID, export.Version) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".id", "export ID must be a lower-case dotted ID ending in .v%d", export.Version))
	}
	if export.Version < 1 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".version", "export version must be positive"))
	}
	if !validLifecycle(export.Status) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidStatus, field+".status", "unknown lifecycle status %q", export.Status))
	}
	if !validText(export.Description, 4096) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".description", "export description is required"))
	}
	if len(export.Facts) == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidFact, field+".facts", "export must expose at least one fact"))
	}
	requiresFactMetadata := !referenceOnly(export.Modes)
	seenFacts := make(map[string]int, len(export.Facts))
	for index, fact := range export.Facts {
		factField := fmt.Sprintf("%s.facts[%d]", field, index)
		if !validFactName(fact.Name) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidFact, factField+".name", "fact name must be lower-case snake_case, optionally dot-separated"))
		}
		if !validDomain(fact.Authority) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDomain, factField+".authority", "fact authority must be one domain ID"))
		}
		classificationPresent := fact.classificationPresent || fact.Classification != ""
		personalDataPresent := fact.personalDataPresent || fact.PersonalData != ""
		metadataOmitted := !classificationPresent && !personalDataPresent
		if !validClassification(fact.Classification) && (requiresFactMetadata || !metadataOmitted) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidFact, factField+".classification", "unknown data classification %q", fact.Classification))
		}
		if !validPersonalData(fact.PersonalData) && (requiresFactMetadata || !metadataOmitted) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidFact, factField+".personalData", "unknown personal-data classification %q", fact.PersonalData))
		}
		if previous, exists := seenFacts[fact.Name]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidFact, factField+".name", "fact %q is repeated (also facts[%d])", fact.Name, previous))
		} else {
			seenFacts[fact.Name] = index
		}
	}
	if len(export.Modes) == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidMode, field+".modes", "export must allow at least one access mode"))
	}
	seenModes := make(map[AccessMode]int, len(export.Modes))
	for index, mode := range export.Modes {
		modeField := fmt.Sprintf("%s.modes[%d]", field, index)
		if !validMode(mode) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidMode, modeField, "unknown access mode %q", mode))
		}
		if previous, exists := seenModes[mode]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidMode, modeField, "access mode %q is repeated (also modes[%d])", mode, previous))
		} else {
			seenModes[mode] = index
		}
	}
	if !validCompatibility(export.Compatibility.Strategy) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeIncompatibleImport, field+".compatibility.strategy", "unknown compatibility strategy %q", export.Compatibility.Strategy))
	}
	if export.Compatibility.MinimumConsumerVersion < 1 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeIncompatibleImport, field+".compatibility.minimumConsumerVersion", "minimum consumer version must be positive"))
	}
	return diagnostics
}

func validateImport(contract *Import, field string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !validContractID(contract.ID, contract.Version) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".id", "import ID must be a lower-case dotted ID ending in .v%d", contract.Version))
	}
	if contract.Version < 1 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".version", "import version must be positive"))
	}
	if !validDomain(contract.From.Domain) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDomain, field+".from.domain", "producer domain must be a lower-case slash-separated semantic ID"))
	}
	if !validContractIDAnyVersion(contract.From.Export) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".from.export", "producer export must be a versioned dotted contract ID"))
	}
	if !validSemanticID(contract.As) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".as", "local import name must be a lower-case dotted semantic ID"))
	}
	if !validMode(contract.Mode) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidMode, field+".mode", "unknown access mode %q", contract.Mode))
	}
	if !validLifecycle(contract.Status) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidStatus, field+".status", "unknown lifecycle status %q", contract.Status))
	}
	if !validText(contract.Justification, 4096) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".justification", "import justification is required"))
	}
	if len(contract.Facts) == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidFact, field+".facts", "import must minimize and name at least one fact"))
	}
	diagnostics = append(diagnostics, validateFactNames(contract.Facts, field+".facts")...)

	if contract.Transport != nil {
		diagnostics = append(diagnostics, validateTransport(contract.Transport, field+".transport", contract.Status)...)
	}
	if contract.Bootstrap != nil {
		diagnostics = append(diagnostics, validateTransport(contract.Bootstrap, field+".bootstrap", contract.Status)...)
	}
	if contract.Updates != nil {
		diagnostics = append(diagnostics, validateTransport(contract.Updates, field+".updates", contract.Status)...)
	}
	if contract.Consistency != nil {
		diagnostics = append(diagnostics, validateConsistency(contract.Consistency, field+".consistency")...)
	}
	if contract.Deletion != nil {
		diagnostics = append(diagnostics, validateDeletion(contract.Deletion, field+".deletion")...)
	}
	if contract.LocalModel != nil {
		diagnostics = append(diagnostics, validateLocalModel(contract.LocalModel, field+".localModel")...)
	}

	switch contract.Mode {
	case ModeProjection:
		if contract.Transport != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".transport", "a projection uses bootstrap and updates, not one ambiguous transport"))
		}
		if contract.Bootstrap == nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".bootstrap", "a projection requires an explicit bootstrap transport"))
		}
		if contract.Updates == nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".updates", "a projection requires an explicit updates transport"))
		}
		if contract.Consistency == nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".consistency", "a projection requires freshness, ordering, and idempotency guarantees"))
		}
		if contract.Deletion == nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".deletion", "a projection requires a deletion strategy"))
		}
		if contract.LocalModel == nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".localModel", "a projection requires an explicit local model"))
		} else {
			diagnostics = append(diagnostics, validateProjection(contract, field)...)
		}
	case ModeQuery, ModeCommand, ModeSnapshot:
		if contract.Transport == nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidTransport, field+".transport", "%s access requires one explicit transport", contract.Mode))
		}
		if contract.Bootstrap != nil || contract.Updates != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidTransport, field, "%s access cannot declare projection bootstrap or updates", contract.Mode))
		}
		if (contract.Mode == ModeQuery || contract.Mode == ModeSnapshot) && contract.Consistency == nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidConsistency, field+".consistency", "%s access requires explicit freshness and failure behavior", contract.Mode))
		}
	case ModeReference:
		if contract.Bootstrap != nil || contract.Updates != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidTransport, field, "reference access cannot declare projection bootstrap or updates"))
		}
	}

	seenBindings := make(map[string]int, len(contract.Bindings))
	for index, binding := range contract.Bindings {
		bindingField := fmt.Sprintf("%s.bindings[%d]", field, index)
		if contract.Status == StatusPlanned {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidBinding, bindingField, "a planned target cannot claim a current observed project binding"))
		}
		if binding.Kind != BindingProjectDependency {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidBinding, bindingField+".kind", "unsupported binding kind %q", binding.Kind))
		}
		if !validProjectID(binding.ConsumerProject) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProject, bindingField+".consumerProject", "consumer project must be one canonical Putnami project ID"))
		}
		if !validProjectID(binding.ProducerProject) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProject, bindingField+".producerProject", "producer project must be one canonical Putnami project ID"))
		}
		key := bindingKey(binding.Kind, binding.ConsumerProject, binding.ProducerProject)
		if previous, exists := seenBindings[key]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateBinding, bindingField, "binding is repeated (also bindings[%d])", previous))
		} else {
			seenBindings[key] = index
		}
	}
	return diagnostics
}

func validateTransport(transport *Transport, field string, contractStatus LifecycleStatus) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !validTransport(transport.Kind) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidTransport, field+".kind", "unknown transport kind %q", transport.Kind))
	}
	if !validLifecycle(transport.Availability) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidStatus, field+".availability", "unknown transport availability %q", transport.Availability))
	}
	if transport.Kind == TransportNone {
		if transport.Contract != "" {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidTransport, field+".contract", "none transport cannot name a contract"))
		}
	} else if !validContractIDAnyVersion(transport.Contract) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidTransport, field+".contract", "transport contract must be a versioned dotted ID"))
	}
	if contractStatus == StatusActive && transport.Availability != StatusActive {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidStatus, field+".availability", "an active import cannot depend on a %s transport", transport.Availability))
	}
	return diagnostics
}

func validateConsistency(consistency *Consistency, field string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	duration, err := time.ParseDuration(consistency.MaxStaleness)
	if err != nil || duration <= 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidConsistency, field+".maxStaleness", "max staleness must be one positive Go duration such as 5m"))
	}
	if !validFailureMode(consistency.OnMissing) || consistency.OnMissing == FailureUseStale {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidConsistency, field+".onMissing", "unknown or impossible missing-data behavior %q", consistency.OnMissing))
	}
	if !validFailureMode(consistency.OnStale) || consistency.OnStale == FailureUnavailable {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidConsistency, field+".onStale", "unknown stale-data behavior %q", consistency.OnStale))
	}
	if !validOrdering(consistency.Ordering) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidConsistency, field+".ordering", "unknown ordering strategy %q", consistency.Ordering))
	}
	if !validFactName(consistency.SourceVersion) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidConsistency, field+".sourceVersion", "source version must name one source field"))
	}
	if consistency.Ordering != OrderingNone && !validFactName(consistency.IdempotencyKey) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidConsistency, field+".idempotencyKey", "ordered updates require an idempotency key field"))
	}
	if !validLateEvents(consistency.LateEvents) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidConsistency, field+".lateEvents", "unknown late-event strategy %q", consistency.LateEvents))
	}
	return diagnostics
}

func validateDeletion(deletion *Deletion, field string) []diag.Diagnostic {
	if !validDeletion(deletion.Strategy) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDeletion, field+".strategy", "unknown deletion strategy %q", deletion.Strategy)}
	}
	if deletion.Strategy == DeletionTombstone && !validFactName(deletion.TombstoneField) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDeletion, field+".tombstoneField", "tombstone deletion requires one tombstone field")}
	}
	if deletion.Strategy != DeletionTombstone && deletion.TombstoneField != "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDeletion, field+".tombstoneField", "only tombstone deletion names a tombstone field")}
	}
	return nil
}

func validateLocalModel(model *LocalModel, field string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !validSemanticID(model.Name) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".name", "local model name must be a dotted semantic ID"))
	}
	if !validLocalModelKind(model.Kind) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".kind", "unknown local model kind %q", model.Kind))
	}
	if !validFactName(model.SourceIdentity) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".sourceIdentity", "source identity must name one projected field"))
	}
	if len(model.ProjectedFields) == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".projectedFields", "local model must name its copied fields"))
	}
	diagnostics = append(diagnostics, validateFactNames(model.ProjectedFields, field+".projectedFields")...)
	diagnostics = append(diagnostics, validateFactNames(model.LocalFields, field+".localFields")...)
	projected := stringSet(model.ProjectedFields)
	for index, local := range model.LocalFields {
		if projected[local] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, fmt.Sprintf("%s.localFields[%d]", field, index), "field %q cannot be both projected and locally authoritative", local))
		}
	}
	for member, value := range map[string]string{
		"provenanceField": model.ProvenanceField,
		"observedAtField": model.ObservedAtField,
		"freshnessField":  model.FreshnessField,
	} {
		if !validFactName(value) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+"."+member, "%s must name one local metadata field", member))
		}
	}
	if !validSemanticID(model.Writer) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".writer", "writer must identify the sole projector or ingester"))
	}
	if !validRebuild(model.Rebuild) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".rebuild", "unknown rebuild strategy %q", model.Rebuild))
	}
	if model.Rebuildable && model.Rebuild == RebuildNotApplicable {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".rebuild", "a rebuildable model needs a concrete rebuild strategy"))
	}
	return diagnostics
}

func validateProjection(contract *Import, field string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	model := contract.LocalModel
	if model.Kind != LocalModelProjection {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".localModel.kind", "projection access requires a projection local model"))
	}
	if !model.Rebuildable || model.Rebuild == RebuildNotApplicable {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".localModel.rebuildable", "a projection must be rebuildable"))
	}
	if contract.Consistency != nil {
		if contract.Consistency.Ordering == OrderingNone {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".consistency.ordering", "a projection requires monotone ordering"))
		}
		if contract.Consistency.IdempotencyKey == "" {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".consistency.idempotencyKey", "a projection requires an idempotency key"))
		}
	}
	if !sameStringSet(contract.Facts, model.ProjectedFields) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".localModel.projectedFields", "projected fields must exactly match the minimized imported facts"))
	}
	if !stringSet(model.ProjectedFields)[model.SourceIdentity] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProjection, field+".localModel.sourceIdentity", "source identity must be one of the projected fields"))
	}
	return diagnostics
}

// ValidateRepository validates cross-document domain, export, import, project,
// and exact binding references. It returns stable path-qualified diagnostics.
func ValidateRepository(sources []ManifestSource) []diag.Diagnostic {
	ordered := append([]ManifestSource(nil), sources...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	var diagnostics []diag.Diagnostic
	domains := make(map[string]ManifestSource, len(ordered))
	projects := make(map[string]string)
	exports := make(map[string]exportSource)
	imports := make(map[string]string)
	bindings := make(map[string]string)

	for _, source := range ordered {
		if source.Manifest == nil {
			diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeParseError, "", "manifest is nil")))
			continue
		}
		for _, finding := range ValidateManifest(source.Manifest) {
			diagnostics = append(diagnostics, pathDiagnostic(source.Path, finding))
		}
		manifest := source.Manifest
		if previous, exists := domains[manifest.Domain]; exists {
			diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeDuplicateDomain, "domain", "domain %q is also declared by %s", manifest.Domain, previous.Path)))
		} else {
			domains[manifest.Domain] = source
		}
		for index, project := range manifest.Projects {
			if previous, exists := projects[project]; exists && previous != manifest.Domain {
				diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeDuplicateProject, fmt.Sprintf("projects[%d]", index), "project %q is already owned by domain %q", project, previous)))
			} else {
				projects[project] = manifest.Domain
			}
		}
		for index := range manifest.Exports {
			export := &manifest.Exports[index]
			if previous, exists := exports[export.ID]; exists {
				diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeDuplicateExport, fmt.Sprintf("exports[%d].id", index), "export %q is also declared by %s", export.ID, previous.path)))
			} else {
				exports[export.ID] = exportSource{domain: manifest.Domain, path: source.Path, export: export}
			}
		}
		for index, imported := range manifest.Imports {
			if previous, exists := imports[imported.ID]; exists {
				diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeDuplicateImport, fmt.Sprintf("imports[%d].id", index), "import %q is also declared by %s", imported.ID, previous)))
			} else {
				imports[imported.ID] = source.Path
			}
			for bindingIndex, binding := range imported.Bindings {
				key := bindingKey(binding.Kind, binding.ConsumerProject, binding.ProducerProject)
				if previous, exists := bindings[key]; exists {
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeDuplicateBinding, fmt.Sprintf("imports[%d].bindings[%d]", index, bindingIndex), "binding is already authorized by import %q", previous)))
				} else {
					bindings[key] = imported.ID
				}
			}
		}
	}

	for _, source := range ordered {
		if source.Manifest == nil {
			continue
		}
		manifest := source.Manifest
		for exportIndex, export := range manifest.Exports {
			for factIndex, fact := range export.Facts {
				if _, exists := domains[fact.Authority]; !exists {
					field := fmt.Sprintf("exports[%d].facts[%d].authority", exportIndex, factIndex)
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeUnknownDomain, field, "fact authority domain %q is not declared", fact.Authority)))
				}
			}
		}
		for importIndex, imported := range manifest.Imports {
			field := fmt.Sprintf("imports[%d]", importIndex)
			_, domainExists := domains[imported.From.Domain]
			if !domainExists {
				diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeUnknownDomain, field+".from.domain", "producer domain %q is not declared", imported.From.Domain)))
			}
			published, exportExists := exports[imported.From.Export]
			if !exportExists {
				diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeUnknownExport, field+".from.export", "export %q is not declared", imported.From.Export)))
			} else {
				if published.domain != imported.From.Domain {
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeIncompatibleImport, field+".from", "export %q belongs to domain %q, not %q", imported.From.Export, published.domain, imported.From.Domain)))
				}
				if !containsMode(published.export.Modes, imported.Mode) {
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeIncompatibleImport, field+".mode", "export %q does not allow %s access", imported.From.Export, imported.Mode)))
				}
				publishedFacts := make(map[string]bool, len(published.export.Facts))
				for _, fact := range published.export.Facts {
					publishedFacts[fact.Name] = true
				}
				for factIndex, fact := range imported.Facts {
					if !publishedFacts[fact] {
						diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeIncompatibleImport, fmt.Sprintf("%s.facts[%d]", field, factIndex), "fact %q is not exposed by export %q", fact, imported.From.Export)))
					}
				}
				if imported.Version < published.export.Compatibility.MinimumConsumerVersion {
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeIncompatibleImport, field+".version", "import version %d is below export minimum consumer version %d", imported.Version, published.export.Compatibility.MinimumConsumerVersion)))
				}
				if imported.Status == StatusActive && published.export.Status != StatusActive {
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeIncompatibleImport, field+".status", "active import depends on %s export %q", published.export.Status, imported.From.Export)))
				}
			}
			for bindingIndex, binding := range imported.Bindings {
				bindingField := fmt.Sprintf("%s.bindings[%d]", field, bindingIndex)
				if owner, exists := projects[binding.ConsumerProject]; !exists {
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeUnknownProject, bindingField+".consumerProject", "consumer project %q is not mapped to a domain", binding.ConsumerProject)))
				} else if owner != manifest.Domain {
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeInvalidBinding, bindingField+".consumerProject", "consumer project belongs to domain %q, not %q", owner, manifest.Domain)))
				}
				if owner, exists := projects[binding.ProducerProject]; !exists {
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeUnknownProject, bindingField+".producerProject", "producer project %q is not mapped to a domain", binding.ProducerProject)))
				} else if owner != imported.From.Domain {
					diagnostics = append(diagnostics, pathDiagnostic(source.Path, diag.Errorf(ErrorCodeInvalidBinding, bindingField+".producerProject", "producer project belongs to domain %q, not %q", owner, imported.From.Domain)))
				}
			}
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

type exportSource struct {
	domain string
	path   string
	export *Export
}

// ValidateBaseline validates adoption-time debt metadata and duplicate IDs.
func ValidateBaseline(baseline *Baseline) []diag.Diagnostic {
	if baseline == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "baseline is nil")}
	}
	var diagnostics []diag.Diagnostic
	if baseline.ProtocolVersion != ProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocol version %d is not supported", baseline.ProtocolVersion))
	}
	if baseline.Findings == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "findings", "findings is required; use an empty array when no adoption debt remains"))
	}
	diagnostics = append(diagnostics, validateDebtRecords(baseline.Findings, "findings")...)
	sortDiagnostics(diagnostics)
	return diagnostics
}

// ValidateWaiverFile validates temporary exception metadata and duplicate IDs.
func ValidateWaiverFile(waivers *WaiverFile) []diag.Diagnostic {
	if waivers == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "waiver file is nil")}
	}
	var diagnostics []diag.Diagnostic
	if waivers.ProtocolVersion != ProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocol version %d is not supported", waivers.ProtocolVersion))
	}
	if waivers.Waivers == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "waivers", "waivers is required; use an empty array when no temporary exceptions remain"))
	}
	diagnostics = append(diagnostics, validateDebtRecords(waivers.Waivers, "waivers")...)
	sortDiagnostics(diagnostics)
	return diagnostics
}

// ValidateDebtFiles rejects one violation being classified simultaneously as
// adoption debt and a later temporary exception. The files are independently
// parseable, but their dispositions are mutually exclusive in one repository.
func ValidateDebtFiles(baseline *Baseline, waivers *WaiverFile) []diag.Diagnostic {
	if baseline == nil || waivers == nil {
		return nil
	}
	known := make(map[string]bool, len(baseline.Findings))
	for _, record := range baseline.Findings {
		known[record.Finding] = true
	}
	var diagnostics []diag.Diagnostic
	for index, record := range waivers.Waivers {
		if known[record.Finding] {
			diagnostics = append(diagnostics, diag.Errorf(
				ErrorCodeDuplicateDebtRecord,
				fmt.Sprintf("waivers[%d].finding", index),
				"finding is already classified as adoption debt in %s and cannot also be waived",
				BaselineFilename,
			))
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateDebtRecords(records []DebtRecord, collection string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	seen := make(map[string]int, len(records))
	for index, record := range records {
		field := fmt.Sprintf("%s[%d]", collection, index)
		if !strings.HasPrefix(record.Finding, "architecture.") || strings.ContainsAny(record.Finding, " \t\r\n") {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDebtRecord, field+".finding", "finding must be one exact stable architecture finding ID"))
		}
		for member, value := range map[string]string{"owner": record.Owner, "reason": record.Reason, "scope": record.Scope} {
			if !validText(value, 4096) {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDebtRecord, field+"."+member, "%s is required", member))
			}
		}
		if record.RemoveWhen == "" && record.Expires == "" {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDebtRecord, field, "debt requires removeWhen or expires"))
		}
		if record.Expires != "" {
			if _, err := time.Parse("2006-01-02", record.Expires); err != nil {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDebtRecord, field+".expires", "expires must be an RFC 3339 full-date (YYYY-MM-DD)"))
			}
		}
		if previous, exists := seen[record.Finding]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateDebtRecord, field+".finding", "finding is repeated (also %s[%d])", collection, previous))
		} else {
			seen[record.Finding] = index
		}
	}
	return diagnostics
}

func pathDiagnostic(source string, diagnostic diag.Diagnostic) diag.Diagnostic {
	if source == "" {
		return diagnostic
	}
	if diagnostic.Field == "" {
		diagnostic.Field = source
	} else {
		diagnostic.Field = source + "#" + diagnostic.Field
	}
	return diagnostic
}

func validDomain(value string) bool { return domainPattern.MatchString(value) }

func validSemanticID(value string) bool {
	return len(value) <= 256 && semanticIDPattern.MatchString(value)
}

func validContractID(value string, version int) bool {
	if len(value) > 256 {
		return false
	}
	matches := contractIDPattern.FindStringSubmatch(value)
	if len(matches) != 3 {
		return false
	}
	parsed, err := strconv.Atoi(matches[2])
	return err == nil && parsed == version
}

func validContractIDAnyVersion(value string) bool {
	return len(value) <= 256 && contractIDPattern.MatchString(value)
}

func validFactName(value string) bool {
	return len(value) <= 256 && factPattern.MatchString(value)
}

func validText(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if character == 0 || character == 0x7f {
			return false
		}
	}
	return true
}

func validProjectID(value string) bool {
	if value == "" || len(value) > 1024 || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	cleaned := path.Clean(value)
	if cleaned != value || value == "/" {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(value, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validLifecycle(value LifecycleStatus) bool {
	switch value {
	case StatusPlanned, StatusActive, StatusLegacy, StatusDeprecated:
		return true
	default:
		return false
	}
}

func validMode(value AccessMode) bool {
	switch value {
	case ModeReference, ModeQuery, ModeSnapshot, ModeProjection, ModeCommand:
		return true
	default:
		return false
	}
}

func validTransport(value TransportKind) bool {
	switch value {
	case TransportNone, TransportInProcess, TransportAPI, TransportEvent, TransportFile:
		return true
	default:
		return false
	}
}

func validClassification(value Classification) bool {
	switch value {
	case ClassificationPublic, ClassificationInternal, ClassificationConfidential, ClassificationRestricted:
		return true
	default:
		return false
	}
}

func validPersonalData(value PersonalData) bool {
	switch value {
	case PersonalDataNone, PersonalDataPersonal, PersonalDataSensitive:
		return true
	default:
		return false
	}
}

func validCompatibility(value CompatibilityStrategy) bool {
	switch value {
	case CompatibilityAdditive, CompatibilityVersioned, CompatibilityBreakingWithMigration:
		return true
	default:
		return false
	}
}

func validFailureMode(value FailureMode) bool {
	switch value {
	case FailureFailOpen, FailureFailClosed, FailureUseStale, FailureUnavailable:
		return true
	default:
		return false
	}
}

func validOrdering(value OrderingStrategy) bool {
	switch value {
	case OrderingNone, OrderingSourceVersion, OrderingSourceSequence, OrderingEventTime:
		return true
	default:
		return false
	}
}

func validLateEvents(value LateEventStrategy) bool {
	switch value {
	case LateEventIgnoreOlder, LateEventReject, LateEventApply:
		return true
	default:
		return false
	}
}

func validDeletion(value DeletionStrategy) bool {
	switch value {
	case DeletionTombstone, DeletionHardDelete, DeletionRetain, DeletionNotApplicable:
		return true
	default:
		return false
	}
}

func validLocalModelKind(value LocalModelKind) bool {
	switch value {
	case LocalModelProjection, LocalModelSnapshot, LocalModelReference:
		return true
	default:
		return false
	}
}

func validRebuild(value RebuildStrategy) bool {
	switch value {
	case RebuildBootstrap, RebuildReplay, RebuildBootstrapAndReplay, RebuildNotApplicable:
		return true
	default:
		return false
	}
}

func validOwnershipKind(value OwnershipKind) bool {
	switch value {
	case OwnershipFact, OwnershipModel, OwnershipSchema, OwnershipAPI, OwnershipEvent:
		return true
	default:
		return false
	}
}

func validateFactNames(values []string, field string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	seen := make(map[string]int, len(values))
	for index, value := range values {
		itemField := fmt.Sprintf("%s[%d]", field, index)
		if !validFactName(value) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidFact, itemField, "fact name must be lower-case snake_case, optionally dot-separated"))
		}
		if previous, exists := seen[value]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidFact, itemField, "fact %q is repeated (also %s[%d])", value, field, previous))
		} else {
			seen[value] = index
		}
	}
	return diagnostics
}

func containsMode(values []AccessMode, wanted AccessMode) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func referenceOnly(values []AccessMode) bool {
	return len(values) == 1 && values[0] == ModeReference
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftSet := stringSet(left)
	for _, value := range right {
		if !leftSet[value] {
			return false
		}
	}
	return true
}

func bindingKey(kind BindingKind, consumerProject, producerProject string) string {
	return string(kind) + "\x00" + consumerProject + "\x00" + producerProject
}
