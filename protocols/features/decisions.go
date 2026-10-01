package features

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The committed decision registries.
//
// A settled value that lives only in prose gets re-decided. Measured on this
// repository's own history: the `Deletes:` rule settled 2026-09-03 held in at
// most 9% of the pull requests a week later, three settled intentions were
// re-opened in eight days, and a later change dropped scale-to-zero for about
// 500 $/month of cloud spend instead of 25 $. There were 173 decision records
// in the tree at the time and `validate` read none of them. Prose does not
// hold against an agent; a check that fails does.
//
// A decisions.json file is that check's input. Each entry carries a stable id,
// the one-line statement, the date it was settled, who settled it, and either
// a CHECK that `validate` can prove or the mark `reviewOnly`. Changing a
// settled value then means editing the registry in the same change, which puts
// the re-decision in the diff a reviewer reads instead of hiding it in the
// code.
//
// A registry has a SCOPE: the directory it sits in. The one at the workspace
// root holds the decisions every project follows; the one in a project
// directory holds the decisions of that project. Every path a registry names —
// its check globs and its `adr` link — is relative to its own directory and
// cannot leave it, so a project registry can only judge its own project's
// files. Ids stay unique across every registry of a workspace, because a
// failure message and a pull request cite an id, not a file. One registry per
// scope keeps a decision next to the code it binds: the agent orienting on a
// project reads that project's decisions, and two teams settling decisions in
// two projects never edit the same file.
//
// The FORMAT lives here and the CONTENT lives in each repository: this package
// owns the wire and the semantics of a check, every workspace commits its own
// registries. That split is the specs.baseline.json precedent — a committed
// document with a protocol schema, a canonical encoder, and strict parsing —
// and it exists for the same reason: the rule a repository settles is not
// Putnami's to know, but whether a repository's own file is well-formed is.
//
// The ADR is justification only. The registry carries the enforceable
// statement; a record under doc/adr explains why it was taken. Neither
// restates the other, so there is no second copy of a rule to drift.

const (
	// DecisionsFilename is the file name of a committed decision registry, at
	// the workspace root or in a project directory.
	DecisionsFilename = "decisions.json"
	// DecisionsProtocolVersion selects the exact registry wire contract.
	DecisionsProtocolVersion = 1
	// DecisionsSchemaURL is the published JSON schema for the file.
	DecisionsSchemaURL = "https://putnami.dev/schemas/putnami-decisions.json"
	// DecisionsSettledLayout is the only accepted `settled` spelling. A date
	// that is read by a human in a failure message and sorted by a machine has
	// exactly one form; anything else would make two registries incomparable.
	DecisionsSettledLayout = "2006-01-02"

	// decisionsMaxEntries bounds one committed registry. A registry is rendered
	// whole wherever an agent reads it — the workspace guidance for the root
	// one, the project context for a project one — so the bound is what keeps
	// that text finite rather than a truncation rule that would silently hide a
	// settled decision, the one failure this whole file exists to stop.
	decisionsMaxEntries = 256
	// decisionsMaxCheckFiles bounds one check's glob set.
	decisionsMaxCheckFiles = 32

	decisionIDMaxLength        = 64
	decisionStatementMaxLength = 256
	decisionSettledByMaxLength = 128
	decisionPointerMaxLength   = 512
	decisionGlobMaxLength      = 512
)

