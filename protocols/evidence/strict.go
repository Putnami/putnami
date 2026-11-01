package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Validation diagnostic codes. ParseAndValidate and ValidateDocument emit only
// these.
const (
	ErrorCodeParseError                 = "evidence.parse_error"
	ErrorCodeUnknownField               = "evidence.unknown_field"
	ErrorCodeUnsupportedProtocolVersion = "evidence.unsupported_protocol_version"
	ErrorCodeRequired                   = "evidence.required"
	ErrorCodeInvalidSubject             = "evidence.invalid_subject"
	ErrorCodeInvalidCI                  = "evidence.invalid_ci"
	ErrorCodeInvalidPath                = "evidence.invalid_path"
	ErrorCodeInvalidComponent           = "evidence.invalid_component"
	ErrorCodeDuplicate                  = "evidence.duplicate"
	ErrorCodeUnknownComponent           = "evidence.unknown_component"
	ErrorCodeInvalidImpact              = "evidence.invalid_impact"
	ErrorCodeInvalidCount               = "evidence.invalid_count"
	ErrorCodeInvalidDependencyChange    = "evidence.invalid_dependency_change"
	ErrorCodeInvalidTimestamp           = "evidence.invalid_timestamp"
	ErrorCodeInvalidDiagnostic          = "evidence.invalid_diagnostic"
	ErrorCodeTooLarge                   = "evidence.too_large"
)

// ValidErrorCodes is the closed validation-code taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:                 true,
	ErrorCodeUnknownField:               true,
	ErrorCodeUnsupportedProtocolVersion: true,
	ErrorCodeRequired:                   true,
	ErrorCodeInvalidSubject:             true,
	ErrorCodeInvalidCI:                  true,
	ErrorCodeInvalidPath:                true,
	ErrorCodeInvalidComponent:           true,
	ErrorCodeDuplicate:                  true,
	ErrorCodeUnknownComponent:           true,
	ErrorCodeInvalidImpact:              true,
	ErrorCodeInvalidCount:               true,
	ErrorCodeInvalidDependencyChange:    true,
	ErrorCodeInvalidTimestamp:           true,
	ErrorCodeInvalidDiagnostic:          true,
	ErrorCodeTooLarge:                   true,
}

var (
	shaPattern       = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	projectIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)
	slugPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	codePattern      = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)+$`)
	// timestampPattern is RFC 3339 with an optional fraction and a zone.
	timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`)
)

// member describes the members an object may carry. A nil member is a scalar;
// items describes the elements of an array member.
type member struct {
	fields map[string]*member
	items  *member
}

func object(fields map[string]*member) *member { return &member{fields: fields} }
func array(items *member) *member              { return &member{items: items} }

var documentShape = object(map[string]*member{
	"protocolVersion": nil,
	"subject": object(map[string]*member{
		"provider": nil, "host": nil, "projectId": nil, "repository": nil,
		"sha": nil, "baseSha": nil, "branch": nil,
		"changeRequest": object(map[string]*member{"number": nil}),
	}),
	"ci":          object(map[string]*member{"system": nil, "runId": nil, "url": nil}),
	"producer":    object(map[string]*member{"name": nil, "version": nil}),
	"generatedAt": nil,
	"changedFiles": array(object(map[string]*member{
		"path": nil, "status": nil, "previousPath": nil, "component": nil,
	})),
	"components": array(object(map[string]*member{
		"id": nil, "name": nil, "ecosystem": nil, "source": nil, "impact": nil, "dependsOn": nil,
	})),
	"tests": array(object(map[string]*member{
		"component": nil, "passed": nil, "failed": nil, "skipped": nil, "durationMs": nil,
		"failingCases": array(object(map[string]*member{"name": nil, "suite": nil, "file": nil, "message": nil})),
	})),
	"coverage": array(object(map[string]*member{"component": nil, "coveredLines": nil, "totalLines": nil})),
	"changedLineCoverage": object(map[string]*member{
		"coveredLines": nil, "totalLines": nil,
		"files": array(object(map[string]*member{"path": nil, "coveredLines": nil, "totalLines": nil, "uncoveredLines": nil})),
	}),
	"dependencyChanges": array(object(map[string]*member{
		"ecosystem": nil, "lockfile": nil, "name": nil, "change": nil, "from": nil, "to": nil, "component": nil,
	})),
	"diagnostics": array(object(map[string]*member{"severity": nil, "code": nil, "message": nil, "field": nil})),
})

