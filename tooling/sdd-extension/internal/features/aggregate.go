package features

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspaceproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const (
	contributionSourceCurrent     = "current"
	contributionSourceUnavailable = "unavailable"
)

type evidenceOccurrence struct {
	path   string
	field  string
	record featureproto.EvidenceRecord
}

type authoredFeature struct {
	feature featureproto.Feature
	path    string
	field   string
}

// Aggregate performs strict bounded discovery, exact contribution resolution,
// and deterministic maturity assessment. Any structural error suppresses the
// snapshot; assessment warnings remain visible beside successful output.
func Aggregate(request Request) Result {
	if request.Revision.Kind == "" {
		request.Revision.Kind = RevisionKindWorktree
	}
	discovered := discover(request)
	summary := ScopeSummary{
		Scoped:           request.Scope.Active(),
		SelectedRoots:    request.Scope.Roots(),
		SelectedFeatures: discovered.selected,
		ExternalRecords:  discovered.external,
	}
	// finalize is the ONE place a scoped run decides which findings it owns.
	// Validation still runs over the workspace-wide identity tier — that is what
	// keeps duplicate detection exact — and attribution afterwards drops only the
	// findings whose artifact belongs to no selected feature.
	finalize := func(input []diag.Diagnostic) []diag.Diagnostic {
		return normalizeDiagnostics(scopeDiagnostics(discovered, input))
	}
	diagnostics := append([]diag.Diagnostic(nil), discovered.diagnostics...)
	diagnostics = append(diagnostics, validateRevision(request.Revision)...)
	diagnostics = append(diagnostics, featureproto.ValidateRepository(discovered.manifests, discovered.evidence)...)
	diagnostics = append(diagnostics, validateSnapshotMetadataBounds(discovered.manifests, discovered.evidence)...)

	catalog := buildContributionCatalog(discovered.capabilities)
	diagnostics = append(diagnostics, catalog.diagnostics...)
	occurrences := flattenEvidence(discovered.evidence)
	diagnostics = append(diagnostics, validateEvidenceContributionReferences(occurrences, catalog, discovered.capabilities)...)
	if scoped := finalize(diagnostics); diag.HasErrors(scoped) {
		return Result{Diagnostics: scoped, Scope: summary}
	}

	bindings := newBindingResolver(request.Workspace, request.Reader)
	enriched := make([]ContributionAssessment, 0, len(catalog.all))
	enrichedByIdentity := make(map[string]ContributionAssessment, len(catalog.byIdentity))
	for _, contribution := range catalog.all {
		current, findings := bindings.enrichContribution(contribution)
		diagnostics = append(diagnostics, findings...)
		enriched = append(enriched, current)
		if current.Referenceable {
			enrichedByIdentity[contributionKey(current.Identity)] = current
		}
	}

	byRequirement := make(map[string][]evidenceOccurrence)
	referenced := make(map[string]bool)
	for _, occurrence := range occurrences {
		key := occurrence.record.Feature + "\x00" + occurrence.record.Requirement
		byRequirement[key] = append(byRequirement[key], occurrence)
		if reference := occurrence.record.Subject.Contribution; reference != nil {
			referenced[contributionKey(*reference)] = true
		}
	}

	selected := selectedFeatureSet(discovered)
	var authored []authoredFeature
	for _, source := range discovered.manifests {
		manifest := featureproto.CanonicalManifest(source.Manifest)
		for index, feature := range manifest.Features {
			if selected != nil && !selected[feature.ID] {
				// The identity tier read this declaration so duplicates and
				// relation targets stay exact; assessing it would put a feature the
				// caller did not select into the answer.
				continue
			}
			authored = append(authored, authoredFeature{
				feature: feature,
				path:    source.Path,
				field:   fmt.Sprintf("%s#features[%d]", source.Path, index),
			})
		}
	}
	sort.Slice(authored, func(i, j int) bool { return authored[i].feature.ID < authored[j].feature.ID })

	assessments := make([]FeatureAssessment, 0, len(authored))
	for _, declaration := range authored {
		assessment, findings := assessFeature(declaration.feature, declaration.path, declaration.field, byRequirement, enrichedByIdentity, bindings)
		diagnostics = append(diagnostics, findings...)
		assessments = append(assessments, assessment)
	}

	// Project-level featureAuthority answers the same ownership question as
	// contribution evidence, but only for a root that authors no feature of its
	// own. Keep both indexes workspace-wide so scoped evaluation never changes
	// whether an authority resolves.
	authoredIDs, authoredRoots := authoredFeatureAuthority(discovered.manifests)
	reportedUnresolvedAuthority := make(map[string]bool)
	unclassified := make([]ContributionAssessment, 0)
	for _, contribution := range enriched {
		if contribution.Referenceable && referenced[contributionKey(contribution.Identity)] {
			continue
		}
		project := contributionOwnerProject(request.Workspace, contribution.Identity.OwnerProject)
		if project != nil && !authoredRoots[project.Path] {
			authority := projectFeatureAuthority(project)
			owner, none := "", ""
			if authority != nil {
				owner = strings.TrimSpace(authority.Owner)
				none = strings.TrimSpace(authority.None)
			}
			switch {
			case owner == "" && none != "":
				continue
			case owner != "" && none == "" && authoredIDs[owner]:
				continue
			case owner != "" && none == "" && !reportedUnresolvedAuthority[project.Path]:
				reportedUnresolvedAuthority[project.Path] = true
				name := project.Name
				if name == "" {
					name = project.ID
				}
				diagnostics = append(diagnostics, diag.Warningf(
					featureproto.WarningCodeUnresolvedFeatureAuthority,
					project.Path,
					"project %q claims feature %q owns its contributions, but no authored feature has that id",
					name,
					owner,
				))
			}
		}
		unclassified = append(unclassified, contribution)
		field := "capabilities#contribution"
		if len(contribution.Containers) > 0 {
			field = contribution.Containers[0] + "#contribution"
		}
		diagnostics = append(diagnostics, diag.Warningf(
			featureproto.WarningCodeUnclassifiedContribution,
			field,
			"technical contribution %s has no feature evidence",
			formatIdentity(contribution.Identity),
		))
	}

	diagnostics = finalize(diagnostics)
	if diag.HasErrors(diagnostics) {
		return Result{Diagnostics: diagnostics, Scope: summary}
	}
	snapshot := canonicalSnapshot(&Snapshot{
		Compatibility:  SnapshotCompatibility,
		Revision:       request.Revision,
		SourceBindings: bindings.snapshotBindings(),
		Features:       assessments,
		Unclassified:   unclassified,
	})
	return Result{Snapshot: snapshot, Diagnostics: diagnostics, Scope: summary}
}

