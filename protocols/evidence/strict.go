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
	"time"

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
	ErrorCodeInvalidSBOM                = "evidence.invalid_sbom"
	ErrorCodeInvalidDecision            = "evidence.invalid_decision"
	ErrorCodeInvalidDeclaration         = "evidence.invalid_declaration"
	ErrorCodeInvalidSpecVerification    = "evidence.invalid_spec_verification"
	ErrorCodeInvalidArchitecture        = "evidence.invalid_architecture"
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
	ErrorCodeInvalidSBOM:                true,
	ErrorCodeInvalidDecision:            true,
	ErrorCodeInvalidDeclaration:         true,
	ErrorCodeInvalidSpecVerification:    true,
	ErrorCodeInvalidArchitecture:        true,
	ErrorCodeInvalidTimestamp:           true,
	ErrorCodeInvalidDiagnostic:          true,
	ErrorCodeTooLarge:                   true,
}

// Patterns. The schema declares the same expressions, and a conformance test
// keeps the two equal.
const (
	shaExpression       = `^([0-9a-f]{40}|[0-9a-f]{64})$`
	projectIDExpression = `^[1-9][0-9]*$`
	// hostExpression is a lowercase DNS name with an optional port: labels of
	// letters, digits and inner hyphens, no trailing dot.
	hostExpression = `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[0-9]{1,5})?$`
	slugExpression = `^[a-z][a-z0-9-]*$`
	codeExpression = `^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)+$`
	// specVersionExpression is a dotted version number: 1.5, 2.3.
	specVersionExpression = `^[0-9]+\.[0-9]+(\.[0-9]+)?$`
	// dateExpression is a calendar date; time.Parse then refuses an
	// impossible one.
	dateExpression = `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`
	// timestampExpression is RFC 3339 with an optional fraction and a zone;
	// time.Parse then refuses an impossible date or time.
	timestampExpression = `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$`
)

var (
	shaPattern         = regexp.MustCompile(shaExpression)
	projectIDPattern   = regexp.MustCompile(projectIDExpression)
	hostPattern        = regexp.MustCompile(hostExpression)
	slugPattern        = regexp.MustCompile(slugExpression)
	codePattern        = regexp.MustCompile(codeExpression)
	specVersionPattern = regexp.MustCompile(specVersionExpression)
	datePattern        = regexp.MustCompile(dateExpression)
	timestampPattern   = regexp.MustCompile(timestampExpression)
	drivePattern       = regexp.MustCompile(`^[A-Za-z]:`)
)

// requiredMembers are the top-level members a document always carries, even
// empty.
var requiredMembers = []string{"protocolVersion", "subject", "ci", "producer", "generatedAt", "changedFiles", "components"}

// member describes the members an object may carry. A nil member is a scalar;
// items describes the elements of an array member.
type member struct {
	fields map[string]*member
	items  *member
}

func object(fields map[string]*member) *member { return &member{fields: fields} }
func array(items *member) *member              { return &member{items: items} }

var (
	dependencyStateShape = object(map[string]*member{
		"licenses": nil, "vulnerabilities": array(object(map[string]*member{"id": nil, "severity": nil})),
	})
	sbomDocumentShape = object(map[string]*member{"format": nil, "specVersion": nil})
)

