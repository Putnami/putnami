// Package schema provides struct-tag-based validation for the Putnami Go framework.
// It supports required/optional fields, type coercion, and composable constraints.
package schema

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// FieldError represents a validation error on a field.
type FieldError struct {
	Field      string `json:"field"`
	Message    string `json:"message"`
	Constraint string `json:"constraint,omitempty"`
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Result holds the outcome of a validation.
type Result struct {
	// Data contains only the fields that passed validation. Fields that are
	// absent (without a default) or that failed any constraint are excluded, so
	// a value present in Data is always one that validated successfully.
	Data   map[string]any
	Errors []FieldError
}

// HasErrors returns true if validation produced errors.
func (r *Result) HasErrors() bool {
	return len(r.Errors) > 0
}

// fieldDescriptor holds pre-parsed field metadata cached per reflect.Type.
type fieldDescriptor struct {
	name        string
	index       []int
	fieldType   reflect.Type
	defaultTag  string
	hasDefault  bool
	constraints []constraint
	isRequired  bool
}

var (
	// legacySchemaCache preserves Validate's historical top-level field
	// selection. Validate consumes caller-authored maps rather than JSON, so
	// encoding/json promotion would change its public contract.
	legacySchemaCache sync.Map // map[reflect.Type][]fieldDescriptor
	// jsonSchemaCache holds the encoding/json field projection used only by
	// ValidateDecoded and schema generators.
	jsonSchemaCache sync.Map // map[reflect.Type][]fieldDescriptor
)

func getJSONFieldDescriptors(schemaType reflect.Type) []fieldDescriptor {
	if cached, ok := jsonSchemaCache.Load(schemaType); ok {
		return cached.([]fieldDescriptor) //nolint:errcheck // type is always []fieldDescriptor
	}
	descriptors := make([]fieldDescriptor, 0, schemaType.NumField())
	for _, jsonField := range JSONFields(schemaType) {
		field := jsonField.Field
		defaultTag, hasDefault := field.Tag.Lookup("default")
		tag := field.Tag.Get("validate")
		constraints := parseConstraints(tag)
		descriptors = append(descriptors, fieldDescriptor{
			name:        jsonField.Name,
			index:       append([]int(nil), jsonField.Index...),
			fieldType:   field.Type,
			defaultTag:  defaultTag,
			hasDefault:  hasDefault,
			constraints: constraints,
			isRequired:  hasConstraint(constraints, "required"),
		})
	}
	jsonSchemaCache.Store(schemaType, descriptors)
	return descriptors
}

func getLegacyFieldDescriptors(schemaType reflect.Type) []fieldDescriptor {
	if cached, ok := legacySchemaCache.Load(schemaType); ok {
		return cached.([]fieldDescriptor) //nolint:errcheck // type is always []fieldDescriptor
	}
	descriptors := make([]fieldDescriptor, 0, schemaType.NumField())
	for index := range schemaType.NumField() {
		field := schemaType.Field(index)
		if !field.IsExported() {
			continue
		}
		constraints := parseConstraints(field.Tag.Get("validate"))
		descriptors = append(descriptors, fieldDescriptor{
			name:        legacyFieldName(field),
			index:       []int{index},
			fieldType:   field.Type,
			defaultTag:  field.Tag.Get("default"),
			constraints: constraints,
			isRequired:  hasConstraint(constraints, "required"),
		})
	}
	legacySchemaCache.Store(schemaType, descriptors)
	return descriptors
}

// JSONField is one struct field selected by encoding/json-compatible promotion
// and dominance rules. Index is the full reflection path from the outer struct,
// including anonymous embedded fields.
type JSONField struct {
	Name  string
	Field reflect.StructField
	Index []int
}

// JSONFields returns the deterministic set of fields encoded for a struct.
// A shallower promoted field wins; at equal depth an explicitly named JSON
// field wins; unresolved ties and json:"-" fields are omitted.
func JSONFields(schemaType reflect.Type) []JSONField {
	for schemaType != nil && schemaType.Kind() == reflect.Ptr {
		schemaType = schemaType.Elem()
	}
	if schemaType == nil || schemaType.Kind() != reflect.Struct {
		return nil
	}

	type candidate struct {
		field reflect.StructField
		index []int
		depth int
		order int
	}
	type frame struct {
		typ      reflect.Type
		index    []int
		depth    int
		ancestry map[reflect.Type]bool
	}
	byName := map[string][]candidate{}
	queue := []frame{{typ: schemaType, ancestry: map[reflect.Type]bool{schemaType: true}}}
	order := 0
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for index := range current.typ.NumField() {
			field := current.typ.Field(index)
			indirectType := field.Type
			if indirectType.Kind() == reflect.Ptr {
				indirectType = indirectType.Elem()
			}
			if field.Anonymous {
				if !field.IsExported() && indirectType.Kind() != reflect.Struct {
					continue
				}
			} else if !field.IsExported() {
				continue
			}
			fieldIndex := append(append([]int(nil), current.index...), index)
			name, omitted, tagged := parsedJSONName(field)
			if omitted {
				continue
			}
			if field.Anonymous && !tagged && indirectType.Kind() == reflect.Struct {
				if !current.ancestry[indirectType] {
					ancestry := make(map[reflect.Type]bool, len(current.ancestry)+1)
					for ancestor := range current.ancestry {
						ancestry[ancestor] = true
					}
					ancestry[indirectType] = true
					queue = append(queue, frame{typ: indirectType, index: fieldIndex, depth: current.depth + 1, ancestry: ancestry})
				}
				continue
			}
			if name == "" {
				name = field.Name
			}
			byName[name] = append(byName[name], candidate{field: field, index: fieldIndex, depth: current.depth, order: order})
			order++
		}
	}

	type winner struct {
		JSONField
		order int
	}
	winners := make([]winner, 0, len(byName))
	for name, candidates := range byName {
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].depth != candidates[j].depth {
				return candidates[i].depth < candidates[j].depth
			}
			if left, right := explicitlyNamed(candidates[i].field), explicitlyNamed(candidates[j].field); left != right {
				return left
			}
			return candidates[i].order < candidates[j].order
		})
		if len(candidates) > 1 && candidates[0].depth == candidates[1].depth &&
			explicitlyNamed(candidates[0].field) == explicitlyNamed(candidates[1].field) {
			continue
		}
		selected := candidates[0]
		winners = append(winners, winner{JSONField: JSONField{Name: name, Field: selected.field, Index: selected.index}, order: selected.order})
	}
	sort.Slice(winners, func(i, j int) bool { return winners[i].order < winners[j].order })
	result := make([]JSONField, len(winners))
	for index := range winners {
		result[index] = winners[index].JSONField
	}
	return result
}

