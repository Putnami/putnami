package qualify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Validation diagnostic codes. ParseAndValidateContract and
// ParseAndValidateVerdict emit only these; the TypeScript twin emits the same
// code for the same fixture.
const (
	ErrorCodeParseError                 = "qualify.parse_error"
	ErrorCodeUnknownField               = "qualify.unknown_field"
	ErrorCodeUnsupportedProtocolVersion = "qualify.unsupported_protocol_version"
	ErrorCodeRequired                   = "qualify.required"
	ErrorCodeInvalidState               = "qualify.invalid_state"
	ErrorCodeInvalidTarget              = "qualify.invalid_target"
	ErrorCodeInvalidBinding             = "qualify.invalid_binding"
	ErrorCodeInvalidPhase               = "qualify.invalid_phase"
	ErrorCodeInvalidRequest             = "qualify.invalid_request"
	ErrorCodeInvalidSource              = "qualify.invalid_source"
	ErrorCodeInvalidDigest              = "qualify.invalid_digest"
	ErrorCodeInvalidCleanup             = "qualify.invalid_cleanup"
	ErrorCodeInvalidTimestamp           = "qualify.invalid_timestamp"
	ErrorCodeUnprovenPass               = "qualify.unproven_pass"
)

// ValidErrorCodes is the closed validation-code taxonomy.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError:                 true,
	ErrorCodeUnknownField:               true,
	ErrorCodeUnsupportedProtocolVersion: true,
	ErrorCodeRequired:                   true,
	ErrorCodeInvalidState:               true,
	ErrorCodeInvalidTarget:              true,
	ErrorCodeInvalidBinding:             true,
	ErrorCodeInvalidPhase:               true,
	ErrorCodeInvalidRequest:             true,
	ErrorCodeInvalidSource:              true,
	ErrorCodeInvalidDigest:              true,
	ErrorCodeInvalidCleanup:             true,
	ErrorCodeInvalidTimestamp:           true,
	ErrorCodeUnprovenPass:               true,
}

// Phase diagnostic codes: what a producer records on a non-pass phase so a
// consumer branches on a code instead of on a message.
const (
	PhaseCodeTargetUnreachable  = "qualify.target_unreachable"
	PhaseCodeCompositionFailed  = "qualify.composition_failed"
	PhaseCodeNotReady           = "qualify.not_ready"
	PhaseCodeVersionMissing     = "qualify.version_missing"
	PhaseCodeVersionMismatch    = "qualify.version_mismatch"
	PhaseCodeRequestFailed      = "qualify.request_failed"
	PhaseCodeNoRouteInventory   = "qualify.no_route_inventory"
	PhaseCodeNoDerivableRequest = "qualify.no_derivable_request"
	PhaseCodeTeardownPartial    = "qualify.teardown_partial"
	PhaseCodeCanceled           = "qualify.canceled"
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// timestampPattern is RFC 3339 with an optional fraction. The TypeScript
	// twin applies the same expression, so the two readers agree byte for byte.
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

var (
	sourceShape  = object(map[string]*member{"kind": nil, "path": nil, "digest": nil})
	requestShape = object(map[string]*member{"id": nil, "method": nil, "path": nil, "maxStatus": nil, "provenance": nil})

	contractShape = object(map[string]*member{
		"protocolVersion": nil,
		"project":         nil,
		"derivedFrom":     array(sourceShape),
		"requests":        array(requestShape),
		"digest":          nil,
	})

	verdictShape = object(map[string]*member{
		"protocolVersion": nil,
		"project":         nil,
		"target":          object(map[string]*member{"kind": nil, "url": nil, "compositionId": nil}),
		"binding": object(map[string]*member{
			"kind": nil, "fingerprint": nil, "dirty": nil, "headSHA": nil,
			"expectedSHA": nil, "observedSHA": nil, "version": nil,
		}),
		"contract": object(map[string]*member{"digest": nil, "requests": nil, "derivedFrom": array(sourceShape)}),
		"phases": array(object(map[string]*member{
			"name": nil, "state": nil, "durationMs": nil,
			"diagnostics": array(object(map[string]*member{"severity": nil, "code": nil, "message": nil, "field": nil})),
		})),
		"requests":   array(object(map[string]*member{"id": nil, "status": nil, "durationMs": nil, "state": nil, "reason": nil})),
		"state":      nil,
		"startedAt":  nil,
		"finishedAt": nil,
		"cleanup":    object(map[string]*member{"state": nil, "leftovers": nil}),
	})
)

// ParseAndValidateContract strictly decodes a contract document and validates
// it, recomputing its digest.
func ParseAndValidateContract(data []byte) (*Contract, []diag.Diagnostic) {
	var contract Contract
	if diags := strictDecode(data, contractShape, &contract); diags != nil {
		return nil, diags
	}
	if diags := ValidateContract(&contract); diag.HasErrors(diags) {
		return nil, diags
	}
	return &contract, nil
}

// ParseAndValidateVerdict strictly decodes a verdict document and validates it.
func ParseAndValidateVerdict(data []byte) (*Verdict, []diag.Diagnostic) {
	var verdict Verdict
	if diags := strictDecode(data, verdictShape, &verdict); diags != nil {
		return nil, diags
	}
	if diags := ValidateVerdict(&verdict); diag.HasErrors(diags) {
		return nil, diags
	}
	return &verdict, nil
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

// ValidateContract checks a decoded contract: version, project, sources,
// request shape and uniqueness, and that Digest is ContractDigest(Requests).
func ValidateContract(contract *Contract) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if contract.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(ErrorCodeUnsupportedProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported (want %d)", contract.ProtocolVersion, ProtocolVersion))
	}
	if strings.TrimSpace(contract.Project) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeRequired, "project", "project is required"))
	}
	diags = append(diags, validateSources("derivedFrom", contract.DerivedFrom)...)
	seen := map[string]bool{}
	for index, request := range contract.Requests {
		field := fmt.Sprintf("requests[%d]", index)
		if request.Method != "GET" && request.Method != "HEAD" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidRequest, field+".method",
				"method %q is not a read-only method (GET or HEAD)", request.Method))
		}
		if !strings.HasPrefix(request.Path, "/") {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidRequest, field+".path", "path must begin with '/'"))
		}
		if request.ID != request.Method+" "+request.Path {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidRequest, field+".id",
				"id %q must be %q", request.ID, request.Method+" "+request.Path))
		}
		if seen[request.ID] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidRequest, field+".id", "id %q is not unique", request.ID))
		}
		seen[request.ID] = true
		if request.MaxStatus < 100 || request.MaxStatus > 599 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidRequest, field+".maxStatus",
				"maxStatus %d is not an HTTP status", request.MaxStatus))
		}
		if strings.TrimSpace(request.Provenance) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidRequest, field+".provenance", "provenance is required"))
		}
	}
	switch {
	case !digestPattern.MatchString(contract.Digest):
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, "digest",
			"digest must be sha256: followed by 64 lowercase hexadecimal characters"))
	case contract.Digest != ContractDigest(contract.Requests):
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, "digest",
			"digest %q does not match the requests (%q)", contract.Digest, ContractDigest(contract.Requests)))
	}
	return diags
}

