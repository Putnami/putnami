package clicore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ManifestPath returns the root manifest a setup command edits:
// putnami.workspace.json, else putnami.json, else "" when neither exists.
func ManifestPath(workspaceRoot string) string {
	for _, name := range []string{"putnami.workspace.json", "putnami.json"} {
		file := filepath.Join(workspaceRoot, name)
		if _, err := os.Stat(file); err == nil {
			return file
		}
	}
	return ""
}

// UpsertJSONField sets key to value inside the object that path names in the
// JSON document data, and leaves every other byte as written. Missing objects
// along path are created; an existing value under key is replaced. value must
// be JSON, indented with two spaces when it spans several lines. Setup commands
// use it so a manifest edit keeps the human's formatting and key order.
func UpsertJSONField(data []byte, path []string, key string, value []byte) ([]byte, error) {
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid JSON")
	}
	start := skipJSONSpace(data, 0)
	if start >= len(data) || data[start] != '{' {
		return nil, fmt.Errorf("manifest root must be a JSON object")
	}
	object, err := scanJSONObject(data, start)
	if err != nil {
		return nil, err
	}
	for depth, name := range path {
		field, ok := object.field(name)
		if !ok {
			missing := jsonObjectWithSingleField(key, value)
			for i := len(path) - 1; i > depth; i-- {
				missing = jsonObjectWithSingleField(path[i], missing)
			}
			return insertJSONField(data, object, name, missing), nil
		}
		if !isJSONObject(data, field.valueStart) {
			return nil, fmt.Errorf("manifest %s must be a JSON object", strings.Join(path[:depth+1], "."))
		}
		object, err = scanJSONObject(data, skipJSONSpace(data, field.valueStart))
		if err != nil {
			return nil, err
		}
	}
	if field, ok := object.field(key); ok {
		return replaceJSONValue(data, field, value), nil
	}
	return insertJSONField(data, object, key, value), nil
}

type jsonObject struct {
	start        int
	end          int
	fields       []jsonField
	lastValueEnd int
}

type jsonField struct {
	key        string
	keyStart   int
	valueStart int
	valueEnd   int
}

func (o jsonObject) field(key string) (jsonField, bool) {
	for _, field := range o.fields {
		if field.key == key {
			return field, true
		}
	}
	return jsonField{}, false
}

func scanJSONObject(data []byte, start int) (jsonObject, error) {
	start = skipJSONSpace(data, start)
	if start >= len(data) || data[start] != '{' {
		return jsonObject{}, fmt.Errorf("expected JSON object")
	}
	object := jsonObject{start: start}
	i := skipJSONSpace(data, start+1)
	if i < len(data) && data[i] == '}' {
		object.end = i
		return object, nil
	}
	for {
		i = skipJSONSpace(data, i)
		if i >= len(data) || data[i] != '"' {
			return jsonObject{}, fmt.Errorf("expected JSON object key")
		}
		keyStart := i
		keyEnd, err := scanJSONString(data, i)
		if err != nil {
			return jsonObject{}, err
		}
		var key string
		if err := json.Unmarshal(data[keyStart:keyEnd], &key); err != nil {
			return jsonObject{}, err
		}
		i = skipJSONSpace(data, keyEnd)
		if i >= len(data) || data[i] != ':' {
			return jsonObject{}, fmt.Errorf("expected ':' after JSON object key")
		}
		valueStart := skipJSONSpace(data, i+1)
		valueEnd, err := skipJSONValue(data, valueStart)
		if err != nil {
			return jsonObject{}, err
		}
		object.fields = append(object.fields, jsonField{
			key:        key,
			keyStart:   keyStart,
			valueStart: valueStart,
			valueEnd:   valueEnd,
		})
		object.lastValueEnd = valueEnd
		i = skipJSONSpace(data, valueEnd)
		if i >= len(data) {
			return jsonObject{}, fmt.Errorf("unterminated JSON object")
		}
		switch data[i] {
		case ',':
			i++
		case '}':
			object.end = i
			return object, nil
		default:
			return jsonObject{}, fmt.Errorf("expected ',' or '}' after JSON object value")
		}
	}
}

