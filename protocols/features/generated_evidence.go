package features

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
)

// generatedEvidenceIDDigestLength is how much of the contribution digest enters
// a generated evidence ID. Twelve lower-hex characters is 48 bits: enough that
// a collision inside one feature requirement is not reachable by a workspace,
// and short enough that a reviewer can still compare two IDs at a glance.
const generatedEvidenceIDDigestLength = 12

// GeneratedEvidenceMapping is one explicit association between an authored
// feature requirement and one exact canonical contribution.
//
// It is the whole authoring surface for generated technical evidence. A
// producer supplies the tuple; every other field of the emitted record —
// identity, stage, outcome, issuer, source binding, and subject — is derived,
// so an author can neither weaken a claim nor invent a source binding.
//
// Contribution carries no owner project: the emitting producer owns exactly one
// project, and BuildGeneratedEvidence stamps that project onto every mapping.
// A workload therefore cannot claim a contribution owned by one of its
// dependencies.
type GeneratedEvidenceMapping struct {
	// Feature is the authored feature ID this mapping proves part of.
	Feature string
	// Requirement is the feature-local requirement ID being supported.
	Requirement string
	// Kind is the contribution kind, from the capability v2 vocabulary.
	Kind capabilities.ContributionKind
	// Subkind is the contribution subkind, empty for the kinds that forbid one.
	Subkind string
	// Key is the contribution key.
	Key string
	// Provenance locates the declaration that authored this mapping. Its root
	// must equal the producer's source root.
	Provenance EvidenceProvenance
}

// GeneratedEvidenceInput is everything one build or test producer needs to turn
// its explicit mappings into strict evidence. It is pure data: no filesystem,
// no clock, and no discovery. The caller resolves the authored manifest, the
// emitted contribution identities, and the source binding, so the same input
// yields the same bytes in every language.
type GeneratedEvidenceInput struct {
	// Issuer is the named build or test producer accepting these assertions.
	Issuer Issuer
	// Source is the exact root state the records are interpreted against.
	// Producers select their own project root and stamp the computed binding.
	Source SourceSelector
	// Authored is the durable feature intent discovered for the producing
	// project. A nil manifest means no feature was authored there, which makes
	// every mapping an unknown-feature failure rather than a silent skip.
	Authored *Manifest
	// Contributions are the canonical identities the producer is publishing in
	// the same run. A mapping that names anything else does not resolve.
	Contributions []capabilities.ContributionIdentity
	// Mappings are the explicit associations to publish, in any order.
	Mappings []GeneratedEvidenceMapping
}

// GeneratedEvidenceID derives the stable identity of one generated record.
//
// The identity is a pure function of the association, never of declaration or
// discovery order, so re-running a producer on unchanged sources reproduces the
// same IDs and therefore the same canonical bytes. The contribution is folded
// into a digest because a contribution key is free semantic text — "GET
// /tasks/{id}", "sql:cliagg", "task.created" — and the ID grammar accepts only
// lower-case kebab segments. The exact identity stays readable in the record's
// own subject, so nothing is lost by not spelling it in the ID.
func GeneratedEvidenceID(feature, requirement string, contribution capabilities.ContributionIdentity) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		contribution.OwnerProject,
		string(contribution.Kind),
		contribution.Subkind,
		contribution.Key,
	}, "\x00")))
	return feature + "/" + requirement + "/" + hex.EncodeToString(digest[:])[:generatedEvidenceIDDigestLength]
}

// BuildGeneratedEvidence resolves explicit mappings into one canonical evidence
// document, or reports why they cannot be published.
//
// Nothing is inferred. A mapping publishes a record only when its feature, its
// requirement, and its contribution all already exist, the requirement accepts
// capability evidence, and the contribution belongs to the producing project.
// Any other outcome is a diagnostic and no document: publishing partial output
// would leave a producer's generated file claiming less than it did last run
// while looking like a complete result.
//
// Byte-identical duplicate mappings coalesce; two mappings that agree on
// identity but disagree on anything else are a contradiction and fail. Since
// provenance names the declaring call site, coalescing is only reachable for
// duplicates within one declaration's proof list — two modules stating the
// same association carry different provenance and fail, so exactly one
// declaration owns each proof.
func BuildGeneratedEvidence(input GeneratedEvidenceInput) (*EvidenceDocument, []diag.Diagnostic) {
	var diagnostics []diag.Diagnostic
	if input.Issuer.Kind != IssuerKindBuild && input.Issuer.Kind != IssuerKindTest {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidAuthority, "issuer.kind",
			"generated evidence must be issued by a build or test producer, not %q", input.Issuer.Kind))
	}
	diagnostics = append(diagnostics, validateSourceSelector("source", input.Source)...)
	if input.Source.Root != capabilities.LocationRootProject {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, "source.root",
			"generated capability evidence is interpreted against the owning project root"))
	}

	published := make(map[string]capabilities.ContributionIdentity, len(input.Contributions))
	for _, identity := range input.Contributions {
		published[contributionIdentityKey(identity)] = identity
	}

	records := make(map[string]EvidenceRecord, len(input.Mappings))
	for _, mapping := range sortedGeneratedEvidenceMappings(input.Mappings) {
		field := generatedMappingField(mapping)
		contribution := capabilities.ContributionIdentity{
			OwnerProject: input.Source.OwnerProject,
			Kind:         mapping.Kind,
			Subkind:      mapping.Subkind,
			Key:          mapping.Key,
		}
		for _, finding := range capabilities.ValidateContributionReference(contribution) {
			finding.Field = field + "." + finding.Field
			diagnostics = append(diagnostics, finding)
		}
		requirement, findings := resolveGeneratedRequirement(field, input.Authored, mapping)
		diagnostics = append(diagnostics, findings...)
		if _, ok := published[contributionIdentityKey(contribution)]; !ok {
			diagnostics = append(diagnostics, diag.Errorf(capabilities.ErrorCodeUnresolvedReference, field+".contribution",
				"no published contribution resolves identity %s", generatedIdentityLabel(contribution)))
			continue
		}
		if len(findings) > 0 {
			continue
		}
		reference := contribution
		record := EvidenceRecord{
			ID:          GeneratedEvidenceID(mapping.Feature, mapping.Requirement, contribution),
			Feature:     mapping.Feature,
			Requirement: mapping.Requirement,
			Stage:       requirement.Stage,
			Outcome:     EvidenceOutcomeSupports,
			Issuer:      input.Issuer,
			Source:      input.Source,
			Subject:     EvidenceSubject{Kind: EvidenceKindCapability, Contribution: &reference},
			Provenance:  mapping.Provenance,
		}
		if existing, duplicate := records[record.ID]; duplicate {
			if !equalGeneratedRecord(existing, record) {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateEvidence, field,
					"mapping duplicates evidence %q with different content", record.ID))
			}
			continue
		}
		records[record.ID] = record
	}

	document := &EvidenceDocument{
		Schema:          EvidenceSchemaURL,
		ProtocolVersion: EvidenceProtocolVersion,
		Evidence:        make([]EvidenceRecord, 0, len(records)),
	}
	for _, record := range records {
		document.Evidence = append(document.Evidence, record)
	}
	document = CanonicalEvidenceDocument(document)
	if document.Evidence == nil {
		// Canonicalization copies with append, which turns an empty slice back
		// into nil. The wire distinguishes "no records" from "no collection", so
		// a producer with nothing to publish must still emit an empty array.
		document.Evidence = []EvidenceRecord{}
	}
	diagnostics = append(diagnostics, ValidateEvidenceDocument(document)...)
	sortDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	return document, diagnostics
}

