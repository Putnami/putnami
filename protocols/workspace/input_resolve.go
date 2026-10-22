// Input-resolve protocol v1. The reasoning is recorded in
// doc/adr/0004-extensions-expand-their-input-selectors.md.
//
// A task file pattern can name an input selector, `<scheme>:<name>`, such as
// go-embed:build. The extension that declares the task lists the selector in
// its manifest's runtime.inputSelectors and expands it: the core asks for the
// files each selector reads now, and the extension answers with those files
// and the basis it computed them from. The basis is a list of pattern sets in
// the file-pattern grammar; InputBasisDigest hashes the tree as the basis
// selects it, so a consumer can keep an answer while that digest holds.
//
// Every path of the exchange is relative to the asked project directory,
// except the project itself, which is relative to the workspace root. The
// core runs the executable with the workspace root as its working directory
// and the probe's timeout.

package workspace

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// InputResolveProtocolVersion is the wire version of InputResolveRequest and
// InputResolveResult. Like the probe's, it travels in the payload, and a
// mismatch is rejected rather than guessed at.
const InputResolveProtocolVersion = 1

// inputSelectorNamePattern is the grammar of an input selector name:
// `<scheme>:<name>`, such as go-embed:build. The scheme `git` is reserved for
// the core's `git:` file pattern, so IsInputSelectorName also refuses it, and
// a name is at most inputSelectorNameMaxBytes.
//
// schemas/input-resolve.json states the same pattern and bound, and refuses
// the reserved scheme with `not`. An extension manifest applies the same rule
// to runtime.inputSelectors (protocols/extension validInputSelectorName).
// TestInputResolveSchemaAgreesWithTheValidator reads the schema.
const inputSelectorNamePattern = `^[a-z][a-z0-9-]*:[a-z0-9][a-z0-9-]*$`

// inputSelectorNameMaxBytes bounds an input selector name.
const inputSelectorNameMaxBytes = 128

// inputSelectorReservedScheme is the scheme of the core's own `git:` file
// pattern. No selector uses it.
const inputSelectorReservedScheme = "git"

// inputPathSegment is one segment of a clean relative slash path: not empty,
// not "." or "..", and free of slashes and backslashes.
const inputPathSegment = `(?:[^./\\][^/\\]*|\.[^./\\][^/\\]*|\.\.[^/\\]+)`

// inputPathPattern is the grammar of a path the exchange carries: a clean
// relative slash path inside its root. It admits no empty, "." or ".."
// segment, no backslash and no leading slash, so it is path.Clean's own
// spelling and cannot leave the root.
//
// schemas/input-resolve.json states the same pattern for files, for a project
// directory other than ".", and, after an optional "!", for a basis pattern.
const inputPathPattern = `^` + inputPathSegment + `(?:/` + inputPathSegment + `)*$`

// maxInputResolveErrorBytes bounds the error of one answer.
const maxInputResolveErrorBytes = 4096

var (
	inputSelectorNameRE = regexp.MustCompile(inputSelectorNamePattern)
	inputPathRE         = regexp.MustCompile(inputPathPattern)
)

// IsInputSelectorName reports whether s is an input selector name:
// `<scheme>:<name>`, where the scheme matches [a-z][a-z0-9-]* and is not
// `git`, the name matches [a-z0-9][a-z0-9-]*, and s is at most 128 bytes.
// A task file pattern that is a selector name is expanded by the extension
// whose manifest lists it in runtime.inputSelectors.
func IsInputSelectorName(s string) bool {
	if len(s) > inputSelectorNameMaxBytes || !inputSelectorNameRE.MatchString(s) {
		return false
	}
	scheme, _, _ := strings.Cut(s, ":")
	return scheme != inputSelectorReservedScheme
}

// InputResolveRequest is the core's question to one extension: the files each
// listed input reads now.
type InputResolveRequest struct {
	// Version is InputResolveProtocolVersion.
	Version int `json:"version"`
	// Extension is the asked extension's name, echoed back in the result so a
	// misrouted answer is detectable.
	Extension string `json:"extension"`
	// Inputs are the asked inputs, in the core's order; the result answers
	// them in the same order. At least one. Two inputs never share a project,
	// a selector and a pattern set.
	Inputs []InputResolveQuery `json:"inputs"`
}

