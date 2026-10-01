package cli

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"sort"
)

// Violation is one breach of the v2 machine contract: a stable code and the
// JSON path it was found at. Codes and paths are part of the contract — the
// cross-language conformance corpus (conformance/manifest.json) pins the exact
// set each fixture produces, so a Go/TypeScript divergence in either fails both
// runtimes' tests instead of drifting silently.
type Violation struct {
	// Code is one of the closed set of violation codes declared below.
	Code string `json:"code"`
	// Path is where in the document the breach was found: dot-separated members
	// from the root, with array elements indexed ("run.counts.total",
	// "commands[11]"). It is empty for a breach of the document as a whole,
	// such as ViolationInvalidJSON.
	Path string `json:"path"`
}

// Violation codes. The set is closed: a corpus case naming a code outside it is
// itself a test failure.
const (
	// ViolationInvalidJSON: the document did not parse, or is not an object.
	ViolationInvalidJSON = "cli.result.invalid_json"
	// ViolationUnknownField: a member the document type does not declare.
	ViolationUnknownField = "cli.result.unknown_field"
	// ViolationMissingField: a required member is absent.
	ViolationMissingField = "cli.result.missing_field"
	// ViolationUnexpectedField: a declared member that this variant forbids
	// (e.g. "task" on a session:end record).
	ViolationUnexpectedField = "cli.result.unexpected_field"
	// ViolationInvalidType: a member has the wrong JSON type.
	ViolationInvalidType = "cli.result.invalid_type"
	// ViolationInvalidEnum: a value outside a closed vocabulary.
	ViolationInvalidEnum = "cli.result.invalid_enum"
	// ViolationInvalidValue: a value of the right type outside its allowed
	// range (an empty required string, a negative count).
	ViolationInvalidValue = "cli.result.invalid_value"
	// ViolationInvalidProtocolVersion: protocolVersion is not 2.
	ViolationInvalidProtocolVersion = "cli.result.invalid_protocol_version"
	// ViolationInvalidKey: identity.key disagrees with the structured identity
	// it is derived from.
	ViolationInvalidKey = "cli.result.invalid_key"
	// ViolationCountMismatch: the run histograms do not add up.
	ViolationCountMismatch = "cli.result.count_mismatch"
	// ViolationOutcomeMismatch: the verdict breaks the unified-success rule or
	// the abort > failure > success precedence.
	ViolationOutcomeMismatch = "cli.result.outcome_mismatch"
	// ViolationExitCodeMismatch: an exit code disagrees with the verdict it
	// accompanies, or two surfaces of one document disagree about it.
	ViolationExitCodeMismatch = "cli.result.exit_code_mismatch"
	// ViolationErrorMismatch: the error member's presence or class disagrees
	// with the verdict.
	ViolationErrorMismatch = "cli.result.error_mismatch"
	// ViolationBudgetExceeded: a bounded machine-output sequence or its final
	// record exceeds the selected byte/record budget.
	ViolationBudgetExceeded = "cli.result.budget_exceeded"
	// ViolationElisionMismatch: live selection or split elision counters do not
	// match the complete retained artifact.
	ViolationElisionMismatch = "cli.result.elision_mismatch"
	// ViolationUnsanitized: terminal controls or an unredacted sensitive value
	// reached a machine-output record.
	ViolationUnsanitized = "cli.result.unsanitized"
)

// ValidViolationCodes is the closed violation vocabulary.
var ValidViolationCodes = map[string]bool{
	ViolationInvalidJSON:            true,
	ViolationUnknownField:           true,
	ViolationMissingField:           true,
	ViolationUnexpectedField:        true,
	ViolationInvalidType:            true,
	ViolationInvalidEnum:            true,
	ViolationInvalidValue:           true,
	ViolationInvalidProtocolVersion: true,
	ViolationInvalidKey:             true,
	ViolationCountMismatch:          true,
	ViolationOutcomeMismatch:        true,
	ViolationExitCodeMismatch:       true,
	ViolationErrorMismatch:          true,
	ViolationBudgetExceeded:         true,
	ViolationElisionMismatch:        true,
	ViolationUnsanitized:            true,
}