// resolveGeneratedRequirement reads the stage and accepted subject kinds from
// the authored requirement. A producer never chooses them: choosing the stage
// it proves and the proof at once is exactly the self-certification the
// evidence contract exists to prevent.
func resolveGeneratedRequirement(field string, authored *Manifest, mapping GeneratedEvidenceMapping) (Requirement, []diag.Diagnostic) {
	if authored == nil {
		return Requirement{}, []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownFeature, field+".feature",
			"mapping references feature %q but the project authors no feature manifest", mapping.Feature)}
	}
	manifest := CanonicalManifest(authored)
	var matches []Feature
	for _, feature := range manifest.Features {
		if feature.ID == mapping.Feature {
			matches = append(matches, feature)
		}
	}
	if len(matches) != 1 {
		return Requirement{}, []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownFeature, field+".feature",
			"mapping references feature %q, which the authored manifest does not declare exactly once", mapping.Feature)}
	}
	requirement, found := findRequirement(matches[0], mapping.Requirement)
	if !found {
		return Requirement{}, []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownRequirement, field+".requirement",
			"mapping references requirement %q, which feature %q does not declare", mapping.Requirement, mapping.Feature)}
	}
	if rank, valid := StageRank(requirement.Stage); !valid || rank == 0 {
		return Requirement{}, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidStage, field+".requirement",
			"requirement %q declares stage %q, which cannot be supported by evidence", mapping.Requirement, requirement.Stage)}
	}
	if !containsEvidenceKind(requirement.EvidenceKinds, EvidenceKindCapability) {
		return Requirement{}, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidSubject, field+".requirement",
			"requirement %q does not accept capability evidence", mapping.Requirement)}
	}
	return requirement, nil
}

// sortedGeneratedEvidenceMappings orders mappings so diagnostics report in a
// stable sequence regardless of module composition or plugin discovery order.
func sortedGeneratedEvidenceMappings(input []GeneratedEvidenceMapping) []GeneratedEvidenceMapping {
	mappings := append([]GeneratedEvidenceMapping(nil), input...)
	sort.SliceStable(mappings, func(i, j int) bool {
		return compareGeneratedMapping(mappings[i], mappings[j]) < 0
	})
	return mappings
}

func compareGeneratedMapping(left, right GeneratedEvidenceMapping) int {
	for _, pair := range [][2]string{
		{left.Feature, right.Feature},
		{left.Requirement, right.Requirement},
		{string(left.Kind), string(right.Kind)},
		{left.Subkind, right.Subkind},
		{left.Key, right.Key},
		{string(left.Provenance.Root), string(right.Provenance.Root)},
		{left.Provenance.Path, right.Provenance.Path},
		{left.Provenance.Symbol, right.Provenance.Symbol},
	} {
		if value := strings.Compare(pair[0], pair[1]); value != 0 {
			return value
		}
	}
	return 0
}

func equalGeneratedRecord(left, right EvidenceRecord) bool {
	if left.Subject.Contribution == nil || right.Subject.Contribution == nil {
		return false
	}
	if *left.Subject.Contribution != *right.Subject.Contribution {
		return false
	}
	left.Subject.Contribution, right.Subject.Contribution = nil, nil
	return left == right
}

func contributionIdentityKey(identity capabilities.ContributionIdentity) string {
	return strings.Join([]string{
		identity.OwnerProject, string(identity.Kind), identity.Subkind, identity.Key,
	}, "\x00")
}

func generatedIdentityLabel(identity capabilities.ContributionIdentity) string {
	return "(" + strings.Join([]string{
		identity.OwnerProject, string(identity.Kind), identity.Subkind, identity.Key,
	}, ",") + ")"
}

func generatedMappingField(mapping GeneratedEvidenceMapping) string {
	return "mappings[" + mapping.Feature + "/" + mapping.Requirement + "]"
}