// authoredFeatureAuthority indexes only durable declarations. A project root
// with at least one authored feature must map each technical contribution with
// evidence; featureAuthority cannot turn that finer-grained check off.
func authoredFeatureAuthority(manifests []featureproto.ManifestSource) (map[string]bool, map[string]bool) {
	ids := make(map[string]bool)
	roots := make(map[string]bool)
	for _, source := range manifests {
		manifest := featureproto.CanonicalManifest(source.Manifest)
		for _, feature := range manifest.Features {
			ids[feature.ID] = true
			roots[featureManifestRoot(source.Path)] = true
		}
	}
	return ids, roots
}

func contributionOwnerProject(ws *workspace.Workspace, owner string) *workspace.Project {
	if ws == nil || owner == "" {
		return nil
	}
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		if owner == project.Name || owner == project.ID || owner == project.SourceName {
			return project
		}
	}
	return nil
}

func projectFeatureAuthority(project *workspace.Project) *workspaceproto.ProjectFeatureAuthority {
	if project == nil || project.Config == nil {
		return nil
	}
	return project.Config.FeatureAuthority
}

// selectedFeatureSet returns the identities a scoped run assesses, or nil when
// the run is unscoped and assesses every authored feature.
func selectedFeatureSet(discovered discoveredRepository) map[string]bool {
	if discovered.outOfScope == nil || discovered.selected == nil {
		return nil
	}
	selected := make(map[string]bool, len(discovered.selected))
	for _, id := range discovered.selected {
		selected[id] = true
	}
	return selected
}