func validateSources(field string, sources []Source) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for index, source := range sources {
		at := fmt.Sprintf("%s[%d]", field, index)
		if source.Kind != SourceHTTPRoutes {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSource, at+".kind",
				"source kind %q is not %q", source.Kind, SourceHTTPRoutes))
		}
		if strings.TrimSpace(source.Path) == "" || strings.TrimSpace(source.Digest) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidSource, at, "a source names its path and its digest"))
		}
	}
	return diags
}

// ValidateVerdict checks a decoded verdict. Beyond shape, it enforces the
// fail-closed rule: a passed verdict must carry its proof — every phase of
// PhaseNames passed, at least one request and every request passed, an
// observed sha that starts with the expected one, and a clean teardown.
func ValidateVerdict(verdict *Verdict) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if verdict.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(ErrorCodeUnsupportedProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported (want %d)", verdict.ProtocolVersion, ProtocolVersion))
	}
	if strings.TrimSpace(verdict.Project) == "" {
		diags = append(diags, diag.Errorf(ErrorCodeRequired, "project", "project is required"))
	}
	diags = append(diags, validateTarget(verdict.Target)...)
	diags = append(diags, validateBinding(verdict.Binding)...)
	if !digestPattern.MatchString(verdict.Contract.Digest) {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidDigest, "contract.digest",
			"digest must be sha256: followed by 64 lowercase hexadecimal characters"))
	}
	if verdict.Contract.Requests < 0 {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidRequest, "contract.requests", "requests must not be negative"))
	}
	diags = append(diags, validateSources("contract.derivedFrom", verdict.Contract.DerivedFrom)...)
	diags = append(diags, validatePhases(verdict.Phases)...)
	diags = append(diags, validateRequestResults(verdict.Requests)...)
	if !verdict.State.IsValid() {
		diags = append(diags, diag.Errorf(ErrorCodeInvalidState, "state", "state %q is not in the closed vocabulary", verdict.State))
	}
	for field, value := range map[string]string{"startedAt": verdict.StartedAt, "finishedAt": verdict.FinishedAt} {
		if !timestampPattern.MatchString(value) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidTimestamp, field, "%s must be an RFC 3339 timestamp", field))
		}
	}
	if verdict.Cleanup != nil {
		diags = append(diags, validateCleanup(*verdict.Cleanup)...)
	}
	if verdict.State.IsPass() {
		diags = append(diags, unprovenPass(verdict)...)
	}
	sortDiagnostics(diags)
	return diags
}

