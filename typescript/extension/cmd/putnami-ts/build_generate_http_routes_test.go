package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	httproutes "go.putnami.dev/protocol/http-routes"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// webRoutesFragment and applicationRoutesFragment are the route facts the
// @putnami/web and @putnami/application preBuild hooks write to
// .gen/http-routes.d through writeHttpRoutesFragment: a page, a page with its
// action at the same path, a hashed-asset prefix, and a typed API endpoint,
// each with the project identity the hook stamps.
const webRoutesFragment = `{"routes":[
  {"match":"exact","path":"/","methods":["GET","HEAD"],"publicEdge":true,"provenance":{"project":"@test/pkg","package":"@putnami/web","sourceKind":"file-route","evidencePath":"src/app/page.tsx"}},
  {"match":"template","path":"/blog/{slug}","methods":["GET","HEAD"],"publicEdge":true,"provenance":{"project":"@test/pkg","package":"@putnami/web","sourceKind":"file-route","evidencePath":"src/app/blog/[slug]/page.tsx"}},
  {"match":"template","path":"/blog/{slug}","methods":["POST"],"publicEdge":true,"provenance":{"project":"@test/pkg","package":"@putnami/web","sourceKind":"file-route","evidencePath":"src/app/blog/[slug]/action.ts"}},
  {"match":"prefix","path":"/react/","methods":["GET","HEAD"],"publicEdge":true,"provenance":{"project":"@test/pkg","package":"@putnami/web","sourceKind":"static-mount","evidencePath":".gen/public/react"}}
]}
`

const applicationRoutesFragment = `{"routes":[
  {"match":"template","path":"/api/users/{id}","methods":["GET"],"publicEdge":true,"provenance":{"project":"@test/pkg","package":"@putnami/application","sourceKind":"typed-api","evidencePath":"src/api/users/[id]/get.ts"}}
]}
`

// unsupportedRoutesFragment is a fragment the shared validator rejects with
// http_routes.unsupported_pattern, so the aggregate fails after the hooks ran.
const unsupportedRoutesFragment = `{"routes":[
  {"match":"exact","path":"/docs/*","methods":["GET","HEAD"],"publicEdge":true,"provenance":{"project":"@test/pkg","package":"@putnami/web","sourceKind":"file-route","evidencePath":"src/app/docs/[...page]/page.tsx"}}
]}
`

