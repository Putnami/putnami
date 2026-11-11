package features

import (
	"errors"
	"io/fs"
	"path"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The one-file-per-decision registry form.
//
// A scope's `decisions/` directory holds one `<id>.json` file per decision.
// Each file is one decision with its own `protocolVersion`, and its name is its
// id, so the id is unique inside the scope by construction and a reader finds
// a cited decision by its name alone. Two changes that each settle a decision
// add two files: neither edits a line the other edits, so both merge without
// a conflict, whatever order they land in.
//
// The single `decisions.json` file stays a supported form. A scope holds one
// form or the other: a decision that could sit in either place would answer to
// two files, and a reader could not tell which one a change has to edit.

const (
	// DecisionsDirname is the directory of the one-file-per-decision form, at
	// the workspace root or in a project directory.
	DecisionsDirname = "decisions"
	// DecisionSchemaURL is the published JSON schema of one decision file.
	DecisionSchemaURL = "https://putnami.dev/schemas/putnami-decision.json"
	// decisionFileExtension is the extension of a decision file. Any other
	// entry of the directory is not a decision and is not read.
	decisionFileExtension = ".json"
)

// DecisionDocument is one committed `decisions/<id>.json` file: a decision with
// the protocol version of its wire.
type DecisionDocument struct {
	// Schema identifies the JSON Schema used to author the file.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the exact wire contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// ID is the decision's stable identity; the file is named `<ID>.json`.
	ID string `json:"id"`
	// Statement is the settled value in one line.
	Statement string `json:"statement"`
	// Settled is the YYYY-MM-DD date the decision was taken.
	Settled string `json:"settled"`
	// SettledBy names who took it.
	SettledBy string `json:"settledBy"`
	// ADR optionally links the record that explains why, relative to the
	// scope directory: the directory that holds `decisions/`.
	ADR string `json:"adr,omitempty"`
	// ReviewOnly marks a decision no check can prove.
	ReviewOnly *bool `json:"reviewOnly,omitempty"`
	// Check is how validate proves the decision.
	Check *DecisionCheck `json:"check,omitempty"`
}

// Decision returns the decision the document holds.
func (d DecisionDocument) Decision() Decision {
	return Decision{
		ID:         d.ID,
		Statement:  d.Statement,
		Settled:    d.Settled,
		SettledBy:  d.SettledBy,
		ADR:        d.ADR,
		ReviewOnly: d.ReviewOnly,
		Check:      d.Check,
	}
}

// DecisionsDirectoryPath is the workspace-relative `decisions/` directory of
// the scope at the slash-separated workspace-relative directory scope. The
// empty scope, and ".", is the workspace root.
func DecisionsDirectoryPath(scope string) string {
	scope = strings.Trim(path.Clean("/"+scope), "/")
	if scope == "" {
		return DecisionsDirname
	}
	return scope + "/" + DecisionsDirname
}

// DecisionFilePath is the workspace-relative file of decision id in the scope
// at scope.
func DecisionFilePath(scope, id string) string {
	return DecisionsDirectoryPath(scope) + "/" + id + decisionFileExtension
}

// ParseAndValidateDecisionFile strictly parses and validates one decision file
// whose base name is name. It refuses a wrong or missing protocol version, an
// unknown member, an explicit null, a trailing value, an invalid decision, and
// a file whose name is not its id followed by `.json`. It fails closed: a file
// with any error returns nil.
func ParseAndValidateDecisionFile(name string, data []byte) (*Decision, []diag.Diagnostic) {
	if err := requireExactProtocolVersion(data, DecisionsProtocolVersion); err != nil {
		return nil, []diag.Diagnostic{versionOrParseDiagnostic(err)}
	}
	var document DecisionDocument
	if err := decodeStrictJSON(data, &document); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	if err := rejectExplicitNulls(data); err != nil {
		return nil, []diag.Diagnostic{parseDiagnostic(err)}
	}
	decision := document.Decision()
	diagnostics := validateDecision("", decision, nil)
	if want := decision.ID + decisionFileExtension; name != want && decisionIDPattern.MatchString(decision.ID) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, "id",
			"decision id %q does not match the file name %s; a decision file is named <id>%s", decision.ID, name, decisionFileExtension))
	}
	sortDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		return nil, diagnostics
	}
	return &decision, diagnostics
}

// MarshalDecisionFile encodes the canonical wire form of one decision file:
// the published schema URL and protocol version stamped, and the check's globs
// sorted.
func MarshalDecisionFile(decision Decision) ([]byte, error) {
	canonical := CanonicalDecisionRegistry(&DecisionRegistry{Decisions: []Decision{decision}}).Decisions[0]
	return marshalCanonical(DecisionDocument{
		Schema:          DecisionSchemaURL,
		ProtocolVersion: DecisionsProtocolVersion,
		ID:              canonical.ID,
		Statement:       canonical.Statement,
		Settled:         canonical.Settled,
		SettledBy:       canonical.SettledBy,
		ADR:             canonical.ADR,
		ReviewOnly:      canonical.ReviewOnly,
		Check:           canonical.Check,
	})
}

// DecisionFiles reads a workspace for ReadScopeDecisions. Every path is
// slash-separated and relative to the workspace root.
type DecisionFiles interface {
	// ReadFile returns the content of the regular file name. An absent file
	// is an error that wraps fs.ErrNotExist.
	ReadFile(name string) ([]byte, error)
	// ReadDir returns the base names of the regular files directly in the
	// directory dir. An absent directory is an error that wraps
	// fs.ErrNotExist.
	ReadDir(dir string) ([]string, error)
}