// ValidDocumentKinds is the closed set of v2 documents.
var ValidDocumentKinds = map[DocumentKind]bool{
	DocumentResultEnvelope:      true,
	DocumentSessionStreamRecord: true,
	DocumentMCPResult:           true,
	DocumentSessionFile:         true,
	DocumentSessionPlanFile:     true,
	DocumentReportFile:          true,
}

// ValidateDocument validates one v2 machine document of the given kind and
// returns every violation it found, sorted by path then code.
//
// Validation walks the DECODED JSON value rather than round-tripping through
// the Go structs on purpose: the TypeScript mirror walks the same value the
// same way, so both runtimes report the same codes at the same paths and the
// corpus can pin them exactly. An empty result means the document conforms.
func ValidateDocument(kind DocumentKind, data []byte) []Violation {
	v := &validator{}
	if !ValidDocumentKinds[kind] {
		v.add(ViolationInvalidJSON, "")
		return v.sorted()
	}
	hasUnpairedSurrogate := kind == DocumentSessionStreamRecord && hasUnpairedJSONSurrogate(data)
	root, ok := decodeDocument(v, data)
	if !ok {
		return v.sorted()
	}
	switch kind {
	case DocumentResultEnvelope:
		validateResultEnvelope(v, root)
	case DocumentSessionStreamRecord:
		validateSessionStreamRecord(v, root)
		if _, bounded := childValue(root, "machineOutput"); bounded {
			if hasUnpairedSurrogate {
				v.add(ViolationUnsanitized, "")
			} else {
				checkMachineOutputSanitized(v, "", root)
			}
		}
		checkFinalRecordReserve(v, root)
	case DocumentMCPResult:
		validateMCPResult(v, root)
	case DocumentSessionFile:
		validateSessionFile(v, root)
	case DocumentSessionPlanFile:
		validateSessionPlanFile(v, root)
	case DocumentReportFile:
		validateReportFile(v, root)
	}
	return v.sorted()
}

// decodeDocument parses data as a single JSON object, keeping numbers exact so
// integer members can be checked without float rounding.
func decodeDocument(v *validator, data []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		v.add(ViolationInvalidJSON, "")
		return nil, false
	}
	if dec.More() {
		v.add(ViolationInvalidJSON, "")
		return nil, false
	}
	obj, ok := root.(map[string]any)
	if !ok {
		v.add(ViolationInvalidJSON, "")
		return nil, false
	}
	return obj, true
}

// validator accumulates violations.
type validator struct {
	violations []Violation
}

func (v *validator) add(code, path string) {
	v.violations = append(v.violations, Violation{Code: code, Path: path})
}

