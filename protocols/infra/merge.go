package infra

import (
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Merge aggregates per-project contributions into a single AggregatedManifest
// for workload. Callers must pre-validate each ProjectContribution.Manifest
// — Merge does not re-run strict parsing.
//
// Merge identity per resource kind:
//
//   - databases: (name, engine). Same name with two engines is two
//     entries. Schemas form a sorted-union set (additive — not a
//     conflict).
//   - events.publishes / events.subscribes: name (deduplicated as a set).
//   - storage: name. Access is unioned (read ∪ write = readwrite) and public
//     is ORed — both are additive capabilities. Conflicting non-empty
//     retentions raise ErrorCodeConflictingValue and the prior entry is kept
//     unchanged.
//   - secrets: name (string set).
//   - scheduledJobs: name. Conflicting schedule values raise
//     ErrorCodeConflictingValue; the prior entry is kept unchanged.
//
// Runtime is not populated by Merge — workload-root runtime is supplied
// via WithRuntime on the returned manifest.
//
// Output is byte-deterministic: top-level slices are sorted by name (or
// by name+engine for databases), source lists are deduped and sorted by
// (project, contributor).
func Merge(workload string, contributions []ProjectContribution) (AggregatedManifest, []diag.Diagnostic) {
	result := AggregatedManifest{
		ProtocolVersion: ProtocolVersion,
		Workload:        workload,
	}
	diags := make([]diag.Diagnostic, 0, len(contributions))

	// Sort contributions deterministically so the order in which Sources
	// appear is independent of caller order.
	ordered := make([]ProjectContribution, len(contributions))
	copy(ordered, contributions)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Project != ordered[j].Project {
			return ordered[i].Project < ordered[j].Project
		}
		return ordered[i].Contributor < ordered[j].Contributor
	})

	dbIdx := map[string]int{}
	pubIdx := map[string]int{}
	subIdx := map[string]int{}
	storageIdx := map[string]int{}
	secretIdx := map[string]int{}
	jobIdx := map[string]int{}

	// Track the precedence of the contributor that set each conflict-prone
	// scalar, so a higher-precedence contributor (manual over framework) can
	// override an inferred value deterministically.
	storageRetentionRank := map[string]int{}
	jobScheduleRank := map[string]int{}
	jobEntrypointRank := map[string]int{}

	// One helper per resource kind keeps each merge rule (identity, additive
	// unions, precedence-ranked scalar conflicts) independently testable; Merge
	// stays a short orchestration loop over the sorted contributions.
	for _, c := range ordered {
		src := Source{Project: c.Project, Contributor: c.Contributor}
		mergeDatabases(&result, dbIdx, src, c.Manifest.Databases)
		mergeEvents(&result, pubIdx, subIdx, src, c.Manifest.Events)
		diags = append(diags, mergeStorage(&result, storageIdx, storageRetentionRank, src, c.Manifest.Storage)...)
		mergeSecrets(&result, secretIdx, src, c.Manifest.Secrets)
		diags = append(diags, mergeScheduledJobs(&result, jobIdx, jobScheduleRank, jobEntrypointRank, src, c.Manifest.ScheduledJobs)...)
	}

	sortAggregated(&result)
	return result, diags
}

// mergeDatabases folds one contribution's databases into result. Identity is
// (name, engine); schemas union additively.
func mergeDatabases(result *AggregatedManifest, dbIdx map[string]int, src Source, databases []Database) {
	for _, db := range databases {
		key := db.Name + "\x00" + string(db.Engine)
		if i, ok := dbIdx[key]; ok {
			existing := &result.Databases[i]
			existing.Schemas = sortedUnion(existing.Schemas, db.Schemas)
			existing.Sources = addSource(existing.Sources, src)
			continue
		}
		schemas := append([]string(nil), db.Schemas...)
		sort.Strings(schemas)
		schemas = dedupSorted(schemas)
		result.Databases = append(result.Databases, AggregatedDatabase{
			Name:    db.Name,
			Engine:  db.Engine,
			Schemas: schemas,
			Sources: []Source{src},
		})
		dbIdx[key] = len(result.Databases) - 1
	}
}