// ScopeDecisions is the registry of one scope, in whichever form it holds.
type ScopeDecisions struct {
	// Scope is the slash-separated workspace-relative directory, "" for the
	// workspace root.
	Scope string
	// Present reports whether the scope holds a registry: a decisions.json
	// file, or a decisions/ directory with at least one decision file.
	Present bool
	// Location is the workspace-relative registry: the decisions.json file or
	// the decisions/ directory. It is empty when Present is false.
	Location string
	// Decisions are the scope's decisions sorted by id, or nil when the
	// registry has an error.
	Decisions []SourcedDecision
}

// SourcedDecision is one decision and the file that holds it.
type SourcedDecision struct {
	Decision Decision
	// Source is the workspace-relative file a change to this decision edits:
	// the scope's decisions.json, or its decisions/<id>.json.
	Source string
}

// ReadScopeDecisions reads the registry of the scope at scope from files.
//
// An absent registry is (Present false, no diagnostic): adoption. Every
// diagnostic is anchored on the file it came from, as "<path>#<member>". The
// read fails closed: when any diagnostic is an error, Decisions is nil,
// because a registry read in part would report the decisions that happened to
// parse as the whole policy.
func ReadScopeDecisions(files DecisionFiles, scope string) (ScopeDecisions, []diag.Diagnostic) {
	scope = strings.Trim(path.Clean("/"+scope), "/")
	result := ScopeDecisions{Scope: scope}
	registryPath := DecisionRegistryPath(scope)
	directoryPath := DecisionsDirectoryPath(scope)

	single, singlePresent, diagnostics := readDecisionRegistryFile(files, registryPath)
	names, listFindings := decisionFileNames(files, directoryPath)
	diagnostics = append(diagnostics, listFindings...)
	directoryPresent := len(names) > 0

	switch {
	case singlePresent && directoryPresent:
		result.Present = true
		result.Location = directoryPath
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecisionRegistry, registryPath,
			"%s and %s/ both hold decisions; a scope holds one form: move each decision of %s to %s/<id>.json and delete %s",
			registryPath, directoryPath, registryPath, directoryPath, registryPath))
	case directoryPresent:
		result.Present = true
		result.Location = directoryPath
		decisions, fileFindings := readDecisionDirectory(files, directoryPath, names)
		diagnostics = append(diagnostics, fileFindings...)
		result.Decisions = decisions
	case singlePresent:
		result.Present = true
		result.Location = registryPath
		if single != nil {
			canonical := CanonicalDecisionRegistry(single)
			result.Decisions = make([]SourcedDecision, 0, len(canonical.Decisions))
			for _, decision := range canonical.Decisions {
				result.Decisions = append(result.Decisions, SourcedDecision{Decision: decision, Source: registryPath})
			}
		}
	}
	sortDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		result.Decisions = nil
	}
	return result, diagnostics
}

// readDecisionRegistryFile reads the single-file form. Absence is
// (nil, false, nil).
func readDecisionRegistryFile(files DecisionFiles, registryPath string) (*DecisionRegistry, bool, []diag.Diagnostic) {
	data, err := files.ReadFile(registryPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, []diag.Diagnostic{diag.Errorf(ErrorCodeDecisionsUnreadable,
			registryPath, "read committed decision registry: %v", err)}
	}
	registry, findings := ParseAndValidateDecisionRegistry(data)
	return registry, true, anchorDecisionFindings(registryPath, findings)
}

// decisionFileNames lists the decision files of a decisions/ directory,
// sorted. An absent directory lists none.
func decisionFileNames(files DecisionFiles, directoryPath string) ([]string, []diag.Diagnostic) {
	entries, err := files.ReadDir(directoryPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDecisionsUnreadable,
			directoryPath, "list committed decision files: %v", err)}
	}
	names := make([]string, 0, len(entries))
	for _, name := range entries {
		if strings.HasSuffix(name, decisionFileExtension) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// readDecisionDirectory reads every decision file of a decisions/ directory.
func readDecisionDirectory(files DecisionFiles, directoryPath string, names []string) ([]SourcedDecision, []diag.Diagnostic) {
	if len(names) > decisionsMaxEntries {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeSensitiveContent, directoryPath,
			"%s holds %d decision files, beyond the bounded limit of %d", directoryPath, len(names), decisionsMaxEntries)}
	}
	var diagnostics []diag.Diagnostic
	decisions := make([]SourcedDecision, 0, len(names))
	for _, name := range names {
		source := directoryPath + "/" + name
		data, err := files.ReadFile(source)
		if err != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDecisionsUnreadable,
				source, "read committed decision file: %v", err))
			continue
		}
		decision, findings := ParseAndValidateDecisionFile(name, data)
		diagnostics = append(diagnostics, anchorDecisionFindings(source, findings)...)
		if decision != nil {
			decisions = append(decisions, SourcedDecision{Decision: *decision, Source: source})
		}
	}
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].Decision.ID < decisions[j].Decision.ID })
	return decisions, diagnostics
}

// anchorDecisionFindings anchors each finding on the committed document it
// came from, in the "<path>#<member>" convention discovery and the repository
// validators share.
func anchorDecisionFindings(document string, findings []diag.Diagnostic) []diag.Diagnostic {
	anchored := make([]diag.Diagnostic, 0, len(findings))
	for _, finding := range findings {
		finding.Field = DecisionDocumentField(document, finding.Field)
		anchored = append(anchored, finding)
	}
	return anchored
}

// DecisionDocumentField anchors member on the committed document at document,
// as "<document>#<member>", or document alone for the whole document.
func DecisionDocumentField(document, member string) string {
	if member == "" {
		return document
	}
	return document + "#" + member
}