// decisionIDPattern is the stable identity a failure message cites and a pull
// request references. It admits the D-001 spelling the registry was designed
// around and any other bounded, space-free token, because the id vocabulary is
// each repository's to choose; what this package fixes is that it contains no
// whitespace and no control character, so a message can quote it unambiguously.
var decisionIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$`)

// DecisionCheckKind is the vocabulary of provable checks.
type DecisionCheckKind string

// DecisionCheckKindJSONValue is the ONLY kind in v1.
//
// It expresses exactly one sentence: "this declared value is (not) X". That is
// what the decisions this registry was built for need — a scaling floor, a
// replica count, a flag — and a vocabulary invented ahead of a real decision is
// a vocabulary nobody can validate against a real failure. Anything the kind
// cannot express is `reviewOnly` until a decision that needs more exists.
const DecisionCheckKindJSONValue DecisionCheckKind = "json-value"

// DecisionRule is the comparison a json-value check applies.
type DecisionRule string

const (
	// DecisionRuleEquals is violated when the resolved value differs.
	DecisionRuleEquals DecisionRule = "equals"
	// DecisionRuleNotEquals is violated when the resolved value matches.
	DecisionRuleNotEquals DecisionRule = "notEquals"
)

// DecisionMissingDisposition decides a file whose pointer resolves to nothing.
type DecisionMissingDisposition string

const (
	// DecisionMissingSatisfied is the default: a file that does not declare
	// the member says nothing about the decision.
	DecisionMissingSatisfied DecisionMissingDisposition = "satisfied"
	// DecisionMissingViolated is for a decision whose whole point is that the
	// member must be declared.
	DecisionMissingViolated DecisionMissingDisposition = "violated"
)

// DecisionRegistry is the committed set of settled decisions.
type DecisionRegistry struct {
	// Schema identifies the JSON Schema used to author the registry.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the exact registry wire contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Decisions lists the settled decisions, sorted by id.
	Decisions []Decision `json:"decisions"`
}

// Decision is one settled value.
type Decision struct {
	// ID is the stable identity a failure message and a pull request cite.
	ID string `json:"id"`
	// Statement is the settled value in one line, quoted verbatim by every
	// failure message.
	Statement string `json:"statement"`
	// Settled is the YYYY-MM-DD date the decision was taken.
	Settled string `json:"settled"`
	// SettledBy names who took it.
	SettledBy string `json:"settledBy"`
	// ADR optionally links the record that explains WHY, under the existing
	// doc/adr convention, relative to the registry's directory. It never
	// restates the statement.
	ADR string `json:"adr,omitempty"`
	// ReviewOnly marks a decision no check can prove. It is a pointer so an
	// authored `"reviewOnly": false` is distinguishable from an absent mark:
	// the first states nothing and is refused, the second is the ordinary
	// checked shape.
	ReviewOnly *bool `json:"reviewOnly,omitempty"`
	// Check is how validate proves the decision. Exactly one of Check and
	// ReviewOnly is present.
	Check *DecisionCheck `json:"check,omitempty"`
}

// DecisionCheck is the v1 provable check: a JSON member in every file a glob
// set names must (not) hold a declared value.
type DecisionCheck struct {
	// Kind is DecisionCheckKindJSONValue in v1.
	Kind DecisionCheckKind `json:"kind"`
	// Files are globs relative to the registry's directory, at least one. `**`
	// matches zero or more path segments, the same spelling extension file
	// contracts use.
	Files []string `json:"files"`
	// Pointer is an RFC 6901 JSON Pointer into a matched document.
	Pointer string `json:"pointer"`
	// Rule selects the comparison.
	Rule DecisionRule `json:"rule"`
	// Value is the declared JSON scalar the rule compares against.
	Value json.RawMessage `json:"value,omitempty"`
	// WhenMissing decides a file whose pointer resolves to nothing; absent
	// means DecisionMissingSatisfied.
	WhenMissing DecisionMissingDisposition `json:"whenMissing,omitempty"`
}

// IsReviewOnly reports the review-only mark.
func (d Decision) IsReviewOnly() bool { return d.ReviewOnly != nil && *d.ReviewOnly }

// Enforced reports whether validate can prove this decision.
func (d Decision) Enforced() bool { return d.Check != nil }

// ViolationMessage is the ONE sentence every violation is reported with.
//
// It names the id, quotes the statement, names the file, states what was found,
// and closes with the date, the author, and the instruction that makes the
// registry work at all: the way out is to change the decision in the registry
// it names, not the code. A generic lint message here would leave a reader with
// a failing build and no way to tell a bug from a deliberate reversal.
func (d Decision) ViolationMessage(registry, path, detail string) string {
	return fmt.Sprintf("decision %s %q is violated by %s: %s\nSettled %s by %s. To change it, change the decision in %s, not the code.",
		d.ID, d.Statement, path, detail, d.Settled, d.SettledBy, registry)
}

// DecisionRegistryPath is the workspace-relative location of the registry
// whose scope is the slash-separated workspace-relative directory scope. The
// empty scope, and ".", is the workspace root.
func DecisionRegistryPath(scope string) string {
	scope = strings.Trim(path.Clean("/"+scope), "/")
	if scope == "" {
		return DecisionsFilename
	}
	return scope + "/" + DecisionsFilename
}

// DecisionScopeRelative maps a slash-separated workspace-relative file path
// into the scope of the registry at scope. It reports false for a file outside
// that directory: a registry never judges a file it does not contain.
func DecisionScopeRelative(scope, relative string) (string, bool) {
	scope = strings.Trim(path.Clean("/"+scope), "/")
	if scope == "" {
		return relative, true
	}
	if !strings.HasPrefix(relative, scope+"/") {
		return "", false
	}
	return relative[len(scope)+1:], true
}

// EffectiveWhenMissing resolves the default.
func (c *DecisionCheck) EffectiveWhenMissing() DecisionMissingDisposition {
	if c == nil || c.WhenMissing == "" {
		return DecisionMissingSatisfied
	}
	return c.WhenMissing
}

// ParseDecisionRegistry strictly parses one committed registry.
func ParseDecisionRegistry(data []byte) (*DecisionRegistry, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, DecisionsProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var registry DecisionRegistry
	if err := decodeStrictJSON(data, &registry); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	return &registry, nil
}

// ParseAndValidateDecisionRegistry performs strict parsing followed by
// validation. It fails closed: a registry with any error returns nil, because
// a partially understood set of settled decisions is worse than none — it
// would report the entries it happened to parse as the whole policy.
func ParseAndValidateDecisionRegistry(data []byte) (*DecisionRegistry, []diag.Diagnostic) {
	registry, diagnostics := ParseDecisionRegistry(data)
	if registry == nil {
		return nil, diagnostics
	}
	diagnostics = append(diagnostics, ValidateDecisionRegistry(registry)...)
	sortDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	return registry, diagnostics
}

// ValidateDecisionRegistry validates one registry in isolation.
//
// Everything a single document can decide is decided here. Whether a linked
// ADR path exists in the worktree is not: that is a filesystem fact, and the
// caller that has the worktree checks it.
func ValidateDecisionRegistry(input *DecisionRegistry) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "decision registry is nil")}
	}
	var diagnostics []diag.Diagnostic
	if input.ProtocolVersion != DecisionsProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported (want %d)", input.ProtocolVersion, DecisionsProtocolVersion))
	}
	if input.Decisions == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "decisions", "decisions is required and must be an array"))
	}
	if len(input.Decisions) > decisionsMaxEntries {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeSensitiveContent, "decisions",
			"registry exceeds the bounded decision limit of %d", decisionsMaxEntries))
		sortDiagnostics(diagnostics)
		return diagnostics
	}
	seen := make(map[string]bool, len(input.Decisions))
	for i, decision := range input.Decisions {
		field := fmt.Sprintf("decisions[%d]", i)
		diagnostics = append(diagnostics, validateDecision(field, decision, seen)...)
		seen[decision.ID] = true
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateDecision(field string, decision Decision, seen map[string]bool) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if len(decision.ID) > decisionIDMaxLength || !decisionIDPattern.MatchString(decision.ID) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".id",
			"decision id must be at most %d characters of letters, digits, '.', '_' or '-'", decisionIDMaxLength))
	} else if seen[decision.ID] {
		// A duplicate id makes the registry undecidable: two statements answer
		// to the same citation, and a failure message would name a decision a
		// reader cannot find.
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".id",
			"decision id %q is duplicated", decision.ID))
	}
	if !boundedText(decision.Statement, decisionStatementMaxLength) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".statement",
			"statement must be one non-empty line of at most %d characters without control characters", decisionStatementMaxLength))
	}
	if parsed, err := time.Parse(DecisionsSettledLayout, decision.Settled); err != nil || parsed.Format(DecisionsSettledLayout) != decision.Settled {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".settled",
			"settled must be a calendar date spelled YYYY-MM-DD, got %q", decision.Settled))
	}
	if !boundedText(decision.SettledBy, decisionSettledByMaxLength) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".settledBy",
			"settledBy must be non-empty bounded text without control characters"))
	}
	if decision.ADR != "" && len(validateDecisionLink(field+".adr", decision.ADR)) > 0 {
		// The shared link rule is the spec one, and its message says
		// "workspace-relative"; a registry's link is relative to the registry.
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".adr",
			"adr must be a %s/<name>%s record relative to the registry's directory, at most %d characters",
			DecisionDirectory, DecisionExtension, decisionLinkMaxLength))
	}
	switch {
	case decision.Check != nil && decision.ReviewOnly != nil:
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field,
			"a decision carries either a check or the reviewOnly mark, never both"))
	case decision.Check == nil && decision.ReviewOnly == nil:
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field,
			"a decision must carry a check validate can prove, or the reviewOnly mark"))
	case decision.ReviewOnly != nil && !*decision.ReviewOnly:
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".reviewOnly",
			"reviewOnly must be true when present; false states neither a check nor a review-only decision"))
	case decision.Check != nil:
		diagnostics = append(diagnostics, validateDecisionCheck(field+".check", decision.Check)...)
	}
	return diagnostics
}

func validateDecisionCheck(field string, check *DecisionCheck) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if check.Kind != DecisionCheckKindJSONValue {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".kind",
			"check kind %q is not supported (want %q)", check.Kind, DecisionCheckKindJSONValue))
	}
	switch {
	case len(check.Files) == 0:
		// A check with no glob reads nothing and can never fail, which is the
		// shape of a decision that looks enforced and is not.
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".files",
			"a check must name at least one glob"))
	case len(check.Files) > decisionsMaxCheckFiles:
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".files",
			"a check names at most %d globs", decisionsMaxCheckFiles))
	default:
		globs := make(map[string]bool, len(check.Files))
		for i, glob := range check.Files {
			globField := fmt.Sprintf("%s.files[%d]", field, i)
			if err := validateDecisionGlob(glob); err != nil {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, globField,
					"glob %q is invalid: %v", glob, err))
				continue
			}
			if globs[glob] {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, globField,
					"glob %q is duplicated", glob))
			}
			globs[glob] = true
		}
	}
	if err := ValidateJSONPointer(check.Pointer); err != nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".pointer",
			"pointer %q is not a valid RFC 6901 JSON Pointer: %v", check.Pointer, err))
	} else if len(check.Pointer) > decisionPointerMaxLength {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".pointer",
			"pointer must be at most %d characters", decisionPointerMaxLength))
	}
	if check.Rule != DecisionRuleEquals && check.Rule != DecisionRuleNotEquals {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".rule",
			"rule %q is not supported (want %q or %q)", check.Rule, DecisionRuleEquals, DecisionRuleNotEquals))
	}
	if _, err := decisionScalar(check.Value); err != nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".value",
			"value must be a JSON string, number or boolean: %v", err))
	}
	switch check.WhenMissing {
	case "", DecisionMissingSatisfied, DecisionMissingViolated:
	default:
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, field+".whenMissing",
			"whenMissing %q is not supported (want %q or %q)", check.WhenMissing, DecisionMissingSatisfied, DecisionMissingViolated))
	}
	return diagnostics
}

// validateDecisionGlob keeps a check's read set inside the registry's
// directory. The globs are relative to it by construction, so an absolute path,
// a Windows drive prefix, a backslash separator or a `..` segment is refused
// rather than resolved: each of them would make the same registry read
// different files on two machines, or a project registry judge a file of
// another scope.
func validateDecisionGlob(glob string) error {
	if strings.TrimSpace(glob) == "" {
		return fmt.Errorf("glob is empty")
	}
	if len(glob) > decisionGlobMaxLength {
		return fmt.Errorf("glob is longer than %d characters", decisionGlobMaxLength)
	}
	if !boundedText(glob, decisionGlobMaxLength) {
		return fmt.Errorf("glob carries a control character")
	}
	if strings.ContainsRune(glob, '\\') {
		return fmt.Errorf(`glob must use "/" separators`)
	}
	if path.IsAbs(glob) {
		return fmt.Errorf("glob must be relative to the registry's directory")
	}
	for _, segment := range strings.Split(glob, "/") {
		if segment == ".." {
			return fmt.Errorf("glob must not escape the registry's directory")
		}
	}
	for _, segment := range strings.Split(glob, "/") {
		if segment == "**" {
			continue
		}
		if strings.Contains(segment, "**") {
			return fmt.Errorf("`**` must be a whole path segment")
		}
		if _, err := path.Match(segment, ""); err != nil {
			return fmt.Errorf("invalid glob syntax: %w", err)
		}
	}
	return nil
}

// MatchDecisionFileGlob matches one slash-separated path, relative to the
// registry's directory, against one check glob. A whole `**` segment matches zero or more path
// segments; every other segment is matched by path.Match, so `*` and `?` never
// cross a separator. The semantics are the ones extension file contracts
// already use, so a repository authors one kind of pattern.
func MatchDecisionFileGlob(relative, glob string) bool {
	relative = strings.TrimPrefix(relative, "./")
	glob = strings.TrimPrefix(glob, "./")
	return matchDecisionSegments(strings.Split(glob, "/"), strings.Split(relative, "/"))
}

func matchDecisionSegments(glob, relative []string) bool {
	if len(glob) == 0 {
		return len(relative) == 0
	}
	if glob[0] == "**" {
		if matchDecisionSegments(glob[1:], relative) {
			return true
		}
		for i := range relative {
			if matchDecisionSegments(glob[1:], relative[i+1:]) {
				return true
			}
		}
		return false
	}
	if len(relative) == 0 {
		return false
	}
	matched, err := path.Match(glob[0], relative[0])
	return err == nil && matched && matchDecisionSegments(glob[1:], relative[1:])
}

// ValidateJSONPointer reports whether pointer is a well-formed RFC 6901 JSON
// Pointer. The empty pointer names the whole document and is valid.
func ValidateJSONPointer(pointer string) error {
	if pointer == "" {
		return nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return fmt.Errorf(`a non-empty pointer must start with "/"`)
	}
	for _, token := range strings.Split(pointer[1:], "/") {
		for i := 0; i < len(token); i++ {
			if token[i] != '~' {
				continue
			}
			if i+1 >= len(token) || (token[i+1] != '0' && token[i+1] != '1') {
				return fmt.Errorf(`"~" must be escaped as "~0" or "~1"`)
			}
			i++
		}
	}
	return nil
}

// ResolveJSONPointer resolves an RFC 6901 pointer against a decoded JSON
// document and reports whether it named anything.
//
// It is hand-rolled rather than taken from a library on purpose: the pointer
// semantics are part of this wire contract, and a dependency would put the
// meaning of a committed decision outside the protocol that defines it. The
// escaping order is the RFC's — `~1` before `~0` — so a member literally named
// `a~1b` round-trips instead of colliding with `a/b`.
func ResolveJSONPointer(document any, pointer string) (any, bool) {
	if err := ValidateJSONPointer(pointer); err != nil {
		return nil, false
	}
	if pointer == "" {
		return document, true
	}
	current := document
	for _, token := range strings.Split(pointer[1:], "/") {
		token = strings.ReplaceAll(token, "~1", "/")
		token = strings.ReplaceAll(token, "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			child, found := node[token]
			if !found {
				return nil, false
			}
			current = child
		case []any:
			index, err := strconv.Atoi(token)
			// RFC 6901 admits "0" and [1-9][0-9]*; "-" names the element after
			// the last, which never exists in a document being read.
			if err != nil || index < 0 || index >= len(node) ||
				(len(token) > 1 && token[0] == '0') {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

// DecisionPointerDisplay renders a pointer the way a failure message reads it:
// `/scaling/minInstances` becomes `scaling.minInstances`. The tokens are
// unescaped first, so the rendering shows the member a reader will search for.
func DecisionPointerDisplay(pointer string) string {
	if pointer == "" {
		return "the document"
	}
	tokens := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, token := range tokens {
		token = strings.ReplaceAll(token, "~1", "/")
		tokens[i] = strings.ReplaceAll(token, "~0", "~")
	}
	return strings.Join(tokens, ".")
}

// DecisionCheckOutcome is the verdict of one check against one matched file.
type DecisionCheckOutcome struct {
	// Violated reports whether this file breaks the decision.
	Violated bool
	// Detail is the tail of the failure sentence: what was found, in the words
	// the file's author would recognize.
	Detail string
}

// EvaluateDecisionCheck judges one matched file's bytes.
//
// A file that is not valid JSON is a VIOLATION naming the parse error, never a
// skip. Skipping would let a decision be defeated by breaking the document the
// check reads — the check would report nothing and the gate would stay green,
// which is the opposite of what a settled decision is for.
func EvaluateDecisionCheck(check *DecisionCheck, document []byte) DecisionCheckOutcome {
	if check == nil {
		return DecisionCheckOutcome{}
	}
	decoded, err := decodeDecisionDocument(document)
	if err != nil {
		return DecisionCheckOutcome{Violated: true, Detail: fmt.Sprintf("the file is not valid JSON: %v", err)}
	}
	display := DecisionPointerDisplay(check.Pointer)
	resolved, found := ResolveJSONPointer(decoded, check.Pointer)
	if !found {
		if check.EffectiveWhenMissing() == DecisionMissingViolated {
			return DecisionCheckOutcome{Violated: true, Detail: fmt.Sprintf("%s is not declared", display)}
		}
		return DecisionCheckOutcome{}
	}
	expected, err := decisionScalar(check.Value)
	if err != nil {
		// ValidateDecisionRegistry refuses this registry before any file is
		// read, so reaching here means a caller evaluated an unvalidated check.
		// Failing closed keeps that mistake loud instead of green.
		return DecisionCheckOutcome{Violated: true, Detail: fmt.Sprintf("the decision declares no comparable value: %v", err)}
	}
	equal := decisionValuesEqual(resolved, expected)
	if (check.Rule == DecisionRuleEquals) == equal {
		return DecisionCheckOutcome{}
	}
	return DecisionCheckOutcome{Violated: true, Detail: fmt.Sprintf("%s = %s", display, renderDecisionValue(resolved))}
}

func decodeDecisionDocument(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	// UseNumber keeps a JSON number as its literal, so a comparison never goes
	// through float64 unless both sides genuinely need to.
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values are not allowed")
		}
		return nil, err
	}
	return value, nil
}

// decisionScalar decodes the declared value and refuses anything but a JSON
// scalar. An object or an array as the declared value would invite structural
// comparison, which v1 does not do.
func decisionScalar(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("value is required")
	}
	decoded, err := decodeDecisionDocument(raw)
	if err != nil {
		return nil, err
	}
	switch decoded.(type) {
	case string, bool, json.Number:
		return decoded, nil
	default:
		return nil, fmt.Errorf("value must be a string, a number or a boolean")
	}
}

// decisionValuesEqual compares a resolved document value with a declared one.
//
// Numbers compare BY VALUE, never by spelling: 0, 0.0 and -0 are the same
// declared floor, and a registry that disagreed with a formatter about which
// one a file holds would fail a build for a reformat. Integers compare exactly
// through int64 so a large identifier is not rounded; anything else falls back
// to float64. A resolved object or array never equals a scalar, so a decision
// about a member cannot be satisfied by a member of the wrong shape.
func decisionValuesEqual(resolved, expected any) bool {
	switch want := expected.(type) {
	case string:
		got, ok := resolved.(string)
		return ok && got == want
	case bool:
		got, ok := resolved.(bool)
		return ok && got == want
	case json.Number:
		got, ok := resolved.(json.Number)
		if !ok {
			return false
		}
		if left, leftErr := got.Int64(); leftErr == nil {
			if right, rightErr := want.Int64(); rightErr == nil {
				return left == right
			}
		}
		left, leftErr := got.Float64()
		right, rightErr := want.Float64()
		return leftErr == nil && rightErr == nil && left == right
	default:
		return false
	}
}

// renderDecisionValue spells a resolved value the way it appears in the file:
// a number keeps its literal, a string keeps its quotes so an empty one is
// visible, and a container is named by kind rather than dumped into a message.
func renderDecisionValue(value any) string {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case string:
		return strconv.Quote(typed)
	case bool:
		return strconv.FormatBool(typed)
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// CanonicalDecisionRegistry returns a deterministically ordered copy:
// decisions sorted by id, each check's globs sorted, and the published schema
// URL stamped so an authored file round-trips to one byte form.
func CanonicalDecisionRegistry(input *DecisionRegistry) *DecisionRegistry {
	if input == nil {
		return nil
	}
	out := *input
	out.Schema = DecisionsSchemaURL
	out.Decisions = append(make([]Decision, 0, len(input.Decisions)), input.Decisions...)
	for i := range out.Decisions {
		if out.Decisions[i].ReviewOnly != nil {
			mark := *out.Decisions[i].ReviewOnly
			out.Decisions[i].ReviewOnly = &mark
		}
		if out.Decisions[i].Check == nil {
			continue
		}
		check := *out.Decisions[i].Check
		check.Files = append(make([]string, 0, len(check.Files)), check.Files...)
		sort.Strings(check.Files)
		check.Value = append(json.RawMessage(nil), check.Value...)
		out.Decisions[i].Check = &check
	}
	sort.Slice(out.Decisions, func(i, j int) bool { return out.Decisions[i].ID < out.Decisions[j].ID })
	return &out
}

// MarshalDecisionRegistry encodes the canonical wire form.
func MarshalDecisionRegistry(registry *DecisionRegistry) ([]byte, error) {
	return marshalCanonical(CanonicalDecisionRegistry(registry))
}