func explicitlyNamed(field reflect.StructField) bool {
	_, _, tagged := parsedJSONName(field)
	return tagged
}

func parsedJSONName(field reflect.StructField) (name string, omitted bool, tagged bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", true, false
	}
	name, _, _ = strings.Cut(tag, ",")
	if !validJSONTagName(name) {
		return "", false, false
	}
	return name, false, name != ""
}

func validJSONTagName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", char):
		case !unicode.IsLetter(char) && !unicode.IsDigit(char):
			return false
		}
	}
	return true
}

// Validate validates a map of values against a struct type.
// The struct's fields are inspected for `validate` tags.
//
// Supported tags:
//
//	validate:"required"         - field must be present and non-zero
//	validate:"uuid"             - UUID v4 format
//	validate:"email"            - email format
//	validate:"url"              - valid URL
//	validate:"min=N"            - minimum numeric value
//	validate:"max=N"            - maximum numeric value
//	validate:"minlen=N"         - minimum string length
//	validate:"maxlen=N"         - maximum string length
//	validate:"pattern=REGEX"    - regex match
//	validate:"oneof=a|b|c"      - must be one of the listed values
//
// Multiple constraints are comma-separated: `validate:"required,minlen=3,maxlen=100"`
func Validate(schemaType reflect.Type, values map[string]any, opts ...ValidateOption) Result {
	cfg := validateConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	schemaType, typeError := normalizedStructType(schemaType)
	if typeError != nil {
		return Result{Data: map[string]any{}, Errors: []FieldError{{Message: typeError.Error()}}}
	}
	return validateFields(schemaType, cfg, false, func(fd fieldDescriptor) (any, bool, bool) {
		value, exists := values[fd.name]
		return value, exists, false
	})
}