// mockRouteHook makes the project's one preBuild hook write the given route
// fragments, keyed by fragment name, the way the framework hooks do. An empty
// map is a hook that ran and emitted no route.
func mockRouteHook(t *testing.T, projectPath string, fragments map[string]string) {
	t.Helper()
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		dir := filepath.Join(projectPath, ".gen", "http-routes.d")
		for name, body := range fragments {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
				return nil, err
			}
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{}}` + "\n"}, nil
	})
}

// canonicalRouteInventory is the putnami.http-routes.v1 artifact the protocol
// itself canonicalizes from the fragments, independent of the extension.
func canonicalRouteInventory(t *testing.T, fragments ...string) []byte {
	t.Helper()
	routes := make([]httproutes.Route, 0, len(fragments))
	for _, fragment := range fragments {
		var parsed struct {
			Routes []httproutes.Route `json:"routes"`
		}
		if err := json.Unmarshal([]byte(fragment), &parsed); err != nil {
			t.Fatalf("parse fragment: %v", err)
		}
		routes = append(routes, parsed.Routes...)
	}
	manifest, diags := httproutes.Canonicalize(routes)
	if diag.HasErrors(diags) {
		t.Fatalf("canonicalize: %v", diags)
	}
	body, diags := httproutes.CanonicalJSON(manifest)
	if diag.HasErrors(diags) {
		t.Fatalf("serialize: %v", diags)
	}
	return body
}

func committedRoutesPath(projectPath string) string {
	return filepath.Join(projectPath, filepath.FromSlash(HTTPRoutesRelPath))
}

func emittedRoutesPath(projectPath string) string {
	return filepath.Join(projectPath, ".gen", filepath.FromSlash(HTTPRoutesRelPath))
}

func writeCommittedRoutes(t *testing.T, projectPath string, body []byte) {
	t.Helper()
	path := committedRoutesPath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCommittedRoutes(t *testing.T, projectPath string) []byte {
	t.Helper()
	body, err := os.ReadFile(committedRoutesPath(projectPath))
	if err != nil {
		t.Fatalf("read committed route inventory: %v", err)
	}
	return body
}

func assertNoCommittedRoutes(t *testing.T, projectPath string) {
	t.Helper()
	if _, err := os.Stat(committedRoutesPath(projectPath)); !os.IsNotExist(err) {
		t.Fatalf("stat committed route inventory = %v, want it absent", err)
	}
}

// Successful hooks commit the aggregate verbatim: the tracked file holds the
// exact canonical bytes the protocol defines, the build asset under .gen is
// unchanged, and the strict parser accepts the committed copy with its digest.
func TestDoGenerate_CommitsTheRouteInventoryVerbatim(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "successful-hooks-commit-the-aggregate-verbatim")
	mockBunResolution(t)
	ctx, projectPath := newInfraHookProject(t)
	mockRouteHook(t, projectPath, map[string]string{"web": webRoutesFragment, "application": applicationRoutesFragment})

	result, err := doGenerate(ctx, jsonl.New(), projectPath)
	if err != nil {
		t.Fatalf("doGenerate: %v", err)
	}

	emitted, err := os.ReadFile(emittedRoutesPath(projectPath))
	if err != nil {
		t.Fatalf("read emitted route inventory: %v", err)
	}
	committed := readCommittedRoutes(t, projectPath)
	if string(committed) != string(emitted) {
		t.Fatalf("committed inventory differs from the emitted one\ncommitted:\n%s\nemitted:\n%s", committed, emitted)
	}
	if want := canonicalRouteInventory(t, webRoutesFragment, applicationRoutesFragment); string(committed) != string(want) {
		t.Fatalf("committed inventory is not the canonical protocol bytes\ngot:\n%s\nwant:\n%s", committed, want)
	}
	manifest, diags := httproutes.ParseAndValidateManifest(committed)
	if manifest == nil || len(diags) != 0 {
		t.Fatalf("strict parse of the committed inventory: manifest=%v diagnostics=%v", manifest, diags)
	}
	if len(manifest.Routes) != 5 {
		t.Fatalf("routes = %d, want 5", len(manifest.Routes))
	}
	if got := result.Assets[HTTPRoutesRelPath]; got != emittedRoutesPath(projectPath) {
		t.Fatalf("route inventory asset = %q, want the .gen build asset %q", got, emittedRoutesPath(projectPath))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(committedRoutesPath(projectPath))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("committed inventory mode = %v, want 0644 like its schema siblings", info.Mode().Perm())
		}
	}
}

// A warm build leaves an unchanged committed inventory untouched, a changed
// one replaces it whole, and staging leaves nothing else in schema/.
func TestDoGenerate_RewritesTheRouteInventoryOnlyWhenItChanges(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "an-unchanged-inventory-is-not-rewritten")
	mockBunResolution(t)
	ctx, projectPath := newInfraHookProject(t)
	mockRouteHook(t, projectPath, map[string]string{"web": webRoutesFragment, "application": applicationRoutesFragment})

	if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
		t.Fatalf("first doGenerate: %v", err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(committedRoutesPath(projectPath), past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
		t.Fatalf("second doGenerate: %v", err)
	}
	info, err := os.Stat(committedRoutesPath(projectPath))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("an unchanged inventory was rewritten: mtime %v, want %v", info.ModTime(), past)
	}

	mockRouteHook(t, projectPath, map[string]string{"web": webRoutesFragment})
	if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
		t.Fatalf("third doGenerate: %v", err)
	}
	if got, want := readCommittedRoutes(t, projectPath), canonicalRouteInventory(t, webRoutesFragment); string(got) != string(want) {
		t.Fatalf("a changed inventory was not committed\ngot:\n%s\nwant:\n%s", got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(committedRoutesPath(projectPath)))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "http-routes.json" {
		t.Fatalf("schema/ holds %v, want only http-routes.json", names)
	}
}

// When the hooks ran and none of them emitted a route, the committed
// inventory has no producer left and is removed, whether a hook ran and
// emitted nothing or the project has no hook at all.
func TestDoGenerate_RemovesTheRouteInventoryWithoutAProducer(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "successful-hooks-without-routes-remove-the-committed-copy")

	t.Run("a hook that emits no route", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		mockRouteHook(t, projectPath, map[string]string{"web": webRoutesFragment})
		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
			t.Fatalf("first doGenerate: %v", err)
		}
		readCommittedRoutes(t, projectPath)

		mockRouteHook(t, projectPath, map[string]string{})
		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
			t.Fatalf("second doGenerate: %v", err)
		}
		assertNoCommittedRoutes(t, projectPath)
		if _, err := os.Stat(emittedRoutesPath(projectPath)); !os.IsNotExist(err) {
			t.Fatalf("stat emitted route inventory = %v, want it absent", err)
		}
	})

	t.Run("a project without hooks", func(t *testing.T) {
		mockBunResolution(t)
		mockAllExec(t, successExec)
		ctx, root := makeTestCtx(t)
		projectPath := filepath.Join(root, "project")
		writeCommittedRoutes(t, projectPath, canonicalRouteInventory(t, webRoutesFragment))

		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
			t.Fatalf("doGenerate: %v", err)
		}
		assertNoCommittedRoutes(t, projectPath)
	})
}

// A run whose hooks did not run, failed, or produced an invalid aggregate
// leaves the committed inventory exactly as it stands: an absent or broken
// producer is never evidence that the project serves no route.
func TestDoGenerate_PreservesTheRouteInventoryWhenHooksFailOrDoNotRun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "failed-or-unrun-hooks-preserve-the-committed-copy")
	prior := canonicalRouteInventory(t, webRoutesFragment, applicationRoutesFragment)

	assertPreserved := func(t *testing.T, projectPath string) {
		t.Helper()
		if got := readCommittedRoutes(t, projectPath); string(got) != string(prior) {
			t.Fatalf("committed inventory changed\ngot:\n%s\nwant:\n%s", got, prior)
		}
	}

	t.Run("bun is not resolvable, so no hook runs", func(t *testing.T) {
		failBunResolution(t)
		ctx, root := makeTestCtx(t)
		projectPath := filepath.Join(root, "project")
		writeCommittedRoutes(t, projectPath, prior)

		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
			t.Fatalf("doGenerate: %v", err)
		}
		assertPreserved(t, projectPath)
	})

	t.Run("a hook fails", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, prior)
		mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
			return &exec.Result{Success: false, ExitCode: 1, Stderr: "hook failed"}, nil
		})

		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err == nil {
			t.Fatal("doGenerate succeeded although its hook failed")
		}
		assertPreserved(t, projectPath)
	})

	t.Run("the aggregate is invalid", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, prior)
		mockRouteHook(t, projectPath, map[string]string{"web": unsupportedRoutesFragment})

		_, err := doGenerate(ctx, jsonl.New(), projectPath)
		if err == nil || !strings.Contains(err.Error(), httproutes.ErrorCodeUnsupportedPattern) {
			t.Fatalf("doGenerate error = %v, want %s", err, httproutes.ErrorCodeUnsupportedPattern)
		}
		assertPreserved(t, projectPath)
	})

	t.Run("a dependency is not installed", func(t *testing.T) {
		ctx, projectPath := setupUninstalledDependencyProject(t)
		writeCommittedRoutes(t, projectPath, prior)

		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err == nil {
			t.Fatal("doGenerate succeeded although a dependency is not installed")
		}
		assertPreserved(t, projectPath)
	})
}

// The committed inventory is canonical JSON with every array expanded. The
// built-in Biome configuration formats it with expand "always", as it does the
// committed OpenAPI contract, so read-only lint accepts the bytes generate
// commits and lint --fix never rewrites them into drift.
func TestBuiltInBiomeConfigKeepsTheCommittedRouteInventoryExpanded(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "biome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Overrides []struct {
			Includes []string `json:"includes"`
			JSON     struct {
				Formatter struct {
					Expand string `json:"expand"`
				} `json:"formatter"`
			} `json:"json"`
		} `json:"overrides"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	for _, override := range config.Overrides {
		for _, include := range override.Includes {
			if include == "**/"+HTTPRoutesRelPath {
				if override.JSON.Formatter.Expand != "always" {
					t.Fatalf("%s formats with expand %q, want always", include, override.JSON.Formatter.Expand)
				}
				return
			}
		}
	}
	t.Fatalf("no Biome override includes **/%s", HTTPRoutesRelPath)
}

