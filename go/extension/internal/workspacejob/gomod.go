package workspacejob

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Module is a module path and an optional version, as `go mod edit -json`
// and `go work edit -json` print them.
type Module struct {
	Path    string
	Version string
}

// Spec is the module in the path[@version] form `go mod edit` and
// `go work edit` accept.
func (m Module) Spec() string {
	if m.Version == "" {
		return m.Path
	}
	return m.Path + "@" + m.Version
}

// Replace is one replace directive.
type Replace struct {
	Old Module
	New Module
}

// Require is one require directive.
type Require struct {
	Path     string
	Version  string
	Indirect bool
}

// WorkFile is the part of `go work edit -json` the lifecycle jobs read.
type WorkFile struct {
	Use []struct {
		DiskPath   string
		ModulePath string
	}
	Replace []Replace
}

// ModFile is the part of `go mod edit -json` the lifecycle jobs read.
type ModFile struct {
	Module struct {
		Path string
	}
	Require []Require
	Exclude []Module
	Replace []Replace
}

// ReadWorkFile parses the go.work at path through the go command.
func (j *Job) ReadWorkFile(path string) (WorkFile, error) {
	var work WorkFile
	out, err := j.Output(nil, j.GoBinary, "work", "edit", "-json", path)
	if err != nil {
		return work, err
	}
	err = json.Unmarshal([]byte(out), &work)
	return work, err
}

// ReadModFile parses the go.mod at path through the go command.
func (j *Job) ReadModFile(path string) (ModFile, error) {
	var mod ModFile
	out, err := j.Output(nil, j.GoBinary, "mod", "edit", "-json", path)
	if err != nil {
		return mod, err
	}
	err = json.Unmarshal([]byte(out), &mod)
	return mod, err
}

// Param returns the job parameter params.<key>.<path...> and whether jq's
// alternative operator keeps it: false when the value is missing, null or
// false, or when the path crosses a value that is not an object.
func Param(params map[string]json.RawMessage, key string, path ...string) (json.RawMessage, bool) {
	raw, ok := params[key]
	if !ok {
		return nil, false
	}
	for _, member := range path {
		trimmed := bytes.TrimSpace(raw)
		if string(trimmed) == "null" {
			return nil, false
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return nil, false
		}
		if raw, ok = object[member]; !ok {
			return nil, false
		}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" {
		return nil, false
	}
	return raw, true
}

// ParamText returns the text `jq -r '.params.<path> // empty'` printed for a
// job parameter (see JQRawText), and "" when Param does not keep the value.
func ParamText(params map[string]json.RawMessage, key string, path ...string) string {
	raw, ok := Param(params, key, path...)
	if !ok {
		return ""
	}
	return JQRawText(raw)
}

// JQRawText is what `jq -r` prints for a value: a string without quotes, an
// object or an array indented by two spaces, anything else in its JSON
// spelling.
func JQRawText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		var indented bytes.Buffer
		if err := json.Indent(&indented, trimmed, "", "  "); err == nil {
			return indented.String()
		}
	}
	return JSONText(trimmed)
}

// IterateOrEmpty yields what jq's `(. // [])[]` yields for raw: nothing for a
// missing, null or false value, the elements of an array, the member values of
// an object in document order. ok is false where jq fails.
func IterateOrEmpty(raw json.RawMessage) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "false" {
		return nil, true
	}
	return jsonIterate(trimmed)
}

// JSONString returns the value of a JSON string, and false for anything else.
func JSONString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(trimmed, []byte(`"`)) {
		return "", false
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err != nil {
		return "", false
	}
	return text, true
}

// JSONText is the text `jq -r` prints for a value: a string without quotes,
// anything else in its JSON spelling.
func JSONText(raw json.RawMessage) string {
	if text, ok := JSONString(raw); ok {
		return text
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return compact.String()
	}
	return strings.TrimSpace(string(raw))
}