// InputResolveQuery is one asked input: a selector that a task input of one
// project names.
type InputResolveQuery struct {
	// Project is the project directory, relative to the workspace root: "."
	// for the root, otherwise a clean relative slash path (inputPathPattern).
	Project string `json:"project"`
	// Selector is the selector name (IsInputSelectorName).
	Selector string `json:"selector"`
	// Patterns are the other file patterns of the task input that names the
	// selector, as declared, in the file-pattern grammar: an extension reads
	// them when its expansion depends on what the input's ordinary patterns
	// select. They carry no selector name. Empty when the input names nothing
	// else.
	Patterns []string `json:"patterns,omitempty"`
}

// InputResolveResult is one extension's answer: one InputResolveAnswer per
// asked input, in request order.
type InputResolveResult struct {
	// Version is InputResolveProtocolVersion.
	Version int `json:"version"`
	// Extension is the answering extension's name.
	Extension string `json:"extension"`
	// Inputs answer the request's inputs, in request order.
	Inputs []InputResolveAnswer `json:"inputs"`
}

// InputResolveAnswer is the answer for one input: either an Error, or the
// Files and the Basis they were computed from.
type InputResolveAnswer struct {
	// Project is the asked input's project directory, echoed back.
	Project string `json:"project"`
	// Selector is the asked input's selector, echoed back.
	Selector string `json:"selector"`
	// Files are every file the selector reads now, relative to the project:
	// sorted, each named once, each a clean relative slash path that a set of
	// Basis selects. The core hashes their exact bytes.
	Files []string `json:"files,omitempty"`
	// Basis states what the answer was computed from. The answer stays exact
	// while InputBasisDigest of the project directory under Basis is
	// unchanged.
	Basis InputBasis `json:"basis,omitzero"`
	// Error says why the selection is invalid now. An answer with an error
	// carries no files and no basis, and fails the task that reads the input.
	// At most 4096 bytes.
	Error string `json:"error,omitempty"`
}

// InputBasis is what an answer was computed from, as pattern sets relative to
// the project directory. Each set is matched on its own, with SelectsPath: a
// set's "!" exclusions remove paths from that set only. A pattern is a clean
// relative slash path in the file-pattern grammar, with an optional leading
// "!", no `git:` prefix and valid glob syntax in every segment; each set
// names at least one include.
type InputBasis struct {
	// Content are the sets whose files' bytes the answer read. The digest
	// covers the bytes of each regular file, the target of each link, and the
	// kind of each path a set selects.
	Content [][]string `json:"content,omitempty"`
	// Listing are the sets whose paths' existence, kind or link target the
	// answer read. The digest covers the kind of each path a set selects, and
	// the target of each link, but no file's bytes.
	Listing [][]string `json:"listing,omitempty"`
}

// isZero reports whether the basis names no pattern set.
func (b InputBasis) isZero() bool {
	return len(b.Content) == 0 && len(b.Listing) == 0
}

// cloneInputResolveRequest returns a deep copy, so an answer function that
// edits its request cannot change the request its answer is checked against.
func cloneInputResolveRequest(r InputResolveRequest) InputResolveRequest {
	r.Inputs = slices.Clone(r.Inputs)
	for i := range r.Inputs {
		r.Inputs[i].Patterns = slices.Clone(r.Inputs[i].Patterns)
	}
	return r
}

// ParseInputResolveRequest strict-decodes an input-resolve request.
func ParseInputResolveRequest(data []byte) (*InputResolveRequest, []diag.Diagnostic) {
	var r InputResolveRequest
	if err := decodeStrictControlDocument(data, &r); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse input-resolve request: %v", err),
		}
	}
	return &r, nil
}

