package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	httproutes "go.putnami.dev/protocol/http-routes"
)

func TestAggregateHTTPRoutesAcrossFrameworkFragments(t *testing.T) {
	project := t.TempDir()
	fragmentDir := filepath.Join(project, ".gen", httpRoutesFragmentDir)
	if err := os.MkdirAll(fragmentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	application := `{"routes":[
  {"match":"template","path":"/api/users/{id}","methods":["GET"],"publicEdge":true,"provenance":{"project":"example/web","package":"@putnami/application","sourceKind":"typed-api","evidencePath":"src/api/users/[id]/get.ts"}},
  {"match":"exact","path":"/favicon.ico","methods":["GET","HEAD"],"publicEdge":true,"provenance":{"project":"example/web","package":"@putnami/application","sourceKind":"public-file","evidencePath":"public/favicon.ico"}}
]}`
	web := `{"routes":[
  {"match":"template","path":"/blog/{slug}","methods":["GET","HEAD"],"publicEdge":true,"provenance":{"project":"example/web","package":"@putnami/web","sourceKind":"file-route","evidencePath":"src/app/blog/[slug]/page.tsx"}},
  {"match":"prefix","path":"/react/","methods":["GET","HEAD"],"publicEdge":true,"provenance":{"project":"example/web","package":"@putnami/web","sourceKind":"static-mount","evidencePath":".gen/public/react"}}
]}`
	if err := os.WriteFile(filepath.Join(fragmentDir, "web.json"), []byte(web), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fragmentDir, "application.json"), []byte(application), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := AggregateHTTPRoutes(project)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := httproutes.ParseAndValidateManifest(body)
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("invalid inventory: manifest=%v diagnostics=%v", manifest, diags)
	}
	if len(manifest.Routes) != 4 {
		t.Fatalf("routes = %d, want 4", len(manifest.Routes))
	}

	first := string(body)
	if _, err := AggregateHTTPRoutes(project); err != nil {
		t.Fatalf("second aggregate: %v", err)
	}
	second, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if first != string(second) {
		t.Fatal("inventory changed across repeated aggregation")
	}
}

func TestAggregateHTTPRoutesRejectsUnsupportedPattern(t *testing.T) {
	project := t.TempDir()
	fragmentDir := filepath.Join(project, ".gen", httpRoutesFragmentDir)
	if err := os.MkdirAll(fragmentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fragment := `{"routes":[{"match":"exact","path":"/docs/*","methods":["GET"],"publicEdge":true,"provenance":{"project":"example/web","sourceKind":"file-route"}}]}`
	if err := os.WriteFile(filepath.Join(fragmentDir, "web.json"), []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := AggregateHTTPRoutes(project)
	if err == nil || !strings.Contains(err.Error(), httproutes.ErrorCodeUnsupportedPattern) {
		t.Fatalf("aggregate error = %v, want %s", err, httproutes.ErrorCodeUnsupportedPattern)
	}
}

func TestResetHTTPRoutesRemovesOnlyGeneratedInventory(t *testing.T) {
	project := t.TempDir()
	fragment := filepath.Join(project, ".gen", httpRoutesFragmentDir, "web.json")
	inventory := filepath.Join(project, ".gen", httpRoutesAssetPath)
	keep := filepath.Join(project, ".gen", "version.json")
	for _, path := range []string{fragment, inventory, keep} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ResetHTTPRoutes(project); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fragment); !os.IsNotExist(err) {
		t.Fatalf("fragment still exists: %v", err)
	}
	if _, err := os.Stat(inventory); !os.IsNotExist(err) {
		t.Fatalf("inventory still exists: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated generated file removed: %v", err)
	}
}