// scopeDiagnostics drops the findings a scoped run does not own.
//
// Attribution is by ARTIFACT, not by message text: every discovery and protocol
// finding names its source as "<workspace path>" or "<workspace path>#<field>",
// so the artifact a finding came from is a fact rather than a heuristic. Only
// artifacts this run actually read and then classified as belonging to no
// selected feature are dropped; a field with no artifact (revision, workspace,
// projects) is workspace-level and always kept, and so is any artifact the run
// followed for a selected feature — which is how an external error that
// invalidates a selected feature stays visible with its own provenance.
func scopeDiagnostics(discovered discoveredRepository, input []diag.Diagnostic) []diag.Diagnostic {
	if len(discovered.outOfScope) == 0 {
		return input
	}
	kept := make([]diag.Diagnostic, 0, len(input))
	for _, finding := range input {
		artifact, _, _ := strings.Cut(finding.Field, "#")
		if discovered.outOfScope[artifact] {
			continue
		}
		kept = append(kept, finding)
	}
	return kept
}

func flattenEvidence(sources []featureproto.EvidenceSource) []evidenceOccurrence {
	var result []evidenceOccurrence
	for _, source := range sources {
		document := featureproto.CanonicalEvidenceDocument(source.Document)
		for index, record := range document.Evidence {
			result = append(result, evidenceOccurrence{
				path:   source.Path,
				field:  fmt.Sprintf("%s#evidence[%d]", source.Path, index),
				record: record,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i].record, result[j].record
		if left.Feature != right.Feature {
			return left.Feature < right.Feature
		}
		leftRank, _ := featureproto.StageRank(left.Stage)
		rightRank, _ := featureproto.StageRank(right.Stage)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.Requirement != right.Requirement {
			return left.Requirement < right.Requirement
		}
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		return result[i].path < result[j].path
	})
	return result
}

func validateSnapshotMetadataBounds(manifests []featureproto.ManifestSource, evidence []featureproto.EvidenceSource) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	check := func(field, value string, maximum int) {
		if len(value) > maximum {
			diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeSensitiveContent, field, "semantic metadata exceeds the bounded snapshot limit"))
		}
	}
	for _, source := range manifests {
		check(source.Path, source.Path, maxProtocolPathBytes)
		manifest := featureproto.CanonicalManifest(source.Manifest)
		check(source.Path+"#namespace", manifest.Namespace, maxSemanticMetadataBytes)
		for featureIndex, feature := range manifest.Features {
			field := fmt.Sprintf("%s#features[%d]", source.Path, featureIndex)
			check(field+".id", feature.ID, maxSemanticMetadataBytes)
			for relationIndex, relation := range feature.Relations {
				check(fmt.Sprintf("%s.relations[%d].target", field, relationIndex), relation.Target, maxSemanticMetadataBytes)
			}
			for requirementIndex, requirement := range feature.Requirements {
				check(fmt.Sprintf("%s.requirements[%d].id", field, requirementIndex), requirement.ID, maxSemanticMetadataBytes)
			}
		}
	}
	for _, source := range evidence {
		check(source.Path, source.Path, maxProtocolPathBytes)
		document := featureproto.CanonicalEvidenceDocument(source.Document)
		for evidenceIndex, record := range document.Evidence {
			field := fmt.Sprintf("%s#evidence[%d]", source.Path, evidenceIndex)
			check(field+".id", record.ID, maxSemanticMetadataBytes)
			check(field+".feature", record.Feature, maxSemanticMetadataBytes)
			check(field+".requirement", record.Requirement, maxSemanticMetadataBytes)
			check(field+".provenance.path", record.Provenance.Path, maxProtocolPathBytes)
			check(field+".provenance.symbol", record.Provenance.Symbol, maxSemanticMetadataBytes)
			if contribution := record.Subject.Contribution; contribution != nil {
				check(field+".subject.contribution.ownerProject", contribution.OwnerProject, maxSemanticMetadataBytes)
				check(field+".subject.contribution.subkind", contribution.Subkind, maxSemanticMetadataBytes)
				check(field+".subject.contribution.key", contribution.Key, maxSemanticMetadataBytes)
			}
			if artifact := record.Subject.Artifact; artifact != nil {
				check(field+".subject.artifact.path", artifact.Path, maxProtocolPathBytes)
			}
		}
	}
	return diagnostics
}

