package configextract

import (
	"os"
	"reflect"
	"testing"

	protocfg "go.putnami.dev/protocol/config"
	"go.putnami.dev/protocol/infra"
)

func sidecarPath(projectPath string) string {
	return infra.SidecarPath(projectPath, sidecarSlug)
}

func readSidecar(t *testing.T, projectPath string) *infra.PerProjectManifest {
	t.Helper()
	m, diags := infra.LoadPerProjectManifest(sidecarPath(projectPath))
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("sidecar failed validation: %s", d.String())
		}
	}
	return m
}

func TestCollectSecretNames_ZeroSecrets(t *testing.T) {
	blocks := []protocfg.Block{
		{Path: "server", Fields: []protocfg.FieldSchema{
			{Name: "host", Type: protocfg.FieldTypeString},
			{Name: "port", Type: protocfg.FieldTypeInt},
		}},
	}
	if got := collectSecretNames(blocks); len(got) != 0 {
		t.Fatalf("expected no secrets, got %v", got)
	}
}

func TestCollectSecretNames_OneSecret_UsesEnvBinding(t *testing.T) {
	blocks := []protocfg.Block{
		{Path: "database", Fields: []protocfg.FieldSchema{
			{Name: "host", Type: protocfg.FieldTypeString},
			{Name: "password", Type: protocfg.FieldTypeString, Env: "DB_PASSWORD", Sensitive: true},
		}},
	}
	want := []string{"db_password"}
	if got := collectSecretNames(blocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCollectSecretNames_MultipleSecrets_SortedAndDeduped(t *testing.T) {
	blocks := []protocfg.Block{
		{Path: "stripe", Fields: []protocfg.FieldSchema{
			{Name: "webhookSecret", Type: protocfg.FieldTypeString, Sensitive: true},
		}},
		{Path: "auth", Fields: []protocfg.FieldSchema{
			{Name: "token", Type: protocfg.FieldTypeString, Env: "SHARED_TOKEN", Sensitive: true},
		}},
		{Path: "cache", Fields: []protocfg.FieldSchema{
			// Same env binding as auth.token → must dedupe to one entry.
			{Name: "token", Type: protocfg.FieldTypeString, Env: "SHARED_TOKEN", Sensitive: true},
		}},
	}
	want := []string{"shared_token", "stripe.webhook_secret"}
	if got := collectSecretNames(blocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCollectSecretNames_Nested(t *testing.T) {
	blocks := []protocfg.Block{
		{Path: "oauth", Fields: []protocfg.FieldSchema{
			{Name: "providers", Type: protocfg.FieldTypeObject, Fields: []protocfg.FieldSchema{
				{Name: "clientSecret", Type: protocfg.FieldTypeString, Sensitive: true},
			}},
			{Name: "keys", Type: protocfg.FieldTypeArray, Items: &protocfg.FieldSchema{
				Type: protocfg.FieldTypeString, Sensitive: true,
			}},
		}},
	}
	want := []string{"oauth.keys", "oauth.providers.client_secret"}
	if got := collectSecretNames(blocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCanonicalizeName_NeedsCanonicalization(t *testing.T) {
	// Output is snake_case so it matches the TypeScript runtime's
	// canonicalSecretName — same source field in either language produces
	// the same canonical name in the aggregated manifest.
	cases := map[string]string{
		"DB_PASSWORD":           "db_password",
		"clientSecret":          "client_secret",
		"auth.clientSecret":     "auth.client_secret",
		"server.http.apiKey":    "server.http.api_key",
		"Weird Name!":           "weird_name_",
		"_leadingUnderscore":    "leading_underscore",
		"STRIPE_WEBHOOK_SECRET": "stripe_webhook_secret",
	}
	for in, want := range cases {
		if got := canonicalizeName(in); got != want {
			t.Errorf("canonicalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteInfraRequirements_EmitsManifest(t *testing.T) {
	dir := t.TempDir()
	blocks := []protocfg.Block{
		{Path: "database", Fields: []protocfg.FieldSchema{
			{Name: "password", Type: protocfg.FieldTypeString, Env: "DB_PASSWORD", Sensitive: true},
		}},
	}
	if err := writeInfraRequirements(dir, blocks); err != nil {
		t.Fatal(err)
	}

	m := readSidecar(t, dir)
	if m.ProtocolVersion != infra.ProtocolVersion {
		t.Fatalf("protocolVersion = %d, want %d", m.ProtocolVersion, infra.ProtocolVersion)
	}
	if !reflect.DeepEqual(m.Secrets, []string{"db_password"}) {
		t.Fatalf("secrets = %v, want [db_password]", m.Secrets)
	}
}

func TestWriteInfraRequirements_NoSecretsEmitsNoFile(t *testing.T) {
	dir := t.TempDir()
	blocks := []protocfg.Block{
		{Path: "server", Fields: []protocfg.FieldSchema{
			{Name: "host", Type: protocfg.FieldTypeString},
		}},
	}
	if err := writeInfraRequirements(dir, blocks); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidecarPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar file, stat err = %v", err)
	}
}

// TestWriteInfraRequirements_SourceToAggregatedManifest exercises the path the
// build-generate pipeline takes: extract the schema from real Go source, write
// the per-project scratch fragment, then merge it the way the generator and
// workload aggregator do and assert the declared secret survives into the
// aggregated manifest.
func TestWriteInfraRequirements_SourceToAggregatedManifest(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod": "module example.com/svc\n",
		"config.go": `package svc

import "go.putnami.dev/config"

type Options struct {
	Host     string ` + "`json:\"host\"`" + `
	Password string ` + "`json:\"password\" env:\"DB_PASSWORD\" sensitive:\"true\"`" + `
}

var DatabaseConfig = config.Config[Options]("database")
`,
	})

	configs, err := extractFromProject(dir)
	if err != nil {
		t.Fatalf("extractFromProject: %v", err)
	}
	if err := writeInfraRequirements(dir, configs); err != nil {
		t.Fatalf("writeInfraRequirements: %v", err)
	}

	m := readSidecar(t, dir)
	if !reflect.DeepEqual(m.Secrets, []string{"db_password"}) {
		t.Fatalf("sidecar secrets = %v, want [db_password]", m.Secrets)
	}

	agg, diags := infra.Merge("svc", []infra.ProjectContribution{{
		Project:     "svc",
		Contributor: infra.FrameworkContributor("generated"),
		Manifest:    *m,
	}})
	for _, d := range diags {
		if d.Severity == "error" {
			t.Fatalf("merge error: %s", d.String())
		}
	}

	got := make([]string, 0, len(agg.Secrets))
	for _, s := range agg.Secrets {
		got = append(got, s.Name)
	}
	if !reflect.DeepEqual(got, []string{"db_password"}) {
		t.Fatalf("aggregated secrets = %v, want [db_password]", got)
	}
}

func TestWriteInfraRequirements_RemovesStaleManifest(t *testing.T) {
	dir := t.TempDir()
	withSecret := []protocfg.Block{
		{Path: "database", Fields: []protocfg.FieldSchema{
			{Name: "password", Type: protocfg.FieldTypeString, Sensitive: true},
		}},
	}
	if err := writeInfraRequirements(dir, withSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidecarPath(dir)); err != nil {
		t.Fatalf("expected sidecar to exist: %v", err)
	}

	// Re-run after the sensitive field is removed: the stale sidecar must go.
	if err := writeInfraRequirements(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidecarPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("expected stale sidecar to be removed, stat err = %v", err)
	}
}
