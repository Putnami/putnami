package http

import (
	"testing"
)

// --- mapAs / ParamsAs / QueryAs / BodyAs ---

type endpointUser struct {
	Name  string `json:"name"`
	Age   int    `json:"age"`
	Email string `json:"email,omitempty"`
	// Untagged field is matched by lowercased field name ("score").
	Score int
}

func TestParamsAs_Struct(t *testing.T) {
	ctx := &EndpointContext{ValidatedParams: map[string]any{
		"name": "ada",
		"age":  41,
	}}

	got, err := ParamsAs[endpointUser](ctx)
	if err != nil {
		t.Fatalf("ParamsAs returned error: %v", err)
	}
	if got.Name != "ada" || got.Age != 41 {
		t.Errorf("ParamsAs = %+v, want Name=ada Age=41", got)
	}
}

func TestQueryAs_Struct(t *testing.T) {
	ctx := &EndpointContext{ValidatedQuery: map[string]any{
		"name": "grace",
	}}

	got, err := QueryAs[endpointUser](ctx)
	if err != nil {
		t.Fatalf("QueryAs returned error: %v", err)
	}
	if got.Name != "grace" {
		t.Errorf("QueryAs Name = %q, want grace", got.Name)
	}
	if got.Age != 0 || got.Email != "" {
		t.Errorf("missing keys should leave zero values, got %+v", got)
	}
}

func TestBodyAs_Struct_JSONTagAndUntaggedField(t *testing.T) {
	ctx := &EndpointContext{ValidatedBody: map[string]any{
		"name":  "lin",
		"Score": 99, // untagged field, matched by lowercased name "score"
		"score": 7,
	}}

	got, err := BodyAs[endpointUser](ctx)
	if err != nil {
		t.Fatalf("BodyAs returned error: %v", err)
	}
	if got.Name != "lin" {
		t.Errorf("Name = %q, want lin", got.Name)
	}
	if got.Score != 7 {
		t.Errorf("Score = %d, want 7 (matched by lowercased field name)", got.Score)
	}
}

func TestBodyAs_NilDataYieldsZeroValue(t *testing.T) {
	ctx := &EndpointContext{ValidatedBody: nil}
	got, err := BodyAs[endpointUser](ctx)
	if err != nil {
		t.Fatalf("BodyAs(nil) returned error: %v", err)
	}
	if got != (endpointUser{}) {
		t.Errorf("expected zero value for nil data, got %+v", got)
	}
}

func TestBodyAs_NilFieldValueSkipped(t *testing.T) {
	ctx := &EndpointContext{ValidatedBody: map[string]any{
		"name": "kay",
		"age":  nil, // nil values are skipped, leaving the zero value
	}}
	got, err := BodyAs[endpointUser](ctx)
	if err != nil {
		t.Fatalf("BodyAs returned error: %v", err)
	}
	if got.Name != "kay" || got.Age != 0 {
		t.Errorf("got %+v, want Name=kay Age=0", got)
	}
}

func TestBodyAs_ConvertibleField(t *testing.T) {
	// int -> int64 is convertible but not assignable.
	type holder struct {
		Count int64 `json:"count"`
	}
	ctx := &EndpointContext{ValidatedBody: map[string]any{"count": 5}}
	got, err := BodyAs[holder](ctx)
	if err != nil {
		t.Fatalf("BodyAs returned error: %v", err)
	}
	if got.Count != 5 {
		t.Errorf("Count = %d, want 5 (int converted to int64)", got.Count)
	}
}

func TestBodyAs_MismatchedTypeLeavesZero(t *testing.T) {
	// A string cannot be assigned or converted to a struct field, so it is
	// left at the zero value rather than panicking.
	type holder struct {
		Count int `json:"count"`
	}
	ctx := &EndpointContext{ValidatedBody: map[string]any{"count": struct{}{}}}
	got, err := BodyAs[holder](ctx)
	if err != nil {
		t.Fatalf("BodyAs returned error: %v", err)
	}
	if got.Count != 0 {
		t.Errorf("Count = %d, want 0 for incompatible type", got.Count)
	}
}

func TestBodyAs_MapTarget(t *testing.T) {
	ctx := &EndpointContext{ValidatedBody: map[string]any{"a": 1, "b": 2}}
	got, err := BodyAs[map[string]any](ctx)
	if err != nil {
		t.Fatalf("BodyAs returned error: %v", err)
	}
	if len(got) != 2 || got["a"] != 1 || got["b"] != 2 {
		t.Errorf("map target = %+v, want {a:1 b:2}", got)
	}
}

func TestBodyAs_NonStructNonMapJSONRoundTrip(t *testing.T) {
	// A target whose Kind is neither struct nor map (here a pointer) takes the
	// JSON round-trip branch in mapAs.
	type named struct {
		Value string `json:"value"`
	}
	ctx := &EndpointContext{ValidatedBody: map[string]any{"value": "x"}}
	got, err := BodyAs[*named](ctx)
	if err != nil {
		t.Fatalf("BodyAs returned error: %v", err)
	}
	if got == nil || got.Value != "x" {
		t.Errorf("got %+v, want &{Value:x}", got)
	}
}

// --- InjectedAs ---

func TestInjectedAs_Found(t *testing.T) {
	type repo struct{ name string }
	want := &repo{name: "users"}
	ctx := &EndpointContext{Injected: map[string]any{"repo": want}}

	got, err := InjectedAs[*repo](ctx, "repo")
	if err != nil {
		t.Fatalf("InjectedAs returned error: %v", err)
	}
	if got != want {
		t.Errorf("InjectedAs returned %v, want %v", got, want)
	}
}

func TestInjectedAs_NotFound(t *testing.T) {
	ctx := &EndpointContext{Injected: map[string]any{}}
	_, err := InjectedAs[int](ctx, "missing")
	if err == nil {
		t.Fatal("expected error for missing dependency, got nil")
	}
}

func TestInjectedAs_WrongType(t *testing.T) {
	ctx := &EndpointContext{Injected: map[string]any{"n": "not-an-int"}}
	_, err := InjectedAs[int](ctx, "n")
	if err == nil {
		t.Fatal("expected error for type mismatch, got nil")
	}
}