func validateEvidenceContributionReferences(occurrences []evidenceOccurrence, catalog contributionCatalog, containers []capabilityproto.ManifestContainer) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	for _, occurrence := range occurrences {
		reference := occurrence.record.Subject.Contribution
		if occurrence.record.Subject.Kind != featureproto.EvidenceKindCapability || reference == nil {
			continue
		}
		key := contributionKey(*reference)
		if _, ok := catalog.byIdentity[key]; ok {
			continue
		}
		findings := catalog.resolutionErrors[key]
		if len(findings) == 0 {
			_, findings = capabilityproto.ResolveContribution(*reference, containers)
		}
		for _, finding := range findings {
			finding.Field = occurrence.field + ".subject.contribution"
			diagnostics = append(diagnostics, finding)
		}
	}
	return diagnostics
}

func assessFeature(
	feature featureproto.Feature,
	source string,
	featureField string,
	byRequirement map[string][]evidenceOccurrence,
	contributions map[string]ContributionAssessment,
	bindings *bindingResolver,
) (FeatureAssessment, []diag.Diagnostic) {
	targetRank, _ := featureproto.StageRank(feature.Target)
	assessment := FeatureAssessment{
		ID:        feature.ID,
		Type:      feature.Type,
		Name:      feature.Name,
		Outcome:   feature.Outcome,
		Owner:     feature.Owner,
		Source:    source,
		Target:    feature.Target,
		Current:   featureproto.MaturityModeled,
		Relations: append([]featureproto.Relation(nil), feature.Relations...),
	}
	var diagnostics []diag.Diagnostic
	activeHumanByStage := make(map[featureproto.MaturityStage]bool)
	for requirementIndex, requirement := range feature.Requirements {
		rank, _ := featureproto.StageRank(requirement.Stage)
		requirementAssessment := RequirementAssessment{
			ID:            requirement.ID,
			Stage:         requirement.Stage,
			EvidenceKinds: append([]featureproto.EvidenceKind(nil), requirement.EvidenceKinds...),
			Claimed:       rank <= targetRank,
		}
		occurrences := byRequirement[feature.ID+"\x00"+requirement.ID]
		var activeSupport, activeContradiction, staleSupport bool
		for _, occurrence := range occurrences {
			evidence, findings := assessEvidence(occurrence, contributions, bindings)
			diagnostics = append(diagnostics, findings...)
			requirementAssessment.Evidence = append(requirementAssessment.Evidence, evidence)
			if evidence.State == EvidenceActive && occurrence.record.Issuer.Kind == featureproto.IssuerKindHuman && occurrence.record.Outcome == featureproto.EvidenceOutcomeSupports {
				activeHumanByStage[requirement.Stage] = true
			}
			if occurrence.record.Outcome == featureproto.EvidenceOutcomeSupports {
				if evidence.State == EvidenceActive {
					activeSupport = true
				} else {
					staleSupport = true
				}
			} else if evidence.State == EvidenceActive {
				activeContradiction = true
			}
		}
		field := fmt.Sprintf("%s.requirements[%d]", featureField, requirementIndex)
		switch {
		case activeContradiction:
			requirementAssessment.State = VerificationContradicted
			diagnostics = append(diagnostics, diag.Warningf(featureproto.WarningCodeContradictedEvidence, field, "requirement %q has active contradictory evidence", requirement.ID))
		case activeSupport:
			requirementAssessment.State = VerificationVerified
		case staleSupport:
			requirementAssessment.State = VerificationStale
			diagnostics = append(diagnostics, diag.Warningf(featureproto.WarningCodeStaleEvidence, field, "requirement %q has only stale supporting evidence", requirement.ID))
		default:
			requirementAssessment.State = VerificationMissing
			diagnostics = append(diagnostics, diag.Warningf(featureproto.WarningCodeMissingEvidence, field, "requirement %q has no supporting evidence", requirement.ID))
		}
		assessment.Requirements = append(assessment.Requirements, requirementAssessment)
	}

	for _, stage := range featureproto.OrderedMaturityStages()[1:] {
		rank, _ := featureproto.StageRank(stage)
		if rank > targetRank {
			break
		}
		if (stage == featureproto.MaturityDesignPartnerProven || stage == featureproto.MaturityGA) && !activeHumanByStage[stage] {
			diagnostics = append(diagnostics, diag.Warningf(
				featureproto.WarningCodeMissingHumanAuthority,
				featureField+".target",
				"maturity stage %q requires active human-issued support",
				stage,
			))
		}
	}

	for _, stage := range featureproto.OrderedMaturityStages()[1:] {
		rank, _ := featureproto.StageRank(stage)
		if rank > targetRank {
			break
		}
		earned := true
		for _, requirement := range assessment.Requirements {
			if requirement.Stage == stage && requirement.State != VerificationVerified {
				earned = false
			}
		}
		if stage == featureproto.MaturityDesignPartnerProven || stage == featureproto.MaturityGA {
			if !activeHumanByStage[stage] {
				earned = false
			}
		}
		if !earned {
			break
		}
		assessment.Current = stage
	}
	return assessment, diagnostics
}

