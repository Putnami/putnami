package configcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/config/authoredmember"
)

func TestAuthoredArraySchemaRetainsNestedObjectNodes(t *testing.T) {
	blocks := []schemaBlock{{Path: "server", Fields: []schemaField{{Name: "callers", Type: "array", Items: []schemaField{
		{Name: "scope", Type: "object", Required: true, Fields: []schemaField{{Name: "workspaceId", Type: "string", Required: true}}},
	}}}}}
	schema, refs, err := normalizeAuthoredSchema(blocks)
	if err != nil {
		t.Fatal(err)
	}
	request := authoredmember.BuildAuthoredMemberRequest{
		WorkspaceID: "workspace", Project: "project", Environment: "prod", Schema: schema, SecretReferences: refs,
		SourceProvenance: authoredmember.SourceProvenance{Repository: "github.com/acme/project", Revision: authoredTestRevision},
		AuthoredLayers: []authoredmember.AuthoredLayer{{Name: "application", Pin: authoredTestRevision, Values: map[string]any{
			"server.callers": []any{map[string]any{"scope": map[string]any{"workspaceId": "workspace"}}},
		}}},
	}
	member, err := (authoredmember.Publisher{}).Build(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (authoredmember.Publisher{}).Validate(t.Context(), member); err != nil {
		t.Fatal(err)
	}
}

func TestAuthoredFieldsCloseOpaqueObjectsWithoutInferringValues(t *testing.T) {
	root := authoredConfigWorkspace(t)
	stubAuthoredGit(t)
	writeJSONFile(t, filepath.Join(root, "schema", "config.json"), map[string]any{
		"appName": "my-app", "configs": []any{map[string]any{
			"path": "server", "fields": []any{map[string]any{"name": "provider", "type": "object"}},
		}},
	})
	if err := os.WriteFile(filepath.Join(root, "conf", "env.prod.yaml"), []byte("server:\n  provider:\n    Issuer: https://issuer.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"app": "my-app", "namespace": "workspace-native", "env": "prod"}
	if _, err := PrepareAuthoredConfig(params, nil, root, clicore.IO{}); err == nil {
		t.Fatal("opaque values inferred their own schema")
	}
	writeJSONFile(t, filepath.Join(root, "schema", "config-authored-fields.json"), authoredmember.NormalizedSchema{
		Fields: []authoredmember.SchemaField{{Path: "server.provider.Issuer", Type: "string"}},
	})
	member, err := PrepareAuthoredConfig(params, nil, root, clicore.IO{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (authoredmember.Publisher{}).Validate(t.Context(), member.Member); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "conf", "env.prod.yaml"), []byte("server:\n  provider:\n    Issuer: https://issuer.example\n    undeclared: value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareAuthoredConfig(params, nil, root, clicore.IO{}); err == nil {
		t.Fatal("undeclared descendant accepted")
	}
}

func TestAuthoredFieldsCannotOverrideOrExtendClosedSensitiveObjects(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "schema"), 0o700); err != nil {
		t.Fatal(err)
	}
	schema := authoredmember.NormalizedSchema{Fields: []authoredmember.SchemaField{
		{Path: "opaque", Type: "object"}, {Path: "secret", Type: "object", Sensitive: true},
		{Path: "closed", Type: "object"}, {Path: "closed.name", Type: "string"},
	}}
	for _, content := range []string{
		`{"fields":[{"path":"opaque","type":"string"}]}`,
		`{"fields":[{"path":"secret.child","type":"string"}]}`,
		`{"fields":[{"path":"closed.extra","type":"string"}]}`,
		`{"fields":[{"path":"unknown.child","type":"string"}]}`,
		`{"fields":[{"path":"opaque.child","type":"string"},{"path":"opaque.child","type":"string"}]}`,
		`{"fields":[],"unknown":true}`, `{"fields":[]} {}`,
	} {
		if err := os.WriteFile(filepath.Join(root, "schema", "config-authored-fields.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := extendAuthoredSchema(root, schema); err == nil {
			t.Fatalf("unsafe field declarations accepted: %s", content)
		}
	}
}

// Every secret becomes a reference, so a prepared boot binding delivers it
// when it is set. The schema field marks it required only when the
// boot needs its value. The Go and TypeScript boots both apply a default and
// never need a block the schema marks optional, so neither case is a required
// reference that Config would refuse to prepare without.
func TestAuthoredSchemaReferencesEverySecretAndRequiresOnlyTheOnesTheBootNeeds(t *testing.T) {
	blocks := []schemaBlock{
		{Path: "session", Fields: []schemaField{
			{Name: "cookieSecret", Type: "string", Required: true, Sensitive: true},
			{Name: "cookieSalt", Type: "string", Required: true, Sensitive: true},
			{Name: "signingKey", Type: "string", Required: true, Sensitive: true, Default: "local-only"},
			{Name: "previousKey", Type: "string", Sensitive: true},
		}},
		{Path: "oauth", Optional: true, Fields: []schemaField{
			{Name: "clientSecret", Type: "string", Required: true, Sensitive: true},
			{Name: "provider", Type: "object", Fields: []schemaField{{Name: "key", Type: "string", Required: true, Sensitive: true}}},
		}},
	}
	schema, refs, err := normalizeAuthoredSchema(blocks)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		paths = append(paths, ref.Path+"="+ref.Reference)
	}
	if got := strings.Join(paths, ","); got != "session.cookieSecret=config-secret://session/cookieSecret,"+
		"session.cookieSalt=config-secret://session/cookieSalt,"+
		"session.signingKey=config-secret://session/signingKey,"+
		"session.previousKey=config-secret://session/previousKey,"+
		"oauth.clientSecret=config-secret://oauth/clientSecret,"+
		"oauth.provider.key=config-secret://oauth/provider/key" {
		t.Fatalf("references = %s, want one per secret", got)
	}
	required := map[string]bool{}
	for _, field := range schema.Fields {
		if field.Sensitive {
			required[field.Path] = field.Required
		}
	}
	for path, want := range map[string]bool{
		"session.cookieSecret": true, "session.cookieSalt": true, "session.signingKey": false,
		"session.previousKey": false, "oauth.clientSecret": false, "oauth.provider.key": false,
	} {
		if got, ok := required[path]; !ok || got != want {
			t.Fatalf("%s required = %v (declared %v), want %v", path, got, ok, want)
		}
	}
	member, err := (authoredmember.Publisher{}).Build(t.Context(), authoredmember.BuildAuthoredMemberRequest{
		WorkspaceID: "workspace", Project: "project", Environment: "prod", Schema: schema, SecretReferences: refs,
		SourceProvenance: authoredmember.SourceProvenance{Repository: "github.com/acme/project", Revision: authoredTestRevision},
		AuthoredLayers:   []authoredmember.AuthoredLayer{{Name: "application", Pin: authoredTestRevision, Values: map[string]any{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (authoredmember.Publisher{}).Validate(t.Context(), member); err != nil {
		t.Fatal(err)
	}
}
