package infra

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// OverridesFilename is the workload-only overrides file name. It lives at
// "<workload>/infra/overrides.json", a sibling of the per-project manifest.
// Like the runtime block, overrides are a workload-root concern: only the
// workload that owns the aggregated manifest decides what it ultimately
// contains, so this never appears in a library's per-project manifest.
const OverridesFilename = "overrides.json"

// Overrides declares workload-level adjustments applied to the aggregated
// manifest after Merge. The contract supports suppression only: dropping specific
// requirements the workload does not want, even though a library or
// framework generator in its dependency graph declared them. This is the
// escape hatch that keeps a workload owner in control when a generator
// over-declares.
type Overrides struct {
	Schema string       `json:"$schema,omitempty"`
	Ignore *IgnoreRules `json:"ignore,omitempty"`
}

// IgnoreRules names the requirements to drop from the aggregated manifest,
// matched by the same identity Merge uses: databases by (name, engine),
// everything else by name.
type IgnoreRules struct {
	Databases     []DatabaseRef `json:"databases,omitempty"`
	Events        *EventsRef    `json:"events,omitempty"`
	Storage       []string      `json:"storage,omitempty"`
	Secrets       []string      `json:"secrets,omitempty"`
	ScheduledJobs []string      `json:"scheduledJobs,omitempty"`
}

// DatabaseRef identifies a database by its merge identity (name, engine).
type DatabaseRef struct {
	Name   string `json:"name"`
	Engine Engine `json:"engine"`
}

// EventsRef names topics to drop from the aggregated events block.
type EventsRef struct {
	Publishes  []string `json:"publishes,omitempty"`
	Subscribes []string `json:"subscribes,omitempty"`
}

// ParseOverrides decodes a workload overrides file from JSON in strict mode
// (unknown fields rejected). It returns a non-nil value only when parsing
// produced no errors.
func ParseOverrides(data []byte) (*Overrides, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var ov Overrides
	if err := dec.Decode(&ov); err != nil {
		code := ErrorCodeParseError
		field := ""
		msg := err.Error()
		if strings.HasPrefix(msg, "json: unknown field ") {
			code = ErrorCodeUnknownField
			field = strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
		}
		return nil, []diag.Diagnostic{diag.Errorf(code, field, "%s", msg)}
	}
	return &ov, nil
}

// ValidateOverrides checks that every referenced resource name and database
// engine is well-formed, so a typo surfaces at parse time rather than as a
// silently-unmatched rule.
func ValidateOverrides(ov *Overrides) []diag.Diagnostic {
	if ov == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "overrides is nil")}
	}
	if ov.Ignore == nil {
		return nil
	}
	ig := ov.Ignore
	var diags []diag.Diagnostic
	for i, ref := range ig.Databases {
		diags = append(diags, validateDatabase(fmt.Sprintf("ignore.databases[%d]", i), ref.Name, ref.Engine)...)
	}
	if ig.Events != nil {
		for i, name := range ig.Events.Publishes {
			diags = append(diags, validateName(fmt.Sprintf("ignore.events.publishes[%d]", i), name)...)
		}
		for i, name := range ig.Events.Subscribes {
			diags = append(diags, validateName(fmt.Sprintf("ignore.events.subscribes[%d]", i), name)...)
		}
	}
	for i, name := range ig.Storage {
		diags = append(diags, validateName(fmt.Sprintf("ignore.storage[%d]", i), name)...)
	}
	for i, name := range ig.Secrets {
		diags = append(diags, validateName(fmt.Sprintf("ignore.secrets[%d]", i), name)...)
	}
	for i, name := range ig.ScheduledJobs {
		diags = append(diags, validateName(fmt.Sprintf("ignore.scheduledJobs[%d]", i), name)...)
	}
	return diags
}

// ParseAndValidateOverrides runs strict parsing followed by validation.
func ParseAndValidateOverrides(data []byte) (*Overrides, []diag.Diagnostic) {
	ov, diags := ParseOverrides(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return ov, append(diags, ValidateOverrides(ov)...)
}

// LoadOverrides reads, strict-parses, and validates a workload overrides file.
// Diagnostics are prefixed with path for caller context.
func LoadOverrides(path string) (*Overrides, []diag.Diagnostic) {
	data, err := readFile(path)
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "",
			"read workload overrides %s: %v", path, err)}
	}
	ov, diags := ParseAndValidateOverrides(data)
	prefixWithPath(diags, path)
	return ov, diags
}