func assessEvidence(occurrence evidenceOccurrence, contributions map[string]ContributionAssessment, bindings *bindingResolver) (EvidenceAssessment, []diag.Diagnostic) {
	record := occurrence.record
	assessment := EvidenceAssessment{
		ID:                         record.ID,
		Document:                   occurrence.path,
		Outcome:                    record.Outcome,
		Issuer:                     record.Issuer,
		Source:                     record.Source,
		Subject:                    record.Subject,
		Provenance:                 record.Provenance,
		Persistent:                 record.Persistent,
		ObservedAt:                 record.ObservedAt,
		ObservedRepositoryRevision: record.ObservedRepositoryRevision,
		State:                      EvidenceActive,
	}
	if record.Persistent {
		return assessment, nil
	}
	var diagnostics []diag.Diagnostic
	current := bindings.resolve(record.Source)
	if current.err != nil {
		assessment.StaleReasons = append(assessment.StaleReasons, StaleSourceBindingUnavailable)
		diagnostics = append(diagnostics, diag.Warningf(
			featureproto.ErrorCodeSourceBindingUnavailable,
			occurrence.field+".source",
			"source binding is unavailable for evidence %q",
			record.ID,
		))
	} else if current.binding != record.Source.Binding {
		assessment.StaleReasons = append(assessment.StaleReasons, StaleSourceBindingMismatch)
	}

	switch record.Subject.Kind {
	case featureproto.EvidenceKindCapability:
		if record.Subject.Contribution != nil {
			contribution := contributions[contributionKey(*record.Subject.Contribution)]
			copy := canonicalContribution(contribution)
			assessment.Contribution = &copy
			if contribution.SourceState != contributionSourceCurrent || contribution.CurrentSourceBinding == "" || contribution.CurrentSourceBinding != record.Source.Binding {
				assessment.StaleReasons = append(assessment.StaleReasons, StaleContributionBinding)
			}
		}
	case featureproto.EvidenceKindArtifact:
		if record.Subject.Artifact != nil && current.err == nil {
			data, err := bindings.reader.ReadFile(current.rootPath, record.Subject.Artifact.Path)
			if err != nil {
				if structural := artifactReadDiagnostic(occurrence.field+".subject.artifact.path", err); structural != nil {
					diagnostics = append(diagnostics, *structural)
				} else {
					assessment.StaleReasons = append(assessment.StaleReasons, StaleArtifactUnavailable)
				}
			} else if len(data) > MaxReadBytes {
				diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeSensitiveContent, occurrence.field+".subject.artifact.path", "referenced artifact exceeds the bounded read limit"))
			} else {
				digest := sha256.Sum256(data)
				actual := "sha256:" + hex.EncodeToString(digest[:])
				if actual != record.Subject.Artifact.Digest {
					assessment.StaleReasons = append(assessment.StaleReasons, StaleArtifactDigest)
				}
			}
		}
	}
	assessment.StaleReasons = uniqueSortedStrings(assessment.StaleReasons)
	if len(assessment.StaleReasons) > 0 {
		assessment.State = EvidenceStale
	}
	return assessment, diagnostics
}

