package architecture

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// BuildGraph aggregates valid declarations into one deterministic global view.
// Call ValidateRepository first when a consumer needs a publishable verdict.
func BuildGraph(sources []ManifestSource) Graph {
	ordered := append([]ManifestSource(nil), sources...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	graph := Graph{Domains: []DomainView{}, Edges: []DeclaredEdge{}}
	for _, source := range ordered {
		if source.Manifest == nil {
			continue
		}
		manifest := CanonicalManifest(source.Manifest)
		// The four collections are materialized rather than copied: `owns` is
		// OPTIONAL in an authored manifest and REQUIRED in the published
		// snapshot, so a domain that owns nothing must project an empty array
		// and not the absence it was authored as. The other three are already
		// required on both sides and go through the same helper so the rule
		// reads once.
		graph.Domains = append(graph.Domains, DomainView{
			ID:       manifest.Domain,
			Owner:    manifest.Owner,
			Source:   source.Path,
			Projects: orEmpty(manifest.Projects),
			Owns:     orEmpty(manifest.Owns),
			Exports:  orEmpty(manifest.Exports),
			Imports:  orEmpty(manifest.Imports),
		})
		for _, imported := range manifest.Imports {
			var local *LocalModel
			if imported.LocalModel != nil {
				copy := *imported.LocalModel
				copy.ProjectedFields = cloneStrings(imported.LocalModel.ProjectedFields)
				copy.LocalFields = cloneStrings(imported.LocalModel.LocalFields)
				local = &copy
			}
			graph.Edges = append(graph.Edges, DeclaredEdge{
				ID:             imported.ID,
				Export:         imported.From.Export,
				ProducerDomain: imported.From.Domain,
				ConsumerDomain: manifest.Domain,
				Mode:           imported.Mode,
				Status:         imported.Status,
				Facts:          orEmpty(imported.Facts),
				LocalModel:     local,
				Bindings:       cloneSlice(imported.Bindings),
			})
		}
	}
	return CanonicalGraph(graph)
}

// CompareObserved compares exact current structural facts with declared ARC
// bindings. Planned contracts carry no current binding and therefore cannot
// silence an observed legacy dependency.
func CompareObserved(graph Graph, observed []ObservedEdge) []Finding {
	graph = CanonicalGraph(graph)
	observed = canonicalObservedEdges(observed)
	type declaredBinding struct {
		edge    DeclaredEdge
		binding Binding
	}
	declared := make(map[string]declaredBinding)
	for _, edge := range graph.Edges {
		for _, binding := range edge.Bindings {
			key := bindingKey(binding.Kind, binding.ConsumerProject, binding.ProducerProject)
			declared[key] = declaredBinding{edge: edge, binding: binding}
		}
	}
	seen := make(map[string]bool, len(observed))
	findings := make([]Finding, 0)
	for _, edge := range observed {
		key := bindingKey(edge.Kind, edge.ConsumerProject, edge.ProducerProject)
		seen[key] = true
		if _, exists := declared[key]; exists {
			continue
		}
		copy := edge
		findings = append(findings, Finding{
			ID:          StableFindingID(ErrorCodeUndeclaredProjectDependency, edge),
			Code:        ErrorCodeUndeclaredProjectDependency,
			Severity:    diag.Error,
			Disposition: DispositionNew,
			Message: fmt.Sprintf(
				"project %s in domain %s depends on %s in domain %s without an exact DARC binding",
				edge.ConsumerProject, edge.ConsumerDomain, edge.ProducerProject, edge.ProducerDomain),
			Edge: &copy,
		})
	}
	for key, declaration := range declared {
		if seen[key] {
			continue
		}
		edge := ObservedEdge{
			Kind:            declaration.binding.Kind,
			ProducerDomain:  declaration.edge.ProducerDomain,
			ConsumerDomain:  declaration.edge.ConsumerDomain,
			ProducerProject: declaration.binding.ProducerProject,
			ConsumerProject: declaration.binding.ConsumerProject,
		}
		findings = append(findings, Finding{
			ID:          StableFindingID(ErrorCodeDeclaredBindingUnobserved, edge),
			Code:        ErrorCodeDeclaredBindingUnobserved,
			Severity:    diag.Error,
			Disposition: DispositionNew,
			Message: fmt.Sprintf(
				"DARC %s declares project dependency %s -> %s, but the workspace graph no longer observes it",
				declaration.edge.ID, edge.ConsumerProject, edge.ProducerProject),
			Edge: &edge,
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	return findings
}

// CompareEvidence compares framework-observed implementations with the declared
// imports they claim to implement.
//
// The two findings are deliberately asymmetric, because the two directions carry
// different certainty:
//
//   - EVIDENCE WITHOUT DECLARATION always fails. A record proves a component
//     exists; if no declaration matches its import and mode, the code is
//     enforcing a contract nobody reviewed. That is true whatever else the
//     workspace looks like.
//   - DECLARED WITHOUT EVIDENCE fails only inside a domain that ALREADY emits
//     evidence. A domain whose workloads use no framework primitive emits
//     nothing, and demanding evidence from it would report every honest
//     declaration in the repository as a violation. Once a domain implements one
//     import, every ACTIVE import it declares is expected to be implemented too —
//     partial adoption inside one domain is the state this catches.
//
// Neither finding can create a permission. Evidence is derived fact, exactly
// like an observed edge, and only a reviewed declaration authorizes anything
// (ADR 0001).
func CompareEvidence(graph Graph, evidence []EvidenceRecord) []Finding {
	graph = CanonicalGraph(graph)
	evidence = canonicalEvidence(evidence)

	declared := make(map[string]DeclaredEdge, len(graph.Edges))
	for _, edge := range graph.Edges {
		declared[evidenceKey(edge.ConsumerDomain, edge.ID, edge.Mode)] = edge
	}
	implemented := make(map[string]bool, len(evidence))
	covered := make(map[string]bool, len(evidence))
	for _, record := range evidence {
		implemented[evidenceKey(record.ConsumerDomain, record.Import, record.Mode)] = true
		covered[record.ConsumerDomain] = true
	}

	findings := make([]Finding, 0)
	for _, record := range evidence {
		if declared[evidenceKey(record.ConsumerDomain, record.Import, record.Mode)].ID != "" {
			continue
		}
		copied := record
		findings = append(findings, Finding{
			ID:          StableEvidenceFindingID(ErrorCodeEvidenceWithoutDeclaration, record),
			Code:        ErrorCodeEvidenceWithoutDeclaration,
			Severity:    diag.Error,
			Disposition: DispositionNew,
			Message: fmt.Sprintf(
				"project %s implements %s access for import %s, which domain %s does not declare",
				record.ConsumerProject, record.Mode, record.Import, record.ConsumerDomain),
			Evidence: &copied,
		})
	}
	for _, edge := range graph.Edges {
		if edge.Status != StatusActive || !covered[edge.ConsumerDomain] {
			continue
		}
		if implemented[evidenceKey(edge.ConsumerDomain, edge.ID, edge.Mode)] {
			continue
		}
		// The record describes what was EXPECTED and not found, so the reader
		// sees the missing implementation rather than only that one is missing.
		// It names no project on purpose: which project should carry it is the
		// domain's decision, not this comparison's.
		expected := EvidenceRecord{
			Kind:           EvidenceFrameworkPrimitive,
			ConsumerDomain: edge.ConsumerDomain,
			Import:         edge.ID,
			Mode:           edge.Mode,
		}
		findings = append(findings, Finding{
			ID:          StableEvidenceFindingID(ErrorCodeDeclaredWithoutEvidence, expected),
			Code:        ErrorCodeDeclaredWithoutEvidence,
			Severity:    diag.Error,
			Disposition: DispositionNew,
			Message: fmt.Sprintf(
				"domain %s declares active %s import %s and implements other imports, but nothing in it implements this one",
				edge.ConsumerDomain, edge.Mode, edge.ID),
			Evidence: &expected,
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	return findings
}

// EvidenceCoverage projects supplied evidence onto the snapshot's coverage
// report. It reports framework-evidence at most, and never claims more: a build
// recorded what a component was configured with, and no request, query, or
// delivered event was watched.
func EvidenceCoverage(evidence []EvidenceRecord) DetectionCoverage {
	coverage := DetectionCoverage{
		ProjectDependencies: CoverageEnforcedForMappedProjects,
		Database:            CoverageNotDetected,
		HTTP:                CoverageNotDetected,
		Events:              CoverageNotDetected,
		DomainAccess:        CoverageNotDetected,
	}
	for _, record := range evidence {
		coverage.DomainAccess = CoverageFrameworkEvidence
		for _, transport := range record.Transports {
			switch transport.Kind {
			case TransportAPI:
				coverage.HTTP = CoverageFrameworkEvidence
			case TransportEvent:
				coverage.Events = CoverageFrameworkEvidence
			}
		}
	}
	return coverage
}

// StableEvidenceFindingID returns a semantic, human-inspectable ID for an
// evidence finding. The consumer project is absent from the identity on purpose:
// a declared-without-evidence finding names no project, and the same missing
// implementation must keep one identity however the domain later assigns it.
func StableEvidenceFindingID(code string, record EvidenceRecord) string {
	parts := []string{
		record.ConsumerDomain,
		record.Import,
		string(record.Mode),
	}
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return code + ":" + strings.Join(parts, ":")
}

func evidenceKey(domain, importID string, mode AccessMode) string {
	return domain + "\x00" + importID + "\x00" + string(mode)
}

func canonicalEvidence(input []EvidenceRecord) []EvidenceRecord {
	// orEmpty, not cloneSlice: `evidence` is a required snapshot member, so an
	// evaluation that observed none reports an empty array rather than null.
	out := orEmpty(input)
	for index := range out {
		out[index].Transports = cloneSlice(input[index].Transports)
		sort.Slice(out[index].Transports, func(i, j int) bool {
			return out[index].Transports[i].Role < out[index].Transports[j].Role
		})
	}
	sort.Slice(out, func(i, j int) bool { return compareEvidence(out[i], out[j]) < 0 })
	return out
}

func compareEvidence(left, right EvidenceRecord) int {
	for _, pair := range [][2]string{
		{left.ConsumerDomain, right.ConsumerDomain},
		{left.Import, right.Import},
		{string(left.Mode), string(right.Mode)},
		{left.ConsumerProject, right.ConsumerProject},
	} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// StableFindingID returns a semantic, human-inspectable ID. Each component is
// escaped so project IDs containing slashes cannot create ambiguous boundaries.
func StableFindingID(code string, edge ObservedEdge) string {
	parts := []string{
		string(edge.Kind),
		edge.ConsumerDomain,
		edge.ProducerDomain,
		edge.ConsumerProject,
		edge.ProducerProject,
	}
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return code + ":" + strings.Join(parts, ":")
}

func canonicalObservedEdges(input []ObservedEdge) []ObservedEdge {
	seen := make(map[string]ObservedEdge, len(input))
	for _, edge := range input {
		key := bindingKey(edge.Kind, edge.ConsumerProject, edge.ProducerProject)
		if previous, exists := seen[key]; exists {
			// Domain labels are deterministic for a valid project mapping. Retain
			// the lexicographically smaller full record if a caller violates that
			// precondition rather than making map order observable.
			if compareObserved(edge, previous) < 0 {
				seen[key] = edge
			}
			continue
		}
		seen[key] = edge
	}
	result := make([]ObservedEdge, 0, len(seen))
	for _, edge := range seen {
		result = append(result, edge)
	}
	sort.Slice(result, func(i, j int) bool { return compareObserved(result[i], result[j]) < 0 })
	return result
}

// orEmpty projects an optional authored collection onto a required published
// one: an absent input becomes an explicit empty array, never JSON null.
//
// The distinction matters only at the projection boundary. Inside a manifest,
// absent and empty are two different statements the protocol keeps apart; in
// the snapshot, the published schema requires the member, so the reader is
// entitled to an array either way.
func orEmpty[T any](input []T) []T {
	if input == nil {
		return []T{}
	}
	return cloneSlice(input)
}
