package events

import (
	"fmt"
	"sort"

	"go.putnami.dev/app"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
)

// sidecarSlug names this producer's per-project infra scratch fragment
// (<project>/.gen/infra/events.json).
const sidecarSlug = "events"

// Describe implements app.Describer. It derives the project's event topic
// publish/subscribe requirements from the registered publishers and handlers
// and writes them to the events producer's scratch fragment at
// <ctx.OutputDir>/infra/events.json, where OutputDir is the workload's ".gen"
// directory.
//
// Topic names are validated against the infra resource-name pattern; a
// violation aborts describe with a diagnostic rather than emitting an invalid
// manifest. Projects with no registered topics emit no scratch fragment and
// any stale one is removed.
func (p *Plugin) Describe(ctx *app.DescribeContext) error {
	if !ctx.Wants(p.Name()) {
		return nil
	}
	manifest, diags := buildInfraManifest(p.publishes, p.subscribeTopics(), p.deliveryProfile())
	if diag.HasErrors(diags) {
		return fmt.Errorf("events: invalid infra topic requirements:\n%s", diag.ErrorText(diags))
	}
	var fragment infra.PerProjectManifest
	if manifest != nil {
		fragment = *manifest
	}
	return infra.WriteSidecarIn(ctx.OutputDir, sidecarSlug, fragment)
}

// subscribeTopics returns the topic names of every registered handler.
func (p *Plugin) subscribeTopics() []string {
	names := make([]string, 0, len(p.handlers))
	for _, h := range p.handlers {
		names = append(names, h.Topic)
	}
	return names
}

// buildInfraManifest derives a per-project infra manifest from the registered
// publish and subscribe topic names and the plugin's delivery mode. Names are
// deduplicated and sorted so the output is deterministic across runs; each
// subscribe carries the project's delivery (pull, the default, marshals back to
// a bare topic string so non-push projects emit the legacy shape). It returns
// (nil, nil) when no topics are registered, and a non-empty diagnostic slice
// when a topic name or delivery value is invalid.
func buildInfraManifest(publishes, subscribes []string, delivery infra.Delivery) (*infra.PerProjectManifest, []diag.Diagnostic) {
	pub := dedupeSorted(publishes)
	subTopics := dedupeSorted(subscribes)
	if len(pub) == 0 && len(subTopics) == 0 {
		return nil, nil
	}
	var subs []infra.Subscription
	for _, topic := range subTopics {
		subs = append(subs, infra.Subscription{Topic: topic, Delivery: delivery})
	}
	m := &infra.PerProjectManifest{
		Schema:          infra.PerProjectSchemaURL,
		ProtocolVersion: infra.ProtocolVersion,
		Events: &infra.Events{
			Publishes:  pub,
			Subscribes: subs,
		},
	}
	if diags := infra.ValidatePerProjectManifest(m); diag.HasErrors(diags) {
		return nil, diags
	}
	return m, nil
}

// dedupeSorted returns the unique values of names in sorted order, or nil when
// names is empty (so an omitempty JSON field stays absent).
func dedupeSorted(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