// ValidateDecoded validates a JSON value that has already been decoded into
// its declared Go type. raw preserves the distinction between an absent
// property, an explicitly-null property, and a present zero value at every
// nesting level. This is the first-party body path: `validate:"required"`
// means JSON presence, matching the generated OpenAPI contract, while Validate
// retains its historical non-zero semantics.
//
// decoded must point at the value populated by json.Decoder. Optional defaults
// are written into that value before the endpoint handler receives it.
func ValidateDecoded(schemaType reflect.Type, decoded any, raw json.RawMessage, opts ...ValidateOption) Result {
	cfg := validateConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if schemaType == nil {
		return Result{Data: map[string]any{}, Errors: []FieldError{{Message: "schema type is nil"}}}
	}
	value := reflect.ValueOf(decoded)
	if !value.IsValid() || value.Kind() != reflect.Ptr || value.IsNil() {
		return Result{Data: map[string]any{}, Errors: []FieldError{{Message: "decoded value must be a non-nil pointer"}}}
	}
	value = value.Elem()
	if value.Type() != schemaType {
		return Result{Data: map[string]any{}, Errors: []FieldError{{Message: fmt.Sprintf("decoded value must point to type %s", schemaType)}}}
	}
	result := validateDecodedValue(schemaType, value, raw, cfg.label)
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	return result
}

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

func validateDecodedValue(schemaType reflect.Type, value reflect.Value, raw json.RawMessage, label string) Result {
	result := Result{Data: map[string]any{}}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		if schemaType.Kind() != reflect.Ptr && !acceptsAnyJSON(schemaType) {
			result.Errors = append(result.Errors, FieldError{Field: label, Message: "must not be null"})
		}
		return result
	}

	if schemaType.Kind() == reflect.Ptr {
		if value.IsNil() {
			result.Errors = append(result.Errors, FieldError{Field: label, Message: "decoded value is nil"})
			return result
		}
		return validateDecodedValue(schemaType.Elem(), value.Elem(), raw, label)
	}
	if isJSONScalarStruct(schemaType) {
		return result
	}

	switch schemaType.Kind() {
	case reflect.Struct:
		return validateDecodedStruct(schemaType, value, raw, label)
	case reflect.Slice, reflect.Array:
		if schemaType.Elem().Kind() == reflect.Uint8 {
			return result
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			result.Errors = append(result.Errors, FieldError{Field: label, Message: "must be a JSON array"})
			return result
		}
		for index, item := range items {
			if index >= value.Len() {
				result.Errors = append(result.Errors, FieldError{Field: indexed(label, index), Message: "decoded array length does not match JSON"})
				break
			}
			child := validateDecodedValue(schemaType.Elem(), value.Index(index), item, indexed(label, index))
			result.Errors = append(result.Errors, child.Errors...)
		}
	case reflect.Map:
		if schemaType.Key().Kind() != reflect.String {
			result.Errors = append(result.Errors, FieldError{Field: label, Message: "JSON object map keys must be strings"})
			return result
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			result.Errors = append(result.Errors, FieldError{Field: label, Message: "must be a JSON object"})
			return result
		}
		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			entry := entries[name]
			key := reflect.ValueOf(name).Convert(schemaType.Key())
			mapValue := value.MapIndex(key)
			if !mapValue.IsValid() {
				result.Errors = append(result.Errors, FieldError{Field: prefixed(label, name), Message: "decoded map entry is missing"})
				continue
			}
			editable := reflect.New(schemaType.Elem()).Elem()
			editable.Set(mapValue)
			child := validateDecodedValue(schemaType.Elem(), editable, entry, prefixed(label, name))
			result.Errors = append(result.Errors, child.Errors...)
			if len(child.Errors) == 0 {
				value.SetMapIndex(key, editable)
			}
		}
	}
	return result
}

