package infra

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	pevents "go.putnami.dev/protocol/events"
)

// Error codes for strict parsing and validation. Tooling and runtime
// compliance suites key off these — keep them in sync with
// ValidErrorCodes below.
const (
	ErrorCodeParseError             = "infra.parse_error"
	ErrorCodeUnknownField           = "infra.unknown_field"
	ErrorCodeInvalidName            = "infra.invalid_name"
	ErrorCodeInvalidEngine          = "infra.invalid_engine"
	ErrorCodeInvalidAccess          = "infra.invalid_access"
	ErrorCodeInvalidDelivery        = "infra.invalid_delivery"
	ErrorCodeInvalidProtocolVersion = "infra.invalid_protocol_version"
	ErrorCodeInvalidContributor     = "infra.invalid_contributor"
	ErrorCodeRuntimeInLibrary       = "infra.runtime_in_library"
	ErrorCodeMissingWorkload        = "infra.missing_workload"
	ErrorCodeMissingSources         = "infra.missing_sources"
	ErrorCodeInvalidScaling         = "infra.invalid_scaling"
	ErrorCodeEmptySchedule          = "infra.empty_schedule"
	ErrorCodeInvalidSchedule        = "infra.invalid_schedule"
	ErrorCodeConflictingValue       = "infra.conflicting_value"
	ErrorCodeUnusedOverride         = "infra.unused_override"
	ErrorCodeBuildInvalidated       = "infra.build_invalidated"
)

// ValidErrorCodes enumerates the canonical infra-protocol error taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:             true,
	ErrorCodeUnknownField:           true,
	ErrorCodeInvalidName:            true,
	ErrorCodeInvalidEngine:          true,
	ErrorCodeInvalidAccess:          true,
	ErrorCodeInvalidDelivery:        true,
	ErrorCodeInvalidProtocolVersion: true,
	ErrorCodeInvalidContributor:     true,
	ErrorCodeRuntimeInLibrary:       true,
	ErrorCodeMissingWorkload:        true,
	ErrorCodeMissingSources:         true,
	ErrorCodeInvalidScaling:         true,
	ErrorCodeEmptySchedule:          true,
	ErrorCodeInvalidSchedule:        true,
	ErrorCodeConflictingValue:       true,
	ErrorCodeUnusedOverride:         true,
	ErrorCodeBuildInvalidated:       true,
}

// resourceNamePattern matches a canonical resource identifier. We reuse
// the same shape as the platform protocol's probe name (lowercase
// letters, digits, '-', '_', '.', '/'; 1–64 chars) so resource names
// look uniform across protocols.
var resourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_./-]{0,63}$`)

// ParsePerProjectManifest decodes a per-project requirements manifest
// from JSON in strict mode (unknown fields rejected). It returns a
// non-nil manifest only when parsing produced no errors.
//
// Runtime concerns must not appear in per-project manifests; if the
// input includes a top-level "runtime" key, the returned diagnostic
// uses ErrorCodeRuntimeInLibrary instead of the generic unknown-field
// rejection. Nested "runtime" keys inside arbitrary unknown structures
// are not misclassified — the check inspects only the top-level object.
func ParsePerProjectManifest(data []byte) (*PerProjectManifest, []diag.Diagnostic) {
	// Top-level "runtime" gets the typed runtime_in_library code so
	// tooling can distinguish it from other unknown-field rejections.
	// We pre-parse into a raw map so nested keys named "runtime" inside
	// other unknown structures don't trip a regex-based sniff.
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err == nil {
		if _, hasRuntime := topLevel["runtime"]; hasRuntime {
			return nil, []diag.Diagnostic{diag.Errorf(
				ErrorCodeRuntimeInLibrary, "runtime",
				"runtime block is workload-only and must not appear in a per-project manifest",
			)}
		}
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m PerProjectManifest
	if err := dec.Decode(&m); err != nil {
		return nil, DecodeDiagnostics(err)
	}
	return &m, nil
}

// ParseAggregatedManifest decodes an aggregated requirements manifest
// from JSON in strict mode. It returns a non-nil manifest only when
// parsing produced no errors.
func ParseAggregatedManifest(data []byte) (*AggregatedManifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m AggregatedManifest
	if err := dec.Decode(&m); err != nil {
		return nil, DecodeDiagnostics(err)
	}
	return &m, nil
}

// DecodeDiagnostics maps a strict encoding/json decode failure onto the
// protocol's typed diagnostics. An unknown field becomes
// ErrorCodeUnknownField carrying the field name; a field this protocol
// retired (see RemovedRuntimeFields) additionally carries the reason and the
// migration, because "unknown field \"min\"" tells an author nothing about
// why their value stopped being read.
//
// Exported so the readers outside this package that strict-decode a fragment
// of the same wire format — the build aggregator's runtime.json reader — map
// their failures the same way.
func DecodeDiagnostics(err error) []diag.Diagnostic {
	msg := err.Error()
	if !strings.HasPrefix(msg, "json: unknown field ") {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "%s", msg)}
	}
	field := strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`)
	if reason, removed := RemovedRuntimeFields[field]; removed {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownField, field,
			"%s was removed in infra protocol %d: %s — %s",
			field, ProtocolVersion, reason, MigrationGuide)}
	}
	return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownField, field, "%s", msg)}
}