func validateTarget(target Target) []diag.Diagnostic {
	switch target.Kind {
	case TargetLocal:
		return nil
	case TargetURL:
		if strings.TrimSpace(target.URL) == "" {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTarget, "target.url", "a url target names its url")}
		}
		return nil
	default:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTarget, "target.kind",
			"target kind %q is not %q or %q", target.Kind, TargetLocal, TargetURL)}
	}
}

func validateBinding(binding Binding) []diag.Diagnostic {
	switch binding.Kind {
	case BindingArtifact:
		if strings.TrimSpace(binding.ExpectedSHA) == "" {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBinding, "binding.expectedSHA",
				"an artifact binding names the sha it expects")}
		}
		return nil
	case BindingTree:
		if strings.TrimSpace(binding.Fingerprint) == "" || strings.TrimSpace(binding.HeadSHA) == "" {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBinding, "binding",
				"a tree binding names its fingerprint and its headSHA")}
		}
		return nil
	default:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidBinding, "binding.kind",
			"binding kind %q is not %q or %q", binding.Kind, BindingTree, BindingArtifact)}
	}
}

func validatePhases(phases []Phase) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := map[string]bool{}
	for index, phase := range phases {
		field := fmt.Sprintf("phases[%d]", index)
		if !isPhaseName(phase.Name) || seen[phase.Name] {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPhase, field+".name",
				"phase %q is unknown or repeated", phase.Name))
		}
		seen[phase.Name] = true
		if !phase.State.IsValid() {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidState, field+".state",
				"state %q is not in the closed vocabulary", phase.State))
		}
		if phase.DurationMs < 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidPhase, field+".durationMs", "durationMs must not be negative"))
		}
	}
	return diags
}

func isPhaseName(name string) bool {
	for _, known := range PhaseNames {
		if name == known {
			return true
		}
	}
	return false
}

func validateRequestResults(results []RequestResult) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for index, result := range results {
		field := fmt.Sprintf("requests[%d]", index)
		if strings.TrimSpace(result.ID) == "" {
			diags = append(diags, diag.Errorf(ErrorCodeRequired, field+".id", "id is required"))
		}
		if !result.State.IsValid() {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidState, field+".state",
				"state %q is not in the closed vocabulary", result.State))
		}
		if result.Status != 0 && (result.Status < 100 || result.Status > 599) {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidRequest, field+".status", "status %d is not an HTTP status", result.Status))
		}
		if result.DurationMs < 0 {
			diags = append(diags, diag.Errorf(ErrorCodeInvalidRequest, field+".durationMs", "durationMs must not be negative"))
		}
	}
	return diags
}

func validateCleanup(cleanup Cleanup) []diag.Diagnostic {
	switch {
	case cleanup.State == CleanupClean && len(cleanup.Leftovers) == 0:
		return nil
	case cleanup.State == CleanupPartial && len(cleanup.Leftovers) > 0:
		return nil
	default:
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidCleanup, "cleanup",
			"cleanup is clean with no leftovers or partial with at least one")}
	}
}

// unprovenPass lists what a passed verdict is missing. A passed verdict that
// omits a phase, runs no request, or skips the sha comparison proves nothing,
// so each of those is refused rather than read as vacuously true.
func unprovenPass(verdict *Verdict) []diag.Diagnostic {
	var diags []diag.Diagnostic
	byName := map[string]State{}
	for _, phase := range verdict.Phases {
		byName[phase.Name] = phase.State
	}
	for _, name := range PhaseNames {
		if state, ok := byName[name]; !ok || !state.IsPass() {
			diags = append(diags, diag.Errorf(ErrorCodeUnprovenPass, "phases",
				"a passed verdict requires phase %q passed", name))
		}
	}
	if len(verdict.Requests) == 0 {
		diags = append(diags, diag.Errorf(ErrorCodeUnprovenPass, "requests", "a passed verdict requires at least one request"))
	}
	for index, result := range verdict.Requests {
		if !result.State.IsPass() {
			diags = append(diags, diag.Errorf(ErrorCodeUnprovenPass, fmt.Sprintf("requests[%d].state", index),
				"a passed verdict requires every request passed"))
		}
	}
	if verdict.Binding.Kind == BindingArtifact && !strings.HasPrefix(verdict.Binding.ObservedSHA, verdict.Binding.ExpectedSHA) {
		diags = append(diags, diag.Errorf(ErrorCodeUnprovenPass, "binding.observedSHA",
			"a passed artifact verdict requires an observed sha starting with the expected sha"))
	}
	if verdict.Cleanup != nil && verdict.Cleanup.State != CleanupClean {
		diags = append(diags, diag.Errorf(ErrorCodeUnprovenPass, "cleanup", "a passed verdict requires a clean teardown"))
	}
	return diags
}

// sortDiagnostics orders findings by field then code, so a report is stable
// whatever map iteration order produced it.
func sortDiagnostics(diags []diag.Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Field != diags[j].Field {
			return diags[i].Field < diags[j].Field
		}
		return diags[i].Code < diags[j].Code
	})
}