// ApplyOverrides returns a copy of m with the requirements named in ov.Ignore
// removed, matched by merge identity. An ignore rule that matches nothing is
// reported as a warning-severity ErrorCodeUnusedOverride so stale rules stay
// visible. Runtime is never suppressible.
func ApplyOverrides(m AggregatedManifest, ov Overrides) (AggregatedManifest, []diag.Diagnostic) {
	if ov.Ignore == nil {
		return m, nil
	}
	ig := ov.Ignore
	var diags []diag.Diagnostic

	if len(ig.Databases) > 0 {
		matched := make([]bool, len(ig.Databases))
		kept := make([]AggregatedDatabase, 0, len(m.Databases))
		for _, db := range m.Databases {
			drop := false
			for i, ref := range ig.Databases {
				if ref.Name == db.Name && ref.Engine == db.Engine {
					drop, matched[i] = true, true
				}
			}
			if !drop {
				kept = append(kept, db)
			}
		}
		m.Databases = kept
		for i, ref := range ig.Databases {
			if !matched[i] {
				diags = append(diags, diag.Warningf(ErrorCodeUnusedOverride,
					fmt.Sprintf("ignore.databases[%d]", i),
					"ignore rule for database %q (%s) matched no aggregated requirement", ref.Name, ref.Engine))
			}
		}
	}

	if ig.Events != nil && m.Events != nil {
		pub, pubDiags := filterTopics(m.Events.Publishes, ig.Events.Publishes, "ignore.events.publishes")
		sub, subDiags := filterTopics(m.Events.Subscribes, ig.Events.Subscribes, "ignore.events.subscribes")
		diags = append(diags, pubDiags...)
		diags = append(diags, subDiags...)
		if len(pub) == 0 && len(sub) == 0 {
			m.Events = nil
		} else {
			m.Events = &AggregatedEvents{Publishes: pub, Subscribes: sub}
		}
	} else if ig.Events != nil {
		diags = append(diags, unusedNameRules("ignore.events.publishes", ig.Events.Publishes)...)
		diags = append(diags, unusedNameRules("ignore.events.subscribes", ig.Events.Subscribes)...)
	}

	if len(ig.Storage) > 0 {
		drop := nameSet(ig.Storage)
		kept := make([]AggregatedStorage, 0, len(m.Storage))
		for _, s := range m.Storage {
			if drop[s.Name] {
				delete(drop, s.Name)
				continue
			}
			kept = append(kept, s)
		}
		m.Storage = kept
		diags = append(diags, unmatchedNames("ignore.storage", ig.Storage, drop)...)
	}

	if len(ig.Secrets) > 0 {
		drop := nameSet(ig.Secrets)
		kept := make([]AggregatedSecret, 0, len(m.Secrets))
		for _, s := range m.Secrets {
			if drop[s.Name] {
				delete(drop, s.Name)
				continue
			}
			kept = append(kept, s)
		}
		m.Secrets = kept
		diags = append(diags, unmatchedNames("ignore.secrets", ig.Secrets, drop)...)
	}

	if len(ig.ScheduledJobs) > 0 {
		drop := nameSet(ig.ScheduledJobs)
		kept := make([]AggregatedScheduledJob, 0, len(m.ScheduledJobs))
		for _, j := range m.ScheduledJobs {
			if drop[j.Name] {
				delete(drop, j.Name)
				continue
			}
			kept = append(kept, j)
		}
		m.ScheduledJobs = kept
		diags = append(diags, unmatchedNames("ignore.scheduledJobs", ig.ScheduledJobs, drop)...)
	}

	return m, diags
}

// filterTopics drops aggregated topics whose name is in ignore, returning the
// kept topics and an unused-rule warning for any ignore name that matched
// nothing.
func filterTopics(topics []AggregatedTopic, ignore []string, field string) ([]AggregatedTopic, []diag.Diagnostic) {
	if len(ignore) == 0 {
		return topics, nil
	}
	drop := nameSet(ignore)
	kept := make([]AggregatedTopic, 0, len(topics))
	for _, t := range topics {
		if drop[t.Name] {
			delete(drop, t.Name)
			continue
		}
		kept = append(kept, t)
	}
	return kept, unmatchedNames(field, ignore, drop)
}

// nameSet builds a lookup set from a list of names.
func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// unmatchedNames reports a warning for each ignore name still present in the
// leftover set (i.e. that matched no aggregated entry).
func unmatchedNames(field string, names []string, leftover map[string]bool) []diag.Diagnostic {
	if len(leftover) == 0 {
		return nil
	}
	var diags []diag.Diagnostic
	for i, n := range names {
		if leftover[n] {
			diags = append(diags, diag.Warningf(ErrorCodeUnusedOverride,
				fmt.Sprintf("%s[%d]", field, i),
				"ignore rule for %q matched no aggregated requirement", n))
		}
	}
	return diags
}

// unusedNameRules reports every name as unused; used when an ignore block
// targets a kind that is entirely absent from the aggregated manifest.
func unusedNameRules(field string, names []string) []diag.Diagnostic {
	diags := make([]diag.Diagnostic, 0, len(names))
	for i, n := range names {
		diags = append(diags, diag.Warningf(ErrorCodeUnusedOverride,
			fmt.Sprintf("%s[%d]", field, i),
			"ignore rule for %q matched no aggregated requirement", n))
	}
	return diags
}
