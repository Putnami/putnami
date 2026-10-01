package architecture

import (
	"encoding/json"
	"sort"
)

// CanonicalManifest returns a deeply copied, identity-sorted declaration.
func CanonicalManifest(input *Manifest) *Manifest {
	if input == nil {
		return nil
	}
	out := *input
	out.Projects = cloneStrings(input.Projects)
	sort.Strings(out.Projects)
	out.Owns = cloneSlice(input.Owns)
	sort.Slice(out.Owns, func(i, j int) bool { return out.Owns[i].ID < out.Owns[j].ID })
	out.Exports = cloneSlice(input.Exports)
	for index := range out.Exports {
		out.Exports[index].Facts = cloneSlice(input.Exports[index].Facts)
		sort.Slice(out.Exports[index].Facts, func(i, j int) bool {
			return out.Exports[index].Facts[i].Name < out.Exports[index].Facts[j].Name
		})
		out.Exports[index].Modes = cloneSlice(input.Exports[index].Modes)
		sort.Slice(out.Exports[index].Modes, func(i, j int) bool {
			return out.Exports[index].Modes[i] < out.Exports[index].Modes[j]
		})
	}
	sort.Slice(out.Exports, func(i, j int) bool { return out.Exports[i].ID < out.Exports[j].ID })
	out.Imports = cloneSlice(input.Imports)
	for index := range out.Imports {
		out.Imports[index] = cloneImport(input.Imports[index])
	}
	sort.Slice(out.Imports, func(i, j int) bool { return out.Imports[i].ID < out.Imports[j].ID })
	return &out
}

func cloneImport(input Import) Import {
	out := input
	out.Facts = cloneStrings(input.Facts)
	sort.Strings(out.Facts)
	if input.Transport != nil {
		copy := *input.Transport
		out.Transport = &copy
	}
	if input.Bootstrap != nil {
		copy := *input.Bootstrap
		out.Bootstrap = &copy
	}
	if input.Updates != nil {
		copy := *input.Updates
		out.Updates = &copy
	}
	if input.Consistency != nil {
		copy := *input.Consistency
		out.Consistency = &copy
	}
	if input.Deletion != nil {
		copy := *input.Deletion
		out.Deletion = &copy
	}
	if input.LocalModel != nil {
		copy := *input.LocalModel
		copy.ProjectedFields = cloneStrings(input.LocalModel.ProjectedFields)
		sort.Strings(copy.ProjectedFields)
		copy.LocalFields = cloneStrings(input.LocalModel.LocalFields)
		sort.Strings(copy.LocalFields)
		out.LocalModel = &copy
	}
	out.Bindings = cloneSlice(input.Bindings)
	sort.Slice(out.Bindings, func(i, j int) bool {
		return compareBinding(out.Bindings[i], out.Bindings[j]) < 0
	})
	return out
}

// CanonicalBaseline returns a sorted copy without mutating debt metadata.
func CanonicalBaseline(input *Baseline) *Baseline {
	if input == nil {
		return nil
	}
	out := *input
	out.Findings = cloneSlice(input.Findings)
	sort.Slice(out.Findings, func(i, j int) bool { return out.Findings[i].Finding < out.Findings[j].Finding })
	return &out
}

// CanonicalWaiverFile returns a finding-ID-sorted copy.
func CanonicalWaiverFile(input *WaiverFile) *WaiverFile {
	if input == nil {
		return nil
	}
	out := *input
	out.Waivers = cloneSlice(input.Waivers)
	sort.Slice(out.Waivers, func(i, j int) bool { return out.Waivers[i].Finding < out.Waivers[j].Finding })
	return &out
}

