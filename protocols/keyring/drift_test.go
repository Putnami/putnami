package keyring

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	keyringSchemaPath = "schemas/keyring.json"
	jwksSchemaPath    = "schemas/jwks.json"
	digestSchemaPath  = "schemas/digest.json"
)

func readSchema(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read schema %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse schema %s: %v", path, err)
	}
	return m
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// goJSONFields extracts the json tag names from the struct underlying v.
func goJSONFields(t *testing.T, v any) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		names = append(names, strings.Split(tag, ",")[0])
	}
	return names
}

// objectProperties returns the property names of the object at the given path
// inside the decoded schema (e.g. "properties" for the root, or "$defs",
// "privateJwk", "properties").
func objectProperties(t *testing.T, schema map[string]any, path ...string) []string {
	t.Helper()
	obj := navigate(t, schema, path...)
	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	return names
}

// stringEnum returns the enum array at the given path.
func stringEnum(t *testing.T, schema map[string]any, path ...string) []string {
	t.Helper()
	cur := any(schema)
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object", path, key)
		}
		cur = m[key]
	}
	arr, ok := cur.([]any)
	if !ok {
		t.Fatalf("path %v does not resolve to an array", path)
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("path %v has a non-string enum value %v", path, v)
		}
		out = append(out, s)
	}
	return out
}

func navigate(t *testing.T, schema map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := schema
	for _, key := range path {
		next, ok := cur[key].(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object", path, key)
		}
		cur = next
	}
	return cur
}

func assertFieldParity(t *testing.T, name string, schemaFields, goFields []string) {
	t.Helper()
	schemaSet := map[string]bool{}
	for _, f := range schemaFields {
		schemaSet[f] = true
	}
	goSet := map[string]bool{}
	for _, f := range goFields {
		goSet[f] = true
	}
	for _, f := range schemaFields {
		if !goSet[f] {
			t.Errorf("%s: field %q exists in schema but not in Go type", name, f)
		}
	}
	for _, f := range goFields {
		if !schemaSet[f] {
			t.Errorf("%s: field %q exists in Go type but not in schema", name, f)
		}
	}
}

// TestDrift_KeyringFields asserts the private keyring document and its private
// JWK item stay in lockstep with the schema.
func TestDrift_KeyringFields(t *testing.T) {
	schema := readSchema(t, keyringSchemaPath)
	assertFieldParity(t, "PrivateKeyring",
		objectProperties(t, schema, "properties"),
		goJSONFields(t, PrivateKeyring{}))
	assertFieldParity(t, "PrivateJWK",
		objectProperties(t, schema, "$defs", "privateJwk", "properties"),
		goJSONFields(t, PrivateJWK{}))
}

// TestDrift_JWKSFields asserts the public JWKS and public JWK stay in lockstep
// with the schema — and, critically, that the public JWK schema carries none of
// the private-material fields.
func TestDrift_JWKSFields(t *testing.T) {
	schema := readSchema(t, jwksSchemaPath)
	assertFieldParity(t, "JWKS",
		objectProperties(t, schema, "properties"),
		goJSONFields(t, JWKS{}))
	publicFields := objectProperties(t, schema, "$defs", "publicJwk", "properties")
	assertFieldParity(t, "JWK", publicFields, goJSONFields(t, JWK{}))
	for _, priv := range privateJWKFields {
		for _, f := range publicFields {
			if f == priv {
				t.Errorf("public JWK schema must not declare private field %q", priv)
			}
		}
	}
}

