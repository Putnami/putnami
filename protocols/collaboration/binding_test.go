package collaboration

import (
	"slices"
	"strings"
	"testing"
)

func TestParseBindings_KeepsEveryContractIndependent(t *testing.T) {
	bindings, diags := ParseBindings([]byte(`{
		"tasks": {"provider": "@acme/github", "version": 1},
		"proposals": {"provider": "@putnami/local-collaboration", "version": 1, "settings": {"repository": "local"}}
	}`))
	if diags != nil {
		t.Fatalf("diagnostics: %v", diags)
	}
	if got := bindings.Contracts(); !slices.Equal(got, []string{ContractProposals, ContractTasks}) {
		t.Fatalf("contracts = %v", got)
	}
	if bindings[ContractTasks].Provider == bindings[ContractProposals].Provider {
		t.Error("two contracts bound to two providers collapsed into one")
	}
	if _, bound := bindings[ContractMemory]; bound {
		t.Error("an unbound contract must stay unbound")
	}
}

// TestParseBindings_ReportsTheValidSubset: one malformed contract binding does
// not unbind the others, and the error names the contract.
func TestParseBindings_ReportsTheValidSubset(t *testing.T) {
	bindings, diags := ParseBindings([]byte(`{
		"tasks": {"provider": "@acme/github", "version": 1},
		"memory": {"provider": "@acme/memory", "version": 9}
	}`))
	if got := distinctCodes(diags); !slices.Equal(got, []string{ErrorCodeUnsupportedVersion}) {
		t.Fatalf("codes = %v", got)
	}
	if diags[0].Field != "memory.version" {
		t.Errorf("field = %q", diags[0].Field)
	}
	if _, ok := bindings[ContractTasks]; !ok {
		t.Error("the valid tasks binding was dropped")
	}
	if _, ok := bindings[ContractMemory]; ok {
		t.Error("the invalid memory binding was kept")
	}
}

func TestParseBindings_Refusals(t *testing.T) {
	cases := []struct {
		name string
		data string
		code string
	}{
		{"not an object", `[]`, ErrorCodeParseError},
		{"not JSON", `{`, ErrorCodeParseError},
		{"duplicate provider member", `{"tasks": {"provider": "a", "provider": "b", "version": 1}}`, ErrorCodeDuplicateMember},
		{"padded provider", `{"tasks": {"provider": " a", "version": 1}}`, ErrorCodeInvalidValue},
		{"versionless", `{"tasks": {"provider": "a"}}`, ErrorCodeRequired},
		{"require of a required operation", `{"tasks": {"provider": "a", "version": 1, "require": ["find"]}}`, ErrorCodeInvalidValue},
		{"require repeated", `{"tasks": {"provider": "a", "version": 1, "require": ["claim", "claim"]}}`, ErrorCodeInvalidValue},
		{"settings not an object", `{"tasks": {"provider": "a", "version": 1, "settings": [1]}}`, ErrorCodeInvalidValue},
		{"credential in a list", `{"tasks": {"provider": "a", "version": 1, "settings": {"hosts": [{"password": "x"}]}}}`, ErrorCodeCredentialSetting},
		{"oversized settings", `{"tasks": {"provider": "a", "version": 1, "settings": {"x": "` + strings.Repeat("a", MaxSettingsBytes) + `"}}}`, ErrorCodeTooLarge},
		{"oversized block", `{"x": "` + strings.Repeat("a", MaxDocumentBytes) + `"}`, ErrorCodeTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ParseBindings([]byte(tc.data))
			if got := distinctCodes(diags); !slices.Equal(got, []string{tc.code}) {
				t.Fatalf("codes = %v, want %s (%v)", got, tc.code, diags)
			}
		})
	}
}

// A setting value that is a URL carrying a credential is refused wherever it
// sits; a URL naming an SSH account, a scp-like remote or a path is not a
// credential.
func TestParseBindings_RefusesCredentialsInURLValues(t *testing.T) {
	binding := func(settings string) string {
		return `{"memory": {"provider": "@acme/memory", "version": 1, "settings": ` + settings + `}}`
	}
	for name, settings := range map[string]string{
		"a token in the user slot": `{"remote": "https://ghp_example@github.com/acme/memory.git"}`,
		"a user and password":      `{"remote": "https://x-access-token:ghp_example@github.com/acme/memory.git"}`,
		"a password under ssh":     `{"remote": "ssh://git:secret@example.com/memory.git"}`,
		"a nested list value":      `{"mirrors": [{"url": "http://user@example.com/m.git"}]}`,
		"an uppercase http scheme": `{"remote": "HTTPS://ghp_example@github.com/acme/memory.git"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, diags := ParseBindings([]byte(binding(settings)))
			if got := distinctCodes(diags); !slices.Equal(got, []string{ErrorCodeCredentialSetting}) {
				t.Fatalf("codes = %v (%v)", got, diags)
			}
			if !strings.HasPrefix(diags[0].Field, "memory.settings.") {
				t.Errorf("field = %q, want the setting's path", diags[0].Field)
			}
		})
	}
	for name, settings := range map[string]string{
		"an SSH account":     `{"remote": "ssh://git@github.com/acme/memory.git"}`,
		"a scp-like remote":  `{"remote": "git@github.com:acme/memory.git"}`,
		"an anonymous https": `{"remote": "https://github.com/acme/memory.git"}`,
		"a path":             `{"path": "../memory.git"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, diags := ParseBindings([]byte(binding(settings))); diags != nil {
				t.Fatalf("diagnostics: %v", diags)
			}
		})
	}
}

func TestCredentialNamesAreRecognizedAcrossSpellings(t *testing.T) {
	for _, name := range []string{"token", "apiToken", "API_KEY", "api-key", "clientSecret", "Password", "privateKey", "Authorization", "bearer", "sessionCookie"} {
		if !IsCredentialName(name) {
			t.Errorf("%q is not recognized as a credential", name)
		}
	}
	for _, name := range []string{"repository", "root", "labels", "branch", "remote", "owner"} {
		if IsCredentialName(name) {
			t.Errorf("%q is mistaken for a credential", name)
		}
	}
}