// ParseAndValidate strictly decodes a document and validates it.
func ParseAndValidate(data []byte) (*Document, []diag.Diagnostic) {
	var document Document
	if diags := strictDecode(data, documentShape, &document); diags != nil {
		return nil, diags
	}
	if diags := ValidateDocument(&document); diag.HasErrors(diags) {
		return nil, diags
	}
	return &document, nil
}

// strictDecode refuses a document that is not one JSON object, that carries a
// member the shape does not declare (compared case-sensitively, which
// encoding/json alone does not do), or whose member types do not decode.
func strictDecode(data []byte, shape *member, into any) []diag.Diagnostic {
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "invalid JSON: %v", err)}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "trailing data after the JSON document")}
	}
	if _, ok := raw.(map[string]any); !ok {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "the document must be a JSON object")}
	}
	if field, ok := firstUnknownMember(raw, shape, ""); !ok {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownField, field, "unknown member %q", field)}
	}
	typed := json.NewDecoder(bytes.NewReader(data))
	typed.DisallowUnknownFields()
	if err := typed.Decode(into); err != nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "invalid member type: %v", err)}
	}
	return nil
}

// firstUnknownMember walks value against shape and returns the dotted path of
// the first undeclared member, visiting object members in sorted order.
func firstUnknownMember(value any, shape *member, path string) (string, bool) {
	if shape == nil {
		return "", true
	}
	switch typed := value.(type) {
	case map[string]any:
		if shape.fields == nil {
			return "", true
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := joinPath(path, key)
			memberShape, declared := shape.fields[key]
			if !declared {
				return child, false
			}
			if field, ok := firstUnknownMember(typed[key], memberShape, child); !ok {
				return field, false
			}
		}
	case []any:
		if shape.items == nil {
			return "", true
		}
		for index, item := range typed {
			if field, ok := firstUnknownMember(item, shape.items, fmt.Sprintf("%s[%d]", path, index)); !ok {
				return field, false
			}
		}
	}
	return "", true
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// ValidateDocument checks a decoded document: version, subject, CI, bounds,
// paths, references, impact, counts and dependency changes.
func ValidateDocument(document *Document) []diag.Diagnostic {
	v := &validator{}
	if document.ProtocolVersion != ProtocolVersion {
		v.add(ErrorCodeUnsupportedProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported (want %d)", document.ProtocolVersion, ProtocolVersion)
	}
	v.subject(document.Subject)
	v.ci(document.CI)
	if strings.TrimSpace(document.Producer.Name) == "" {
		v.add(ErrorCodeRequired, "producer.name", "producer.name is required")
	}
	if strings.TrimSpace(document.Producer.Version) == "" {
		v.add(ErrorCodeRequired, "producer.version", "producer.version is required")
	}
	if !timestampPattern.MatchString(document.GeneratedAt) {
		v.add(ErrorCodeInvalidTimestamp, "generatedAt", "generatedAt must be an RFC 3339 timestamp with a zone")
	}
	if !v.bounds(document) {
		// A document over a bound is refused before its contents are walked.
		return v.diags
	}
	components := v.components(document.Components)
	v.changedFiles(document.ChangedFiles, components)
	v.impact(document, components)
	v.tests(document.Tests, components)
	v.coverage(document.Coverage, components)
	v.changedLineCoverage(document.ChangedLineCoverage)
	v.dependencyChanges(document.DependencyChanges, components)
	v.diagnostics(document.Diagnostics)
	return v.diags
}

type validator struct {
	diags []diag.Diagnostic
}

func (v *validator) add(code, field, format string, args ...any) {
	v.diags = append(v.diags, diag.Errorf(code, field, format, args...))
}

func (v *validator) subject(subject Subject) {
	switch subject.Provider {
	case ProviderGitHub, ProviderGitLab:
		if !isHostname(subject.Host) {
			v.add(ErrorCodeInvalidSubject, "subject.host", "a %s subject names its hostname", subject.Provider)
		}
		if !projectIDPattern.MatchString(subject.ProjectID) {
			v.add(ErrorCodeInvalidSubject, "subject.projectId", "a %s subject names its numeric project id", subject.Provider)
		}
	case ProviderGeneric:
		if subject.Host != "" && !isHostname(subject.Host) {
			v.add(ErrorCodeInvalidSubject, "subject.host", "host %q is not a hostname", subject.Host)
		}
		if subject.ProjectID != "" && !projectIDPattern.MatchString(subject.ProjectID) {
			v.add(ErrorCodeInvalidSubject, "subject.projectId", "projectId %q is not a positive integer", subject.ProjectID)
		}
	default:
		v.add(ErrorCodeInvalidSubject, "subject.provider", "provider %q is not one of %s", subject.Provider, strings.Join(Providers, ", "))
	}
	if strings.TrimSpace(subject.Repository) == "" {
		v.add(ErrorCodeRequired, "subject.repository", "subject.repository is required")
	}
	if !shaPattern.MatchString(subject.SHA) {
		v.add(ErrorCodeInvalidSubject, "subject.sha", "sha must be 40 or 64 lowercase hexadecimal characters")
	}
	if subject.BaseSHA != "" && !shaPattern.MatchString(subject.BaseSHA) {
		v.add(ErrorCodeInvalidSubject, "subject.baseSha", "baseSha must be 40 or 64 lowercase hexadecimal characters")
	}
	if subject.ChangeRequest != nil && subject.ChangeRequest.Number <= 0 {
		v.add(ErrorCodeInvalidSubject, "subject.changeRequest.number", "a change request number is positive")
	}
}

func isHostname(host string) bool {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/@:?# ") {
		return false
	}
	return strings.ToLower(host) == host
}

func (v *validator) ci(ci CI) {
	if !slices.Contains(CISystems, ci.System) {
		v.add(ErrorCodeInvalidCI, "ci.system", "system %q is not one of %s", ci.System, strings.Join(CISystems, ", "))
	}
	if ci.URL == "" {
		return
	}
	parsed, err := url.Parse(ci.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		v.add(ErrorCodeInvalidCI, "ci.url", "url must be an https URL without credentials")
	}
}

// bounds reports whether every member is within its cap.
func (v *validator) bounds(document *Document) bool {
	before := len(v.diags)
	over := func(field string, size, limit int) {
		if size > limit {
			v.add(ErrorCodeTooLarge, field, "%s holds %d entries, more than %d", field, size, limit)
		}
	}
	over("changedFiles", len(document.ChangedFiles), MaxChangedFiles)
	over("components", len(document.Components), MaxComponents)
	over("tests", len(document.Tests), MaxComponents)
	over("coverage", len(document.Coverage), MaxComponents)
	over("dependencyChanges", len(document.DependencyChanges), MaxDependencyChanges)
	over("diagnostics", len(document.Diagnostics), MaxDiagnostics)
	for index, entry := range document.Tests {
		over(fmt.Sprintf("tests[%d].failingCases", index), len(entry.FailingCases), MaxFailingCases)
	}
	for index, component := range document.Components {
		over(fmt.Sprintf("components[%d].dependsOn", index), len(component.DependsOn), MaxComponents)
	}
	if coverage := document.ChangedLineCoverage; coverage != nil {
		over("changedLineCoverage.files", len(coverage.Files), MaxChangedLineCoverageFile)
		for index, file := range coverage.Files {
			over(fmt.Sprintf("changedLineCoverage.files[%d].uncoveredLines", index), len(file.UncoveredLines), MaxUncoveredLines)
		}
	}
	return len(v.diags) == before
}

// validPath reports whether path is relative to the repository root,
// slash-separated, and stays inside it.
func validPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, `\`) {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validComponentID(id string) bool { return id == RootComponent || validPath(id) }

func (v *validator) path(field, path string) {
	if !validPath(path) {
		v.add(ErrorCodeInvalidPath, field, "path %q must be relative to the repository root and stay inside it", path)
	}
}

// reference checks that a non-empty component reference names a component.
func (v *validator) reference(field, id string, components map[string]Component) {
	if id == "" {
		return
	}
	if _, ok := components[id]; !ok {
		v.add(ErrorCodeUnknownComponent, field, "component %q is not in components", id)
	}
}

func (v *validator) components(list []Component) map[string]Component {
	components := make(map[string]Component, len(list))
	for index, component := range list {
		field := fmt.Sprintf("components[%d]", index)
		if !validComponentID(component.ID) {
			v.add(ErrorCodeInvalidPath, field+".id", "component id %q must be %q or a path relative to the repository root", component.ID, RootComponent)
		}
		if _, seen := components[component.ID]; seen {
			v.add(ErrorCodeDuplicate, field+".id", "component %q is listed twice", component.ID)
		}
		components[component.ID] = component
		if !slugPattern.MatchString(component.Ecosystem) {
			v.add(ErrorCodeInvalidComponent, field+".ecosystem", "ecosystem %q is not a lowercase slug", component.Ecosystem)
		}
		if !slices.Contains(ComponentSources, component.Source) {
			v.add(ErrorCodeInvalidComponent, field+".source", "source %q is not one of %s", component.Source, strings.Join(ComponentSources, ", "))
		}
		if !slices.Contains(Impacts, component.Impact) {
			v.add(ErrorCodeInvalidImpact, field+".impact", "impact %q is not one of %s", component.Impact, strings.Join(Impacts, ", "))
		}
	}
	for index, component := range list {
		seen := map[string]bool{}
		for edge, target := range component.DependsOn {
			field := fmt.Sprintf("components[%d].dependsOn[%d]", index, edge)
			if seen[target] {
				v.add(ErrorCodeDuplicate, field, "dependency %q is listed twice", target)
			}
			seen[target] = true
			if target == component.ID {
				v.add(ErrorCodeInvalidComponent, field, "component %q depends on itself", target)
				continue
			}
			v.reference(field, target, components)
		}
	}
	return components
}

func (v *validator) changedFiles(files []ChangedFile, components map[string]Component) {
	seen := map[string]bool{}
	for index, file := range files {
		field := fmt.Sprintf("changedFiles[%d]", index)
		v.path(field+".path", file.Path)
		if seen[file.Path] {
			v.add(ErrorCodeDuplicate, field+".path", "file %q is listed twice", file.Path)
		}
		seen[file.Path] = true
		if !slices.Contains(FileStatuses, file.Status) {
			v.add(ErrorCodeInvalidPath, field+".status", "status %q is not one of %s", file.Status, strings.Join(FileStatuses, ", "))
		}
		switch {
		case file.Status == FileRenamed && file.PreviousPath == "":
			v.add(ErrorCodeInvalidPath, field+".previousPath", "a renamed file names its previous path")
		case file.Status == FileRenamed:
			v.path(field+".previousPath", file.PreviousPath)
		case file.PreviousPath != "":
			v.add(ErrorCodeInvalidPath, field+".previousPath", "only a renamed file names a previous path")
		}
		v.reference(field+".component", file.Component, components)
	}
}

// impact checks that every component's impact follows from the changed files
// and the dependency edges.
func (v *validator) impact(document *Document, components map[string]Component) {
	changed := map[string]bool{}
	for _, file := range document.ChangedFiles {
		if _, ok := components[file.Component]; ok {
			changed[file.Component] = true
		}
	}
	reaches := map[string]bool{}
	var visit func(id string, path map[string]bool) bool
	visit = func(id string, path map[string]bool) bool {
		if known, ok := reaches[id]; ok {
			return known
		}
		if path[id] {
			return false
		}
		path[id] = true
		defer delete(path, id)
		for _, target := range components[id].DependsOn {
			if _, ok := components[target]; !ok || target == id {
				continue
			}
			if changed[target] || visit(target, path) {
				reaches[id] = true
				return true
			}
		}
		// A component inside a cycle is settled by the walk that entered the cycle
		// first; memoize only an answer reached outside one.
		if len(path) == 1 {
			reaches[id] = false
		}
		return false
	}
	for index, component := range document.Components {
		if !slices.Contains(Impacts, component.Impact) {
			continue
		}
		want := ImpactUnaffected
		switch {
		case changed[component.ID]:
			want = ImpactChanged
		case visit(component.ID, map[string]bool{}):
			want = ImpactDependent
		}
		if component.Impact != want {
			v.add(ErrorCodeInvalidImpact, fmt.Sprintf("components[%d].impact", index),
				"component %q is %s, not %s", component.ID, want, component.Impact)
		}
	}
}

func (v *validator) tests(entries []ComponentTests, components map[string]Component) {
	seen := map[string]bool{}
	for index, entry := range entries {
		field := fmt.Sprintf("tests[%d]", index)
		if entry.Component == "" {
			v.add(ErrorCodeRequired, field+".component", "a tests entry names its component")
		}
		if seen[entry.Component] {
			v.add(ErrorCodeDuplicate, field+".component", "component %q has two tests entries", entry.Component)
		}
		seen[entry.Component] = true
		v.reference(field+".component", entry.Component, components)
		if entry.Passed < 0 || entry.Failed < 0 || entry.Skipped < 0 || entry.DurationMs < 0 {
			v.add(ErrorCodeInvalidCount, field, "counts and durations are non-negative")
		}
		if len(entry.FailingCases) > entry.Failed {
			v.add(ErrorCodeInvalidCount, field+".failingCases",
				"%d failing cases are listed for %d failures", len(entry.FailingCases), entry.Failed)
		}
		for caseIndex, failing := range entry.FailingCases {
			if strings.TrimSpace(failing.Name) == "" {
				v.add(ErrorCodeRequired, fmt.Sprintf("%s.failingCases[%d].name", field, caseIndex), "a failing case names its test")
			}
		}
	}
}

func (v *validator) coverage(entries []ComponentCoverage, components map[string]Component) {
	seen := map[string]bool{}
	for index, entry := range entries {
		field := fmt.Sprintf("coverage[%d]", index)
		if entry.Component == "" {
			v.add(ErrorCodeRequired, field+".component", "a coverage entry names its component")
		}
		if seen[entry.Component] {
			v.add(ErrorCodeDuplicate, field+".component", "component %q has two coverage entries", entry.Component)
		}
		seen[entry.Component] = true
		v.reference(field+".component", entry.Component, components)
		v.lines(field, entry.CoveredLines, entry.TotalLines)
	}
}

func (v *validator) lines(field string, covered, total int) {
	if covered < 0 || total < 0 || covered > total {
		v.add(ErrorCodeInvalidCount, field, "covered lines (%d) must be between 0 and total lines (%d)", covered, total)
	}
}

func (v *validator) changedLineCoverage(coverage *ChangedLineCoverage) {
	if coverage == nil {
		return
	}
	v.lines("changedLineCoverage", coverage.CoveredLines, coverage.TotalLines)
	covered, total := 0, 0
	seen := map[string]bool{}
	for index, file := range coverage.Files {
		field := fmt.Sprintf("changedLineCoverage.files[%d]", index)
		v.path(field+".path", file.Path)
		if seen[file.Path] {
			v.add(ErrorCodeDuplicate, field+".path", "file %q is listed twice", file.Path)
		}
		seen[file.Path] = true
		v.lines(field, file.CoveredLines, file.TotalLines)
		covered += file.CoveredLines
		total += file.TotalLines
		previous := 0
		for _, line := range file.UncoveredLines {
			if line <= previous {
				v.add(ErrorCodeInvalidCount, field+".uncoveredLines", "uncovered line numbers are positive and strictly increasing")
				break
			}
			previous = line
		}
		if uncovered := file.TotalLines - file.CoveredLines; uncovered >= 0 && len(file.UncoveredLines) > uncovered {
			v.add(ErrorCodeInvalidCount, field+".uncoveredLines",
				"%d uncovered lines are listed for %d uncovered lines", len(file.UncoveredLines), uncovered)
		}
	}
	if covered != coverage.CoveredLines || total != coverage.TotalLines {
		v.add(ErrorCodeInvalidCount, "changedLineCoverage",
			"the totals (%d of %d) are not the sums of the files (%d of %d)", coverage.CoveredLines, coverage.TotalLines, covered, total)
	}
}

func (v *validator) dependencyChanges(changes []DependencyChange, components map[string]Component) {
	for index, change := range changes {
		field := fmt.Sprintf("dependencyChanges[%d]", index)
		if !slugPattern.MatchString(change.Ecosystem) {
			v.add(ErrorCodeInvalidDependencyChange, field+".ecosystem", "ecosystem %q is not a lowercase slug", change.Ecosystem)
		}
		v.path(field+".lockfile", change.Lockfile)
		if strings.TrimSpace(change.Name) == "" {
			v.add(ErrorCodeInvalidDependencyChange, field+".name", "a dependency change names its package")
		}
		hasFrom, hasTo := change.From != "", change.To != ""
		switch change.Change {
		case DependencyAdded:
			if hasFrom || !hasTo {
				v.add(ErrorCodeInvalidDependencyChange, field, "an added package has a to version and no from version")
			}
		case DependencyRemoved:
			if !hasFrom || hasTo {
				v.add(ErrorCodeInvalidDependencyChange, field, "a removed package has a from version and no to version")
			}
		case DependencyUpgraded, DependencyDowngraded, DependencyChanged:
			if !hasFrom || !hasTo || change.From == change.To {
				v.add(ErrorCodeInvalidDependencyChange, field, "a %s package has two different versions", change.Change)
			}
		default:
			v.add(ErrorCodeInvalidDependencyChange, field+".change", "change %q is not one of %s", change.Change, strings.Join(DependencyChanges, ", "))
		}
		v.reference(field+".component", change.Component, components)
	}
}

func (v *validator) diagnostics(list []diag.Diagnostic) {
	for index, finding := range list {
		field := fmt.Sprintf("diagnostics[%d]", index)
		switch finding.Severity {
		case diag.Error, diag.Warning, diag.Info:
		default:
			v.add(ErrorCodeInvalidDiagnostic, field+".severity", "severity %q is not error, warning or info", finding.Severity)
		}
		if !codePattern.MatchString(finding.Code) {
			v.add(ErrorCodeInvalidDiagnostic, field+".code", "code %q is not a dotted lowercase code", finding.Code)
		}
	}
}