// TestDrift_RequiredFields pins the schema's required-field sets.
func TestDrift_RequiredFields(t *testing.T) {
	cases := []struct {
		name string
		path string
		want []string
	}{
		{"PrivateKeyring", keyringSchemaPath, []string{"keys", "protocolVersion"}},
		{"JWKS", jwksSchemaPath, []string{"keys"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var schema struct {
				Required []string `json:"required"`
			}
			data, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatal(err)
			}
			if got := sortedStrings(schema.Required); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s required = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestDrift_ProtocolVersionPinned asserts the keyring schema pins
// protocolVersion to the current ProtocolVersion.
func TestDrift_ProtocolVersionPinned(t *testing.T) {
	var schema struct {
		Properties struct {
			ProtocolVersion struct {
				Const *int `json:"const"`
			} `json:"protocolVersion"`
		} `json:"properties"`
	}
	data, err := os.ReadFile(keyringSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	got := schema.Properties.ProtocolVersion.Const
	if got == nil {
		t.Fatal("keyring schema properties.protocolVersion has no const")
	}
	if *got != ProtocolVersion {
		t.Errorf("schema protocolVersion const = %d, want %d", *got, ProtocolVersion)
	}
}

// TestDrift_EnumParity asserts the closed enums in the schemas match the Go enum
// values, so a schema edit that drops or adds a value fails the guard.
func TestDrift_EnumParity(t *testing.T) {
	keyring := readSchema(t, keyringSchemaPath)
	jwks := readSchema(t, jwksSchemaPath)
	digest := readSchema(t, digestSchemaPath)

	keyStates := make([]string, 0, len(AllKeyStates()))
	for _, s := range AllKeyStates() {
		keyStates = append(keyStates, string(s))
	}
	assertEnum(t, "keyState", stringEnum(t, keyring, "$defs", "keyState", "enum"), keyStates)
	assertEnum(t, "keyType(private)", stringEnum(t, keyring, "$defs", "keyType", "enum"),
		[]string{string(KeyTypeRSA), string(KeyTypeEC), string(KeyTypeOct)})
	assertEnum(t, "keyType(public)", stringEnum(t, jwks, "$defs", "keyType", "enum"),
		[]string{string(KeyTypeRSA), string(KeyTypeEC)})
	assertEnum(t, "digestAlgorithm", stringEnum(t, digest, "$defs", "algorithm", "enum"),
		[]string{string(DigestPBKDF2SHA256), string(DigestHMACSHA256)})
}

func assertEnum(t *testing.T, name string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(sortedStrings(got), sortedStrings(want)) {
		t.Errorf("%s enum = %v, want %v", name, got, want)
	}
}

// TestDrift_DigestSchemaPattern asserts the digest schema's pattern accepts every
// valid digest fixture and rejects the structurally-malformed ones, so the
// schema and the strict parser agree on the digest string shape.
func TestDrift_DigestSchemaPattern(t *testing.T) {
	var schema struct {
		Pattern string `json:"pattern"`
	}
	data, err := os.ReadFile(digestSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	re, err := regexp.Compile(schema.Pattern)
	if err != nil {
		t.Fatalf("compile digest pattern: %v", err)
	}
	var c digestCorpus
	loadFixture(t, "fixtures/digests.json", &c)
	for _, v := range c.Valid {
		if !re.MatchString(v.Digest) {
			t.Errorf("digest schema pattern rejects valid digest %q", v.Digest)
		}
	}
	// The pattern must reject the structurally-malformed cases it can express
	// (wrong algorithm, bad version literal, missing/extra segments, non-base64
	// alphabet, empty hash). Iteration-range and canonical-integer checks are
	// parser-only, so those fixtures are not asserted here.
	structural := map[string]bool{
		"unknown-algorithm":                  true,
		"unsupported-version":                true,
		"malformed-version-segment":          true,
		"pbkdf2-missing-iterations-segment":  true,
		"hmac-with-extra-iterations-segment": true,
		"salt-uses-base64url-alphabet":       true,
		"hash-empty":                         true,
		"no-leading-dollar":                  true,
		"empty-string":                       true,
	}
	for _, inv := range c.Invalid {
		if structural[inv.ID] && re.MatchString(inv.Digest) {
			t.Errorf("digest schema pattern should reject %s (%s): %q", inv.ID, inv.Reason, inv.Digest)
		}
	}
}