var documentShape = object(map[string]*member{
	"protocolVersion": nil,
	"subject": object(map[string]*member{
		"provider": nil, "host": nil, "projectId": nil, "repository": nil,
		"sha": nil, "baseSha": nil, "branch": nil,
		"changeRequest": object(map[string]*member{"number": nil}),
	}),
	"ci":          object(map[string]*member{"system": nil, "runId": nil, "jobId": nil, "attempt": nil, "url": nil}),
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
	"architecture": array(object(map[string]*member{
		"tool": nil, "rule": nil, "status": nil, "violationCount": nil,
		"violations": array(object(map[string]*member{"message": nil, "file": nil, "line": nil, "component": nil})),
	})),
	"changedLineCoverage": object(map[string]*member{
		"coveredLines": nil, "totalLines": nil,
		"files": array(object(map[string]*member{"path": nil, "coveredLines": nil, "totalLines": nil, "uncoveredLines": nil})),
	}),
	"dependencyChanges": array(object(map[string]*member{
		"ecosystem": nil, "lockfile": nil, "name": nil, "change": nil, "from": nil, "to": nil, "component": nil,
		"before": dependencyStateShape, "after": dependencyStateShape,
	})),
	"sbom": object(map[string]*member{"head": sbomDocumentShape, "base": sbomDocumentShape}),
	"decisions": array(object(map[string]*member{
		"path": nil, "title": nil, "status": nil, "date": nil, "scope": nil, "governs": nil, "change": nil,
	})),
	"declarations": array(object(map[string]*member{
		"path": nil, "status": nil, "previousPath": nil, "before": nil, "after": nil,
	})),
	// specVerification is the features protocol's record, which that protocol
	// shapes: the document carries it whole.
	"specVerification": nil,
	"diagnostics":      array(object(map[string]*member{"severity": nil, "code": nil, "message": nil, "field": nil})),
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
	top, ok := raw.(map[string]any)
	if !ok {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "the document must be a JSON object")}
	}
	if field, ok := firstUnknownMember(raw, shape, ""); !ok {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownField, field, "unknown member %q", field)}
	}
	var diags []diag.Diagnostic
	for _, name := range requiredMembers {
		if value, present := top[name]; !present || value == nil {
			diags = append(diags, diag.Errorf(ErrorCodeRequired, name, "%s is required", name))
		}
	}
	if field, ok := firstOversizedString(raw, ""); !ok {
		diags = append(diags, diag.Errorf(ErrorCodeTooLarge, field, "%s is longer than %d bytes", field, MaxStringBytes))
	}
	if diags != nil {
		return diags
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

// firstOversizedString returns the dotted path of the first string longer than
// MaxStringBytes, visiting object members in sorted order. A declaration's
// documents are bounded by MaxDeclarationBytes instead, and the spec
// verification record by MaxSpecVerificationBytes as a whole.
func firstOversizedString(value any, path string) (string, bool) {
	if path == "specVerification" {
		return "", true
	}
	switch typed := value.(type) {
	case string:
		if declarationDocument(path) {
			return path, len(typed) <= MaxDeclarationBytes
		}
		return path, len(typed) <= MaxStringBytes
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if field, ok := firstOversizedString(typed[key], joinPath(path, key)); !ok {
				return field, false
			}
		}
	case []any:
		for index, item := range typed {
			if field, ok := firstOversizedString(item, fmt.Sprintf("%s[%d]", path, index)); !ok {
				return field, false
			}
		}
	}
	return "", true
}

// declarationDocument reports whether a dotted path is the before or after
// document of a declaration.
func declarationDocument(path string) bool {
	if !strings.HasPrefix(path, "declarations[") {
		return false
	}
	return strings.HasSuffix(path, "].before") || strings.HasSuffix(path, "].after")
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// ValidateDocument checks a decoded document: version, subject, CI, bounds,
// paths, references, impact, counts, dependency changes and decisions.
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
	if !validTimestamp(document.GeneratedAt) {
		v.add(ErrorCodeInvalidTimestamp, "generatedAt", "generatedAt must be an RFC 3339 timestamp with a zone")
	}
	if !v.bounds(document) {
		// A document over a bound is refused before its contents are walked.
		return v.diags
	}
	if document.Subject.BaseSHA == "" && (len(document.ChangedFiles) > 0 || document.ChangedLineCoverage != nil) {
		v.add(ErrorCodeInvalidSubject, "subject.baseSha", "a diff and its changed-line coverage need the base they are measured against")
	}
	components := v.components(document.Components)
	changed := v.changedFiles(document.ChangedFiles, components)
	v.impact(document, components)
	v.tests(document.Tests, components)
	v.coverage(document.Coverage, components)
	v.architecture(document.Architecture, components)
	v.changedLineCoverage(document.ChangedLineCoverage, changed)
	v.sbom(document.SBOM)
	v.dependencyChanges(document.DependencyChanges, components, changed)
	v.dependencyStates(document.DependencyChanges, document.SBOM)
	v.decisions(document.Decisions, changed)
	v.declarations(document.Declarations, changed)
	v.specVerification(document.SpecVerification)
	v.diagnostics(document.Diagnostics)
	return v.diags
}