// ValidatePerProjectManifest checks structural invariants on a parsed
// per-project manifest. It returns one diagnostic per finding.
func ValidatePerProjectManifest(m *PerProjectManifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}

	diags := validateProtocolVersion(m.ProtocolVersion)
	return append(diags, validateResources(m.Databases, m.Events, m.Storage, m.Secrets, m.ScheduledJobs)...)
}

// ValidateAggregatedManifest checks structural invariants on a parsed
// aggregated manifest, including the workload identifier and every
// Source.Contributor.
func ValidateAggregatedManifest(m *AggregatedManifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "manifest is nil")}
	}

	diags := make([]diag.Diagnostic, 0, len(m.Databases)+len(m.Storage)+len(m.Secrets)+len(m.ScheduledJobs))
	diags = append(diags, validateProtocolVersion(m.ProtocolVersion)...)

	if strings.TrimSpace(m.Workload) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeMissingWorkload, "workload",
			"aggregated manifest is missing the workload identifier"))
	}

	for i, db := range m.Databases {
		field := fmt.Sprintf("databases[%d]", i)
		diags = append(diags, validateDatabase(field, db.Name, db.Engine)...)
		diags = append(diags, validateSources(field+".sources", db.Sources)...)
	}
	if m.Events != nil {
		for i, t := range m.Events.Publishes {
			field := fmt.Sprintf("events.publishes[%d]", i)
			diags = append(diags, validateName(field+".name", t.Name)...)
			diags = append(diags, validateSources(field+".sources", t.Sources)...)
		}
		for i, t := range m.Events.Subscribes {
			field := fmt.Sprintf("events.subscribes[%d]", i)
			diags = append(diags, validateName(field+".name", t.Name)...)
			diags = append(diags, validateDelivery(field+".delivery", t.Delivery)...)
			diags = append(diags, validateSources(field+".sources", t.Sources)...)
		}
	}
	for i, b := range m.Storage {
		field := fmt.Sprintf("storage[%d]", i)
		diags = append(diags, validateStorageBucket(field, b.Name, b.Access)...)
		diags = append(diags, validateSources(field+".sources", b.Sources)...)
	}
	for i, s := range m.Secrets {
		field := fmt.Sprintf("secrets[%d]", i)
		diags = append(diags, validateName(field+".name", s.Name)...)
		diags = append(diags, validateSources(field+".sources", s.Sources)...)
	}
	for i, j := range m.ScheduledJobs {
		field := fmt.Sprintf("scheduledJobs[%d]", i)
		diags = append(diags, validateScheduledJob(field, j.Name, j.Schedule)...)
		diags = append(diags, validateSources(field+".sources", j.Sources)...)
	}
	diags = append(diags, validateRuntime("runtime", m.Runtime)...)
	return diags
}

// validateProtocolVersion checks that v is the supported protocol version.
// A manifest left at an earlier version gets migration instructions rather
// than the bare mismatch: the reader cannot repair it, so the diagnostic has
// to say what the author must do. See MigrationGuide.
func validateProtocolVersion(v int) []diag.Diagnostic {
	if v == ProtocolVersion {
		return nil
	}
	if v >= 1 && v < ProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is an earlier infra protocol (this parser reads %d) and contributes nothing until it is migrated: %s",
			v, ProtocolVersion, MigrationGuide)}
	}
	return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
		"protocolVersion %d is not supported by this parser (want %d)", v, ProtocolVersion)}
}

