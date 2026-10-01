package config

import (
	"slices"
	"testing"
)

type sensitiveTestOptions struct {
	Host     string `json:"host"`
	Password string `json:"password" sensitive:"true"`
	Token    string `sensitive:"true"` // no json tag — falls back to Go name
	Plain    string
}

func TestSensitiveFields(t *testing.T) {
	def := Config[sensitiveTestOptions]("database")
	got := SensitiveFields(def)

	wantIncludes := []string{"password", "Token"}
	for _, w := range wantIncludes {
		if !slices.Contains(got, w) {
			t.Errorf("SensitiveFields missing %q; got %v", w, got)
		}
	}
	if slices.Contains(got, "host") {
		t.Errorf("SensitiveFields should not include non-sensitive field 'host'; got %v", got)
	}
	if slices.Contains(got, "Plain") {
		t.Errorf("SensitiveFields should not include non-tagged field 'Plain'; got %v", got)
	}
}

func TestIsSensitiveField(t *testing.T) {
	def := Config[sensitiveTestOptions]("database")
	if !IsSensitiveField(def, "password") {
		t.Error("IsSensitiveField(password) should be true")
	}
	if IsSensitiveField(def, "host") {
		t.Error("IsSensitiveField(host) should be false")
	}
	if IsSensitiveField(def, "absent") {
		t.Error("IsSensitiveField(absent) should be false")
	}
}

func TestSensitiveFields_NonStruct(t *testing.T) {
	def := Config[string]("just.a.string")
	if got := SensitiveFields(def); got != nil {
		t.Errorf("expected nil for non-struct T, got %v", got)
	}
}

func TestSensitiveFields_PointerType(t *testing.T) {
	// Config[*T] must behave the same as Config[T] — silent empty result
	// hides sensitive fields from log redactors.
	def := Config[*sensitiveTestOptions]("database")
	got := SensitiveFields(def)

	for _, w := range []string{"password", "Token"} {
		if !slices.Contains(got, w) {
			t.Errorf("SensitiveFields(*T) missing %q; got %v", w, got)
		}
	}
}

type EmbeddedBase struct {
	APIKey string `json:"apiKey" sensitive:"true"`
	Region string `json:"region"`
}

type withEmbedded struct {
	EmbeddedBase
	Endpoint string `json:"endpoint"`
	Token    string `json:"token" sensitive:"true"`
}

func TestSensitiveFields_AnonymousEmbedded(t *testing.T) {
	def := Config[withEmbedded]("provider")
	got := SensitiveFields(def)

	for _, w := range []string{"apiKey", "token"} {
		if !slices.Contains(got, w) {
			t.Errorf("SensitiveFields missing embedded sensitive %q; got %v", w, got)
		}
	}
	for _, w := range []string{"region", "endpoint"} {
		if slices.Contains(got, w) {
			t.Errorf("SensitiveFields should not include non-sensitive %q; got %v", w, got)
		}
	}
}

type withEmbeddedPtr struct {
	*EmbeddedBase
	Endpoint string `json:"endpoint"`
	Token    string `json:"token" sensitive:"true"`
}

func TestSensitiveFields_AnonymousEmbeddedPointer(t *testing.T) {
	// `*EmbeddedBase` is a common Go embedding shape; the helper must
	// dereference the pointer type to discover sensitive subfields.
	def := Config[withEmbeddedPtr]("provider")
	got := SensitiveFields(def)

	for _, w := range []string{"apiKey", "token"} {
		if !slices.Contains(got, w) {
			t.Errorf("SensitiveFields missing %q from embedded *Base; got %v", w, got)
		}
	}
}
