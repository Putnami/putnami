package configcli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/config/authoredmember"
	"go.putnami.dev/protocol/features/spectest"
)

// signInProvidersWorkspace is a project whose signInProviders.oidc map holds
// objects with a sensitive clientSecret, as a Go map[string]Provider field
// extracts, and whose committed file holds the given oidc entries.
func signInProvidersWorkspace(t *testing.T, secretRequired bool, entries string) string {
	t.Helper()
	root := authoredConfigWorkspace(t)
	writeJSONFile(t, filepath.Join(root, "schema", "config.json"), map[string]any{
		"appName": "my-app",
		"configs": []any{map[string]any{"path": "signInProviders", "fields": []any{
			map[string]any{"name": "enabled", "type": "string", "default": "github"},
			map[string]any{"name": "oidc", "type": "map", "keys": "string", "values": map[string]any{"name": "", "type": "object", "fields": []any{
				map[string]any{"name": "label", "type": "string"},
				map[string]any{"name": "issuer", "type": "string", "required": true},
				map[string]any{"name": "clientId", "type": "string"},
				map[string]any{"name": "clientSecret", "type": "string", "sensitive": true, "required": secretRequired},
				map[string]any{"name": "scopes", "type": "string"},
			}}},
		}}},
	})
	if err := os.WriteFile(filepath.Join(root, "conf", "env.prod.yaml"), []byte("signInProviders:\n  oidc:\n"+entries), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

const gitlabEntryWithoutSecret = "    gitlab:\n      label: GitLab\n      issuer: https://gitlab.com\n      clientId: gitlab-client\n      scopes: openid email\n"

func prepareSignInProviders(t *testing.T, root string) (*PreparedAuthoredConfig, error) {
	t.Helper()
	stubAuthoredGit(t)
	return PrepareAuthoredConfig(map[string]any{"app": "my-app", "namespace": "workspace-native", "env": "prod"}, nil, root, clicore.IO{})
}

func TestPrepareAuthoredConfigBuildsAMapEntryWithoutItsSensitiveField(t *testing.T) {
	spectest.Proves(t, "config/configuration-contract", "authored-member", "the-cloud-cli-member-passes-validate")
	for _, required := range []bool{false, true} {
		root := signInProvidersWorkspace(t, required, gitlabEntryWithoutSecret)
		prepared, err := prepareSignInProviders(t, root)
		if err != nil {
			t.Fatalf("required=%v: %v", required, err)
		}
		if _, err := (authoredmember.Publisher{}).Validate(t.Context(), prepared.Member); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			`"signInProviders.oidc.gitlab.clientId":"gitlab-client"`,
			`"signInProviders.oidc.gitlab.issuer":"https://gitlab.com"`,
			`"signInProviders.oidc.gitlab.label":"GitLab"`,
			`"signInProviders.oidc.gitlab.scopes":"openid email"`,
			`{"path":"signInProviders.oidc.gitlab.clientSecret","reference":"config-secret://signInProviders/oidc/gitlab/clientSecret"}`,
		} {
			if !bytes.Contains(prepared.Member.Bytes, []byte(want)) {
				t.Fatalf("member lacks %s: %s", want, prepared.Member.Bytes)
			}
		}
		field := schemaFieldOf(t, prepared.Member, "signInProviders.oidc.gitlab.clientSecret")
		if !field.Sensitive || field.Required != required {
			t.Fatalf("required=%v: clientSecret field = %+v", required, field)
		}
	}
}

// The member's reference names the key `putnami cloud config secrets set`
// stores, so a secret set at that key resolves for the entry.
func TestAMapEntrySecretReferenceResolvesTheStoredSecret(t *testing.T) {
	spectest.Proves(t, "config/configuration-contract", "authored-member", "the-cloud-cli-member-passes-validate")
	prepared, err := prepareSignInProviders(t, signInProvidersWorkspace(t, false, gitlabEntryWithoutSecret))
	if err != nil {
		t.Fatal(err)
	}
	reference := secretReferenceOf(t, prepared.Member, "signInProviders.oidc.gitlab.clientSecret")
	key, err := authoredmember.SecretReferencePath(reference.Reference)
	if err != nil || key != "signInProviders.oidc.gitlab.clientSecret" {
		t.Fatalf("reference %q selects %q, %v", reference.Reference, key, err)
	}
	block, field, err := splitSecretKey(key)
	if err != nil {
		t.Fatal(err)
	}
	// The secrets plane holds what `config secrets set <key>` wrote: the
	// value at field under block.
	store := map[string]any{"signInProviders": map[string]any{"oidc": map[string]any{"gitlab": map[string]any{"clientSecret": "gitlab-secret"}}}}
	if value, ok := pluckField(store, block, field); !ok || value != "gitlab-secret" {
		t.Fatalf("stored secret at %s.%s = %v, %v", block, field, value, ok)
	}
}

func TestPrepareAuthoredConfigRefusesAMapEntryWithoutARequiredValue(t *testing.T) {
	root := signInProvidersWorkspace(t, false, "    gitlab:\n      label: GitLab\n      clientId: gitlab-client\n")
	if _, err := prepareSignInProviders(t, root); err == nil || !strings.Contains(err.Error(), "required authored value is missing") {
		t.Fatalf("missing issuer error = %v", err)
	}
}

func TestPrepareAuthoredConfigRefusesANonCanonicalMapEntry(t *testing.T) {
	for entries, want := range map[string]string{
		"    \"git.lab\":\n      issuer: https://gitlab.com\n": "not one canonical path segment",
		"    gitlab: https://gitlab.com\n":                     "rejected by schema validation",
	} {
		_, err := prepareSignInProviders(t, signInProvidersWorkspace(t, false, entries))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("non-canonical entry %q error = %v, want %q", entries, err, want)
		}
	}
}

