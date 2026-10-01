package build

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	capabilities "go.putnami.dev/protocol/capabilities"
	diagnostic "go.putnami.dev/protocol/diagnostic"
)

func writeLoaderModule(t *testing.T, projectPath, relative string) string {
	t.Helper()
	path := filepath.Join(projectPath, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("export const loaded = true;\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReconcileCapabilityManifest_AddsLateHookLoadersCanonicallyAndIdempotently(t *testing.T) {
	dir := t.TempDir()
	apiPath := writeLoaderModule(t, dir, ".gen/src/api.ts")
	reactPath := writeLoaderModule(t, dir, ".gen/src/react.ts")
	sqlPath := writeLoaderModule(t, dir, ".gen/src/sql.ts")
	manifestPath := writeCapabilityActivationManifest(t, dir, `{
  "$schema": "https://putnami.dev/schemas/putnami-capabilities.json",
  "protocolVersion": 1,
  "project": "test",
  "schemas": [
    {
      "name": "api-loader",
      "kind": "route",
      "path": ".gen/src/api.ts",
      "provenance": {
        "project": "test",
        "sourceKind": "generated",
        "evidencePath": ".gen/src/api.ts"
      }
    }
  ]
}`)
	exports := map[string]string{
		"api-loader":          apiPath,
		"react-loader":        reactPath,
		"sql-loader":          sqlPath,
		"react-client-loader": writeLoaderModule(t, dir, ".gen/src/react-client.ts"),
		"config-loader":       filepath.Join(dir, ".gen/conf/.env.test.yaml"),
	}

	gotPath, err := ReconcileCapabilityManifest(dir, manifestPath, exports)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if gotPath != manifestPath {
		t.Fatalf("path = %q, want %q", gotPath, manifestPath)
	}
	first, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := capabilities.ParseAndValidateManifest(first)
	if diagnostic.HasErrors(diags) {
		t.Fatalf("reconciled manifest invalid: %v", diags)
	}
	if len(manifest.Schemas) != 2 || manifest.Schemas[0].Name != "api-loader" || manifest.Schemas[1].Name != "react-loader" {
		t.Fatalf("schemas = %+v, want canonical api/react route loaders", manifest.Schemas)
	}
	if len(manifest.Discoverers) != 1 || manifest.Discoverers[0].Name != "sql-loader" ||
		manifest.Discoverers[0].Provenance.EvidencePath != ".gen/src/sql.ts" {
		t.Fatalf("discoverers = %+v, want sql source loader", manifest.Discoverers)
	}

	if _, err := ReconcileCapabilityManifest(dir, manifestPath, exports); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	second, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("reconciliation is not byte-idempotent\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestReadValidatedCapabilityManifest_NormalizesV2Activation(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeCapabilityActivationManifest(t, dir, `{
  "protocolVersion": 2,
  "project": "test",
  "schemas": [{
    "identity": {"ownerProject":"test","kind":"schema","subkind":"route","key":"api-loader"},
    "name": "api-loader",
    "kind": "route",
    "path": ".gen/src/api.ts",
    "provenance": {
      "project": "test",
      "sourceKind": "generated",
      "declaration": {"root":"project","path":"src/api.ts"},
      "artifacts": [{"root":"project","path":".gen/src/api.ts"}]
    }
  }]
}`)
	activation, err := readValidatedCapabilityManifest(dir, manifestPath)
	if err != nil {
		t.Fatalf("read v2 activation: %v", err)
	}
	if activation.ProtocolVersion != 2 || len(activation.Schemas) != 1 || activation.Schemas[0].Path != ".gen/src/api.ts" {
		t.Fatalf("v2 activation = %+v", activation)
	}
}

func TestReconcileCapabilityManifestV2_AddsLateHookLoadersCanonicallyAndIdempotently(t *testing.T) {
	dir := t.TempDir()
	apiPath := writeLoaderModule(t, dir, ".gen/src/api.ts")
	reactPath := writeLoaderModule(t, dir, ".gen/src/react.ts")
	sqlPath := writeLoaderModule(t, dir, ".gen/src/sql.ts")
	manifestPath := writeCapabilityActivationManifest(t, dir, `{
  "$schema": "https://putnami.dev/schemas/putnami-capabilities-v2.json",
  "protocolVersion": 2,
  "project": "test",
  "schemas": [{
    "identity": {"ownerProject":"test","kind":"schema","subkind":"route","key":"api-loader"},
    "name": "api-loader",
    "kind": "route",
    "path": ".gen/src/api.ts",
    "provenance": {
      "project": "test", "package":"test", "sourceKind": "generated",
      "declaration": {"root":"project","path":"putnami.json"},
      "artifacts": [{"root":"project","path":".gen/src/api.ts"}]
    }
  }],
  "packages": [{
    "identity": {"ownerProject":"test","kind":"package","key":"test"},
    "package":"test",
    "provenance": {
      "project":"test", "package":"test", "sourceKind":"framework",
      "declaration":{"root":"project","path":"putnami.json"}
    }
  }]
}`)
	exports := map[string]string{"api-loader": apiPath, "react-loader": reactPath, "sql-loader": sqlPath}

	if _, err := ReconcileCapabilityManifest(dir, manifestPath, exports); err != nil {
		t.Fatalf("reconcile v2: %v", err)
	}
	first, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	document, diags := capabilities.ParseAndValidateManifestDocument(first)
	if diagnostic.HasErrors(diags) || document.V2 == nil {
		t.Fatalf("reconciled v2 invalid: %#v %v", document, diags)
	}
	if len(document.V2.Schemas) != 2 || document.V2.Schemas[1].Identity.Key != "react-loader" {
		t.Fatalf("v2 schemas = %+v", document.V2.Schemas)
	}
	if len(document.V2.Discoverers) != 1 || document.V2.Discoverers[0].Identity.Key != "sql-loader" {
		t.Fatalf("v2 discoverers = %+v", document.V2.Discoverers)
	}
	if bytes.Contains(first, []byte(`"version"`)) || bytes.Contains(first, []byte(`"packageVersions"`)) {
		t.Fatalf("reconciled v2 manifest contains volatile versions: %s", first)
	}
	if _, err := ReconcileCapabilityManifest(dir, manifestPath, exports); err != nil {
		t.Fatalf("second reconcile v2: %v", err)
	}
	second, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("v2 reconciliation is not byte-idempotent\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestReconcileCapabilityManifest_FailsClosedOnLoaderConflicts(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{
			name: "path",
			manifest: `{
  "protocolVersion": 1,
  "project": "test",
  "schemas": [{
    "name": "react-loader",
    "kind": "route",
    "path": ".gen/src/old-react.ts",
    "provenance": {"project":"test","sourceKind":"generated","evidencePath":".gen/src/old-react.ts"}
  }]
}`,
		},
		{
			name: "identity",
			manifest: `{
  "protocolVersion": 1,
  "project": "test",
  "discoverers": [{
    "name": "react-loader",
    "kind": "source",
    "provenance": {"project":"test","sourceKind":"generated","evidencePath":".gen/src/react.ts"}
  }]
}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			manifestPath := writeCapabilityActivationManifest(t, dir, tt.manifest)
			reactPath := writeLoaderModule(t, dir, ".gen/src/react.ts")

			_, err := ReconcileCapabilityManifest(dir, manifestPath, map[string]string{"react-loader": reactPath})
			if err == nil || !strings.Contains(err.Error(), "conflicts") {
				t.Fatalf("expected loader conflict, got %v", err)
			}
			if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
				t.Fatalf("failed reconciliation left a publishable manifest: %v", statErr)
			}
		})
	}
}