// ValidateInputResolveRequest checks the request's structural invariants.
// Diagnostics follow member order and inputs in wire order, so one request
// produces one report.
func ValidateInputResolveRequest(r *InputResolveRequest) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf("nil-request", "", "input-resolve request is nil")}
	}
	var diags []diag.Diagnostic
	if r.Version != InputResolveProtocolVersion {
		diags = append(diags, diag.Errorf("invalid-input-resolve-version", "version",
			"input-resolve request version %d is not supported (want %d)", r.Version, InputResolveProtocolVersion))
	}
	if strings.TrimSpace(r.Extension) == "" {
		diags = append(diags, diag.Errorf("required-field", "extension",
			"input-resolve request must name the asked extension"))
	}
	if len(r.Inputs) == 0 {
		diags = append(diags, diag.Errorf("required-field", "inputs",
			"input-resolve request must ask for at least one input"))
	}
	asked := make(map[string]bool, len(r.Inputs))
	for i, input := range r.Inputs {
		field := fmt.Sprintf("inputs[%d]", i)
		diags = append(diags, validateInputProject(field+".project", input.Project)...)
		diags = append(diags, validateInputSelector(field+".selector", input.Selector)...)
		for k, pattern := range input.Patterns {
			if problem := inputQueryPatternProblem(pattern); problem != "" {
				diags = append(diags, diag.Errorf("invalid-value", fmt.Sprintf("%s.patterns[%d]", field, k),
					"pattern %q %s", pattern, problem))
			}
		}
		key := inputQueryKey(input)
		if asked[key] {
			diags = append(diags, diag.Errorf("duplicate-value", field,
				"input %q of project %q is asked more than once with the same patterns", input.Selector, input.Project))
		}
		asked[key] = true
	}
	return diags
}

// ParseAndValidateInputResolveRequest combines strict parsing and validation.
func ParseAndValidateInputResolveRequest(data []byte) (*InputResolveRequest, []diag.Diagnostic) {
	r, diags := ParseInputResolveRequest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return r, append(diags, ValidateInputResolveRequest(r)...)
}

// ParseInputResolveResult strict-decodes an input-resolve result.
func ParseInputResolveResult(data []byte) (*InputResolveResult, []diag.Diagnostic) {
	var r InputResolveResult
	if err := decodeStrictControlDocument(data, &r); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse input-resolve result: %v", err),
		}
	}
	return &r, nil
}

// ValidateInputResolveResult checks a result on its own: the version, the
// attribution, and each answer's grammar. An answer carries either an error
// and nothing else, or sorted unique clean files that its basis selects, and
// a basis whose sets are valid. ValidateInputResolveAnswer checks the result
// against the request it answers; a consumer runs both. Diagnostics come out
// in answer order.
func ValidateInputResolveResult(r *InputResolveResult) []diag.Diagnostic {
	if r == nil {
		return []diag.Diagnostic{diag.Errorf("nil-result", "", "input-resolve result is nil")}
	}
	var diags []diag.Diagnostic
	if r.Version != InputResolveProtocolVersion {
		diags = append(diags, diag.Errorf("invalid-input-resolve-version", "version",
			"input-resolve result version %d is not supported (want %d)", r.Version, InputResolveProtocolVersion))
	}
	if strings.TrimSpace(r.Extension) == "" {
		diags = append(diags, diag.Errorf("required-field", "extension",
			"input-resolve result must name the answering extension"))
	}
	if len(r.Inputs) == 0 {
		diags = append(diags, diag.Errorf("required-field", "inputs",
			"input-resolve result must answer at least one input"))
	}
	for i, answer := range r.Inputs {
		field := fmt.Sprintf("inputs[%d]", i)
		diags = append(diags, validateInputProject(field+".project", answer.Project)...)
		diags = append(diags, validateInputSelector(field+".selector", answer.Selector)...)
		if answer.Error != "" {
			switch {
			case strings.TrimSpace(answer.Error) == "":
				diags = append(diags, diag.Errorf("required-field", field+".error",
					"input %q error must say what is wrong", answer.Selector))
			case len(answer.Error) > maxInputResolveErrorBytes:
				diags = append(diags, diag.Errorf("limit-exceeded", field+".error",
					"input %q error is %d bytes; at most %d are allowed",
					answer.Selector, len(answer.Error), maxInputResolveErrorBytes))
			}
			if len(answer.Files) != 0 || !answer.Basis.isZero() {
				diags = append(diags, diag.Errorf("conflicting-field", field,
					"input %q answer carries an error with files or a basis; an error answer carries neither", answer.Selector))
			}
			continue
		}
		diags = append(diags, validateInputBasis(field+".basis", answer.Basis)...)
		diags = append(diags, validateInputFiles(field+".files", answer.Files, answer.Basis)...)
	}
	return diags
}