// mergeEvents folds one contribution's publishes/subscribes into result.
// Publishes dedupe by name; subscribes dedupe by topic and merge delivery to
// the more specific model.
func mergeEvents(result *AggregatedManifest, pubIdx, subIdx map[string]int, src Source, events *Events) {
	if events == nil {
		return
	}
	for _, name := range events.Publishes {
		ensureEvents(result)
		if i, ok := pubIdx[name]; ok {
			result.Events.Publishes[i].Sources = addSource(result.Events.Publishes[i].Sources, src)
			continue
		}
		result.Events.Publishes = append(result.Events.Publishes, AggregatedTopic{
			Name:    name,
			Sources: []Source{src},
		})
		pubIdx[name] = len(result.Events.Publishes) - 1
	}
	for _, sub := range events.Subscribes {
		ensureEvents(result)
		if i, ok := subIdx[sub.Topic]; ok {
			existing := &result.Events.Subscribes[i]
			existing.Delivery = mergeDelivery(existing.Delivery, sub.Delivery)
			existing.Sources = addSource(existing.Sources, src)
			continue
		}
		result.Events.Subscribes = append(result.Events.Subscribes, AggregatedTopic{
			Name:     sub.Topic,
			Delivery: mergeDelivery("", sub.Delivery),
			Sources:  []Source{src},
		})
		subIdx[sub.Topic] = len(result.Events.Subscribes) - 1
	}
}

// mergeStorage folds one contribution's storage buckets into result. Access and
// public are additive capabilities; a conflicting non-empty retention resolves
// by contributor precedence and yields a diagnostic.
func mergeStorage(result *AggregatedManifest, storageIdx, storageRetentionRank map[string]int, src Source, buckets []StorageBucket) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for _, b := range buckets {
		if i, ok := storageIdx[b.Name]; ok {
			existing := &result.Storage[i]
			// Access and public are additive capabilities, not conflicting
			// scalars: when two projects share a bucket the grant is the
			// union of what each needs, so neither contributor under-grants
			// the other. This mirrors how database schemas merge by union.
			existing.Access = unionStorageAccess(existing.Access, b.Access)
			existing.Public = existing.Public || b.Public
			if b.Retention != "" && existing.Retention != "" && b.Retention != existing.Retention {
				incomingRank := ContributorPrecedence(src.Contributor)
				if incomingRank > storageRetentionRank[b.Name] {
					// Higher-precedence value (e.g. a manual override of a
					// framework-inferred default) wins; report it as a
					// non-fatal override for visibility.
					diags = append(diags, diag.Warningf(ErrorCodeConflictingValue,
						fmt.Sprintf("storage[%q].retention", b.Name),
						"bucket %q retention %q from %s overrides prior %q from %s",
						b.Name, b.Retention, src.Project, existing.Retention, sourcesLabel(existing.Sources)))
					existing.Retention = b.Retention
					storageRetentionRank[b.Name] = incomingRank
					existing.Sources = addSource(existing.Sources, src)
					continue
				}
				// Equal or lower precedence: the prior value stands and the
				// disagreement is a real, unresolved conflict.
				diags = append(diags, diag.Errorf(ErrorCodeConflictingValue,
					fmt.Sprintf("storage[%q].retention", b.Name),
					"bucket %q retention %q conflicts with prior %q from %s",
					b.Name, b.Retention, existing.Retention, sourcesLabel(existing.Sources)))
				continue
			}
			if existing.Retention == "" {
				existing.Retention = b.Retention
				if b.Retention != "" {
					storageRetentionRank[b.Name] = ContributorPrecedence(src.Contributor)
				}
			}
			existing.Sources = addSource(existing.Sources, src)
			continue
		}
		result.Storage = append(result.Storage, AggregatedStorage{
			Name:      b.Name,
			Access:    b.Access,
			Public:    b.Public,
			Retention: b.Retention,
			Sources:   []Source{src},
		})
		storageIdx[b.Name] = len(result.Storage) - 1
		if b.Retention != "" {
			storageRetentionRank[b.Name] = ContributorPrecedence(src.Contributor)
		}
	}
	return diags
}

// mergeSecrets folds one contribution's secrets into result as a name set.
func mergeSecrets(result *AggregatedManifest, secretIdx map[string]int, src Source, secrets []string) {
	for _, name := range secrets {
		if i, ok := secretIdx[name]; ok {
			result.Secrets[i].Sources = addSource(result.Secrets[i].Sources, src)
			continue
		}
		result.Secrets = append(result.Secrets, AggregatedSecret{
			Name:    name,
			Sources: []Source{src},
		})
		secretIdx[name] = len(result.Secrets) - 1
	}
}

