// Package openapiutil contains OpenAPI artifact helpers shared by the Go
// generate and describe phases.
package openapiutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SchemaRelPath is the canonical project-relative OpenAPI artifact path.
const SchemaRelPath = "schema/openapi.json"

// IsOpenAPIPath reports whether rel points at the canonical OpenAPI artifact.
func IsOpenAPIPath(rel string) bool {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "./")
	return rel == SchemaRelPath
}

// Canonicalize parses an OpenAPI JSON document and emits stable, indented JSON.
// Object keys are sorted by encoding/json; semantically unordered arrays that
// commonly churn in generated specs are normalized here. Provider-authored
// values are never reordered — see [isProviderValueKey].
func Canonicalize(body []byte) ([]byte, error) {
	doc, err := parseDocument(body, "openapi")
	if err != nil {
		return nil, err
	}
	normalizeValue(doc)
	return marshalDocument(doc)
}

// Merge combines two OpenAPI JSON documents. The second document is treated as
// the higher-fidelity producer for duplicate operations, while paths that only
// exist in the first document are retained. This preserves route surface across
// generate/describe without silently replacing one surface with another.
//
// A first-party next document is the exception: it carries x-putnami-client, so
// it is the provider's complete and authoritative contract. Its paths,
// components and security replace the previous document's wholesale, and a
// removed route disappears from the published bytes.
func Merge(previous, next []byte, previousLabel, nextLabel string) ([]byte, error) {
	prevDoc, err := parseDocument(previous, previousLabel)
	if err != nil {
		return nil, err
	}
	nextDoc, err := parseDocument(next, nextLabel)
	if err != nil {
		return nil, err
	}

	merged, ok := deepCopy(prevDoc).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: copied OpenAPI JSON must be an object", previousLabel)
	}
	if documentHasFirstPartyContract(nextDoc) {
		// Components and document security are part of the provider-authored
		// first-party contract. Treat them atomically: a recursive merge would
		// keep removed properties, required fields, schemes, or requirements
		// from the lower-fidelity static visitor.
		delete(merged, "components")
		delete(merged, "security")
		// The route set is atomic for the same reason. A first-party describe
		// enumerates every registered route, so a path or method the previous
		// document still carries is either a deleted route or a static guess.
		// Retaining it would republish an operation the provider no longer serves
		// and regenerate a client method that calls nothing.
		delete(merged, "paths")
	}
	if err := mergeDocument(merged, nextDoc, previousLabel, nextLabel); err != nil {
		return nil, err
	}
	normalizeValue(merged)
	return marshalDocument(merged)
}

func documentHasFirstPartyContract(document map[string]any) bool {
	_, ok := document["x-putnami-client"]
	return ok
}

func parseDocument(body []byte, label string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%s: parse OpenAPI JSON: %w", label, err)
	}
	doc, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: OpenAPI JSON must be an object", label)
	}
	if _, ok := doc["paths"].(map[string]any); !ok {
		return nil, fmt.Errorf("%s: OpenAPI JSON missing object field paths", label)
	}
	return doc, nil
}

func marshalDocument(doc map[string]any) ([]byte, error) {
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode canonical OpenAPI JSON: %w", err)
	}
	return append(body, '\n'), nil
}

func mergeDocument(dst, src map[string]any, dstLabel, srcLabel string) error {
	for key, srcVal := range src {
		dstVal, exists := dst[key]
		if !exists {
			dst[key] = deepCopy(srcVal)
			continue
		}

		switch key {
		case "openapi":
			if !jsonEqual(dstVal, srcVal) {
				return fmt.Errorf("OpenAPI version conflict between %s and %s: %s vs %s", dstLabel, srcLabel, compactJSON(dstVal), compactJSON(srcVal))
			}
		case "paths":
			merged, err := mergePaths(dstVal, srcVal, dstLabel, srcLabel)
			if err != nil {
				return err
			}
			dst[key] = merged
		case "components":
			dst[key] = mergeValue(key, dstVal, srcVal)
		case "servers", "security", "tags":
			dst[key] = unionArray(dstVal, srcVal)
		case "info":
			dst[key] = mergeValue(key, dstVal, srcVal)
		default:
			dst[key] = mergeValue(key, dstVal, srcVal)
		}
	}
	return nil
}

