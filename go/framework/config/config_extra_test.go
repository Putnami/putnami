package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"

	"go.putnami.dev/protocol/features/spectest"
)

// ---------------------------------------------------------------------------
// config.Provide — DI integration
// ---------------------------------------------------------------------------

func TestProvide(t *testing.T) {
	def := Config[ServerConfig]("server")
	source := NewMapSource("test", 50, map[string]any{
		"server": map[string]any{
			"host": "di.example.com",
			"port": 9000,
		},
	})

	reg := Provide(def, source)

	c := inject.NewContainer("test", nil)
	if err := c.Register(reg); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	raw, err := c.Get(Token(def))
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	cfg, ok := raw.(ServerConfig)
	if !ok {
		t.Fatalf("expected ServerConfig, got %T", raw)
	}
	if cfg.Host != "di.example.com" {
		t.Errorf("expected host=di.example.com, got %q", cfg.Host)
	}
	if cfg.Port != 9000 {
		t.Errorf("expected port=9000, got %d", cfg.Port)
	}
}

func TestProvideUsesDefaultsWhenNoSource(t *testing.T) {
	def := Config[ServerConfig]("server")
	reg := Provide(def) // no explicit sources — falls back to DiscoverSources

	c := inject.NewContainer("test-defaults", nil)
	if err := c.Register(reg); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	raw, err := c.Get(Token(def))
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	cfg, ok := raw.(ServerConfig)
	if !ok {
		t.Fatalf("expected ServerConfig, got %T", raw)
	}
	// Default values from struct tags should be applied.
	if cfg.Host != "localhost" {
		t.Errorf("expected default host=localhost, got %q", cfg.Host)
	}
}

// ---------------------------------------------------------------------------
// EnvSource — Name / Priority
// ---------------------------------------------------------------------------

func TestEnvSourceName(t *testing.T) {
	s := NewEnvSource("APP")
	if s.Name() != "env" {
		t.Errorf("expected Name()='env', got %q", s.Name())
	}
}

func TestEnvSourcePriority(t *testing.T) {
	s := NewEnvSource("APP")
	if s.Priority() != 80 {
		t.Errorf("expected Priority()=80, got %d", s.Priority())
	}
}

func TestEnvSourceLoadReturnsNilWhenNoMatchingVars(t *testing.T) {
	// Use an unlikely prefix so no real env var matches.
	s := NewEnvSource("XYZZY_UNLIKELY_PREFIX_QWERTY")
	data, err := s.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data != nil {
		t.Errorf("expected nil when no matching env vars, got %v", data)
	}
}

func TestEnvSourceLoadDeepNesting(t *testing.T) {
	t.Setenv("MYAPP_DB_POOL_MAX", "50")
	t.Setenv("MYAPP_DB_POOL_MIN", "5")

	tree, err := NewEnvSource("MYAPP").Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	db, ok := tree["db"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested 'db' map, got %T", tree["db"])
	}
	pool, ok := db["pool"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested 'pool' map, got %T", db["pool"])
	}
	if pool["max"] != "50" {
		t.Errorf("expected db.pool.max=50, got %v", pool["max"])
	}
	if pool["min"] != "5" {
		t.Errorf("expected db.pool.min=5, got %v", pool["min"])
	}
}

// ---------------------------------------------------------------------------
// MapSource — Name (0%)
// ---------------------------------------------------------------------------

func TestMapSourceName(t *testing.T) {
	s := NewMapSource("my-source", 50, nil)
	if s.Name() != "my-source" {
		t.Errorf("expected Name()='my-source', got %q", s.Name())
	}
}

// ---------------------------------------------------------------------------
// parseFloat (0%) via setFieldFromString
// ---------------------------------------------------------------------------