func validTimestamp(value string) bool {
	if !timestampPattern.MatchString(value) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
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
	return len(host) <= 253+6 && hostPattern.MatchString(host)
}

func (v *validator) ci(ci CI) {
	if !slices.Contains(CISystems, ci.System) {
		v.add(ErrorCodeInvalidCI, "ci.system", "system %q is not one of %s", ci.System, strings.Join(CISystems, ", "))
	}
	if ci.Attempt != nil && *ci.Attempt < 1 {
		v.add(ErrorCodeInvalidCI, "ci.attempt", "an attempt number starts at 1")
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
	over("architecture", len(document.Architecture), MaxArchitectureRules)
	for index, rule := range document.Architecture {
		over(fmt.Sprintf("architecture[%d].violations", index), len(rule.Violations), MaxViolations)
	}
	over("dependencyChanges", len(document.DependencyChanges), MaxDependencyChanges)
	over("decisions", len(document.Decisions), MaxDecisions)
	over("declarations", len(document.Declarations), MaxDeclarations)
	over("diagnostics", len(document.Diagnostics), MaxDiagnostics)
	for index, decision := range document.Decisions {
		over(fmt.Sprintf("decisions[%d].scope", index), len(decision.Scope), MaxDecisionScope)
		over(fmt.Sprintf("decisions[%d].governs", index), len(decision.Governs), MaxGovernedFiles)
	}
	for index, entry := range document.Tests {
		over(fmt.Sprintf("tests[%d].failingCases", index), len(entry.FailingCases), MaxFailingCases)
	}
	edges := 0
	for _, component := range document.Components {
		edges += len(component.DependsOn)
	}
	over("components[].dependsOn", edges, MaxEdges)
	for index, change := range document.DependencyChanges {
		for side, state := range []*DependencyState{change.Before, change.After} {
			if state == nil {
				continue
			}
			field := fmt.Sprintf("dependencyChanges[%d].%s", index, []string{"before", "after"}[side])
			over(field+".licenses", len(state.Licenses), MaxLicenses)
			over(field+".vulnerabilities", len(state.Vulnerabilities), MaxVulnerabilities)
		}
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
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, `\`) || drivePattern.MatchString(path) {
		return false
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return false
		}
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
			switch {
			case target == "":
				v.add(ErrorCodeUnknownComponent, field, "a dependency edge names a component")
			case target == component.ID:
				v.add(ErrorCodeInvalidComponent, field, "component %q depends on itself", target)
			default:
				v.reference(field, target, components)
			}
		}
	}
	return components
}

// changedFiles checks the diff and returns the status of each changed path.
func (v *validator) changedFiles(files []ChangedFile, components map[string]Component) map[string]string {
	seen := map[string]string{}
	for index, file := range files {
		field := fmt.Sprintf("changedFiles[%d]", index)
		v.path(field+".path", file.Path)
		if _, twice := seen[file.Path]; twice {
			v.add(ErrorCodeDuplicate, field+".path", "file %q is listed twice", file.Path)
		}
		seen[file.Path] = file.Status
		if !slices.Contains(FileStatuses, file.Status) {
			v.add(ErrorCodeInvalidPath, field+".status", "status %q is not one of %s", file.Status, strings.Join(FileStatuses, ", "))
		}
		switch {
		case file.Status == FileRenamed && file.PreviousPath == "":
			v.add(ErrorCodeInvalidPath, field+".previousPath", "a renamed file names its previous path")
		case file.Status == FileRenamed && file.PreviousPath == file.Path:
			v.add(ErrorCodeInvalidPath, field+".previousPath", "a renamed file's previous path differs from its path")
		case file.Status == FileRenamed:
			v.path(field+".previousPath", file.PreviousPath)
		case file.PreviousPath != "":
			v.add(ErrorCodeInvalidPath, field+".previousPath", "only a renamed file names a previous path")
		}
		v.reference(field+".component", file.Component, components)
		if _, known := components[file.Component]; known && !contains(file.Component, file.Path) {
			v.add(ErrorCodeInvalidComponent, field+".component", "file %q is not inside component %q", file.Path, file.Component)
		}
	}
	return seen
}

// contains reports whether path lies inside the component directory id.
func contains(id, path string) bool {
	return id == RootComponent || strings.HasPrefix(path, id+"/")
}

// impact checks that every component's impact follows from the changed files
// and the dependency edges.
func (v *validator) impact(document *Document, components map[string]Component) {
	changed := map[string]bool{}
	for _, file := range document.ChangedFiles {
		// A file attributed to a component it does not lie in is reported by
		// changedFiles; it makes no component changed.
		if _, ok := components[file.Component]; ok && contains(file.Component, file.Path) {
			changed[file.Component] = true
		}
	}
	// One breadth-first walk over the reversed edges, from every changed
	// component at once, marks each component that reaches one: linear in the
	// components and the edges, cycles included.
	dependents := map[string][]string{}
	for _, component := range document.Components {
		for _, target := range component.DependsOn {
			dependents[target] = append(dependents[target], component.ID)
		}
	}
	reaches := map[string]bool{}
	queue := make([]string, 0, len(changed))
	for id := range changed {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, dependent := range dependents[id] {
			if !reaches[dependent] {
				reaches[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	for index, component := range document.Components {
		if !slices.Contains(Impacts, component.Impact) {
			continue
		}
		want := ImpactUnaffected
		switch {
		case changed[component.ID]:
			want = ImpactChanged
		case reaches[component.ID]:
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

func (v *validator) architecture(rules []ArchitectureRule, components map[string]Component) {
	seen := map[[2]string]bool{}
	for index, rule := range rules {
		field := fmt.Sprintf("architecture[%d]", index)
		if strings.TrimSpace(rule.Tool) == "" {
			v.add(ErrorCodeRequired, field+".tool", "an architecture rule names its tool")
		} else if !slugPattern.MatchString(rule.Tool) {
			v.add(ErrorCodeInvalidArchitecture, field+".tool", "tool %q must be a lowercase slug", rule.Tool)
		}
		if strings.TrimSpace(rule.Rule) == "" {
			v.add(ErrorCodeRequired, field+".rule", "an architecture rule names its rule")
		}
		key := [2]string{rule.Tool, rule.Rule}
		if seen[key] {
			v.add(ErrorCodeDuplicate, field+".rule", "rule %q of %s is listed twice", rule.Rule, rule.Tool)
		}
		seen[key] = true
		switch rule.Status {
		case ArchitectureKept:
			if rule.ViolationCount != 0 || len(rule.Violations) > 0 {
				v.add(ErrorCodeInvalidCount, field+".violationCount", "a kept rule reports no violation")
			}
		case ArchitectureBroken:
			if rule.ViolationCount < max(1, len(rule.Violations)) {
				v.add(ErrorCodeInvalidCount, field+".violationCount",
					"a broken rule counts at least one violation and its %d listed ones, not %d", len(rule.Violations), rule.ViolationCount)
			}
		default:
			v.add(ErrorCodeInvalidArchitecture, field+".status", "status %q is not kept or broken", rule.Status)
		}
		for violationIndex, violation := range rule.Violations {
			v.violation(fmt.Sprintf("%s.violations[%d]", field, violationIndex), violation, components)
		}
	}
}

func (v *validator) violation(field string, violation ArchitectureViolation, components map[string]Component) {
	if strings.TrimSpace(violation.Message) == "" {
		v.add(ErrorCodeRequired, field+".message", "a violation carries its message")
	}
	if violation.File != "" {
		v.path(field+".file", violation.File)
	}
	if violation.Line < 0 || (violation.Line > 0 && violation.File == "") {
		v.add(ErrorCodeInvalidArchitecture, field+".line", "a line is from 1 and needs a file")
	}
	if violation.Component == "" {
		return
	}
	v.reference(field+".component", violation.Component, components)
	if _, known := components[violation.Component]; known && violation.File != "" && validPath(violation.File) && !contains(violation.Component, violation.File) {
		v.add(ErrorCodeInvalidComponent, field+".component", "file %q is not inside component %q", violation.File, violation.Component)
	}
}

func (v *validator) lines(field string, covered, total int) {
	if covered < 0 || total < 0 || covered > total {
		v.add(ErrorCodeInvalidCount, field, "covered lines (%d) must be between 0 and total lines (%d)", covered, total)
	}
}

func (v *validator) changedLineCoverage(coverage *ChangedLineCoverage, changed map[string]string) {
	if coverage == nil {
		return
	}
	for index, file := range coverage.Files {
		if status, ok := changed[file.Path]; !ok || status == FileDeleted {
			v.add(ErrorCodeInvalidPath, fmt.Sprintf("changedLineCoverage.files[%d].path", index),
				"file %q is not a changed file that still exists", file.Path)
		}
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

func (v *validator) dependencyChanges(changes []DependencyChange, components map[string]Component, changed map[string]string) {
	for index, change := range changes {
		field := fmt.Sprintf("dependencyChanges[%d]", index)
		if !slugPattern.MatchString(change.Ecosystem) {
			v.add(ErrorCodeInvalidDependencyChange, field+".ecosystem", "ecosystem %q is not a lowercase slug", change.Ecosystem)
		}
		v.path(field+".lockfile", change.Lockfile)
		if _, ok := changed[change.Lockfile]; validPath(change.Lockfile) && !ok {
			v.add(ErrorCodeInvalidDependencyChange, field+".lockfile", "lockfile %q is not a changed file", change.Lockfile)
		}
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

// sbom checks the software bills of materials the document names.
func (v *validator) sbom(sbom *SBOM) {
	if sbom == nil {
		return
	}
	for _, side := range []struct {
		name     string
		document *SBOMDocument
	}{{"head", sbom.Head}, {"base", sbom.Base}} {
		if side.document == nil {
			continue
		}
		field := "sbom." + side.name
		if !slices.Contains(SBOMFormats, side.document.Format) {
			v.add(ErrorCodeInvalidSBOM, field+".format", "format %q is not one of %s", side.document.Format, strings.Join(SBOMFormats, ", "))
		}
		if !specVersionPattern.MatchString(side.document.SpecVersion) {
			v.add(ErrorCodeInvalidSBOM, field+".specVersion", "specVersion %q is not a dotted version number", side.document.SpecVersion)
		}
	}
}

// dependencyStates checks the before and after states of each dependency
// change: a side needs its SBOM and a version, and a state is well formed.
func (v *validator) dependencyStates(changes []DependencyChange, sbom *SBOM) {
	hasBase := sbom != nil && sbom.Base != nil
	hasHead := sbom != nil && sbom.Head != nil
	for index, change := range changes {
		field := fmt.Sprintf("dependencyChanges[%d]", index)
		if change.Before != nil {
			switch {
			case !hasBase:
				v.add(ErrorCodeInvalidDependencyChange, field+".before", "a before state needs the base SBOM it was read from")
			case change.Change == DependencyAdded:
				v.add(ErrorCodeInvalidDependencyChange, field+".before", "an added package has no before state")
			}
			v.dependencyState(field+".before", change.Before)
		}
		if change.After != nil {
			switch {
			case !hasHead:
				v.add(ErrorCodeInvalidDependencyChange, field+".after", "an after state needs the head SBOM it was read from")
			case change.Change == DependencyRemoved:
				v.add(ErrorCodeInvalidDependencyChange, field+".after", "a removed package has no after state")
			}
			v.dependencyState(field+".after", change.After)
		}
	}
}

func (v *validator) dependencyState(field string, state *DependencyState) {
	for index, license := range state.Licenses {
		if strings.TrimSpace(license) == "" {
			v.add(ErrorCodeInvalidDependencyChange, fmt.Sprintf("%s.licenses[%d]", field, index), "a license is not empty")
		}
	}
	seen := map[string]bool{}
	for index, vulnerability := range state.Vulnerabilities {
		item := fmt.Sprintf("%s.vulnerabilities[%d]", field, index)
		if strings.TrimSpace(vulnerability.ID) == "" {
			v.add(ErrorCodeInvalidDependencyChange, item+".id", "a vulnerability names its id")
		} else if seen[vulnerability.ID] {
			v.add(ErrorCodeDuplicate, item+".id", "vulnerability %q is listed twice", vulnerability.ID)
		}
		seen[vulnerability.ID] = true
		if !slices.Contains(Severities, vulnerability.Severity) {
			v.add(ErrorCodeInvalidDependencyChange, item+".severity", "severity %q is not one of %s", vulnerability.Severity, strings.Join(Severities, ", "))
		}
	}
}

// decisions checks the decision records: each well formed, and consistent
// with the diff.
func (v *validator) decisions(list []Decision, changed map[string]string) {
	seen := map[string]bool{}
	for index, decision := range list {
		field := fmt.Sprintf("decisions[%d]", index)
		v.path(field+".path", decision.Path)
		if seen[decision.Path] {
			v.add(ErrorCodeDuplicate, field+".path", "decision %q is listed twice", decision.Path)
		}
		seen[decision.Path] = true
		if strings.TrimSpace(decision.Title) == "" {
			v.add(ErrorCodeInvalidDecision, field+".title", "a decision names its title")
		}
		if !slices.Contains(DecisionStatuses, decision.Status) {
			v.add(ErrorCodeInvalidDecision, field+".status", "status %q is not one of %s", decision.Status, strings.Join(DecisionStatuses, ", "))
		}
		if decision.Date != "" && !validDate(decision.Date) {
			v.add(ErrorCodeInvalidDecision, field+".date", "date %q is not a YYYY-MM-DD date", decision.Date)
		}
		patterns := map[string]bool{}
		for item, pattern := range decision.Scope {
			itemField := fmt.Sprintf("%s.scope[%d]", field, item)
			v.path(itemField, pattern)
			if patterns[pattern] {
				v.add(ErrorCodeDuplicate, itemField, "scope pattern %q is listed twice", pattern)
			}
			patterns[pattern] = true
		}
		governed := map[string]bool{}
		for item, file := range decision.Governs {
			itemField := fmt.Sprintf("%s.governs[%d]", field, item)
			if _, ok := changed[file]; !ok {
				v.add(ErrorCodeInvalidDecision, itemField, "governed file %q is not a changed file", file)
			}
			if governed[file] {
				v.add(ErrorCodeDuplicate, itemField, "governed file %q is listed twice", file)
			}
			governed[file] = true
		}
		v.decisionChange(field, decision, changed)
	}
}

// decisionChange checks that a decision's change is its record's file status,
// and that a decision concerns the change.
func (v *validator) decisionChange(field string, decision Decision, changed map[string]string) {
	status, isChanged := changed[decision.Path]
	switch {
	case decision.Change == "" && isChanged:
		v.add(ErrorCodeInvalidDecision, field+".change", "record %q is a changed file, so its decision names its change", decision.Path)
	case decision.Change == "":
		if len(decision.Governs) == 0 {
			v.add(ErrorCodeInvalidDecision, field, "decision %q neither governs a changed file nor is changed", decision.Path)
		}
	case !slices.Contains(FileStatuses, decision.Change):
		v.add(ErrorCodeInvalidDecision, field+".change", "change %q is not one of %s", decision.Change, strings.Join(FileStatuses, ", "))
	case !isChanged:
		v.add(ErrorCodeInvalidDecision, field+".change", "record %q is not a changed file", decision.Path)
	case status != decision.Change:
		v.add(ErrorCodeInvalidDecision, field+".change", "record %q is %s, not %s", decision.Path, status, decision.Change)
	}
}

// declarations checks the declaration files: each a declaration, once, with
// the status and the previous path the diff gives it, and the documents its
// status allows.
func (v *validator) declarations(list []Declaration, changed map[string]string) {
	seen := map[string]bool{}
	for index, declaration := range list {
		field := fmt.Sprintf("declarations[%d]", index)
		v.path(field+".path", declaration.Path)
		if seen[declaration.Path] {
			v.add(ErrorCodeDuplicate, field+".path", "declaration %q is listed twice", declaration.Path)
		}
		seen[declaration.Path] = true
		if !IsDeclaration(declaration.Path) && !IsDeclaration(declaration.PreviousPath) {
			v.add(ErrorCodeInvalidDeclaration, field+".path", "%q is not a declaration", declaration.Path)
		}
		switch status, isChanged := changed[declaration.Path]; {
		case !slices.Contains(FileStatuses, declaration.Status):
			v.add(ErrorCodeInvalidDeclaration, field+".status", "status %q is not one of %s", declaration.Status, strings.Join(FileStatuses, ", "))
		case !isChanged:
			v.add(ErrorCodeInvalidDeclaration, field+".path", "declaration %q is not a changed file", declaration.Path)
		case status != declaration.Status:
			v.add(ErrorCodeInvalidDeclaration, field+".status", "declaration %q is %s, not %s", declaration.Path, status, declaration.Status)
		}
		switch {
		case declaration.Status == FileRenamed && declaration.PreviousPath == "":
			v.add(ErrorCodeInvalidDeclaration, field+".previousPath", "a renamed declaration names its previous path")
		case declaration.Status == FileRenamed:
			v.path(field+".previousPath", declaration.PreviousPath)
		case declaration.PreviousPath != "":
			v.add(ErrorCodeInvalidDeclaration, field+".previousPath", "only a renamed declaration names a previous path")
		}
		if declaration.Status == FileAdded && declaration.Before != nil {
			v.add(ErrorCodeInvalidDeclaration, field+".before", "an added declaration has no document at the base")
		}
		if declaration.Status == FileDeleted && declaration.After != nil {
			v.add(ErrorCodeInvalidDeclaration, field+".after", "a deleted declaration has no document at the commit")
		}
		for side, document := range []*string{declaration.Before, declaration.After} {
			if document != nil && len(*document) > MaxDeclarationBytes {
				v.add(ErrorCodeTooLarge, field+"."+[]string{"before", "after"}[side], "the document is longer than %d bytes", MaxDeclarationBytes)
			}
		}
	}
}

// specVerification checks that the spec verification record is one JSON
// object within its bound. The features protocol validates its contents.
func (v *validator) specVerification(record json.RawMessage) {
	if record == nil {
		return
	}
	if len(record) > MaxSpecVerificationBytes {
		v.add(ErrorCodeTooLarge, "specVerification", "specVerification is longer than %d bytes", MaxSpecVerificationBytes)
		return
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(record, &object); err != nil || object == nil {
		v.add(ErrorCodeInvalidSpecVerification, "specVerification", "specVerification must be a JSON object")
	}
}

func validDate(value string) bool {
	if !datePattern.MatchString(value) {
		return false
	}
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
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
		if strings.TrimSpace(finding.Message) == "" {
			v.add(ErrorCodeInvalidDiagnostic, field+".message", "a diagnostic explains itself")
		}
	}
}