func artifactReadDiagnostic(field string, err error) *diag.Diagnostic {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	kind, ok := readerErrorKind(err)
	if !ok {
		return nil
	}
	var finding diag.Diagnostic
	switch kind {
	case ReaderErrorPathEscape:
		finding = diag.Errorf(featureproto.ErrorCodePathEscape, field, "artifact path escapes its selected source root")
	case ReaderErrorSymlinkEscape:
		finding = diag.Errorf(featureproto.ErrorCodeSymlinkEscape, field, "artifact symlink escapes its selected source root")
	case ReaderErrorOutsideWorkspace:
		finding = diag.Errorf(featureproto.ErrorCodeOutsideDiscoveryRoot, field, "artifact resolves outside the selected workspace")
	case ReaderErrorReadLimit, ReaderErrorEvidenceFileLimit:
		finding = diag.Errorf(featureproto.ErrorCodeSensitiveContent, field, "artifact exceeds a bounded feature-reader limit")
	case ReaderErrorInvalidPath, ReaderErrorUnsupportedFile:
		finding = diag.Errorf(featureproto.ErrorCodeInvalidPath, field, "artifact is not one contained regular file")
	default:
		return nil
	}
	return &finding
}

type bindingResult struct {
	selector SourceBinding
	rootPath string
	binding  string
	err      error
}

type bindingResolver struct {
	reader   Reader
	projects map[string][]*workspace.Project
	packages map[string][]*workspace.Project
	cache    map[string]bindingResult
}

func newBindingResolver(ws *workspace.Workspace, reader Reader) *bindingResolver {
	resolver := &bindingResolver{
		reader:   reader,
		projects: make(map[string][]*workspace.Project),
		packages: make(map[string][]*workspace.Project),
		cache:    make(map[string]bindingResult),
	}
	if ws == nil {
		return resolver
	}
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		resolver.projects[project.Name] = append(resolver.projects[project.Name], project)
		resolver.packages[project.Name+"\x00"+project.Version] = append(resolver.packages[project.Name+"\x00"+project.Version], project)
	}
	return resolver
}

func (resolver *bindingResolver) resolve(source featureproto.SourceSelector) bindingResult {
	selector := SourceBinding{Root: source.Root, OwnerProject: source.OwnerProject, Package: source.Package, Version: source.Version}
	key := bindingKey(selector)
	if cached, ok := resolver.cache[key]; ok {
		return cached
	}
	result := bindingResult{selector: selector}
	switch source.Root {
	case capabilityproto.LocationRootWorkspace:
		result.rootPath = ""
	case capabilityproto.LocationRootProject:
		projects := resolver.projects[source.OwnerProject]
		if len(projects) != 1 {
			result.err = errors.New("project source binding unavailable")
		} else {
			result.rootPath = projects[0].Path
		}
	case capabilityproto.LocationRootPackage:
		// Two callers reach this branch with different selector shapes. An
		// evidence SourceSelector carries an exact package AND version, so the
		// versioned key stays primary. A capability Provenance does not:
		// canonicalization clears Provenance.Version ("producers never emit it in
		// the durable manifest"), so it always arrives empty while the workspace
		// keys projects by name AND version — the versioned key could never match
		// and every package-root contribution reported "source binding
		// unavailable" regardless of the tree. Resolve that shape by name, under
		// the same exactly-one-match rule.
		projects := resolver.packages[source.Package+"\x00"+source.Version]
		if len(projects) == 0 && source.Version == "" {
			projects = resolver.projects[source.Package]
		}
		if len(projects) != 1 {
			result.err = errors.New("package source binding unavailable")
		} else {
			result.rootPath = projects[0].Path
		}
	default:
		result.err = errors.New("source binding root unavailable")
	}
	if result.err == nil {
		result.binding, result.err = resolver.reader.SourceBinding(result.rootPath)
	}
	result.selector.Binding = result.binding
	result.selector.Unavailable = result.err != nil
	resolver.cache[key] = result
	return result
}

