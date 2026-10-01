package sdd

import (
	"sort"

	archproto "go.putnami.dev/protocol/architecture"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// DetectProjectDependencies extracts exact cross-domain dependencies from the
// already-resolved Putnami graph. Edges with an unmapped endpoint stay outside
// v1 coverage rather than being guessed from paths or names.
func DetectProjectDependencies(ws *workspace.Workspace, sources []archproto.ManifestSource) []archproto.ObservedEdge {
	if ws == nil || ws.Graph == nil {
		return nil
	}
	domainByProject := make(map[string]string)
	for _, source := range sources {
		if source.Manifest == nil {
			continue
		}
		for _, project := range source.Manifest.Projects {
			domainByProject[project] = source.Manifest.Domain
		}
	}
	var observed []archproto.ObservedEdge
	for _, consumer := range ws.Projects {
		if consumer == nil {
			continue
		}
		consumerDomain, mapped := domainByProject[consumer.ID]
		if !mapped {
			continue
		}
		for _, producerID := range ws.Graph.DependenciesOf(consumer.ID) {
			producerDomain, mapped := domainByProject[producerID]
			if !mapped || producerDomain == consumerDomain {
				continue
			}
			observed = append(observed, archproto.ObservedEdge{
				Kind:            archproto.BindingProjectDependency,
				ProducerDomain:  producerDomain,
				ConsumerDomain:  consumerDomain,
				ProducerProject: producerID,
				ConsumerProject: consumer.ID,
			})
		}
	}
	sort.Slice(observed, func(i, j int) bool {
		left, right := observed[i], observed[j]
		if left.ConsumerDomain != right.ConsumerDomain {
			return left.ConsumerDomain < right.ConsumerDomain
		}
		if left.ProducerDomain != right.ProducerDomain {
			return left.ProducerDomain < right.ProducerDomain
		}
		if left.ConsumerProject != right.ConsumerProject {
			return left.ConsumerProject < right.ConsumerProject
		}
		return left.ProducerProject < right.ProducerProject
	})
	return observed
}