func validateDecodedStruct(schemaType reflect.Type, value reflect.Value, raw json.RawMessage, label string) Result {
	result := Result{Data: map[string]any{}}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		result.Errors = append(result.Errors, FieldError{Field: label, Message: "must be a JSON object"})
		return result
	}

	for _, fd := range getJSONFieldDescriptors(schemaType) {
		fieldLabel := prefixed(label, fd.name)
		for _, constraint := range fd.constraints {
			if definitionError := constraintDefinitionError(constraint); definitionError != nil {
				result.Errors = append(result.Errors, FieldError{Field: fieldLabel, Message: definitionError.Error(), Constraint: constraint.Name})
			}
		}

		fieldRaw, exists := fields[fd.name]
		if fd.isRequired && !exists {
			result.Errors = append(result.Errors, FieldError{Field: fieldLabel, Message: "is required", Constraint: "required"})
			continue
		}
		if !exists {
			if !fd.hasDefault {
				continue
			}
		}
		fieldValue, fieldErr := decodedField(value, fd.index)
		if fieldErr != nil {
			result.Errors = append(result.Errors, FieldError{Field: fieldLabel, Message: fieldErr.Error()})
			continue
		}
		if !exists {
			defaultRaw, err := setDecodedDefault(fieldValue, fd.defaultTag)
			if err != nil {
				result.Errors = append(result.Errors, FieldError{Field: fieldLabel, Message: err.Error(), Constraint: "default"})
				continue
			}
			fieldRaw = defaultRaw
		}

		if bytes.Equal(bytes.TrimSpace(fieldRaw), []byte("null")) {
			if fd.fieldType.Kind() != reflect.Ptr && !acceptsAnyJSON(fd.fieldType) {
				result.Errors = append(result.Errors, FieldError{Field: fieldLabel, Message: "must not be null"})
			} else {
				result.Data[fd.name] = nil
			}
			continue
		}

		fieldValid := true
		for _, constraint := range fd.constraints {
			if constraint.Name == "required" || constraintDefinitionError(constraint) != nil {
				continue
			}
			constraintValue, accessible := indirectInterface(fieldValue)
			if !accessible {
				result.Errors = append(result.Errors, FieldError{
					Field:      fieldLabel,
					Message:    "cannot apply a value constraint to an unexported field",
					Constraint: constraint.Name,
				})
				fieldValid = false
				continue
			}
			if err := checkConstraint(constraint, constraintValue); err != nil {
				result.Errors = append(result.Errors, FieldError{Field: fieldLabel, Message: err.Error(), Constraint: constraint.Name})
				fieldValid = false
			}
		}

		child := validateDecodedValue(fd.fieldType, fieldValue, fieldRaw, fieldLabel)
		result.Errors = append(result.Errors, child.Errors...)
		if fieldValid && len(child.Errors) == 0 {
			if fieldValue.CanInterface() {
				result.Data[fd.name] = fieldValue.Interface()
			} else {
				// encoding/json can traverse the exported children of an unexported
				// anonymous struct. Reflection cannot expose that struct as an
				// interface without unsafe, so use the recursively validated data.
				// This also retains defaults that validation wrote into child fields.
				result.Data[fd.name] = child.Data
			}
		}
	}
	return result
}

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// acceptsAnyJSON reports whether a Go type is declared as an opaque JSON value
// (clientcontract.OpaqueJSONKey): json.RawMessage and the empty interface. Null
// is one of the values that declaration admits, so the decoded value keeps it —
// the bytes `null` in a json.RawMessage, a nil interface — instead of being
// refused as a missing value.
func acceptsAnyJSON(schemaType reflect.Type) bool {
	return schemaType == rawMessageType || (schemaType.Kind() == reflect.Interface && schemaType.NumMethod() == 0)
}

func isJSONScalarStruct(schemaType reflect.Type) bool {
	if schemaType.Kind() != reflect.Struct {
		return false
	}
	return schemaType.Implements(jsonUnmarshalerType) || reflect.PointerTo(schemaType).Implements(jsonUnmarshalerType) ||
		schemaType.Implements(textUnmarshalerType) || reflect.PointerTo(schemaType).Implements(textUnmarshalerType)
}

func setDecodedDefault(target reflect.Value, defaultTag string) (json.RawMessage, error) {
	raw, err := DefaultJSON(defaultTag, target.Type())
	if err != nil {
		return nil, err
	}
	if !target.CanAddr() || !target.CanSet() {
		return nil, fmt.Errorf("default target is not settable")
	}
	if err := json.Unmarshal(raw, target.Addr().Interface()); err != nil {
		return nil, fmt.Errorf("default is not valid for %s", target.Type())
	}
	return raw, nil
}

// DefaultJSON converts a struct default tag into its exact JSON representation
// for targetType. String and byte-slice defaults use their tag text as the
// value; numeric, boolean, composite, and pointer defaults must contain valid
// JSON accepted by encoding/json for the declared Go type.
func DefaultJSON(defaultTag string, targetType reflect.Type) (json.RawMessage, error) {
	if targetType == nil {
		return nil, fmt.Errorf("default target type is nil")
	}
	originalType := targetType
	nullable := false
	for targetType.Kind() == reflect.Ptr {
		nullable = true
		targetType = targetType.Elem()
	}
	var raw json.RawMessage
	if targetType.Kind() == reflect.String || targetType.Kind() == reflect.Slice && targetType.Elem().Kind() == reflect.Uint8 {
		encoded, err := json.Marshal(defaultTag)
		if err != nil {
			return nil, fmt.Errorf("default is not valid for %s", originalType)
		}
		raw = encoded
	} else {
		raw = json.RawMessage(defaultTag)
	}
	if !json.Valid(raw) || !nullable && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("default is not valid for %s", originalType)
	}
	probe := reflect.New(originalType)
	if err := json.Unmarshal(raw, probe.Interface()); err != nil {
		return nil, fmt.Errorf("default is not valid for %s", originalType)
	}
	return append(json.RawMessage(nil), raw...), nil
}

func indirectInterface(value reflect.Value) (any, bool) {
	for value.IsValid() && value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return nil, true
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return nil, true
	}
	if !value.CanInterface() {
		return nil, false
	}
	return value.Interface(), true
}

func decodedField(value reflect.Value, index []int) (reflect.Value, error) {
	for _, fieldIndex := range index {
		for value.Kind() == reflect.Ptr {
			if value.IsNil() {
				if !value.CanSet() {
					return reflect.Value{}, fmt.Errorf("embedded field is not settable")
				}
				value.Set(reflect.New(value.Type().Elem()))
			}
			value = value.Elem()
		}
		if value.Kind() != reflect.Struct || fieldIndex >= value.NumField() {
			return reflect.Value{}, fmt.Errorf("decoded field path is invalid")
		}
		value = value.Field(fieldIndex)
	}
	return value, nil
}

func indexed(label string, index int) string {
	return fmt.Sprintf("%s[%d]", label, index)
}

type fieldLookup func(fieldDescriptor) (value any, exists bool, null bool)

func validateFields(schemaType reflect.Type, cfg validateConfig, requiredPresenceOnly bool, lookup fieldLookup) Result {
	result := Result{Data: make(map[string]any)}
	for _, fd := range getLegacyFieldDescriptors(schemaType) {
		for _, c := range fd.constraints {
			if defErr := constraintDefinitionError(c); defErr != nil {
				result.Errors = append(result.Errors, FieldError{Field: prefixed(cfg.label, fd.name), Message: defErr.Error(), Constraint: c.Name})
			}
		}
		value, exists, null := lookup(fd)
		if (!exists || value == nil && !null) && fd.defaultTag != "" {
			value = coerceDefault(fd.defaultTag, fd.fieldType)
			exists = true
		}
		if fd.isRequired && (!exists || !requiredPresenceOnly && isZero(value)) {
			result.Errors = append(result.Errors, FieldError{Field: prefixed(cfg.label, fd.name), Message: "is required", Constraint: "required"})
			continue
		}
		if null {
			if fd.fieldType.Kind() != reflect.Ptr {
				result.Errors = append(result.Errors, FieldError{Field: prefixed(cfg.label, fd.name), Message: "must not be null"})
			}
			continue
		}
		if !exists || value == nil {
			continue
		}
		if cfg.coerce {
			coerced, err := coerceValue(value, fd.fieldType)
			if err != nil {
				result.Errors = append(result.Errors, FieldError{Field: prefixed(cfg.label, fd.name), Message: err.Error()})
				continue
			}
			value = coerced
		}
		fieldValid := true
		for _, c := range fd.constraints {
			if c.Name == "required" || constraintDefinitionError(c) != nil {
				continue
			}
			if err := checkConstraint(c, value); err != nil {
				result.Errors = append(result.Errors, FieldError{Field: prefixed(cfg.label, fd.name), Message: err.Error(), Constraint: c.Name})
				fieldValid = false
			}
		}
		if fieldValid {
			result.Data[fd.name] = value
		}
	}
	return result
}

func normalizedStructType(schemaType reflect.Type) (reflect.Type, error) {
	if schemaType == nil {
		return nil, fmt.Errorf("schema type is nil")
	}
	for schemaType.Kind() == reflect.Ptr {
		schemaType = schemaType.Elem()
	}
	if schemaType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("schema type must be a struct, got %s", schemaType.Kind())
	}
	return schemaType, nil
}

// ValidateOption configures validation behavior.
type ValidateOption func(*validateConfig)

type validateConfig struct {
	coerce bool
	label  string
}

// WithCoerce enables automatic type coercion (e.g., string to int).
func WithCoerce() ValidateOption {
	return func(c *validateConfig) { c.coerce = true }
}

// WithLabel prefixes error field names (e.g., "body", "query").
func WithLabel(label string) ValidateOption {
	return func(c *validateConfig) { c.label = label }
}

// --- constraint parsing ---

type constraint struct {
	Name       string
	Param      string
	compiledRe *regexp.Regexp // cached compiled regex for "pattern" constraints
	parseErr   error          // non-nil when the constraint definition is invalid (e.g. uncompilable pattern)
}

func parseConstraints(tag string) []constraint {
	if tag == "" {
		return nil
	}
	parts := strings.Split(tag, ",")
	constraints := make([]constraint, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, param, _ := strings.Cut(part, "=")
		c := constraint{Name: name, Param: param}
		if name == "pattern" && param != "" {
			re, err := regexp.Compile(param)
			if err != nil {
				c.parseErr = fmt.Errorf("invalid pattern %q: %w", param, err)
			} else {
				c.compiledRe = re
			}
		}
		constraints = append(constraints, c)
	}
	return constraints
}

func hasConstraint(constraints []constraint, name string) bool {
	for _, c := range constraints {
		if c.Name == name {
			return true
		}
	}
	return false
}

// constraintDefinitionError reports a schema-definition fault for a constraint —
// a typo'd/unknown constraint name or an uncompilable/empty pattern — as distinct
// from a per-value validation failure. It is evaluated against the live constraint
// registry so constraints registered after a descriptor is cached are still
// recognized.
func constraintDefinitionError(c constraint) error {
	if c.parseErr != nil {
		return c.parseErr
	}
	switch c.Name {
	case "required":
		return nil
	case "pattern":
		if c.Param == "" {
			return fmt.Errorf("pattern constraint requires a regex parameter")
		}
		return nil
	}
	if _, ok := getConstraintFunc(c.Name); !ok {
		return fmt.Errorf("unknown constraint %q", c.Name)
	}
	return nil
}

// --- constraint registry ---

// constraintFunc validates a value against a constraint parameter.
// It returns an error if validation fails, or nil if the value is valid.
type constraintFunc func(value any, param string) error

var (
	constraintsMu      sync.RWMutex
	constraintRegistry = make(map[string]constraintFunc)
)

// registerConstraint registers a custom validation constraint.
// If a constraint with the same name already exists, it is replaced.
func registerConstraint(name string, fn constraintFunc) {
	constraintsMu.Lock()
	constraintRegistry[name] = fn
	constraintsMu.Unlock()
}

func getConstraintFunc(name string) (constraintFunc, bool) {
	constraintsMu.RLock()
	fn, ok := constraintRegistry[name]
	constraintsMu.RUnlock()
	return fn, ok
}

// --- built-in constraint checks ---

var (
	uuidRegex  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)
)

func init() {
	registerConstraint("uuid", func(value any, _ string) error {
		s, ok := value.(string)
		if !ok || !uuidRegex.MatchString(strings.ToLower(s)) {
			return fmt.Errorf("must be a valid UUID v4")
		}
		return nil
	})
	registerConstraint("email", func(value any, _ string) error {
		s, ok := value.(string)
		if !ok || !emailRegex.MatchString(s) {
			return fmt.Errorf("must be a valid email address")
		}
		return nil
	})
	registerConstraint("url", func(value any, _ string) error {
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		u, err := url.ParseRequestURI(s)
		if err != nil {
			return fmt.Errorf("must be a valid URL")
		}
		// Restrict to http/https. url.ParseRequestURI accepts any scheme,
		// so without this a "url" value validated for a link or a
		// server-side fetch could be javascript:/data: (stored XSS when
		// rendered into an href) or file:/gopher:/etc. (SSRF / local-file
		// read when fetched). Allow only the safe web schemes by default.
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			return nil
		default:
			return fmt.Errorf("must be an http or https URL")
		}
	})
	registerConstraint("min", func(value any, param string) error {
		comparison, err := compareNumber(value, param)
		if err != nil {
			return fmt.Errorf("must be a number")
		}
		if comparison < 0 {
			return fmt.Errorf("must be >= %s", param)
		}
		return nil
	})
	registerConstraint("max", func(value any, param string) error {
		comparison, err := compareNumber(value, param)
		if err != nil {
			return fmt.Errorf("must be a number")
		}
		if comparison > 0 {
			return fmt.Errorf("must be <= %s", param)
		}
		return nil
	})
	registerConstraint("minlen", func(value any, param string) error {
		s := fmt.Sprintf("%v", value)
		min, err := strconv.Atoi(param)
		if err != nil {
			return fmt.Errorf("invalid minlen constraint: %s", param)
		}
		// Count runes, not bytes: the constraint reads as "characters" and a
		// multi-byte UTF-8 value (accented Latin, CJK, emoji) must be measured
		// the way a user counts it, not by its byte length.
		if utf8.RuneCountInString(s) < min {
			return fmt.Errorf("must be at least %s characters", param)
		}
		return nil
	})
	registerConstraint("maxlen", func(value any, param string) error {
		s := fmt.Sprintf("%v", value)
		max, err := strconv.Atoi(param)
		if err != nil {
			return fmt.Errorf("invalid maxlen constraint: %s", param)
		}
		// Count runes, not bytes (see minlen): a 5-rune string like "héllo"
		// must pass maxlen=5 even though it is 6 bytes.
		if utf8.RuneCountInString(s) > max {
			return fmt.Errorf("must be at most %s characters", param)
		}
		return nil
	})
	registerConstraint("oneof", func(value any, param string) error {
		s := fmt.Sprintf("%v", value)
		options := strings.Split(param, "|")
		for _, opt := range options {
			if s == opt {
				return nil
			}
		}
		return fmt.Errorf("must be one of: %s", strings.Join(options, ", "))
	})
}

func checkConstraint(c constraint, value any) error {
	// "pattern" uses the pre-compiled regex from the constraint struct.
	if c.Name == "pattern" {
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		if c.compiledRe == nil {
			return fmt.Errorf("invalid pattern: %s", c.Param)
		}
		if !c.compiledRe.MatchString(s) {
			return fmt.Errorf("must match pattern %s", c.Param)
		}
		return nil
	}

	fn, ok := getConstraintFunc(c.Name)
	if !ok {
		return fmt.Errorf("unknown constraint: %s", c.Name)
	}
	return fn(value, c.Param)
}

// --- helpers ---

func prefixed(label, name string) string {
	if label == "" {
		return name
	}
	return label + "." + name
}

func legacyFieldName(field reflect.StructField) string {
	if tag := field.Tag.Get("json"); tag != "" {
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			return name
		}
	}
	return field.Name
}

func isZero(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.IsZero()
}

func compareNumber(value any, limit string) (int, error) {
	var literal string
	switch number := value.(type) {
	case json.Number:
		literal = number.String()
	case string:
		literal = number
	default:
		reflected := reflect.ValueOf(value)
		if !reflected.IsValid() {
			return 0, fmt.Errorf("cannot convert %T to number", value)
		}
		switch reflected.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			literal = strconv.FormatInt(reflected.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			literal = strconv.FormatUint(reflected.Uint(), 10)
		case reflect.Float32, reflect.Float64:
			literal = strconv.FormatFloat(reflected.Float(), 'g', -1, reflected.Type().Bits())
		case reflect.String:
			literal = reflected.String()
		default:
			return 0, fmt.Errorf("cannot convert %T to number", value)
		}
	}
	left, ok := new(big.Rat).SetString(literal)
	if !ok {
		return 0, fmt.Errorf("invalid numeric value %q", literal)
	}
	right, ok := new(big.Rat).SetString(limit)
	if !ok {
		return 0, fmt.Errorf("invalid numeric limit %q", limit)
	}
	return left.Cmp(right), nil
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float", v)
	}
}

// coerceValue converts a textual path, query or form value to the field's
// declared type. Every declarable integer width is parsed at its own size:
// narrowing a wider parse wraps silently, and an unconverted string never
// reaches an integer field at all — the endpoint's struct binding can neither
// assign nor convert it, so the handler would read a zero it was never sent.
// A value outside the declared range is therefore a validation error, and a
// value that is not a number at all passes through unchanged so the declared
// constraints report it.
func coerceValue(value any, targetType reflect.Type) (any, error) {
	s, ok := value.(string)
	if !ok {
		return value, nil
	}

	switch targetType.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, targetType.Bits())
		if err == nil {
			return reflect.ValueOf(n).Convert(targetType).Interface(), nil
		}
		if errors.Is(err, strconv.ErrRange) {
			return value, fmt.Errorf("must be within the range of %s", targetType.Kind())
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, targetType.Bits())
		if err == nil {
			return reflect.ValueOf(n).Convert(targetType).Interface(), nil
		}
		if errors.Is(err, strconv.ErrRange) || isNegativeInteger(s) {
			return value, fmt.Errorf("must be within the range of %s", targetType.Kind())
		}
	case reflect.Float32, reflect.Float64:
		if n, err := strconv.ParseFloat(s, targetType.Bits()); err == nil {
			return reflect.ValueOf(n).Convert(targetType).Interface(), nil
		}
	case reflect.Bool:
		if b, err := strconv.ParseBool(s); err == nil {
			return b, nil
		}
	}
	return value, nil
}

// isNegativeInteger reports whether text is a well-formed negative integer.
// ParseUint calls one a syntax error, but a caller that sent -1 where a uint32
// is declared sent a number outside the declared range, not a non-number.
func isNegativeInteger(text string) bool {
	if !strings.HasPrefix(text, "-") {
		return false
	}
	_, err := strconv.ParseInt(text, 10, 64)
	return err == nil || errors.Is(err, strconv.ErrRange)
}

func coerceDefault(defaultStr string, targetType reflect.Type) any {
	switch targetType.Kind() {
	case reflect.String:
		return defaultStr
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.Bool:
		// A default is authored text like a query value, so it is read at the
		// declared width by the same rule.
		coerced, err := coerceValue(defaultStr, targetType)
		if err != nil {
			return reflect.Zero(targetType).Interface()
		}
		if _, unconverted := coerced.(string); unconverted {
			return reflect.Zero(targetType).Interface()
		}
		return coerced
	default:
		return defaultStr
	}
}