func mergePaths(dstVal, srcVal any, dstLabel, srcLabel string) (any, error) {
	dst, ok := dstVal.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: paths must be an object", dstLabel)
	}
	src, ok := srcVal.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: paths must be an object", srcLabel)
	}

	for path, srcItem := range src {
		dstItem, exists := dst[path]
		if !exists {
			dst[path] = deepCopy(srcItem)
			continue
		}
		merged, err := mergePathItem(path, dstItem, srcItem, dstLabel, srcLabel)
		if err != nil {
			return nil, err
		}
		dst[path] = merged
	}
	return dst, nil
}

func mergePathItem(path string, dstVal, srcVal any, dstLabel, srcLabel string) (any, error) {
	dst, ok := dstVal.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: path %s must be an object", dstLabel, path)
	}
	src, ok := srcVal.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: path %s must be an object", srcLabel, path)
	}

	for key, srcOperation := range src {
		dstOperation, exists := dst[key]
		if !exists {
			dst[key] = deepCopy(srcOperation)
			continue
		}
		if isHTTPMethod(key) {
			// A first-party operation is the provider's complete, strict contract.
			// The generate phase can only emit a shallow route stub, including a
			// guessed HTTP 200 response. Merging that stub field-by-field would
			// retain guessed statuses and schemas which the runtime provider did
			// not declare. Once describe supplies the first-party marker, replace
			// the entire operation while still retaining paths known only to the
			// static visitor.
			if operationHasFirstPartyContract(srcOperation) {
				dst[key] = deepCopy(srcOperation)
				continue
			}
			dst[key] = mergeValue(key, dstOperation, srcOperation)
			continue
		}
		dst[key] = mergeValue(key, dstOperation, srcOperation)
	}
	return dst, nil
}

func operationHasFirstPartyContract(value any) bool {
	operation, ok := value.(map[string]any)
	if !ok {
		return false
	}
	_, ok = operation["x-putnami-client"]
	return ok
}

func mergeValue(key string, dstVal, srcVal any) any {
	if isProviderValueKey(key) {
		// A provider-authored value is opaque data, not a structure to merge.
		// Recursing into it would splice two documents' examples together and
		// apply the keyword rules below to business arrays that merely share a
		// name with an OpenAPI keyword. Take the higher-fidelity document's
		// value whole, and keep the previous one when the newer producer has
		// nothing to say.
		if isEmptyValue(srcVal) {
			return deepCopy(dstVal)
		}
		return deepCopy(srcVal)
	}

	dstMap, dstIsMap := dstVal.(map[string]any)
	srcMap, srcIsMap := srcVal.(map[string]any)
	if dstIsMap && srcIsMap {
		for k, v := range srcMap {
			if existing, ok := dstMap[k]; ok {
				dstMap[k] = mergeValue(k, existing, v)
			} else {
				dstMap[k] = deepCopy(v)
			}
		}
		return dstMap
	}

	if dstSlice, ok := dstVal.([]any); ok {
		if srcSlice, ok := srcVal.([]any); ok {
			switch key {
			case "parameters":
				return mergeParameters(dstSlice, srcSlice)
			case "required", "tags", "servers", "security":
				return unionArray(dstSlice, srcSlice)
			default:
				if jsonEqual(dstSlice, srcSlice) {
					return dstSlice
				}
				return deepCopy(srcSlice)
			}
		}
	}

	if isEmptyValue(srcVal) {
		return deepCopy(dstVal)
	}
	return deepCopy(srcVal)
}

