package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
)

// The evidence fragment: one fact a task observed about a change, one JSON
// object per line of a JSON Lines file (ADR 0006 of this protocol). The file
// is a declared output of the task, so the cache keeps it with the task, and
// a reader that applies a policy decides what the facts mean: a fragment
// states no project, task, commit or verdict.
const (
	// FragmentProtocolVersion is the version every fragment line carries.
	FragmentProtocolVersion = 1
	// FragmentArtifactID is the reserved declared-artifact identity of a
	// task's fragment file. The task declares the file as a required file of
	// its command output, so every run and every cache hit replace it whole,
	// and a reader finds the fragments of a task served from the cache as it
	// finds those of a task that ran.
	FragmentArtifactID = "putnami-evidence"
	// FragmentFilename is the file the reserved artifact names, in the task's
	// command output.
	FragmentFilename = FragmentArtifactID + ".jsonl"
	// MaxFragments is the most fragments one file holds.
	MaxFragments = 50000
	// MaxFragmentFileBytes is the largest fragment file.
	MaxFragmentFileBytes = 16 << 20
)

// Fragment kinds. The vocabulary is closed: this version reads findings, and
// a later one adds a kind by addition.
const (
	// FragmentKindFinding is a rule a check applied and the place it reports.
	FragmentKindFinding = "finding"
)

// FragmentKinds is the closed fragment-kind vocabulary.
var FragmentKinds = []string{FragmentKindFinding}

// Finding severities.
const (
	FindingError   = "error"
	FindingWarning = "warning"
	FindingInfo    = "info"
)

// FindingSeverities is the closed finding-severity vocabulary.
var FindingSeverities = []string{FindingError, FindingWarning, FindingInfo}

// ErrorCodeInvalidFragment names a fragment whose members are well typed but
// whose values break a rule of its kind.
const ErrorCodeInvalidFragment = "evidence.invalid_fragment"

// Fragment is one line of a fragment file.
type Fragment struct {
	// ProtocolVersion is FragmentProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Kind is one of FragmentKinds.
	Kind string `json:"kind"`
	// Source names the shape the producer translated.
	Source FragmentSource `json:"source"`
	// Subject is where the fact is located, relative to the producing task's
	// project.
	Subject FragmentSubject `json:"subject"`
	// Rule names the rule a finding reports, as its checker names it.
	Rule string `json:"rule,omitempty"`
	// Severity is one of FindingSeverities.
	Severity string `json:"severity,omitempty"`
	// Message says what the finding reports.
	Message string `json:"message,omitempty"`
}

// FragmentSource names what a fragment was translated from.
type FragmentSource struct {
	// Format is an open lowercase slug: putnami-sdd, go-test, junit, sarif.
	Format string `json:"format"`
	// Tool optionally names the program that produced the source.
	Tool string `json:"tool,omitempty"`
}

// FragmentSubject is the place a fragment is about.
type FragmentSubject struct {
	// Path is a clean slash path relative to the producing task's project,
	// which it never leaves.
	Path string `json:"path"`
	// Symbol optionally names a symbol of the file, or a member of a document.
	Symbol string `json:"symbol,omitempty"`
	// Line and Column optionally place the subject, from 1.
	Line   int `json:"line,omitempty"`
	Column int `json:"column,omitempty"`
}

var fragmentShape = object(map[string]*member{
	"protocolVersion": nil,
	"kind":            nil,
	"source":          object(map[string]*member{"format": nil, "tool": nil}),
	"subject":         object(map[string]*member{"path": nil, "symbol": nil, "line": nil, "column": nil}),
	"rule":            nil,
	"severity":        nil,
	"message":         nil,
})

// fragmentRequired are the members every fragment carries.
var fragmentRequired = []string{"protocolVersion", "kind", "source", "subject"}

// ParseFragments reads a fragment file strictly: every line one fragment, the
// file within its bounds. Any refused line fails the whole file, and every
// finding names its line as fragments[<index>], counted from 0.
func ParseFragments(data []byte) ([]Fragment, []diag.Diagnostic) {
	if len(data) > MaxFragmentFileBytes {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeTooLarge, "fragments",
			"the fragment file holds %d bytes, more than %d", len(data), MaxFragmentFileBytes)}
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > MaxFragments {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeTooLarge, "fragments",
			"the fragment file holds %d fragments, more than %d", len(lines), MaxFragments)}
	}
	fragments := make([]Fragment, 0, len(lines))
	var diags []diag.Diagnostic
	for index, line := range lines {
		field := fmt.Sprintf("fragments[%d]", index)
		fragment, lineDiags := parseFragment(field, line)
		diags = append(diags, lineDiags...)
		if lineDiags == nil {
			fragments = append(fragments, fragment)
		}
	}
	if diags != nil {
		return nil, diags
	}
	return fragments, nil
}

