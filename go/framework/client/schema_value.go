package client

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

func cloneSchemas(source map[string]clientcontract.Schema) map[string]clientcontract.Schema {
	if source == nil {
		return nil
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return nil
	}
	var encoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil
	}
	out := make(map[string]clientcontract.Schema, len(encoded))
	for name, value := range encoded {
		schema, diagnostics := clientcontract.ParseAndValidateSchema(value)
		if len(diagnostics) != 0 {
			return nil
		}
		out[name] = *schema
	}
	return out
}

func successContent(response *Response, successes []OperationSuccess) (*OperationContent, error) {
	if response == nil {
		return nil, falseResponse("service returned no response")
	}
	mediaType := strings.TrimSpace(strings.Split(response.Headers.Get("Content-Type"), ";")[0])
	for i := range successes {
		if successes[i].Status != response.StatusCode {
			continue
		}
		if len(successes[i].Content) == 0 {
			return nil, nil
		}
		if mediaType == "" {
			return nil, falseResponse("service response content type is missing")
		}
		for j := range successes[i].Content {
			content := &successes[i].Content[j]
			if content.Streamed && content.IsBinary() && content.MediaType == "*/*" {
				if !phttp.ConcreteMediaType(response.Headers.Get("Content-Type")) {
					return nil, falseResponse("service response content type is not concrete or valid")
				}
				return content, nil
			}
			if strings.EqualFold(successes[i].Content[j].MediaType, mediaType) {
				return &successes[i].Content[j], nil
			}
		}
		return nil, falseResponse("service response content type is not declared")
	}
	return nil, falseResponse("service response status is not declared")
}

func falseResponse(message string) error {
	return perrors.New(CodeClientResponse, message)
}

// projectJSON validates a document the client emits — a request body or a
// client stream message — against its declared schema. A closed object refuses
// every property it does not declare.
func projectJSON(raw []byte, schema *clientcontract.Schema, schemas map[string]clientcontract.Schema, secrets []string, redact bool) (json.RawMessage, bool) {
	return projectDocument(raw, schema, schemas, secrets, redact, false)
}

// projectResponseJSON validates a document the provider sent — a success body,
// a stream message or declared error details — and returns its projection onto
// the declared schema. A property a closed object does not declare is dropped
// from the projection rather than refused: a provider adds an optional response
// property as a compatible change, and a client generated from the earlier
// contract keeps reading the properties it declares. Every declared property is
// validated as strictly as projectJSON validates it.
func projectResponseJSON(raw []byte, schema *clientcontract.Schema, schemas map[string]clientcontract.Schema, secrets []string, redact bool) (json.RawMessage, bool) {
	return projectDocument(raw, schema, schemas, secrets, redact, true)
}

func projectDocument(raw []byte, schema *clientcontract.Schema, schemas map[string]clientcontract.Schema, secrets []string, redact, dropUndeclared bool) (json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	projected, ok := projectSchemaValue(value, schema, schemas, secrets, redact, dropUndeclared, 0)
	if !ok {
		return nil, false
	}
	result, err := json.Marshal(projected)
	return result, err == nil
}