func mergeParameters(dst, src []any) []any {
	byKey := map[string]any{}
	var anonymous []any

	add := func(param any) {
		m, ok := param.(map[string]any)
		if !ok {
			anonymous = append(anonymous, deepCopy(param))
			return
		}
		key := stringField(m, "in") + "\x00" + stringField(m, "name")
		if key == "\x00" {
			anonymous = append(anonymous, deepCopy(param))
			return
		}
		if existing, ok := byKey[key]; ok {
			byKey[key] = mergeValue("parameter", existing, param)
		} else {
			byKey[key] = deepCopy(param)
		}
	}

	for _, param := range dst {
		add(param)
	}
	for _, param := range src {
		add(param)
	}

	out := make([]any, 0, len(byKey)+len(anonymous))
	for _, param := range byKey {
		out = append(out, param)
	}
	out = append(out, anonymous...)
	return sortParameters(out)
}

func unionArray(dstVal, srcVal any) any {
	dst, ok := dstVal.([]any)
	if !ok {
		return deepCopy(srcVal)
	}
	src, ok := srcVal.([]any)
	if !ok {
		return deepCopy(srcVal)
	}

	seen := map[string]bool{}
	out := make([]any, 0, len(dst)+len(src))
	for _, arr := range [][]any{dst, src} {
		for _, item := range arr {
			key := compactJSON(item)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, deepCopy(item))
		}
	}
	// Declared order is preserved. The arrays that reach this helper carry
	// meaning in their order: the first entry of servers is the default base
	// URL, and a security array is an ordered list of alternatives a client
	// tries in turn. Sorting them lexicographically would silently rewrite the
	// provider's declaration. Deduplication alone keeps the output stable,
	// because both inputs are already deterministic.
	return out
}

// isProviderValueKey reports whether key introduces a provider-authored value
// rather than OpenAPI structure. Everything below such a key is data the
// provider declared — a default, an example, an enum member, or a Putnami
// extension payload — so canonicalization copies it verbatim instead of
// applying keyword rules to it. Without this boundary a default of
// {"required": ["z", "a"]} comes back as ["a", "z"] and the published contract
// no longer states what the provider declared.
func isProviderValueKey(key string) bool {
	switch key {
	case "default", "example", "examples", "enum", "const":
		return true
	}
	return strings.HasPrefix(key, "x-")
}

func normalizeValue(v any) {
	switch x := v.(type) {
	case map[string]any:
		for key, value := range x {
			if isProviderValueKey(key) {
				continue
			}
			normalizeValue(value)
			switch key {
			case "parameters":
				if params, ok := value.([]any); ok {
					x[key] = sortParameters(params)
				}
			case "required":
				if required, ok := value.([]any); ok {
					x[key] = sortStringArray(required)
				}
			}
		}
	case []any:
		for _, item := range x {
			normalizeValue(item)
		}
	}
}

func sortParameters(params []any) []any {
	out := make([]any, len(params))
	copy(out, params)
	sort.SliceStable(out, func(i, j int) bool {
		return parameterSortKey(out[i]) < parameterSortKey(out[j])
	})
	return out
}

func parameterSortKey(param any) string {
	m, ok := param.(map[string]any)
	if !ok {
		return "z/" + compactJSON(param)
	}
	return fmt.Sprintf("%02d/%s/%s/%s",
		parameterLocationRank(stringField(m, "in")),
		stringField(m, "name"),
		stringField(m, "required"),
		compactJSON(param),
	)
}

func parameterLocationRank(loc string) int {
	switch loc {
	case "path":
		return 0
	case "query":
		return 1
	case "header":
		return 2
	case "cookie":
		return 3
	default:
		return 9
	}
}

func sortStringArray(values []any) []any {
	stringsOnly := make([]string, 0, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			return values
		}
		stringsOnly = append(stringsOnly, s)
	}
	sort.Strings(stringsOnly)
	out := make([]any, len(stringsOnly))
	for i, value := range stringsOnly {
		out[i] = value
	}
	return out
}

func isHTTPMethod(key string) bool {
	switch strings.ToLower(key) {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	default:
		return false
	}
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return compactJSON(x)
	}
}

func jsonEqual(a, b any) bool {
	return compactJSON(a) == compactJSON(b)
}

func compactJSON(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(body)
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, value := range x {
			out[key] = deepCopy(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = deepCopy(value)
		}
		return out
	default:
		return x
	}
}
