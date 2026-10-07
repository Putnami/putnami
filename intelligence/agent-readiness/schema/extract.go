package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// IDBase is where the public schemas are published.
const IDBase = "https://putnami.dev/schemas/agent-readiness/"

const componentRefPrefix = "#/components/schemas/"

// Extract turns one OpenAPI 3.0 component and every component it references
// into a standalone JSON Schema (draft 2020-12) document. References move from
// #/components/schemas/ to #/$defs/, and OpenAPI's nullable keyword becomes a
// "null" type alternative. The output is deterministic: keys are sorted and the
// document ends with a newline.
func Extract(openapi []byte, root, fileName, title string) ([]byte, error) {
	var document struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openapi, &document); err != nil {
		return nil, fmt.Errorf("decode OpenAPI document: %w", err)
	}
	components := document.Components.Schemas
	if _, ok := components[root]; !ok {
		return nil, fmt.Errorf("OpenAPI component %q not found", root)
	}

	defs := map[string]any{}
	pending := []string{root}
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		if _, done := defs[name]; done {
			continue
		}
		component, ok := components[name]
		if !ok {
			return nil, fmt.Errorf("OpenAPI component %q referenced from %q not found", name, root)
		}
		converted, refs, err := convert(component)
		if err != nil {
			return nil, fmt.Errorf("component %q: %w", name, err)
		}
		defs[name] = converted
		sort.Strings(refs)
		pending = append(pending, refs...)
	}

	out := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     IDBase + fileName,
		"title":   title,
		"$ref":    "#/$defs/" + root,
		"$defs":   defs,
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(out); err != nil {
		return nil, fmt.Errorf("encode schema: %w", err)
	}
	return buffer.Bytes(), nil
}

// convert rewrites one schema node and returns the component names it
// references.
func convert(node any) (any, []string, error) {
	switch typed := node.(type) {
	case map[string]any:
		var refs []string
		out := make(map[string]any, len(typed))
		for key, value := range typed {
			if key == "nullable" {
				continue
			}
			if key == "$ref" {
				ref, _ := value.(string)
				name, ok := strings.CutPrefix(ref, componentRefPrefix)
				if !ok {
					return nil, nil, fmt.Errorf("unsupported reference %q", ref)
				}
				refs = append(refs, name)
				out[key] = "#/$defs/" + name
				continue
			}
			converted, nested, err := convert(value)
			if err != nil {
				return nil, nil, err
			}
			refs = append(refs, nested...)
			out[key] = converted
		}
		if nullable, _ := typed["nullable"].(bool); nullable {
			return allowNull(out), refs, nil
		}
		return out, refs, nil
	case []any:
		var refs []string
		out := make([]any, len(typed))
		for i, value := range typed {
			converted, nested, err := convert(value)
			if err != nil {
				return nil, nil, err
			}
			refs = append(refs, nested...)
			out[i] = converted
		}
		return out, refs, nil
	default:
		return node, nil, nil
	}
}

// allowNull adds JSON null to a schema that OpenAPI marked nullable.
func allowNull(node map[string]any) map[string]any {
	if kind, ok := node["type"].(string); ok {
		node["type"] = []any{kind, "null"}
		if values, ok := node["enum"].([]any); ok {
			node["enum"] = append(values, nil)
		}
		return node
	}
	return map[string]any{"anyOf": []any{node, map[string]any{"type": "null"}}}
}