// The solo and batched generate entry points share doGenerate, so the
// committed inventory is the same bytes whichever path wrote it.
func TestDoGenerate_CommittedRouteInventoryIsBatchEqualToSolo(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "a-batched-generate-commits-the-solo-bytes")
	mockBunResolution(t)
	fragments := map[string]string{"web": webRoutesFragment, "application": applicationRoutesFragment}

	soloCtx, soloPath := newInfraHookProject(t)
	mockRouteHook(t, soloPath, fragments)
	if status, _, err := runBuildGenerate(soloCtx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("solo generate = (%q, %v)", status, err)
	}
	solo := readCommittedRoutes(t, soloPath)

	batchCtx, batchPath := newInfraHookProject(t)
	mockRouteHook(t, batchPath, fragments)
	batchCtx.SelectedProjects = []pctx.ProjectRef{
		{ID: "/project", Name: "@test/pkg", Path: "project", FullPath: batchPath},
	}
	_, data, err := runBuildGenerateBatch(batchCtx)
	if err != nil {
		t.Fatalf("batch generate: %v", err)
	}
	if results := batchResultsFor(t, data); len(results) != 1 || results[0].Status != "OK" {
		t.Fatalf("batch results = %+v, want one OK project", results)
	}
	batch := readCommittedRoutes(t, batchPath)

	if string(solo) != string(batch) {
		t.Fatalf("batch committed inventory != solo\nsolo:\n%s\nbatch:\n%s", solo, batch)
	}
}