func (resolver *bindingResolver) enrichContribution(input ContributionAssessment) (ContributionAssessment, []diag.Diagnostic) {
	out := canonicalContribution(input)
	source := featureproto.SourceSelector{}
	if input.ProtocolVersion == capabilityproto.ProtocolVersionV2 && input.Provenance.Declaration != nil {
		source.Root = input.Provenance.Declaration.Root
		switch source.Root {
		case capabilityproto.LocationRootProject:
			source.OwnerProject = input.Identity.OwnerProject
		case capabilityproto.LocationRootPackage:
			source.Package = input.Provenance.Package
			source.Version = input.Provenance.Version
		}
	} else if input.Provenance.Package != "" && input.Provenance.Version != "" {
		source.Root = capabilityproto.LocationRootPackage
		source.Package = input.Provenance.Package
		source.Version = input.Provenance.Version
	} else {
		source.Root = capabilityproto.LocationRootProject
		source.OwnerProject = input.Identity.OwnerProject
	}
	current := resolver.resolve(source)
	out.CurrentSourceBinding = current.binding
	field := "capabilities#provenance"
	if len(out.Containers) > 0 {
		field = out.Containers[0] + "#provenance"
	}
	if current.err != nil {
		out.SourceState = contributionSourceUnavailable
		return out, []diag.Diagnostic{diag.Warningf(
			capabilityproto.ErrorCodeSourceBindingUnavailable,
			field,
			"source binding is unavailable for contribution %s",
			formatIdentity(out.Identity),
		)}
	}
	out.SourceState = contributionSourceCurrent
	return out, nil
}

func (resolver *bindingResolver) snapshotBindings() []SourceBinding {
	bindings := make([]SourceBinding, 0, len(resolver.cache))
	for _, result := range resolver.cache {
		bindings = append(bindings, result.selector)
	}
	sort.Slice(bindings, func(i, j int) bool { return compareSourceBinding(bindings[i], bindings[j]) < 0 })
	if bindings == nil {
		return []SourceBinding{}
	}
	return bindings
}

func bindingKey(selector SourceBinding) string {
	return string(selector.Root) + "\x00" + selector.OwnerProject + "\x00" + selector.Package + "\x00" + selector.Version
}

func formatIdentity(identity capabilityproto.ContributionIdentity) string {
	return fmt.Sprintf("(%q,%q,%q,%q)", identity.OwnerProject, identity.Kind, identity.Subkind, identity.Key)
}

func uniqueSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func normalizeDiagnostics(input []diag.Diagnostic) []diag.Diagnostic {
	result := append([]diag.Diagnostic(nil), input...)
	for index := range result {
		result[index].Field = boundedDiagnosticText(result[index].Field, 1024)
		result[index].Message = boundedDiagnosticText(result[index].Message, 2048)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if severityRank(left.Severity) != severityRank(right.Severity) {
			return severityRank(left.Severity) < severityRank(right.Severity)
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		return left.Message < right.Message
	})
	return result
}

func boundedDiagnosticText(value string, maximum int) string {
	value = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, value)
	if len(value) <= maximum {
		return value
	}
	cutoff := maximum - 3
	for cutoff > 0 && !utf8.ValidString(value[:cutoff]) {
		cutoff--
	}
	return value[:cutoff] + "..."
}

func severityRank(severity diag.Severity) int {
	switch severity {
	case diag.Error:
		return 0
	case diag.Warning:
		return 1
	default:
		return 2
	}
}

var revisionPattern = regexp.MustCompile(`^git:(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func validateRevision(revision Revision) []diag.Diagnostic {
	switch revision.Kind {
	case RevisionKindWorktree:
		if revision.Commit != "" || (revision.Head != "" && !revisionPattern.MatchString(revision.Head)) {
			return []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeInvalidSubject, "revision", "worktree revision metadata is malformed")}
		}
	case RevisionKindGit:
		if revision.Head != "" || !revisionPattern.MatchString(revision.Commit) {
			return []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeInvalidSubject, "revision", "immutable Git revision metadata is malformed")}
		}
	default:
		return []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeInvalidSubject, "revision.kind", "revision kind is not supported")}
	}
	return nil
}