func insertJSONField(data []byte, object jsonObject, key string, value []byte) []byte {
	fieldIndent := childJSONIndent(data, object)
	objectIndent := lineIndent(data, object.start)
	field := []byte(fieldIndent + strconv.Quote(key) + ": " + indentJSONAfterFirst(value, fieldIndent))
	if len(object.fields) == 0 {
		replacement := []byte("{\n" + string(field) + "\n" + objectIndent + "}")
		return replaceRange(data, object.start, object.end+1, replacement)
	}
	insertion := []byte(",\n" + string(field))
	return replaceRange(data, object.lastValueEnd, object.lastValueEnd, insertion)
}

func replaceJSONValue(data []byte, field jsonField, value []byte) []byte {
	indent := lineIndent(data, field.valueStart)
	return replaceRange(data, field.valueStart, field.valueEnd, []byte(indentJSONAfterFirst(value, indent)))
}

func replaceRange(data []byte, start, end int, replacement []byte) []byte {
	out := make([]byte, 0, len(data)-(end-start)+len(replacement))
	out = append(out, data[:start]...)
	out = append(out, replacement...)
	out = append(out, data[end:]...)
	return out
}

func jsonObjectWithSingleField(key string, value []byte) []byte {
	fieldIndent := "  "
	return []byte("{\n" + fieldIndent + strconv.Quote(key) + ": " + indentJSONAfterFirst(value, fieldIndent) + "\n}")
}

func indentJSONAfterFirst(value []byte, indent string) string {
	lines := strings.Split(string(value), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = indent + lines[i]
	}
	return strings.Join(lines, "\n")
}

func childJSONIndent(data []byte, object jsonObject) string {
	if len(object.fields) > 0 {
		return lineIndent(data, object.fields[0].keyStart)
	}
	return lineIndent(data, object.start) + "  "
}

func lineIndent(data []byte, offset int) string {
	if offset > len(data) {
		offset = len(data)
	}
	start := offset
	for start > 0 && data[start-1] != '\n' && data[start-1] != '\r' {
		start--
	}
	end := start
	for end < len(data) && (data[end] == ' ' || data[end] == '\t') {
		end++
	}
	return string(data[start:end])
}

func isJSONObject(data []byte, start int) bool {
	start = skipJSONSpace(data, start)
	return start < len(data) && data[start] == '{'
}

func skipJSONSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

func skipJSONValue(data []byte, i int) (int, error) {
	i = skipJSONSpace(data, i)
	if i >= len(data) {
		return 0, fmt.Errorf("expected JSON value")
	}
	switch data[i] {
	case '"':
		return scanJSONString(data, i)
	case '{':
		object, err := scanJSONObject(data, i)
		if err != nil {
			return 0, err
		}
		return object.end + 1, nil
	case '[':
		return skipJSONArray(data, i)
	default:
		return skipJSONLiteral(data, i)
	}
}

func scanJSONString(data []byte, start int) (int, error) {
	if start >= len(data) || data[start] != '"' {
		return 0, fmt.Errorf("expected JSON string")
	}
	for i := start + 1; i < len(data); i++ {
		switch data[i] {
		case '\\':
			i++
		case '"':
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated JSON string")
}

func skipJSONArray(data []byte, start int) (int, error) {
	i := skipJSONSpace(data, start+1)
	if i < len(data) && data[i] == ']' {
		return i + 1, nil
	}
	for {
		next, err := skipJSONValue(data, i)
		if err != nil {
			return 0, err
		}
		i = skipJSONSpace(data, next)
		if i >= len(data) {
			return 0, fmt.Errorf("unterminated JSON array")
		}
		switch data[i] {
		case ',':
			i = skipJSONSpace(data, i+1)
		case ']':
			return i + 1, nil
		default:
			return 0, fmt.Errorf("expected ',' or ']' after JSON array value")
		}
	}
}

func skipJSONLiteral(data []byte, start int) (int, error) {
	i := start
	for i < len(data) {
		switch data[i] {
		case ' ', '\n', '\r', '\t', ',', '}', ']':
			if i == start {
				return 0, fmt.Errorf("expected JSON literal")
			}
			return i, nil
		default:
			i++
		}
	}
	if i == start {
		return 0, fmt.Errorf("expected JSON literal")
	}
	return i, nil
}
