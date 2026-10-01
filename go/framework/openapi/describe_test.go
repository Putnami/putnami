package openapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/app"

	"go.putnami.dev/protocol/features/spectest"
)

// configurePlugin walks the same path Application.Describe takes — Configure
// must run before Describe so the spec exists in memory.
func configurePlugin(t *testing.T, p *Plugin) {
	t.Helper()
	p.AddRoute(DiscoveredRoute{
		Method: "GET",
		Path:   "/users/{id}",
		Returns: reflect.TypeFor[struct {
			ID string `json:"id"`
		}](),
	})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure: %v", err)
	}
}

func TestDescribeWritesJSONAndGz(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "build-time-artifacts", "describe-writes-the-openapi-document-below-the-output-directory")
	p := NewPlugin(PluginOptions{Title: "Test API", Version: "9.9.9"})
	configurePlugin(t, p)

	out := t.TempDir()
	if err := p.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	jsonPath := filepath.Join(out, DefaultDescribePath)
	body, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Errorf("openapi = %v, want 3.0.3", doc["openapi"])
	}
	if info := doc["info"].(map[string]any); info["title"] != "Test API" {
		t.Errorf("title = %v, want Test API", info["title"])
	}

	gzPath := filepath.Join(out, DescribeGzPath)
	gzData, err := os.ReadFile(gzPath)
	if err != nil {
		t.Fatalf("read gz: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	gunzipped, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !bytes.Equal(body, gunzipped) {
		t.Error("gz contents do not match json contents")
	}
}

func TestDescribeShortCircuitsWhenNotTargeted(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "build-time-artifacts", "the-openapi-describe-short-circuits-when-the-project-is-not-targeted")
	p := NewPlugin(PluginOptions{})
	configurePlugin(t, p)

	out := t.TempDir()
	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"proto"}}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, DefaultDescribePath)); !os.IsNotExist(err) {
		t.Errorf("expected no openapi.json when target=proto, got err=%v", err)
	}
}

func TestDescribeIsNoopBeforeConfigure(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "build-time-artifacts", "the-openapi-plugin-writes-nothing-before-configure")
	p := NewPlugin(PluginOptions{})
	// Note: not calling Configure — spec is nil. Describe must handle this
	// without error so describe-mode runs against a partially-configured app
	// don't fail catastrophically.
	out := t.TempDir()
	if err := p.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, DefaultDescribePath)); !os.IsNotExist(err) {
		t.Errorf("expected no file when spec is nil, got err=%v", err)
	}
}

// TestDescribeRemovesAStaleSpecWhenNothingIsGenerated pins the removal an
// incremental build depends on.
//
// The Go extension declares <project>/.gen/schema as build-describe's cached
// output and captures it from DISK — the scheduler walks the declared directory,
// it does not read the job's artifact list. A spec the current sources no longer
// produce would be recorded under this run's key and restored by every later hit
// on it, while a clean-tree build of the same sources produced an entry without
// it.
func TestDescribeRemovesAStaleSpecWhenNothingIsGenerated(t *testing.T) {
	out := t.TempDir()
	if err := os.MkdirAll(filepath.Join(out, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{DefaultDescribePath, DescribeGzPath} {
		if err := os.WriteFile(filepath.Join(out, rel), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p := NewPlugin(PluginOptions{}) // never configured, so there is no spec
	if err := p.Describe(&app.DescribeContext{OutputDir: out}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for _, rel := range []string{DefaultDescribePath, DescribeGzPath} {
		if _, err := os.Stat(filepath.Join(out, rel)); !os.IsNotExist(err) {
			t.Errorf("%s: a spec this run did not produce survived: %v", rel, err)
		}
	}
}

// A run that is not targeted at the spec is not authoritative for it, so it must
// leave an existing one exactly where it is.
func TestDescribeKeepsTheSpecWhenNotTargeted(t *testing.T) {
	out := t.TempDir()
	kept := filepath.Join(out, DefaultDescribePath)
	if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewPlugin(PluginOptions{})
	if err := p.Describe(&app.DescribeContext{OutputDir: out, Targets: []string{"proto"}}); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("an untargeted run removed a spec it says nothing about: %v", err)
	}
}