// parseFragment reads and validates one line.
func parseFragment(field string, line []byte) (Fragment, []diag.Diagnostic) {
	var fragment Fragment
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return fragment, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, field, "line is not JSON: %v", err)}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fragment, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, field, "line holds more than one JSON value")}
	}
	top, ok := raw.(map[string]any)
	if !ok {
		return fragment, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, field, "line must be a JSON object")}
	}
	if unknown, ok := firstUnknownMember(raw, fragmentShape, field); !ok {
		return fragment, []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownField, unknown, "unknown member %q", unknown)}
	}
	if at, ok := firstNullMember(raw, field); !ok {
		return fragment, []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidFragment, at, "%s is null; an absent member is left out", at)}
	}
	if at, ok := firstRepeatedMember(line, field); !ok {
		return fragment, []diag.Diagnostic{diag.Errorf(ErrorCodeDuplicate, at, "%s is stated twice", at)}
	}
	if oversized, ok := firstOversizedString(raw, field); !ok {
		return fragment, []diag.Diagnostic{diag.Errorf(ErrorCodeTooLarge, oversized, "%s is longer than %d bytes", oversized, MaxStringBytes)}
	}
	var diags []diag.Diagnostic
	for _, name := range fragmentRequired {
		if value, present := top[name]; !present || value == nil {
			diags = append(diags, diag.Errorf(ErrorCodeRequired, joinPath(field, name), "%s is required", name))
		}
	}
	if diags != nil {
		return fragment, diags
	}
	typed := json.NewDecoder(bytes.NewReader(line))
	typed.DisallowUnknownFields()
	if err := typed.Decode(&fragment); err != nil {
		return fragment, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, field, "invalid member type: %v", err)}
	}
	diags = ValidateFragment(field, fragment)
	// A line or column the line states counts from 1: an explicit 0 is no
	// place, not an absent one.
	if subject, ok := top["subject"].(map[string]any); ok {
		for _, name := range []string{"line", "column"} {
			if value, present := subject[name]; present && value == json.Number("0") {
				diags = append(diags, diag.Errorf(ErrorCodeInvalidFragment, joinPath(field, "subject."+name), "%s counts from 1", name))
			}
		}
	}
	return fragment, diags
}

// firstNullMember reports the first member, in sorted order, whose value is
// null.
func firstNullMember(value any, path string) (string, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return "", true
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		at := joinPath(path, key)
		if object[key] == nil {
			return at, false
		}
		if inner, ok := firstNullMember(object[key], at); !ok {
			return inner, false
		}
	}
	return "", true
}

// firstRepeatedMember reports the first member an object of line states
// twice, which a decoder would otherwise resolve silently to the last value.
func firstRepeatedMember(line []byte, path string) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	type frame struct {
		object bool
		path   string
		seen   map[string]bool
		key    bool
	}
	stack := []*frame{{path: path}}
	pending := path
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", true
		}
		top := stack[len(stack)-1]
		switch token {
		case json.Delim('{'):
			stack = append(stack, &frame{object: true, path: pending, seen: map[string]bool{}, key: true})
			continue
		case json.Delim('['):
			stack = append(stack, &frame{path: pending})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
			top = stack[len(stack)-1]
			if top.object {
				top.key = true
			}
			continue
		}
		if top.object && top.key {
			name, _ := token.(string)
			pending = joinPath(top.path, name)
			if top.seen[name] {
				return pending, false
			}
			top.seen[name] = true
			top.key = false
			continue
		}
		if top.object {
			top.key = true
		}
	}
}

// ValidateFragment checks one decoded fragment's values against its kind.
func ValidateFragment(field string, fragment Fragment) []diag.Diagnostic {
	var diags []diag.Diagnostic
	add := func(code, at, format string, args ...any) {
		diags = append(diags, diag.Errorf(code, joinPath(field, at), format, args...))
	}
	if fragment.ProtocolVersion != FragmentProtocolVersion {
		add(ErrorCodeUnsupportedProtocolVersion, "protocolVersion", "protocolVersion %d is not %d", fragment.ProtocolVersion, FragmentProtocolVersion)
	}
	if !slices.Contains(FragmentKinds, fragment.Kind) {
		add(ErrorCodeInvalidFragment, "kind", "kind %q is not one of %v", fragment.Kind, FragmentKinds)
	}
	if !slugPattern.MatchString(fragment.Source.Format) {
		add(ErrorCodeInvalidFragment, "source.format", "format %q is not a lowercase slug", fragment.Source.Format)
	}
	if !validPath(fragment.Subject.Path) {
		add(ErrorCodeInvalidPath, "subject.path", "path %q must be relative to the task's project and stay inside it", fragment.Subject.Path)
	}
	if fragment.Subject.Line < 0 || fragment.Subject.Column < 0 || (fragment.Subject.Column > 0 && fragment.Subject.Line == 0) {
		add(ErrorCodeInvalidFragment, "subject", "a line and a column count from 1, and a column needs its line")
	}
	if fragment.Kind == FragmentKindFinding {
		if fragment.Rule == "" {
			add(ErrorCodeRequired, "rule", "a finding names its rule")
		}
		if !slices.Contains(FindingSeverities, fragment.Severity) {
			add(ErrorCodeInvalidFragment, "severity", "severity %q is not one of %v", fragment.Severity, FindingSeverities)
		}
		if fragment.Message == "" {
			add(ErrorCodeRequired, "message", "a finding says what it reports")
		}
	}
	return diags
}

// EncodeFragments writes fragments as a fragment file: one compact JSON
// object per line, each validated first. The file is read back with
// ParseFragments, so it never writes one the reader refuses.
func EncodeFragments(fragments []Fragment) ([]byte, error) {
	var buffer bytes.Buffer
	for index, fragment := range fragments {
		if diags := ValidateFragment(fmt.Sprintf("fragments[%d]", index), fragment); diags != nil {
			return nil, fmt.Errorf("%s: %s", diags[0].Field, diags[0].Message)
		}
		line, err := json.Marshal(fragment)
		if err != nil {
			return nil, err
		}
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	if _, diags := ParseFragments(buffer.Bytes()); diags != nil {
		return nil, fmt.Errorf("%s: %s", diags[0].Field, diags[0].Message)
	}
	return buffer.Bytes(), nil
}