// mergeScheduledJobs folds one contribution's scheduled jobs into result.
// Conflicting schedule/entrypoint scalars resolve by contributor precedence and
// yield diagnostics.
func mergeScheduledJobs(result *AggregatedManifest, jobIdx, jobScheduleRank, jobEntrypointRank map[string]int, src Source, jobs []ScheduledJob) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for _, j := range jobs {
		incomingRank := ContributorPrecedence(src.Contributor)
		if i, ok := jobIdx[j.Name]; ok {
			existing := &result.ScheduledJobs[i]
			if j.Schedule != existing.Schedule {
				if incomingRank <= jobScheduleRank[j.Name] {
					diags = append(diags, diag.Errorf(ErrorCodeConflictingValue,
						fmt.Sprintf("scheduledJobs[%q].schedule", j.Name),
						"job %q schedule %q conflicts with prior %q from %s",
						j.Name, j.Schedule, existing.Schedule, sourcesLabel(existing.Sources)))
					continue
				}
				diags = append(diags, diag.Warningf(ErrorCodeConflictingValue,
					fmt.Sprintf("scheduledJobs[%q].schedule", j.Name),
					"job %q schedule %q from %s overrides prior %q from %s",
					j.Name, j.Schedule, src.Project, existing.Schedule, sourcesLabel(existing.Sources)))
				existing.Schedule = j.Schedule
				jobScheduleRank[j.Name] = incomingRank
			}
			if j.Entrypoint != "" && existing.Entrypoint != "" && j.Entrypoint != existing.Entrypoint {
				if incomingRank <= jobEntrypointRank[j.Name] {
					diags = append(diags, diag.Errorf(ErrorCodeConflictingValue,
						fmt.Sprintf("scheduledJobs[%q].entrypoint", j.Name),
						"job %q entrypoint %q conflicts with prior %q from %s",
						j.Name, j.Entrypoint, existing.Entrypoint, sourcesLabel(existing.Sources)))
					continue
				}
				diags = append(diags, diag.Warningf(ErrorCodeConflictingValue,
					fmt.Sprintf("scheduledJobs[%q].entrypoint", j.Name),
					"job %q entrypoint %q from %s overrides prior %q from %s",
					j.Name, j.Entrypoint, src.Project, existing.Entrypoint, sourcesLabel(existing.Sources)))
				existing.Entrypoint = j.Entrypoint
				jobEntrypointRank[j.Name] = incomingRank
			}
			if existing.Entrypoint == "" {
				existing.Entrypoint = j.Entrypoint
				if j.Entrypoint != "" {
					jobEntrypointRank[j.Name] = incomingRank
				}
			}
			existing.Sources = addSource(existing.Sources, src)
			continue
		}
		result.ScheduledJobs = append(result.ScheduledJobs, AggregatedScheduledJob{
			Name:       j.Name,
			Schedule:   j.Schedule,
			Entrypoint: j.Entrypoint,
			Sources:    []Source{src},
		})
		jobIdx[j.Name] = len(result.ScheduledJobs) - 1
		jobScheduleRank[j.Name] = incomingRank
		if j.Entrypoint != "" {
			jobEntrypointRank[j.Name] = incomingRank
		}
	}
	return diags
}

// WithRuntime returns a copy of m with Runtime set to runtime. It is the
// caller's responsibility to provide the workload-root Runtime block;
// Merge() does not infer it from library contributions.
func WithRuntime(m AggregatedManifest, runtime *Runtime) AggregatedManifest {
	m.Runtime = runtime
	return m
}

// ensureEvents lazily initializes the Events block on the aggregated
// manifest so it stays nil when no project contributes events.
func ensureEvents(m *AggregatedManifest) {
	if m.Events == nil {
		m.Events = &AggregatedEvents{}
	}
}

// addSource appends src to sources, deduplicating by (project,
// contributor), and returns the result sorted by the same key.
func addSource(sources []Source, src Source) []Source {
	for _, s := range sources {
		if s.Project == src.Project && s.Contributor == src.Contributor {
			return sources
		}
	}
	sources = append(sources, src)
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Project != sources[j].Project {
			return sources[i].Project < sources[j].Project
		}
		return sources[i].Contributor < sources[j].Contributor
	})
	return sources
}

