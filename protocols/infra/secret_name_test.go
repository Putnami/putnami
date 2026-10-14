package infra

import (
	"strings"
	"testing"
)

func TestSecretName(t *testing.T) {
	cases := map[string]string{
		"DB_PASSWORD":           "db_password",
		"clientSecret":          "client_secret",
		"auth.clientSecret":     "auth.client_secret",
		"crm.apiKey":            "crm.api_key",
		"server.http.apiKey":    "server.http.api_key",
		"Weird Name!":           "weird_name_",
		"_leadingUnderscore":    "leading_underscore",
		"STRIPE_WEBHOOK_SECRET": "stripe_webhook_secret",
		strings.Repeat("a", 65): strings.Repeat("a", 64),
	}
	for in, want := range cases {
		got := SecretName(in)
		if got != want {
			t.Errorf("SecretName(%q) = %q, want %q", in, got, want)
		}
		if diags := validateName("secrets[0]", got); len(diags) != 0 {
			t.Errorf("SecretName(%q) = %q violates the resource-name grammar: %v", in, got, diags)
		}
	}
}
