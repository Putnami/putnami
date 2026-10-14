package configextract

import (
	"os"
	"reflect"
	"strings"
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

func TestCollectSecretNames_OneSecret_UsesConfigKeyNotEnvBinding(t *testing.T) {
	blocks := []protocfg.Block{
		{Path: "database", Fields: []protocfg.FieldSchema{
			{Name: "host", Type: protocfg.FieldTypeString},
			{Name: "password", Type: protocfg.FieldTypeString, Env: "DB_PASSWORD", Sensitive: true},
		}},
	}
	want := []string{"database.password"}
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
			// Same env binding as auth.token, but another config key.
			{Name: "token", Type: protocfg.FieldTypeString, Env: "SHARED_TOKEN", Sensitive: true},
		}},
		{Path: "stripe", Fields: []protocfg.FieldSchema{
			// Same config key as the first stripe block → one entry.
			{Name: "webhookSecret", Type: protocfg.FieldTypeString, Sensitive: true},
		}},
	}
	want := []string{"auth.token", "cache.token", "stripe.webhookSecret"}
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
	want := []string{"oauth.keys", "oauth.providers.clientSecret"}
	if got := collectSecretNames(blocks); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
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
	if !reflect.DeepEqual(m.Secrets, []string{"database.password"}) {
		t.Fatalf("secrets = %v, want [database.password]", m.Secrets)
	}
}

func TestWriteInfraRequirements_RefusesAnInvalidSecretName(t *testing.T) {
	dir := t.TempDir()
	blocks := []protocfg.Block{
		{Path: "billing", Fields: []protocfg.FieldSchema{
			{Name: "api key", Type: protocfg.FieldTypeString, Sensitive: true},
		}},
	}
	err := writeInfraRequirements(dir, blocks)
	if err == nil || !strings.Contains(err.Error(), "billing.api key") {
		t.Fatalf("err = %v, want an error naming billing.api key", err)
	}
	if _, statErr := os.Stat(sidecarPath(dir)); !os.IsNotExist(statErr) {
		t.Fatalf("expected no sidecar file, stat err = %v", statErr)
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
	if !reflect.DeepEqual(m.Secrets, []string{"database.password"}) {
		t.Fatalf("sidecar secrets = %v, want [database.password]", m.Secrets)
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
	if !reflect.DeepEqual(got, []string{"database.password"}) {
		t.Fatalf("aggregated secrets = %v, want [database.password]", got)
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
