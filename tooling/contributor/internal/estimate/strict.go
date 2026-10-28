package estimate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// maxDocumentBytes bounds a plan file and the policy member it reads.
const maxDocumentBytes = 1 << 20

// maxDepth bounds the nesting of a strictly decoded document.
const maxDepth = 16

// decodeStrict parses data as one JSON value of valid UTF-8, with numbers
// kept as json.Number. It refuses an object that repeats a member, nesting
// deeper than maxDepth, and anything after the value. Objects decode to
// map[string]any and arrays to []any.
func decodeStrict(data []byte, root string) (any, error) {
	if len(data) > maxDocumentBytes {
		return nil, fmt.Errorf("%s: exceeds %d bytes", root, maxDocumentBytes)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s: not valid UTF-8", root)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readValue(decoder, root, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: more than one JSON value", root)
	}
	return value, nil
}

func readValue(decoder *json.Decoder, path string, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%s: nesting exceeds %d levels", path, maxDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("%s: invalid JSON: %w", path, err)
			}
			key, _ := token.(string)
			if _, seen := object[key]; seen {
				return nil, fmt.Errorf("%s: member %q appears twice", path, key)
			}
			value, err := readValue(decoder, member(path, key), depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := decoder.Token(); err != nil {
			return nil, fmt.Errorf("%s: invalid JSON: %w", path, err)
		}
		return object, nil
	case '[':
		list := []any{}
		for decoder.More() {
			value, err := readValue(decoder, index(path, len(list)), depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, value)
		}
		if _, err := decoder.Token(); err != nil {
			return nil, fmt.Errorf("%s: invalid JSON: %w", path, err)
		}
		return list, nil
	}
	return nil, fmt.Errorf("%s: invalid JSON", path)
}

func member(path, name string) string { return path + "." + name }

func index(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }
