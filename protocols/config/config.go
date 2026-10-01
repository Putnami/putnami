// Package config defines the shared config protocol types used by
// extractors, remote clients, the config server, and publish-time
// validation across Go and TypeScript.
//
// This is the canonical source of truth for the config schema manifest,
// field type vocabulary, resolve request/response shapes, and layer
// metadata. Both Go and TS implementations must produce identical
// manifests and hashes for equivalent inputs.
package config

import "time"

// FieldType enumerates the canonical field types shared across languages.
// Extractors must map language-specific types to one of these values.
const (
	FieldTypeString   = "string"
	FieldTypeInt      = "int"
	FieldTypeFloat    = "float"
	FieldTypeBool     = "bool"
	FieldTypeDuration = "duration"
	FieldTypeObject   = "object"
	FieldTypeArray    = "array"
	FieldTypeMap      = "map"
)

var fieldTypeValues = []string{
	FieldTypeString,
	FieldTypeInt,
	FieldTypeFloat,
	FieldTypeBool,
	FieldTypeDuration,
	FieldTypeObject,
	FieldTypeArray,
	FieldTypeMap,
}

var mapKeyTypeValues = []string{
	FieldTypeString,
	FieldTypeInt,
	FieldTypeBool,
}

// ValidFieldTypes is the set of allowed field type values.
var ValidFieldTypes = stringSet(fieldTypeValues)

// ValidMapKeyTypes is the set of primitive types accepted as map keys.
// Extractors must reject non-primitive map keys (e.g. struct keys) so that
// schemas remain portable across language ecosystems (TS has no analog for
// composite map keys).
var ValidMapKeyTypes = stringSet(mapKeyTypeValues)

// FieldTypeValues returns the canonical field type vocabulary in stable order.
func FieldTypeValues() []string {
	return append([]string(nil), fieldTypeValues...)
}

// MapKeyTypeValues returns the canonical map-key type vocabulary in stable order.
func MapKeyTypeValues() []string {
	return append([]string(nil), mapKeyTypeValues...)
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[value] = true
	}
	return out
}

// SchemaManifest is the top-level document produced by config schema
// extraction and consumed by the config server at publish time.
type SchemaManifest struct {
	// AppName is the application the schema belongs to; it namespaces the config
	// on the server.
	AppName string `json:"appName"`
	// Version is the app version this schema was extracted for.
	Version string `json:"version"`
	// SchemaHash is the canonical SHA-256 of the schema (see hash.go); clients and
	// server compare it to detect drift between extracted and resolved schemas.
	SchemaHash string `json:"schemaHash"`
	// Configs are the config blocks (top-level config paths) the app declares.
	Configs []Block `json:"configs"`
}

// RegisteredSchemaManifest is the server-facing schema document returned by
// read and history endpoints. It extends SchemaManifest with server-owned
// registration metadata.
type RegisteredSchemaManifest struct {
	SchemaManifest
	// RegisteredAt is the server-assigned time the schema version was registered;
	// nil until the server persists it.
	RegisteredAt *time.Time `json:"registeredAt,omitempty"`
}

// Block describes a single config path (e.g. "server", "database").
//
// Blocks are required at the top of the resolved config tree by default
// — applications are expected to populate every block they declare.
// Optional opt-in lets a schema express the rarer case of a block whose
// presence is contingent (feature-flag-gated, optional integration,
// per-environment overrides). It is emitted with omitempty so the
// canonical JSON form of existing required blocks is unchanged and
// their hash is preserved.
type Block struct {
	// Path is the config path this block occupies in the resolved tree (e.g.
	// "server", "database").
	Path string `json:"path"`
	// Fields are the field schemas declared directly under this block's path.
	Fields []FieldSchema `json:"fields"`
	// Optional marks a block whose presence is contingent (feature-gated,
	// per-environment); a required block (the default) must be populated.
	Optional bool `json:"optional,omitempty"`
}

// FieldSchema describes a single field within a config block.
//
// Composite shapes use distinct slots so the canonicalizer and validator can
// walk them without ambiguity:
//
//   - Type == "object"  → Fields holds the nested field schemas (empty when
//     the struct is opaque to the extractor, e.g. a cross-package type that
//     could not be resolved).
//   - Type == "array"   → Items describes the element schema.
//   - Type == "map"     → Keys names the primitive key type (must be in
//     ValidMapKeyTypes) and Values describes the value schema.
//
// Slots not relevant to the field's Type must be left zero so that the
// canonical JSON form (and therefore the schema hash) stays minimal.
type FieldSchema struct {
	// Name is the field's config key within its block.
	Name string `json:"name"`
	// Type is the canonical field type (one of the FieldType* values).
	Type string `json:"type"`
	// Description is human/automation-facing documentation for the field.
	Description string `json:"description,omitempty"`
	// Required marks a field the app must provide a value for.
	Required bool `json:"required,omitempty"`
	// Default is the string-encoded default value applied when unset.
	Default string `json:"default,omitempty"`
	// Env is the environment variable that overrides this field, when one is
	// bound.
	Env string `json:"env,omitempty"`
	// Sensitive marks a field whose value is a secret and must not be logged or
	// stored in plain config.
	Sensitive bool `json:"sensitive,omitempty"`
	// ProductionUnsafeDefault marks a field whose fallback default is unsafe in
	// production (an in-memory store, a process-generated key, a permissive
	// transport). Doctor flags such a field when it is left unset in the
	// production config sources, because the unsafe default would silently apply.
	//
	// It is additive and emitted with omitempty: an unset marker serializes to
	// nothing, so the canonical JSON form — and therefore the schema hash — of an
	// existing field is byte-for-byte unchanged. Both Go and TS extractors must
	// place it in the same position (after Sensitive) so a set marker hashes
	// identically across languages.
	ProductionUnsafeDefault bool `json:"productionUnsafeDefault,omitempty"`
	// Constraints are extractor-declared validation rules (e.g. "min=1"),
	// interpreted by validators.
	Constraints []string `json:"constraints,omitempty"`
	// Fields holds the nested field schemas when Type == "object".
	Fields []FieldSchema `json:"fields,omitempty"`
	// Items describes the element schema when Type == "array".
	Items *FieldSchema `json:"items,omitempty"`
	// Keys names the primitive key type when Type == "map" (see ValidMapKeyTypes).
	Keys string `json:"keys,omitempty"`
	// Values describes the value schema when Type == "map".
	Values *FieldSchema `json:"values,omitempty"`
}

// ResolveRequest is the input for remote config resolution.
// Clients send this to the config server to fetch resolved config.
type ResolveRequest struct {
	// AppName is the application whose config to resolve.
	AppName string `json:"appName"`
	// Version narrows resolution to a specific app version; empty resolves the
	// version-agnostic layers.
	Version string `json:"version,omitempty"`
	// Environment is the target environment (e.g. "production") that selects the
	// per-environment layers.
	Environment string `json:"environment"`
	// SchemaHash, when set, asks the server to confirm the resolved config matches
	// the client's schema (populates ResolveResponse.SchemaMatch).
	SchemaHash string `json:"schemaHash,omitempty"`
}

// ResolveResponse is the resolved config output from the server.
// Warnings contains validation messages when resolved values don't match
// the schema (missing required fields, type mismatches, etc.).
type ResolveResponse struct {
	// Config is the merged config tree resolved across the dimension layers.
	Config map[string]any `json:"config"`
	// Resolved reports whether the server found any config to resolve (false means
	// no matching entries).
	Resolved bool `json:"resolved"`
	// SchemaMatch reports whether the resolved config matched the request's
	// SchemaHash; false when the request omitted the hash or a mismatch occurred.
	SchemaMatch bool `json:"schemaMatch"`
	// Layers lists which dimension layers contributed, in application order.
	Layers []LayerInfo `json:"layers,omitempty"`
	// Warnings carries validation messages (missing required fields, type
	// mismatches) that did not block resolution.
	Warnings []string `json:"warnings,omitempty"`
}

// LayerInfo describes which dimension layer contributed to the resolved config.
type LayerInfo struct {
	// Dimension names the layer (e.g. "app/env") that contributed values.
	Dimension string `json:"dimension"`
	// Priority is the layer's precedence; higher priority layers override lower
	// ones during the merge.
	Priority int `json:"priority"`
}

// ResolutionLayers returns the ordered dimension layers for config resolution.
// Layers are ordered from least specific to most specific (later layers win):
//
//   - */*          (priority 10) — global defaults
//   - */env        (priority 20) — global per-environment
//   - app/*        (priority 30) — app defaults
//   - app/env      (priority 40) — app per-environment
//   - app/env/ver  (priority 50) — app per-environment per-version (optional)
func ResolutionLayers(appName, environment, version string) []Dimension {
	layers := []Dimension{
		{AppName: "*", Environment: "*", Priority: 10},
		{AppName: "*", Environment: environment, Priority: 20},
		{AppName: appName, Environment: "*", Priority: 30},
		{AppName: appName, Environment: environment, Priority: 40},
	}
	if version != "" {
		layers = append(layers, Dimension{
			AppName:     appName,
			Environment: environment,
			Version:     version,
			Priority:    50,
		})
	}
	return layers
}

// Dimension represents a layer in the config resolution stack.
type Dimension struct {
	AppName     string
	Environment string
	Version     string
	Priority    int
}

// DeepMerge merges src into dst recursively. Values in src override dst.
func DeepMerge(dst, src map[string]any) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := dst[k].(map[string]any); ok {
				DeepMerge(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}