// CanonicalGraph returns a deeply copied, identity-sorted global graph.
func CanonicalGraph(input Graph) Graph {
	out := Graph{
		Domains: cloneSlice(input.Domains),
		Edges:   cloneSlice(input.Edges),
	}
	for index := range out.Domains {
		domain := &out.Domains[index]
		domain.Projects = cloneStrings(input.Domains[index].Projects)
		sort.Strings(domain.Projects)
		domain.Owns = cloneSlice(input.Domains[index].Owns)
		sort.Slice(domain.Owns, func(i, j int) bool { return domain.Owns[i].ID < domain.Owns[j].ID })
		domain.Exports = cloneSlice(input.Domains[index].Exports)
		for exportIndex := range domain.Exports {
			domain.Exports[exportIndex].Facts = cloneSlice(input.Domains[index].Exports[exportIndex].Facts)
			sort.Slice(domain.Exports[exportIndex].Facts, func(i, j int) bool {
				return domain.Exports[exportIndex].Facts[i].Name < domain.Exports[exportIndex].Facts[j].Name
			})
			domain.Exports[exportIndex].Modes = cloneSlice(input.Domains[index].Exports[exportIndex].Modes)
			sort.Slice(domain.Exports[exportIndex].Modes, func(i, j int) bool {
				return domain.Exports[exportIndex].Modes[i] < domain.Exports[exportIndex].Modes[j]
			})
		}
		sort.Slice(domain.Exports, func(i, j int) bool { return domain.Exports[i].ID < domain.Exports[j].ID })
		domain.Imports = cloneSlice(input.Domains[index].Imports)
		for importIndex := range domain.Imports {
			domain.Imports[importIndex] = cloneImport(input.Domains[index].Imports[importIndex])
		}
		sort.Slice(domain.Imports, func(i, j int) bool { return domain.Imports[i].ID < domain.Imports[j].ID })
	}
	sort.Slice(out.Domains, func(i, j int) bool { return out.Domains[i].ID < out.Domains[j].ID })
	for index := range out.Edges {
		out.Edges[index].Facts = cloneStrings(input.Edges[index].Facts)
		sort.Strings(out.Edges[index].Facts)
		out.Edges[index].Bindings = cloneSlice(input.Edges[index].Bindings)
		sort.Slice(out.Edges[index].Bindings, func(i, j int) bool {
			return compareBinding(out.Edges[index].Bindings[i], out.Edges[index].Bindings[j]) < 0
		})
		if input.Edges[index].LocalModel != nil {
			copy := *input.Edges[index].LocalModel
			copy.ProjectedFields = cloneStrings(input.Edges[index].LocalModel.ProjectedFields)
			sort.Strings(copy.ProjectedFields)
			copy.LocalFields = cloneStrings(input.Edges[index].LocalModel.LocalFields)
			sort.Strings(copy.LocalFields)
			out.Edges[index].LocalModel = &copy
		}
	}
	sort.Slice(out.Edges, func(i, j int) bool { return out.Edges[i].ID < out.Edges[j].ID })
	return out
}

// CanonicalSnapshot returns a deeply copied stable snapshot.
func CanonicalSnapshot(input *Snapshot) *Snapshot {
	if input == nil {
		return nil
	}
	out := *input
	out.Graph = CanonicalGraph(input.Graph)
	out.Observed = cloneSlice(input.Observed)
	sort.Slice(out.Observed, func(i, j int) bool { return compareObserved(out.Observed[i], out.Observed[j]) < 0 })
	out.Evidence = canonicalEvidence(input.Evidence)
	out.Findings = cloneSlice(input.Findings)
	for index := range out.Findings {
		if input.Findings[index].Edge != nil {
			copy := *input.Findings[index].Edge
			out.Findings[index].Edge = &copy
		}
		if input.Findings[index].Evidence != nil {
			copy := *input.Findings[index].Evidence
			copy.Transports = cloneSlice(input.Findings[index].Evidence.Transports)
			out.Findings[index].Evidence = &copy
		}
	}
	sort.Slice(out.Findings, func(i, j int) bool { return out.Findings[i].ID < out.Findings[j].ID })
	return &out
}

// MarshalManifest emits canonical two-space-indented JSON with one trailing newline.
func MarshalManifest(value *Manifest) ([]byte, error) {
	return marshalCanonical(CanonicalManifest(value))
}

// MarshalBaseline emits canonical baseline bytes.
func MarshalBaseline(value *Baseline) ([]byte, error) {
	return marshalCanonical(CanonicalBaseline(value))
}

// MarshalWaiverFile emits canonical waiver bytes.
func MarshalWaiverFile(value *WaiverFile) ([]byte, error) {
	return marshalCanonical(CanonicalWaiverFile(value))
}

// MarshalSnapshot emits canonical deterministic machine bytes.
func MarshalSnapshot(value *Snapshot) ([]byte, error) {
	return marshalCanonical(CanonicalSnapshot(value))
}

func marshalCanonical(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// cloneSlice copies a slice while PRESERVING the difference between an absent
// collection and an empty one.
//
// `append([]T(nil), input...)` looks like the same thing and is not: for a
// non-nil input of length zero it returns nil, so a manifest that declares
// `"exports": []` — a domain that publishes nothing, which the protocol requires
// to be stated — canonicalizes to `"exports": null`, and the strict reader then
// refuses the canonical writer's own output. Every clone below goes through
// this function for that reason.
func cloneSlice[T any](input []T) []T {
	if input == nil {
		return nil
	}
	out := make([]T, len(input))
	copy(out, input)
	return out
}

func cloneStrings(input []string) []string { return cloneSlice(input) }

func compareBinding(left, right Binding) int {
	if left.Kind != right.Kind {
		if left.Kind < right.Kind {
			return -1
		}
		return 1
	}
	if left.ConsumerProject != right.ConsumerProject {
		if left.ConsumerProject < right.ConsumerProject {
			return -1
		}
		return 1
	}
	if left.ProducerProject < right.ProducerProject {
		return -1
	}
	if left.ProducerProject > right.ProducerProject {
		return 1
	}
	return 0
}

func compareObserved(left, right ObservedEdge) int {
	if left.Kind != right.Kind {
		if left.Kind < right.Kind {
			return -1
		}
		return 1
	}
	for _, pair := range [][2]string{
		{left.ConsumerDomain, right.ConsumerDomain},
		{left.ProducerDomain, right.ProducerDomain},
		{left.ConsumerProject, right.ConsumerProject},
		{left.ProducerProject, right.ProducerProject},
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