// validateResources runs per-kind structural validation that is common
// between per-project and aggregated manifests.
func validateResources(dbs []Database, events *Events, storage []StorageBucket, secrets []string, jobs []ScheduledJob) []diag.Diagnostic {
	diags := make([]diag.Diagnostic, 0, len(dbs)+len(storage)+len(secrets)+len(jobs))
	for i, db := range dbs {
		diags = append(diags, validateDatabase(fmt.Sprintf("databases[%d]", i), db.Name, db.Engine)...)
	}
	if events != nil {
		for i, t := range events.Publishes {
			diags = append(diags, validateName(fmt.Sprintf("events.publishes[%d]", i), t)...)
		}
		for i, t := range events.Subscribes {
			field := fmt.Sprintf("events.subscribes[%d]", i)
			diags = append(diags, validateName(field, t.Topic)...)
			diags = append(diags, validateDelivery(field+".delivery", t.Delivery)...)
		}
	}
	for i, b := range storage {
		diags = append(diags, validateStorageBucket(fmt.Sprintf("storage[%d]", i), b.Name, b.Access)...)
	}
	for i, s := range secrets {
		diags = append(diags, validateName(fmt.Sprintf("secrets[%d]", i), s)...)
	}
	for i, j := range jobs {
		diags = append(diags, validateScheduledJob(fmt.Sprintf("scheduledJobs[%d]", i), j.Name, j.Schedule)...)
	}
	return diags
}

func validateDatabase(field, name string, engine Engine) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, validateName(field+".name", name)...)
	if !ValidEngines[engine] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidEngine, field+".engine",
			"engine %q is not an accepted engine: %s", engine, strings.Join(EngineNames(), ", ")))
	}
	return diags
}

// validateStorageBucket validates a bucket's name and, when set, its access
// level. Access is optional (an empty value declares no grant), so only a
// non-empty value out of the closed set is rejected — the same shape as the
// storage protocol's own access validation.
func validateStorageBucket(field, name string, access StorageAccess) []diag.Diagnostic {
	diags := validateName(field+".name", name)
	if access != "" && !ValidStorageAccess[access] {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidAccess, field+".access",
			"access %q is not an accepted access level: %s", access, strings.Join(StorageAccessNames(), ", ")))
	}
	return diags
}

func validateScheduledJob(field, name, schedule string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	diags = append(diags, validateName(field+".name", name)...)
	if strings.TrimSpace(schedule) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeEmptySchedule, field+".schedule",
			"scheduledJobs[%q].schedule must be a non-empty cron expression", name))
		return diags
	}
	if err := validateCronExpression(schedule); err != nil {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidSchedule, field+".schedule",
			"scheduledJobs[%q].schedule %q is not a valid 5-field cron expression: %v", name, schedule, err))
	}
	return diags
}

// Cron validation targets the Cloud Scheduler (unix-cron) grammar: five
// whitespace-separated fields, no seconds field and no @macros. This is
// the lowest-common-denominator that survives across deploy targets, so
// generate-time validation catches expressions the deployer would later
// reject (extra fields like "0 0 1 1 1 1 1", or macros like "@every 5m").
var cronFieldNames = [5]string{"minute", "hour", "day-of-month", "month", "day-of-week"}

// cronFieldSpec bounds the numeric range of one cron field and lists any
// case-insensitive name aliases it accepts (month and day-of-week only).
type cronFieldSpec struct {
	min, max int
	names    map[string]int
}

var cronFieldSpecs = [5]cronFieldSpec{
	{min: 0, max: 59},
	{min: 0, max: 23},
	{min: 1, max: 31},
	{min: 1, max: 12, names: cronMonthNames},
	{min: 0, max: 7, names: cronWeekdayNames}, // 0 and 7 both mean Sunday
}

var cronMonthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var cronWeekdayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

// validateCronExpression reports whether expr is a well-formed 5-field
// Cloud Scheduler cron string. It returns a descriptive error on the first
// problem found, or nil when every field parses within its bounds.
func validateCronExpression(expr string) error {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("expected 5 space-separated fields, got %d", len(fields))
	}
	for i, f := range fields {
		if err := validateCronField(f, cronFieldSpecs[i]); err != nil {
			return fmt.Errorf("%s: %w", cronFieldNames[i], err)
		}
	}
	return nil
}

// validateCronField validates one field, which is a comma-separated list
// of parts. Each part is "*", a single value, a "lo-hi" range, or any of
// those followed by a "/step" increment.
func validateCronField(field string, spec cronFieldSpec) error {
	for _, part := range strings.Split(field, ",") {
		if err := validateCronPart(part, spec); err != nil {
			return err
		}
	}
	return nil
}