// sortedUnion returns the union of a and b as a sorted, deduplicated
// slice.
func sortedUnion(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		seen[v] = true
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// dedupSorted removes adjacent duplicates from a sorted slice.
func dedupSorted(s []string) []string {
	if len(s) < 2 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// deliveryRank orders delivery models by how specific a provisioning need they
// express. push (a provider push subscription) outranks stream, which outranks
// the pull default (and the equivalent empty value). It lets mergeDelivery
// resolve a topic subscribed with different deliveries across projects
// deterministically, independent of contributor order.
func deliveryRank(d Delivery) int {
	switch d {
	case DeliveryPush:
		return 3
	case DeliveryStream:
		return 2
	default: // pull or the empty default
		return 0
	}
}

// mergeDelivery returns the more specific of two delivery models, canonicalizing
// the pull default back to the empty value so an aggregated pull subscription
// stays absent on the wire (byte-identical to the pre-push shape).
func mergeDelivery(a, b Delivery) Delivery {
	if deliveryRank(b) > deliveryRank(a) {
		a = b
	}
	if deliveryRank(a) == 0 {
		return ""
	}
	return a
}

// unionStorageAccess returns the least access grant that covers both a and b.
// Access is an additive capability over the lattice {read, write} → readwrite,
// so two projects that share a bucket get the union of what each needs
// (read ∪ write = readwrite, x ∪ readwrite = readwrite). An empty access
// contributes nothing, so it is the identity element: "" ∪ x = x.
func unionStorageAccess(a, b StorageAccess) StorageAccess {
	read := a == StorageAccessRead || a == StorageAccessReadWrite ||
		b == StorageAccessRead || b == StorageAccessReadWrite
	write := a == StorageAccessWrite || a == StorageAccessReadWrite ||
		b == StorageAccessWrite || b == StorageAccessReadWrite
	switch {
	case read && write:
		return StorageAccessReadWrite
	case read:
		return StorageAccessRead
	case write:
		return StorageAccessWrite
	default:
		return ""
	}
}

// sortAggregated sorts every top-level slice on m by its primary
// identity for byte-deterministic output.
func sortAggregated(m *AggregatedManifest) {
	sort.SliceStable(m.Databases, func(i, j int) bool {
		if m.Databases[i].Name != m.Databases[j].Name {
			return m.Databases[i].Name < m.Databases[j].Name
		}
		return m.Databases[i].Engine < m.Databases[j].Engine
	})
	if m.Events != nil {
		sort.SliceStable(m.Events.Publishes, func(i, j int) bool {
			return m.Events.Publishes[i].Name < m.Events.Publishes[j].Name
		})
		sort.SliceStable(m.Events.Subscribes, func(i, j int) bool {
			return m.Events.Subscribes[i].Name < m.Events.Subscribes[j].Name
		})
	}
	sort.SliceStable(m.Storage, func(i, j int) bool {
		return m.Storage[i].Name < m.Storage[j].Name
	})
	sort.SliceStable(m.Secrets, func(i, j int) bool {
		return m.Secrets[i].Name < m.Secrets[j].Name
	})
	sort.SliceStable(m.ScheduledJobs, func(i, j int) bool {
		return m.ScheduledJobs[i].Name < m.ScheduledJobs[j].Name
	})
}

// ContributorPrecedence ranks a contributor for scalar-conflict resolution.
// Developer-authored ("manual") declarations outrank framework-generated
// ones, so a workload owner keeps control and can override a value a
// framework inferred. Same-rank disagreements fall back to deterministic
// first-by-sort resolution and are reported as unresolved conflicts.
//
// Exported so external tooling can rank contributors the same way Merge
// does — for instance, a deployer audit tool that needs to explain which
// of two competing values would win at aggregation time.
func ContributorPrecedence(c ContributorID) int {
	if c == ContributorManual {
		return 1
	}
	return 0
}

// sourcesLabel renders a sources slice as a compact "[project (contributor), ...]"
// string suitable for diagnostic messages.
func sourcesLabel(sources []Source) string {
	if len(sources) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(sources))
	for _, s := range sources {
		parts = append(parts, fmt.Sprintf("%s (%s)", s.Project, s.Contributor))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