// The TypeScript framework keys the loader of a second api(), static() or
// events() plugin by slot (api-1-loader, static-1-loader, events-1-loader) and
// emits it as a generated source discoverer, never as a route schema: the
// route-loader list here names only the first-slot keys, so an extension that
// predates the slots still reconciles a manifest from a framework that emits
// them. Every slotted loader must reconcile as a source discoverer and be
// registered by the packaged serve entrypoint under its own key (ADR 0006 of
// @putnami/application).
func TestReconcileCapabilityManifestV2_SlottedLoadersAreSourceDiscoverersAndActivate(t *testing.T) {
	dir := t.TempDir()
	writeLoaderModule(t, dir, "src/main.ts")
	exports := map[string]string{"api-loader": writeLoaderModule(t, dir, ".gen/src/api.ts")}
	for _, key := range []string{"api-1-loader", "static-loader", "static-1-loader", "events-loader", "events-1-loader"} {
		exports[key] = writeLoaderModule(t, dir, ".gen/src/"+key+".ts")
	}
	manifestPath := writeCapabilityActivationManifest(t, dir, `{
  "$schema": "https://putnami.dev/schemas/putnami-capabilities-v2.json",
  "protocolVersion": 2,
  "project": "test",
  "schemas": [{
    "identity": {"ownerProject":"test","kind":"schema","subkind":"route","key":"api-loader"},
    "name": "api-loader",
    "kind": "route",
    "path": ".gen/src/api.ts",
    "provenance": {
      "project": "test", "package":"test", "sourceKind": "generated",
      "declaration": {"root":"project","path":"putnami.json"},
      "artifacts": [{"root":"project","path":".gen/src/api.ts"}]
    }
  }],
  "packages": [{
    "identity": {"ownerProject":"test","kind":"package","key":"test"},
    "package":"test",
    "provenance": {
      "project":"test", "package":"test", "sourceKind":"framework",
      "declaration":{"root":"project","path":"putnami.json"}
    }
  }]
}`)

	if _, err := ReconcileCapabilityManifest(dir, manifestPath, exports); err != nil {
		t.Fatalf("reconcile slotted loaders: %v", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	document, diags := capabilities.ParseAndValidateManifestDocument(data)
	if diagnostic.HasErrors(diags) || document.V2 == nil {
		t.Fatalf("reconciled v2 invalid: %#v %v", document, diags)
	}
	routes := make([]string, 0, len(document.V2.Schemas))
	var sources []string
	for _, schema := range document.V2.Schemas {
		routes = append(routes, schema.Name)
	}
	for _, discoverer := range document.V2.Discoverers {
		if discoverer.Kind == capabilities.DiscovererKindSource {
			sources = append(sources, discoverer.Name)
		}
	}
	if strings.Join(routes, ",") != "api-loader,static-loader" {
		t.Fatalf("route schemas = %v, want the first-slot api and static loaders only", routes)
	}
	if strings.Join(sources, ",") != "api-1-loader,events-1-loader,events-loader,static-1-loader" {
		t.Fatalf("source discoverers = %v", sources)
	}

	entry, err := GenerateBundledServe(dir, manifestPath, nil)
	if err != nil {
		t.Fatalf("generate bundled serve: %v", err)
	}
	body, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	for key := range exports {
		if count := strings.Count(string(body), "registerModuleLoader(\""+key+"\","); count != 1 {
			t.Fatalf("bundled serve registers %q %d times, want once:\n%s", key, count, body)
		}
	}
}