// projectSchemaValue projects value onto schema. With dropUndeclared, a
// property a closed object does not declare is left out of the projection; the
// projection never carries it, redacted or not.
func projectSchemaValue(value any, schema *clientcontract.Schema, schemas map[string]clientcontract.Schema, secrets []string, redact, dropUndeclared bool, depth int) (any, bool) {
	if schema == nil || depth > 128 {
		return nil, false
	}
	if schema.Ref != "" {
		if value == nil && schema.Nullable != nil && *schema.Nullable {
			return nil, true
		}
		name := strings.TrimPrefix(schema.Ref, "#/components/schemas/")
		resolved, ok := schemas[name]
		if !ok {
			return nil, false
		}
		return projectSchemaValue(value, &resolved, schemas, secrets, redact, dropUndeclared, depth+1)
	}
	if schema.IsOpaqueJSON() {
		// Any JSON value, null included, is a member of an opaque declaration.
		// Redaction treats it like an undeclared member: it has no schema to
		// reason about, so credential material is scrubbed and a scalar that
		// equals one invalidates the payload.
		if redact {
			return scrubRemoteValue(value, secrets)
		}
		return value, true
	}
	if value == nil {
		return nil, schema.Nullable != nil && *schema.Nullable
	}
	if len(schema.OneOf) > 0 {
		if schema.Discriminator != nil {
			object, ok := value.(map[string]any)
			if !ok {
				return nil, false
			}
			tag, ok := object[schema.Discriminator.PropertyName].(string)
			if !ok {
				return nil, false
			}
			if ref := schema.Discriminator.Mapping[tag]; ref != "" {
				selected := clientcontract.Schema{Ref: ref}
				return projectSchemaValue(value, &selected, schemas, secrets, redact, dropUndeclared, depth+1)
			}
		}
		// A value that matches its variants without dropping anything selects
		// among them exactly as a strict projection does. Only a value no
		// variant matches that way is projected again with undeclared
		// properties dropped, and it still has to match exactly one variant.
		result, matches := projectOneOf(value, schema, schemas, secrets, redact, false, depth)
		if matches == 0 && dropUndeclared {
			result, matches = projectOneOf(value, schema, schemas, secrets, redact, true, depth)
		}
		return result, matches == 1
	}

	if len(schema.Enum) > 0 {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		found := false
		for _, allowed := range schema.Enum {
			if bytes.Equal(encoded, allowed) {
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}

	switch schema.Type {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		result := make(map[string]any, len(object))
		for _, required := range schema.Required {
			_, declared := schema.Properties[required]
			if _, exists := object[required]; !exists || redact && redactedKey(required, declared, secrets) {
				return nil, false
			}
		}
		for key, child := range object {
			property, declared := schema.Properties[key]
			if redact && redactedKey(key, declared, secrets) {
				continue
			}
			if declared {
				projected, valid := projectSchemaValue(child, &property, schemas, secrets, redact, dropUndeclared, depth+1)
				if !valid {
					return nil, false
				}
				result[key] = projected
				continue
			}
			if schema.AdditionalProperties == nil ||
				(schema.AdditionalProperties.Allowed != nil && !*schema.AdditionalProperties.Allowed) {
				if dropUndeclared {
					continue
				}
				return nil, false
			}
			switch {
			case schema.AdditionalProperties.Schema != nil:
				projected, valid := projectSchemaValue(child, schema.AdditionalProperties.Schema, schemas, secrets, redact, dropUndeclared, depth+1)
				if !valid {
					return nil, false
				}
				result[key] = projected
			case redact:
				scrubbed, keep := scrubRemoteValue(child, secrets)
				if !keep {
					continue
				}
				result[key] = scrubbed
			default:
				result[key] = child
			}
		}
		return result, true
	case "array":
		array, ok := value.([]any)
		if !ok || schema.Items == nil {
			return nil, false
		}
		if schema.MinItems != nil && len(array) < *schema.MinItems || schema.MaxItems != nil && len(array) > *schema.MaxItems {
			return nil, false
		}
		result := make([]any, len(array))
		seen := map[string]bool{}
		for i := range array {
			projected, valid := projectSchemaValue(array[i], schema.Items, schemas, secrets, redact, dropUndeclared, depth+1)
			if !valid {
				return nil, false
			}
			result[i] = projected
			if schema.UniqueItems != nil && *schema.UniqueItems {
				canonical, err := json.Marshal(projected)
				if err != nil {
					return nil, false
				}
				if seen[string(canonical)] {
					return nil, false
				}
				seen[string(canonical)] = true
			}
		}
		return result, true
	case "string":
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		if schema.MinLength != nil && len([]rune(text)) < *schema.MinLength || schema.MaxLength != nil && len([]rune(text)) > *schema.MaxLength {
			return nil, false
		}
		if schema.Pattern != "" {
			pattern, err := regexp.Compile(schema.Pattern)
			if err != nil || !pattern.MatchString(text) {
				return nil, false
			}
		}
		if schema.Format == "byte" {
			if _, err := base64.StdEncoding.DecodeString(text); err != nil {
				return nil, false
			}
		}
		if redact && secretMaterialInText(text, secrets) {
			// A declared string keeps its type, so the marker preserves the
			// schema the reader validates the sanitized payload against.
			return "[REDACTED]", true
		}
		return text, true
	case "integer":
		number, ok := value.(json.Number)
		if !ok || strings.ContainsAny(number.String(), ".eE") {
			return nil, false
		}
		if strings.HasPrefix(schema.Format, "uint") {
			if _, err := strconv.ParseUint(number.String(), 10, integerBits(schema.Format)); err != nil {
				return nil, false
			}
		} else if _, err := strconv.ParseInt(number.String(), 10, integerBits(schema.Format)); err != nil {
			return nil, false
		}
		if redact && secretScalar(number, secrets) {
			// No integer stands in for a redaction marker, so the whole declared
			// payload is dropped rather than published with the credential in it.
			return nil, false
		}
		return number, numericBounds(number, schema)
	case "number":
		number, ok := value.(json.Number)
		if !ok {
			return nil, false
		}
		if _, err := strconv.ParseFloat(number.String(), 64); err != nil {
			return nil, false
		}
		if redact && secretScalar(number, secrets) {
			return nil, false
		}
		return number, numericBounds(number, schema)
	case "boolean":
		boolean, ok := value.(bool)
		if ok && redact && secretScalar(boolean, secrets) {
			return nil, false
		}
		return boolean, ok
	default:
		return nil, false
	}
}

// projectOneOf projects value onto every variant of schema and reports how
// many accepted it, with the projection of the last one that did.
func projectOneOf(value any, schema *clientcontract.Schema, schemas map[string]clientcontract.Schema, secrets []string, redact, dropUndeclared bool, depth int) (any, int) {
	var result any
	matches := 0
	for i := range schema.OneOf {
		if candidate, ok := projectSchemaValue(value, &schema.OneOf[i], schemas, secrets, redact, dropUndeclared, depth+1); ok {
			result = candidate
			matches++
		}
	}
	return result, matches
}

func integerBits(format string) int {
	switch format {
	case "int32", "uint32":
		return 32
	default:
		return 64
	}
}

func numericBounds(number json.Number, schema *clientcontract.Schema) bool {
	value, ok := new(big.Rat).SetString(number.String())
	if !ok {
		return false
	}
	if schema.Minimum != nil {
		minimum, ok := new(big.Rat).SetString(schema.Minimum.String())
		if !ok || value.Cmp(minimum) < 0 || schema.ExclusiveMinimum != nil && *schema.ExclusiveMinimum && value.Cmp(minimum) == 0 {
			return false
		}
	}
	if schema.Maximum != nil {
		maximum, ok := new(big.Rat).SetString(schema.Maximum.String())
		if !ok || value.Cmp(maximum) > 0 || schema.ExclusiveMaximum != nil && *schema.ExclusiveMaximum && value.Cmp(maximum) == 0 {
			return false
		}
	}
	return true
}

// redactedKey reports whether a key must be dropped before a sanitized error
// payload is exposed. A key whose own text carries credential material always
// goes. The credential-shaped-name heuristic applies only to keys the provider
// did not declare: a declared property is business data, so a field named
// tokenCount or authorizationLevel keeps its value.
func redactedKey(key string, declared bool, secrets []string) bool {
	if secretMaterialInText(key, secrets) {
		return true
	}
	if declared {
		return credentialNamedField(key)
	}
	return credentialShapedName(key)
}
