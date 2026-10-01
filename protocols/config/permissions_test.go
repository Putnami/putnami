package config

import "testing"

func TestAllScopes(t *testing.T) {
	scopes := AllScopes()
	if len(scopes) != 6 {
		t.Fatalf("expected 6 scopes, got %d", len(scopes))
	}

	expected := map[string]bool{
		ScopeSchemaRead:  true,
		ScopeSchemaWrite: true,
		ScopeValueRead:   true,
		ScopeValueWrite:  true,
		ScopeSecretRead:  true,
		ScopeSecretWrite: true,
	}

	for _, s := range scopes {
		if !expected[s] {
			t.Errorf("unexpected scope: %q", s)
		}
	}
}

func TestScopeConstants(t *testing.T) {
	tests := []struct {
		name  string
		scope string
		want  string
	}{
		{"schema read", ScopeSchemaRead, "config.schema.read"},
		{"schema write", ScopeSchemaWrite, "config.schema.write"},
		{"value read", ScopeValueRead, "config.value.read"},
		{"value write", ScopeValueWrite, "config.value.write"},
		{"secret read", ScopeSecretRead, "config.secret.read"},
		{"secret write", ScopeSecretWrite, "config.secret.write"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.scope != tt.want {
				t.Errorf("scope = %q, want %q", tt.scope, tt.want)
			}
		})
	}
}