// sorted returns the violations in the contract's deterministic order: by path,
// then by code. Both runtimes sort, so neither traversal order nor map
// iteration order can make the two disagree.
func (v *validator) sorted() []Violation {
	out := append([]Violation(nil), v.violations...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// check validates one member's value at path.
type check func(v *validator, path string, value any)

// field is one declared member of an object.
type field struct {
	name     string
	required bool
	check    check
}

func req(name string, c check) field { return field{name: name, required: true, check: c} }
func opt(name string, c check) field { return field{name: name, check: c} }

// join builds a child path.
func join(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

// object checks value is an object with exactly the declared members: unknown
// members and missing required members are violations, and every present
// member is checked. It returns the object so the semantic rules below can read
// the values structural validation already accepted.
func object(v *validator, path string, value any, fields []field) map[string]any {
	obj, ok := value.(map[string]any)
	if !ok {
		v.add(ViolationInvalidType, path)
		return nil
	}
	declared := make(map[string]bool, len(fields))
	for _, f := range fields {
		declared[f.name] = true
	}
	for _, name := range sortedKeys(obj) {
		if !declared[name] {
			v.add(ViolationUnknownField, join(path, name))
		}
	}
	for _, f := range fields {
		child, present := obj[f.name]
		if !present {
			if f.required {
				v.add(ViolationMissingField, join(path, f.name))
			}
			continue
		}
		f.check(v, join(path, f.name), child)
	}
	return obj
}

func sortedKeys(obj map[string]any) []string {
	out := make([]string, 0, len(obj))
	for k := range obj {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// nonEmptyString requires a string with at least one character.
func nonEmptyString(v *validator, path string, value any) {
	text, ok := value.(string)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	if text == "" {
		v.add(ViolationInvalidValue, path)
	}
}

func nonEmptyStringBytesAtMost(maximum int) check {
	return func(v *validator, path string, value any) {
		text, ok := value.(string)
		if !ok {
			v.add(ViolationInvalidType, path)
			return
		}
		if text == "" || len([]byte(text)) > maximum {
			v.add(ViolationInvalidValue, path)
		}
	}
}

// enumOf requires a string drawn from a closed vocabulary.
func enumOf(allowed ...string) check {
	return func(v *validator, path string, value any) {
		text, ok := value.(string)
		if !ok {
			v.add(ViolationInvalidType, path)
			return
		}
		if slices.Contains(allowed, text) {
			return
		}
		v.add(ViolationInvalidEnum, path)
	}
}

// intAtLeast requires an integer no smaller than min.
func intAtLeast(minimum int64) check {
	return func(v *validator, path string, value any) {
		n, ok := wholeNumber(value)
		if !ok {
			v.add(ViolationInvalidType, path)
			return
		}
		if n < minimum {
			v.add(ViolationInvalidValue, path)
		}
	}
}

// percentage requires a JSON number in [0,100]. It is the contract's only
// non-integer member: a coverage share is a measurement, not a count, and
// rounding it to an integer would erase the difference between 89.4% and 89.6%
// against a 90% threshold.
//
// A number outside the range — including one too large for a float64, which is
// exactly the JavaScript mirror's Infinity — is an invalid VALUE rather than an
// invalid type: the wire form was a number, it just was not a percentage.
func percentage(v *validator, path string, value any) {
	number, ok := value.(json.Number)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	share, err := number.Float64()
	if err != nil || math.IsNaN(share) || math.IsInf(share, 0) || share < 0 || share > 100 {
		v.add(ViolationInvalidValue, path)
	}
}

// protocolVersion requires exactly ResultProtocolVersion.
func protocolVersion(v *validator, path string, value any) {
	n, ok := wholeNumber(value)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	if n != ResultProtocolVersion {
		v.add(ViolationInvalidProtocolVersion, path)
	}
}

func boolean(v *validator, path string, value any) {
	if _, ok := value.(bool); !ok {
		v.add(ViolationInvalidType, path)
	}
}

// anyValue accepts whatever a command chose to put in a free-form member.
func anyValue(_ *validator, _ string, _ any) {}

// openObject accepts any object without inspecting its members — used for
// payloads another protocol owns.
func openObject(v *validator, path string, value any) {
	if _, ok := value.(map[string]any); !ok {
		v.add(ViolationInvalidType, path)
	}
}

// arrayOf checks every element of an array.
func arrayOf(item check) check {
	return func(v *validator, path string, value any) {
		items, ok := value.([]any)
		if !ok {
			v.add(ViolationInvalidType, path)
			return
		}
		for i, element := range items {
			item(v, indexPath(path, i), element)
		}
	}
}

// intMapValues checks an object whose values are all non-negative integers.
func intMapValues(v *validator, path string, value any) {
	obj, ok := value.(map[string]any)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	for _, name := range sortedKeys(obj) {
		intAtLeast(0)(v, join(path, name), obj[name])
	}
}

// maxSafeInteger bounds every integer member to IEEE-754's exactly
// representable range (2^53 - 1), so a value Go could hold but a JavaScript
// consumer would silently round is rejected identically by both runtimes.
const maxSafeInteger = 1<<53 - 1

// wholeNumber reads a JSON number carrying an integral value in the safe
// range. The rule matches what JSON Schema's "integer" implies ("1.0" and
// "1e2" conform) and the TypeScript mirror's Number.isSafeInteger exactly, so
// the two runtimes accept the same numbers whatever their wire encoding.
func wholeNumber(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	if n, err := number.Int64(); err == nil {
		if n < -maxSafeInteger || n > maxSafeInteger {
			return 0, false
		}
		return n, true
	}
	f, err := number.Float64()
	if err != nil || f != math.Trunc(f) || f < -maxSafeInteger || f > maxSafeInteger {
		return 0, false
	}
	return int64(f), true
}

func indexPath(path string, i int) string {
	return path + "[" + itoa(i) + "]"
}

// itoa avoids a strconv import for the only number this package formats.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for i > 0 {
		pos--
		digits[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(digits[pos:])
}