func validateCronPart(part string, spec cronFieldSpec) error {
	if part == "" {
		return fmt.Errorf("empty list element")
	}

	base := part
	if slash := strings.IndexByte(part, '/'); slash >= 0 {
		base = part[:slash]
		step, err := strconv.Atoi(part[slash+1:])
		if err != nil || step < 1 {
			return fmt.Errorf("invalid step %q", part[slash+1:])
		}
		// A step larger than the field's maximum can only ever match the
		// first value, which Cloud Scheduler rejects rather than silently
		// collapsing (e.g. "*/60" in the minute field).
		if step > spec.max {
			return fmt.Errorf("step %d exceeds field maximum %d", step, spec.max)
		}
	}

	if base == "*" {
		return nil
	}

	if dash := strings.IndexByte(base, '-'); dash >= 0 {
		lo, err := resolveCronValue(base[:dash], spec)
		if err != nil {
			return err
		}
		hi, err := resolveCronValue(base[dash+1:], spec)
		if err != nil {
			return err
		}
		if lo > hi {
			return fmt.Errorf("range %q is descending", base)
		}
		return nil
	}

	_, err := resolveCronValue(base, spec)
	return err
}

// resolveCronValue maps a single token (a number or a recognized name) to
// its numeric value and checks it against the field's bounds.
func resolveCronValue(tok string, spec cronFieldSpec) (int, error) {
	if spec.names != nil {
		if v, ok := spec.names[strings.ToLower(tok)]; ok {
			return v, nil
		}
	}
	n, err := strconv.Atoi(tok)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", tok)
	}
	if n < spec.min || n > spec.max {
		return 0, fmt.Errorf("value %d out of range %d-%d", n, spec.min, spec.max)
	}
	return n, nil
}

// validateDelivery checks that a subscription's delivery, when set, is a known
// delivery model. The empty value is the back-compatible pull default and is
// always allowed.
func validateDelivery(field string, d Delivery) []diag.Diagnostic {
	if d == "" || pevents.ValidateDeliveryProfile(d) {
		return nil
	}
	return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDelivery, field,
		"delivery %q is not in the set: pull, stream, push", d)}
}

// validateName validates a resource name against the canonical
// resourceNamePattern. Empty names are reported as invalid.
func validateName(field, name string) []diag.Diagnostic {
	if name == "" {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, field, "name is required")}
	}
	if !resourceNamePattern.MatchString(name) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidName, field,
			"name %q does not match canonical pattern %q", name, resourceNamePattern.String())}
	}
	return nil
}

// validateSources checks that the sources list is non-empty (every
// aggregated entry must carry provenance) and that each entry has a
// non-empty Project and a canonical Contributor identifier.
func validateSources(field string, sources []Source) []diag.Diagnostic {
	if len(sources) == 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeMissingSources, field,
			"aggregated entry must declare at least one source")}
	}
	diags := make([]diag.Diagnostic, 0, len(sources))
	for i, s := range sources {
		entry := fmt.Sprintf("%s[%d]", field, i)
		if strings.TrimSpace(s.Project) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidContributor, entry+".project",
				"source project is required"))
		}
		diags = append(diags, ValidateContributor(entry+".contributor", s.Contributor)...)
	}
	return diags
}

// validateRuntime checks that the workload runtime block, if present,
// is structurally valid: scaling ceilings are non-negative integers when
// supplied.
func validateRuntime(field string, rt *Runtime) []diag.Diagnostic {
	if rt == nil || rt.Scaling == nil {
		return nil
	}
	var diags []diag.Diagnostic
	if rt.Scaling.Max != nil && *rt.Scaling.Max < 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidScaling, field+".scaling.max",
			"scaling.max %d must be >= 0", *rt.Scaling.Max))
	}
	if rt.Scaling.Concurrency != nil && *rt.Scaling.Concurrency < 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidScaling, field+".scaling.concurrency",
			"scaling.concurrency %d must be >= 0", *rt.Scaling.Concurrency))
	}
	return diags
}

// ValidateContributor checks that c is either ContributorManual or a
// well-formed framework contributor ("framework:<name>").
func ValidateContributor(field string, c ContributorID) []diag.Diagnostic {
	if c == ContributorManual {
		return nil
	}
	if strings.HasPrefix(string(c), frameworkPrefix) && len(c) > len(frameworkPrefix) {
		return nil
	}
	return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidContributor, field,
		"contributor %q must be %q or %q", c, ContributorManual, frameworkPrefix+"<name>")}
}

// ParseAndValidatePerProjectManifest is a convenience that runs strict
// parsing followed by structural validation.
func ParseAndValidatePerProjectManifest(data []byte) (*PerProjectManifest, []diag.Diagnostic) {
	m, diags := ParsePerProjectManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return m, append(diags, ValidatePerProjectManifest(m)...)
}

// ParseAndValidateAggregatedManifest is a convenience that runs strict
// parsing followed by structural validation.
func ParseAndValidateAggregatedManifest(data []byte) (*AggregatedManifest, []diag.Diagnostic) {
	m, diags := ParseAggregatedManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return m, append(diags, ValidateAggregatedManifest(m)...)
}