// ParseAndValidateInputResolveResult combines strict parsing and standalone
// validation.
func ParseAndValidateInputResolveResult(data []byte) (*InputResolveResult, []diag.Diagnostic) {
	r, diags := ParseInputResolveResult(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return r, append(diags, ValidateInputResolveResult(r)...)
}

// ValidateInputResolveAnswer checks result as the answer to request: it is
// attributed to the asked extension, and it answers every asked input, in
// request order, with that input's project and selector.
//
// It checks only the relation between the two documents and expects each to
// have passed its own validation. Diagnostics come out in input order.
func ValidateInputResolveAnswer(request *InputResolveRequest, result *InputResolveResult) []diag.Diagnostic {
	if request == nil {
		return []diag.Diagnostic{diag.Errorf("nil-request", "", "input-resolve request is nil")}
	}
	if result == nil {
		return []diag.Diagnostic{diag.Errorf("nil-result", "", "input-resolve result is nil")}
	}
	var diags []diag.Diagnostic
	if result.Extension != request.Extension {
		diags = append(diags, diag.Errorf("invalid-value", "extension",
			"answer is attributed to extension %q; the request asked %q", result.Extension, request.Extension))
	}
	if len(result.Inputs) != len(request.Inputs) {
		diags = append(diags, diag.Errorf("invalid-value", "inputs",
			"answer carries %d inputs; the request asks %d, each answered in request order",
			len(result.Inputs), len(request.Inputs)))
	}
	for i := range min(len(result.Inputs), len(request.Inputs)) {
		asked, answer := request.Inputs[i], result.Inputs[i]
		field := fmt.Sprintf("inputs[%d]", i)
		if answer.Project != asked.Project {
			diags = append(diags, diag.Errorf("invalid-value", field+".project",
				"answer %d is for project %q; the request asks %q at that position", i, answer.Project, asked.Project))
		}
		if answer.Selector != asked.Selector {
			diags = append(diags, diag.Errorf("invalid-value", field+".selector",
				"answer %d is for selector %q; the request asks %q at that position", i, answer.Selector, asked.Selector))
		}
	}
	return diags
}

// validateInputProject checks one project directory: "." or a clean relative
// slash path.
func validateInputProject(field, project string) []diag.Diagnostic {
	switch {
	case project == "":
		return []diag.Diagnostic{diag.Errorf("required-field", field,
			"input must name its project directory; %q is the workspace root", ProbeRootPath)}
	case project != ProbeRootPath && !inputPathRE.MatchString(project):
		return []diag.Diagnostic{diag.Errorf("invalid-path", field,
			"project %q must be %q or a clean relative slash path inside the workspace", project, ProbeRootPath)}
	}
	return nil
}

// validateInputSelector checks one selector name.
func validateInputSelector(field, selector string) []diag.Diagnostic {
	switch {
	case selector == "":
		return []diag.Diagnostic{diag.Errorf("required-field", field, "input must name its selector")}
	case !IsInputSelectorName(selector):
		return []diag.Diagnostic{diag.Errorf("invalid-value", field,
			"selector %q must match %s, use a scheme other than %q and be at most %d bytes",
			selector, inputSelectorNamePattern, inputSelectorReservedScheme, inputSelectorNameMaxBytes)}
	}
	return nil
}

// inputQueryPatternProblem says what is wrong with one pattern of a query, or
// returns "". A query pattern is a task input's file pattern as declared: an
// optional "!", an optional `git:` prefix, then a relative slash path whose
// segments are valid globs. It may leave the project with "../", as the
// file-pattern grammar allows. A selector name is not a file pattern: each
// selector is asked as its own input.
func inputQueryPatternProblem(pattern string) string {
	if IsInputSelectorName(pattern) {
		return "names a selector; a query's patterns are file patterns, and each selector is its own input"
	}
	body, _ := strings.CutPrefix(pattern, "!")
	body, _ = GitFilePattern(body)
	switch {
	case strings.TrimSpace(body) == "":
		return "is empty"
	case strings.ContainsRune(body, '\\'):
		return `must use "/" separators`
	case strings.HasPrefix(body, "/"):
		return "must be relative to the project"
	}
	return globSegmentsProblem(body)
}

// inputBasisPatternProblem says what is wrong with one basis pattern, or
// returns "". A basis pattern is an optional "!" and a clean relative slash
// path inside the project whose segments are valid globs, with no `git:`
// prefix: the basis is read from the tree as it is, not from Git.
func inputBasisPatternProblem(pattern string) string {
	body, _ := strings.CutPrefix(pattern, "!")
	switch {
	case strings.HasPrefix(body, "git:"):
		return `must not use the "git:" prefix; a basis selects the tree as it is`
	case !inputPathRE.MatchString(body):
		return "must be a clean relative slash path inside the project"
	}
	return globSegmentsProblem(body)
}

// globSegmentsProblem reports a segment of a slash pattern that path.Match
// refuses, or returns "".
func globSegmentsProblem(pattern string) string {
	for _, segment := range strings.Split(pattern, "/") {
		if _, err := path.Match(segment, ""); err != nil {
			return fmt.Sprintf("has invalid glob syntax in segment %q", segment)
		}
	}
	return ""
}

// validateInputBasis checks every pattern set of a basis.
func validateInputBasis(field string, basis InputBasis) []diag.Diagnostic {
	diags := make([]diag.Diagnostic, 0, len(basis.Content)+len(basis.Listing))
	for k, set := range basis.Content {
		diags = append(diags, validateInputPatternSet(fmt.Sprintf("%s.content[%d]", field, k), set)...)
	}
	for k, set := range basis.Listing {
		diags = append(diags, validateInputPatternSet(fmt.Sprintf("%s.listing[%d]", field, k), set)...)
	}
	return diags
}

// validateInputPatternSet checks one basis pattern set: not empty, every
// pattern valid, and at least one include, because a set of exclusions alone
// selects nothing.
func validateInputPatternSet(field string, set []string) []diag.Diagnostic {
	if len(set) == 0 {
		return []diag.Diagnostic{diag.Errorf("required-field", field, "a basis pattern set names at least one pattern")}
	}
	var diags []diag.Diagnostic
	includes := 0
	for m, pattern := range set {
		if problem := inputBasisPatternProblem(pattern); problem != "" {
			diags = append(diags, diag.Errorf("invalid-value", fmt.Sprintf("%s[%d]", field, m),
				"basis pattern %q %s", pattern, problem))
		}
		if !strings.HasPrefix(pattern, "!") {
			includes++
		}
	}
	if includes == 0 {
		diags = append(diags, diag.Errorf("required-field", field,
			"a basis pattern set names at least one include; exclusions alone select nothing"))
	}
	return diags
}

// validateInputFiles checks an answer's files: each a clean relative slash
// path, in sorted order, named once, and selected by a set of the basis.
func validateInputFiles(field string, files []string, basis InputBasis) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for j, file := range files {
		item := fmt.Sprintf("%s[%d]", field, j)
		if !inputPathRE.MatchString(file) {
			diags = append(diags, diag.Errorf("invalid-path", item,
				"file %q must be a clean relative slash path inside the project", file))
			continue
		}
		if j > 0 {
			switch previous := files[j-1]; {
			case previous == file:
				diags = append(diags, diag.Errorf("duplicate-value", item, "file %q is listed more than once", file))
				continue
			case previous > file:
				diags = append(diags, diag.Errorf("invalid-order", item,
					"file %q is listed after %q; files are sorted", file, previous))
			}
		}
		if !basisSelects(basis, file) {
			diags = append(diags, diag.Errorf("unselected-file", item,
				"file %q is selected by no set of the basis; the basis selects every file an answer lists", file))
		}
	}
	return diags
}

// basisSelects reports whether a set of either kind selects rel.
func basisSelects(basis InputBasis, rel string) bool {
	for _, sets := range [][][]string{basis.Content, basis.Listing} {
		for _, set := range sets {
			if SelectsPath(rel, set) {
				return true
			}
		}
	}
	return false
}

// inputQueryKey identifies a query by its project, its selector and its
// pattern set, whose order and repeats carry no meaning.
func inputQueryKey(q InputResolveQuery) string {
	patterns := slices.Clone(q.Patterns)
	slices.Sort(patterns)
	patterns = slices.Compact(patterns)
	return q.Project + "\x00" + q.Selector + "\x00" + strings.Join(patterns, "\x00")
}