func TestSetFieldFromStringFloat64(t *testing.T) {
	type floatCfg struct {
		Rate float64 `json:"rate"`
	}
	fv := reflect.ValueOf(&floatCfg{}).Elem().Field(0)
	if err := setFieldFromString(fv, "3.14", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fv.Float() != 3.14 {
		t.Errorf("expected 3.14, got %v", fv.Float())
	}
}

func TestSetFieldFromStringFloat64Invalid(t *testing.T) {
	type floatCfg struct {
		Rate float64 `json:"rate"`
	}
	fv := reflect.ValueOf(&floatCfg{}).Elem().Field(0)
	// An invalid float must surface an error instead of being
	// silently discarded; the field is left at its zero value.
	if err := setFieldFromString(fv, "not-a-float", false); err == nil {
		t.Error("expected an error for invalid float, got nil")
	}
	if fv.Float() != 0 {
		t.Errorf("expected zero on invalid float, got %v", fv.Float())
	}
}

func TestSetFieldFromStringDuration(t *testing.T) {
	type durCfg struct {
		Timeout time.Duration `json:"timeout"`
	}
	fv := reflect.ValueOf(&durCfg{}).Elem().Field(0)
	if err := setFieldFromString(fv, "5s", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fv.Int() != int64(5*time.Second) {
		t.Errorf("expected 5s, got %v", time.Duration(fv.Int()))
	}
}

func TestSetFieldFromStringDurationInvalid(t *testing.T) {
	type durCfg struct {
		Timeout time.Duration `json:"timeout"`
	}
	fv := reflect.ValueOf(&durCfg{}).Elem().Field(0)
	// Invalid duration must surface an error; field stays at zero.
	if err := setFieldFromString(fv, "not-a-duration", false); err == nil {
		t.Error("expected an error for invalid duration, got nil")
	}
	if fv.Int() != 0 {
		t.Errorf("expected zero on invalid duration, got %v", fv.Int())
	}
}

// ---------------------------------------------------------------------------
// setFieldFromAny — collection and type-conversion branches
// ---------------------------------------------------------------------------

func TestSetFieldFromAnySlice(t *testing.T) {
	type sliceCfg struct {
		Tags []string `json:"tags"`
	}
	cfg := &sliceCfg{}
	fv := reflect.ValueOf(cfg).Elem().Field(0)

	if err := setFieldFromAny(fv, []any{"alpha", "beta", "gamma"}, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(cfg.Tags, want) {
		t.Errorf("expected %v, got %v", want, cfg.Tags)
	}
}

func TestSetFieldFromAnySliceEmpty(t *testing.T) {
	type sliceCfg struct {
		Tags []string `json:"tags"`
	}
	cfg := &sliceCfg{}
	fv := reflect.ValueOf(cfg).Elem().Field(0)

	if err := setFieldFromAny(fv, []any{}, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tags) != 0 {
		t.Errorf("expected empty slice, got %v", cfg.Tags)
	}
}

func TestSetFieldFromAnySliceElementTypeMismatch(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "mapping", "rejects-slice-element-type-mismatch")
	// A []any whose element is a map[string]any cannot be mapped into a
	// non-struct element type (here []int). The module's fail-loud mapping
	// contract requires an error.
	type sliceCfg struct {
		Counts []int `json:"counts"`
	}
	cfg := &sliceCfg{}
	fv := reflect.ValueOf(cfg).Elem().Field(0)

	err := setFieldFromAny(fv, []any{map[string]any{"nope": 1}}, false)
	if err == nil {
		t.Fatal("expected an error for a mismatched slice element type, got nil")
	}
	if got := errors.GetCode(err); got != CodeConfigMapping {
		t.Errorf("error code = %q, want %q", got, CodeConfigMapping)
	}
}

func TestLoadTypedMaps(t *testing.T) {
	type backend struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	type mapCfg struct {
		Labels   map[string]string  `json:"labels"`
		Backends map[string]backend `json:"backends"`
		Ports    map[int]string     `json:"ports"`
	}

	cfg, err := Load(Config[mapCfg]("service"), NewMapSource("test", 50, map[string]any{
		"service": map[string]any{
			"labels":   map[string]any{"region": "eu-west-3"},
			"backends": map[string]any{"primary": map[string]any{"host": "api.example.com", "port": 8443}},
			"ports":    map[string]any{"8443": "https"},
		},
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if want := map[string]string{"region": "eu-west-3"}; !reflect.DeepEqual(cfg.Labels, want) {
		t.Errorf("Labels = %v, want %v", cfg.Labels, want)
	}
	if want := (backend{Host: "api.example.com", Port: 8443}); cfg.Backends["primary"] != want {
		t.Errorf("Backends[primary] = %+v, want %+v", cfg.Backends["primary"], want)
	}
	if want := map[int]string{8443: "https"}; !reflect.DeepEqual(cfg.Ports, want) {
		t.Errorf("Ports = %v, want %v", cfg.Ports, want)
	}
}

func TestSetFieldFromAnyMapElementTypeMismatch(t *testing.T) {
	type mapCfg struct {
		Counts map[string]int `json:"counts"`
	}
	cfg := &mapCfg{}
	fv := reflect.ValueOf(cfg).Elem().Field(0)

	err := setFieldFromAny(fv, map[string]any{"bad": map[string]any{"nope": 1}}, false)
	if err == nil {
		t.Fatal("expected an error for a mismatched map value type, got nil")
	}
	if got := errors.GetCode(err); got != CodeConfigMapping {
		t.Errorf("error code = %q, want %q", got, CodeConfigMapping)
	}
	if cfg.Counts != nil {
		t.Errorf("Counts = %v, want nil after failed mapping", cfg.Counts)
	}
}

func TestSetFieldFromAnyUnmappableValue(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "mapping", "rejects-unmappable-value")
	// A value matching none of the branches (not assignable, not convertible,
	// not string-parseable, not a nested struct, not a slice) must fail loud
	// rather than silently leaving the field at its zero value.
	type cfg struct {
		Inner struct {
			X int `json:"x"`
		} `json:"inner"`
	}
	c := &cfg{}
	fv := reflect.ValueOf(c).Elem().Field(0)

	// A []any cannot be mapped into a struct field.
	err := setFieldFromAny(fv, []any{1, 2, 3}, false)
	if err == nil {
		t.Fatal("expected an error for an unmappable value, got nil")
	}
	if got := errors.GetCode(err); got != CodeConfigMapping {
		t.Errorf("error code = %q, want %q", got, CodeConfigMapping)
	}
}

func TestSetFieldFromAnyTypeConversion(t *testing.T) {
	// int → float64: the value is convertible but not directly assignable.
	type numCfg struct {
		Rate float64 `json:"rate"`
	}
	cfg := &numCfg{}
	fv := reflect.ValueOf(cfg).Elem().Field(0)

	// Provide an int which is ConvertibleTo float64 but not AssignableTo it.
	if err := setFieldFromAny(fv, int(7), false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Rate != 7.0 {
		t.Errorf("expected 7.0 via type conversion, got %v", cfg.Rate)
	}
}

func TestSetFieldFromAnyNil(t *testing.T) {
	type strCfg struct {
		Name string `json:"name"`
	}
	cfg := &strCfg{Name: "original"}
	fv := reflect.ValueOf(cfg).Elem().Field(0)

	if err := setFieldFromAny(fv, nil, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// nil should be a no-op; value must remain unchanged.
	if cfg.Name != "original" {
		t.Errorf("expected 'original' after nil, got %q", cfg.Name)
	}
}

func TestSetFieldFromAnyNestedStruct(t *testing.T) {
	type inner struct {
		X int `json:"x"`
	}
	type outer struct {
		Inner inner `json:"inner"`
	}
	cfg := &outer{}
	fv := reflect.ValueOf(cfg).Elem().Field(0)

	if err := setFieldFromAny(fv, map[string]any{"x": 42}, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Inner.X != 42 {
		t.Errorf("expected Inner.X=42, got %d", cfg.Inner.X)
	}
}

func TestSetFieldFromAnyStringConversion(t *testing.T) {
	// A string value that is not directly assignable triggers the string branch.
	type portCfg struct {
		Port int `json:"port"`
	}
	cfg := &portCfg{}
	fv := reflect.ValueOf(cfg).Elem().Field(0)

	if err := setFieldFromAny(fv, "8080", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != 8080 {
		t.Errorf("expected Port=8080, got %d", cfg.Port)
	}
}

func TestSetFieldFromAnyNumericToString(t *testing.T) {
	type stringCfg struct {
		Port string `json:"port"`
	}
	cfg := &stringCfg{}
	fv := reflect.ValueOf(cfg).Elem().Field(0)

	if err := setFieldFromAny(fv, 8080, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("expected Port=%q, got %q", "8080", cfg.Port)
	}
}

// ---------------------------------------------------------------------------
// parseFloat directly
// ---------------------------------------------------------------------------

func TestParseFloat(t *testing.T) {
	tests := []struct {
		input   string
		want    float64
		wantErr bool
	}{
		{"1.5", 1.5, false},
		{"0.0", 0.0, false},
		{"-3.14", -3.14, false},
		{"1e10", 1e10, false},
		{"notanumber", 0, true},
	}
	for _, tc := range tests {
		got, err := parseFloat(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseFloat(%q): expected error, got nil (value %v)", tc.input, got)
			}
		} else {
			if err != nil {
				t.Errorf("parseFloat(%q): unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("parseFloat(%q): expected %v, got %v", tc.input, tc.want, got)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Invalid scalars surface an error instead of being silently discarded.
// ---------------------------------------------------------------------------

func TestMapToStructRejectsInvalidScalar(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "mapping", "rejects-invalid-scalar-string")
	type cfg struct {
		Port int `json:"port"`
	}
	var c cfg
	// A non-numeric value must surface an error, not silently leave Port=0.
	if err := mapToStruct(map[string]any{"port": "abc"}, &c); err == nil {
		t.Fatal("expected error for invalid int scalar 'abc', got nil")
	}
	// Trailing garbage that fmt.Sscanf used to accept ("80x" -> 80) must also fail.
	if err := mapToStruct(map[string]any{"port": "80x"}, &c); err == nil {
		t.Fatal("expected error for '80x' (trailing garbage), got nil")
	}
}

func TestMapToStructRejectsInvalidEnvScalar(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "mapping", "rejects-invalid-env-scalar")
	type cfg struct {
		Port int `json:"port" env:"TEST_CFG_INVALID_PORT"`
	}
	t.Setenv("TEST_CFG_INVALID_PORT", "abc")
	var c cfg
	if err := mapToStruct(map[string]any{}, &c); err == nil {
		t.Fatal("expected error for env PORT=abc, got nil")
	}
}

// A default/env tag on an unsupported field kind must fail loud.

func TestMapToStructRejectsUnsupportedKindWithDefaultTag(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "mapping", "rejects-unsupported-kind-with-default-tag")
	type cfg struct {
		Count uint `json:"count" default:"5"`
	}
	var c cfg
	err := mapToStruct(map[string]any{}, &c)
	if err == nil {
		t.Fatal("expected an error for a uint field carrying a default tag, got nil (it would silently stay 0)")
	}
	if got := errors.GetCode(err); got != CodeConfigMapping {
		t.Errorf("error code = %q, want %q", got, CodeConfigMapping)
	}
	if c.Count != 0 {
		t.Errorf("unsupported field should remain at its zero value, got %d", c.Count)
	}
}

func TestMapToStructRejectsUnsupportedKindWithEnvTag(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "mapping", "rejects-unsupported-kind-with-env-tag")
	type cfg struct {
		Tags []string `json:"tags" env:"TEST_CFG_UNSUPPORTED_TAGS"`
	}
	t.Setenv("TEST_CFG_UNSUPPORTED_TAGS", "a,b,c")
	var c cfg
	err := mapToStruct(map[string]any{}, &c)
	if err == nil {
		t.Fatal("expected an error for a []string field driven by an env tag, got nil")
	}
	if got := errors.GetCode(err); got != CodeConfigMapping {
		t.Errorf("error code = %q, want %q", got, CodeConfigMapping)
	}
}

// A malformed sensitive env/default value must not leak via attrs.

// attrValues collects, across the whole error chain, the string values of every
// attr with the given key. mapToStruct redacts a sensitive field's value at the
// inner parse error and the outer wrap, and errors.GetAttrs walks both, so this
// must observe redaction at every layer to prove the secret never reaches an
// attr-iterating logger.
func attrValues(err error, key string) []string {
	var out []string
	for _, a := range errors.GetAttrs(err) {
		if a.Key != key {
			continue
		}
		if s, ok := a.Value.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// assertNoAttrLeak fails if secret appears in any attr value of the error chain
// and asserts the redaction marker is present under wantKey instead. It also
// confirms the field name survives for diagnosability.
func assertNoAttrLeak(t *testing.T, err error, secret, wantKey, fieldName string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a mapping error, got nil")
	}
	for _, a := range errors.GetAttrs(err) {
		if s, ok := a.Value.(string); ok && strings.Contains(s, secret) {
			t.Fatalf("sensitive value %q leaked into attr %q=%q", secret, a.Key, s)
		}
	}
	got := attrValues(err, wantKey)
	if !slices.Contains(got, "[redacted]") {
		t.Errorf("expected a %q attr redacted to \"[redacted]\", got %v", wantKey, got)
	}
	if fields := attrValues(err, "field"); !slices.Contains(fields, fieldName) {
		t.Errorf("expected field name %q preserved for diagnosability, got %v", fieldName, fields)
	}
}

func TestMapToStructRedactsSensitiveEnvValue(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "sensitive-values", "redacts-sensitive-env-value")
	// An int-kind secret bound via env: a malformed numeric API key must never
	// be echoed verbatim into any error attr.
	const secret = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	type cfg struct {
		APIKey int `json:"apiKey" env:"TEST_CFG_APIKEY_1887" sensitive:"true"`
	}
	t.Setenv("TEST_CFG_APIKEY_1887", secret)
	var c cfg
	err := mapToStruct(map[string]any{}, &c)
	if got := errors.GetCode(err); got != CodeConfigMapping {
		t.Errorf("error code = %q, want %q", got, CodeConfigMapping)
	}
	assertNoAttrLeak(t, err, secret, "value", "apiKey")
	if c.APIKey != 0 {
		t.Errorf("malformed field should stay at zero, got %d", c.APIKey)
	}
}

func TestMapToStructRedactsSensitiveDefaultValue(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "sensitive-values", "redacts-sensitive-default-value")
	// A duration-kind secret supplied via default: must redact too. The default
	// path emits the value under the "default" attr key.
	const secret = "rotate-me-please-not-a-duration"
	type cfg struct {
		Window time.Duration `json:"window" default:"rotate-me-please-not-a-duration" sensitive:"true"`
	}
	var c cfg
	err := mapToStruct(map[string]any{}, &c)
	if got := errors.GetCode(err); got != CodeConfigMapping {
		t.Errorf("error code = %q, want %q", got, CodeConfigMapping)
	}
	assertNoAttrLeak(t, err, secret, "default", "window")
}

func TestMapToStructRedactsSensitiveBoolValue(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "sensitive-values", "redacts-sensitive-bool-value")
	// Bool parsing runs through parseBool, which has its own value attr; prove
	// that path redacts as well.
	const secret = "hunter2-totally-not-a-bool"
	type cfg struct {
		Flag bool `json:"flag" env:"TEST_CFG_FLAG_1887" sensitive:"true"`
	}
	t.Setenv("TEST_CFG_FLAG_1887", secret)
	var c cfg
	err := mapToStruct(map[string]any{}, &c)
	assertNoAttrLeak(t, err, secret, "value", "flag")
}

func TestMapToStructRedactsSensitiveMapStringValue(t *testing.T) {
	spectest.Proves(t, "go/typed-configuration", "sensitive-values", "redacts-sensitive-map-string-value")
	// Sensitive values can come from YAML/map/remote sources, not just env and
	// default tags. A malformed string-valued secret from those sources must
	// redact the inner parse attr too.
	const secret = "map-secret-not-an-int"
	type cfg struct {
		APIKey int `json:"apiKey" sensitive:"true"`
	}
	var c cfg
	err := mapToStruct(map[string]any{"apiKey": secret}, &c)
	assertNoAttrLeak(t, err, secret, "value", "apiKey")
}

func TestMapToStructNonSensitiveValueNotRedacted(t *testing.T) {
	// Redaction must be gated on the tag: a non-sensitive field keeps its value
	// in the attr so operators can see the offending input (e.g. PORT=abc).
	const bad = "abc"
	type cfg struct {
		Port int `json:"port" env:"TEST_CFG_PORT_1887_PLAIN"`
	}
	t.Setenv("TEST_CFG_PORT_1887_PLAIN", bad)
	var c cfg
	err := mapToStruct(map[string]any{}, &c)
	if err == nil {
		t.Fatal("expected an error for PORT=abc, got nil")
	}
	if got := attrValues(err, "value"); !slices.Contains(got, bad) {
		t.Errorf("non-sensitive value should be retained in attr, got %v", got)
	}
	if slices.Contains(attrValues(err, "value"), "[redacted]") {
		t.Error("non-sensitive value should not be redacted")
	}
}

// ---------------------------------------------------------------------------
// Load — additional branch: path not an object
// ---------------------------------------------------------------------------

func TestLoadPathNotObject(t *testing.T) {
	def := Config[ServerConfig]("server")
	source := NewMapSource("test", 50, map[string]any{
		"server": "not-a-map", // scalar at path, should error
	})

	_, err := Load(def, source)
	if err == nil {
		t.Error("expected error when path value is not a map")
	}
}

// ---------------------------------------------------------------------------
// Load — integration: float64 field loaded end-to-end
// ---------------------------------------------------------------------------

type FloatConfig struct {
	Rate    float64       `json:"rate"`
	Timeout time.Duration `json:"timeout"`
}

func TestLoadFloatAndDuration(t *testing.T) {
	def := Config[FloatConfig]("svc")
	source := NewMapSource("test", 50, map[string]any{
		"svc": map[string]any{
			"rate":    "2.71",
			"timeout": "10s",
		},
	})

	cfg, err := Load(def, source)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Rate != 2.71 {
		t.Errorf("expected Rate=2.71, got %v", cfg.Rate)
	}
	if cfg.Timeout != 10*time.Second {
		t.Errorf("expected Timeout=10s, got %v", cfg.Timeout)
	}
}