// A map whose values declare no sensitive field stays one opaque value, so a
// project keeps declaring its entries in schema/config-authored-fields.json.
// A secret reached only through a nested map does not expand the outer one.
func TestPrepareAuthoredConfigKeepsAMapWithoutSensitiveValuesOpaque(t *testing.T) {
	root := authoredConfigWorkspace(t)
	writeJSONFile(t, filepath.Join(root, "schema", "config.json"), map[string]any{
		"appName": "my-app",
		"configs": []any{map[string]any{"path": "clients", "fields": []any{
			map[string]any{"name": "services", "type": "map", "keys": "string", "values": map[string]any{"name": "", "type": "object", "fields": []any{
				map[string]any{"name": "url", "type": "string"},
				map[string]any{"name": "credentials", "type": "map", "keys": "string", "values": map[string]any{"name": "", "type": "object", "fields": []any{
					map[string]any{"name": "audience", "type": "string"},
					map[string]any{"name": "clientSecret", "type": "string", "sensitive": true},
				}}},
			}}},
		}}},
	})
	writeJSONFile(t, filepath.Join(root, "schema", "config-authored-fields.json"), map[string]any{"fields": []any{
		map[string]any{"path": "clients.services.billing", "type": "object"},
		map[string]any{"path": "clients.services.billing.url", "type": "string"},
	}})
	if err := os.WriteFile(filepath.Join(root, "conf", "env.prod.yaml"), []byte("clients:\n  services:\n    billing:\n      url: https://billing.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubAuthoredGit(t)
	prepared, err := PrepareAuthoredConfig(map[string]any{"app": "my-app", "namespace": "workspace-native", "env": "prod"}, nil, root, clicore.IO{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prepared.Member.Bytes, []byte(`"clients.services":{"billing":{"url":"https://billing.example"}}`)) {
		t.Fatalf("opaque map value changed: %s", prepared.Member.Bytes)
	}
}

type memberShape struct {
	Schema           authoredmember.NormalizedSchema  `json:"schema"`
	SecretReferences []authoredmember.SecretReference `json:"secretReferences"`
}

func decodeMemberShape(t *testing.T, member authoredmember.AuthoredMember) memberShape {
	t.Helper()
	var shape memberShape
	if err := json.Unmarshal(member.Bytes, &shape); err != nil {
		t.Fatal(err)
	}
	return shape
}

func schemaFieldOf(t *testing.T, member authoredmember.AuthoredMember, path string) authoredmember.SchemaField {
	t.Helper()
	for _, field := range decodeMemberShape(t, member).Schema.Fields {
		if field.Path == path {
			return field
		}
	}
	t.Fatalf("member declares no field %s", path)
	return authoredmember.SchemaField{}
}

func secretReferenceOf(t *testing.T, member authoredmember.AuthoredMember, path string) authoredmember.SecretReference {
	t.Helper()
	for _, reference := range decodeMemberShape(t, member).SecretReferences {
		if reference.Path == path {
			return reference
		}
	}
	t.Fatalf("member references no secret at %s", path)
	return authoredmember.SecretReference{}
}

// A project that declared the entries of a now expanded map in
// schema/config-authored-fields.json keeps building: the declarations the
// expansion already holds change nothing.
func TestPrepareAuthoredConfigAcceptsRedundantEntryDeclarations(t *testing.T) {
	root := signInProvidersWorkspace(t, false, gitlabEntryWithoutSecret)
	writeJSONFile(t, filepath.Join(root, "schema", "config-authored-fields.json"), map[string]any{"fields": []any{
		map[string]any{"path": "signInProviders.oidc.gitlab", "type": "object"},
		map[string]any{"path": "signInProviders.oidc.gitlab.issuer", "type": "string"},
		map[string]any{"path": "signInProviders.oidc.gitlab.label", "type": "string"},
	}})
	if _, err := prepareSignInProviders(t, root); err != nil {
		t.Fatalf("redundant entry declarations: %v", err)
	}
	writeJSONFile(t, filepath.Join(root, "schema", "config-authored-fields.json"), map[string]any{"fields": []any{
		map[string]any{"path": "signInProviders.oidc.gitlab.clientSecret", "type": "string"},
	}})
	if _, err := prepareSignInProviders(t, root); err == nil {
		t.Fatal("a non-sensitive declaration at a sensitive entry path was accepted")
	}
}

// An expanded entry keeps the array rule of a fixed object: an array whose
// items declare a secret cannot be published, even when the entry omits it.
func TestPrepareAuthoredConfigRefusesAnExpandedEntryWithASensitiveArray(t *testing.T) {
	root := signInProvidersWorkspace(t, false, gitlabEntryWithoutSecret)
	writeJSONFile(t, filepath.Join(root, "schema", "config.json"), map[string]any{
		"appName": "my-app",
		"configs": []any{map[string]any{"path": "signInProviders", "fields": []any{
			map[string]any{"name": "oidc", "type": "map", "keys": "string", "values": map[string]any{"name": "", "type": "object", "fields": []any{
				map[string]any{"name": "issuer", "type": "string"},
				map[string]any{"name": "clientSecret", "type": "string", "sensitive": true},
				map[string]any{"name": "extra", "type": "array", "items": map[string]any{"fields": []any{
					map[string]any{"name": "token", "type": "string", "sensitive": true, "required": true},
				}}},
			}}},
		}}},
	})
	if err := os.WriteFile(filepath.Join(root, "conf", "env.prod.yaml"), []byte("signInProviders:\n  oidc:\n    gitlab:\n      issuer: https://gitlab.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSignInProviders(t, root); err == nil || !strings.Contains(err.Error(), "sensitive descendants") {
		t.Fatalf("sensitive array in an entry error = %v", err)
	}
}
