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
	"go.putnami.dev/protocol/infra"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/errs"
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

// emptyRoutesFragment is the fragment writeHttpRoutesFragment writes for a
// hook that evaluated its routes and found none.
const emptyRoutesFragment = `{
  "routes": []
}
`

// unsupportedRoutesFragment is a fragment the shared validator rejects with
// http_routes.unsupported_pattern, so the aggregate fails after the hooks ran.
const unsupportedRoutesFragment = `{"routes":[
  {"match":"exact","path":"/docs/*","methods":["GET","HEAD"],"publicEdge":true,"provenance":{"project":"@test/pkg","package":"@putnami/web","sourceKind":"file-route","evidencePath":"src/app/docs/[...page]/page.tsx"}}
]}
`

// completeHookSummary is the summary line of a preBuild hook that evaluated
// its route contribution. Its data.httpRoutes is the exact literal
// completeHttpRoutesEvaluation() from @putnami/application/http-routes
// serializes to, which the first-party hooks print.
const completeHookSummary = `{"type":"summary","data":{"exports":{},"assets":{},"httpRoutes":{"v":1,"state":"complete"}}}`

// legacyHookSummary is the summary line of a preBuild hook built before the
// route evaluation existed: it reports its outputs and no data.httpRoutes.
const legacyHookSummary = `{"type":"summary","data":{"exports":{},"assets":{}}}`

// unavailableHookSummary is the summary line of a preBuild hook that returned
// without evaluating its routes. Its data.httpRoutes is the exact literal
// unavailableHttpRoutesEvaluation('the application factory failed')
// serializes to.
const unavailableHookSummary = `{"type":"summary","data":{"durationMs":1,"outputs":0,"exports":{},"assets":{},` +
	`"httpRoutes":{"v":1,"state":"unavailable","reason":"the application factory failed"}}}`

// databaseRequirementSidecar is the scratch infra fragment a database producer
// writes; a successful generate commits it into infra/requirements.json.
const databaseRequirementSidecar = `{"protocolVersion":2,"databases":[{"name":"primary","engine":"postgres","schemas":["app"]}]}`

// priorInfraRequirements is a committed infra requirements file that no
// sidecar of these tests produces, so a run that reconciled the requirements
// would change its bytes.
const priorInfraRequirements = `{"protocolVersion":2,"databases":[{"name":"ledger","engine":"postgres","schemas":["ledger"]}]}` + "\n"

// factoryErrorHookMessage is the head of the error the @putnami/application
// preBuild hook reports when the Application factory throws.
const factoryErrorHookMessage = "generate: the Application factory threw, so its routes and capabilities are unknown:\n" +
	"  - src/main.ts: Missing required environment variable: OAUTH_CLIENT_ID"

// missingRouteEvaluationCause is the head of the cause a run reports for the
// preBuild hook of dependency name when its summary has no data.httpRoutes.
func missingRouteEvaluationCause(name string) string {
	return "hook " + name + " reported no route evaluation: its summary has no data.httpRoutes; upgrade " + name +
		" together with @putnami/typescript"
}

// mockRouteHook makes the project's preBuild hooks write the given route
// fragments, keyed by fragment name, the way the framework hooks do, and
// report a complete route evaluation. An empty map is a hook that evaluated
// its routes and wrote no fragment.
func mockRouteHook(t *testing.T, projectPath string, fragments map[string]string) {
	t.Helper()
	mockRouteHookSummary(t, projectPath, fragments, completeHookSummary)
}

// mockRouteHookSummary is mockRouteHook for a hook that prints summary as its
// summary line.
func mockRouteHookSummary(t *testing.T, projectPath string, fragments map[string]string, summary string) {
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
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summary + "\n"}, nil
	})
}

// mockedHookContext is the part of a hook subprocess's --putnami-context file
// a mocked hook reads.
type mockedHookContext struct {
	ProjectRoot string `json:"projectRoot"`
	Extension   string `json:"extension"`
}

// readMockedHookContext reads the --putnami-context file of a hook subprocess.
func readMockedHookContext(args []string) (mockedHookContext, error) {
	var hookCtx mockedHookContext
	data, err := os.ReadFile(argAfter(args, "--putnami-context"))
	if err != nil {
		return hookCtx, err
	}
	err = json.Unmarshal(data, &hookCtx)
	return hookCtx, err
}

// hookRun is one run of a mocked preBuild hook in projectPath.
type hookRun func(projectPath string) (*exec.Result, error)

// mockHookRun makes every hook subprocess behave as run, in the project root
// its context names, so one mock serves every project of a batch.
func mockHookRun(t *testing.T, run hookRun) {
	t.Helper()
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		hookCtx, err := readMockedHookContext(args)
		if err != nil {
			return nil, err
		}
		return run(hookCtx.ProjectRoot)
	})
}

// scratchOutputsThen writes the web route fragment and the database
// requirement sidecar into the project's .gen, then returns successfully with
// summary: every output a successful generate would publish exists in .gen, so
// a test can check what the run publishes.
func scratchOutputsThen(projectPath, summary string) (*exec.Result, error) {
	if err := writeHookScratchOutputs(projectPath); err != nil {
		return nil, err
	}
	return &exec.Result{Success: true, ExitCode: 0, Stdout: summary + "\n"}, nil
}

// completeRouteRun writes the scratch outputs and reports a complete route
// evaluation.
func completeRouteRun(projectPath string) (*exec.Result, error) {
	return scratchOutputsThen(projectPath, completeHookSummary)
}

// unavailableRouteRun writes the scratch outputs and reports the routes
// unavailable.
func unavailableRouteRun(projectPath string) (*exec.Result, error) {
	return scratchOutputsThen(projectPath, unavailableHookSummary)
}

// legacyRouteRun writes the scratch outputs, a valid fragment among them, and
// reports no route evaluation.
func legacyRouteRun(projectPath string) (*exec.Result, error) {
	return scratchOutputsThen(projectPath, legacyHookSummary)
}

// factoryErrorRun writes the scratch outputs, then fails the way the
// @putnami/application hook fails when the Application factory throws: one
// error event with factoryErrorHookMessage, no summary, and exit code 1.
func factoryErrorRun(projectPath string) (*exec.Result, error) {
	if err := writeHookScratchOutputs(projectPath); err != nil {
		return nil, err
	}
	event, err := json.Marshal(map[string]any{"type": "error", "level": "error", "message": factoryErrorHookMessage, "recoverable": false})
	if err != nil {
		return nil, err
	}
	return &exec.Result{Success: false, ExitCode: 1, Stdout: string(event) + "\n"}, nil
}

// writeHookScratchOutputs writes the web route fragment and the database
// requirement sidecar into the project's .gen, as producers do before they
// return.
func writeHookScratchOutputs(projectPath string) error {
	for path, body := range map[string]string{
		filepath.Join(projectPath, ".gen", "http-routes.d", "web.json"): webRoutesFragment,
		filepath.Join(projectPath, ".gen", "infra", "database.json"):    databaseRequirementSidecar,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// refuseProcesses fails the test if any subprocess runs.
func refuseProcesses(t *testing.T) {
	t.Helper()
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		t.Errorf("a process ran (%v), want none", args)
		return &exec.Result{Success: true, ExitCode: 0, Stdout: completeHookSummary + "\n"}, nil
	})
}

// addPreBuildDependency installs a second dependency named name that declares
// a preBuild hook, beside the infra-ext hook newInfraHookProject installs, and
// declares both in the project's package.json.
func addPreBuildDependency(t *testing.T, projectPath, name string) {
	t.Helper()
	depDir := filepath.Join(projectPath, "node_modules", name)
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","dependencies":{"infra-ext":"^1.0.0","`+name+`":"^1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addInfraHookProject adds a project at rel in the workspace of ctx with the
// one infra-ext preBuild hook newInfraHookProject declares, and returns its
// path.
func addInfraHookProject(t *testing.T, ctx *pctx.Context, rel string) string {
	t.Helper()
	projectPath := filepath.Join(ctx.WorkspaceRoot, rel)
	if err := os.MkdirAll(filepath.Join(projectPath, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("const x = 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	depDir := filepath.Join(projectPath, "node_modules", "infra-ext")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/sibling","dependencies":{"infra-ext":"^1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectPath
}

// assertGenerateFailsWithoutPublishing runs doGenerate and checks that it
// fails with code, that its error contains want and no absolute workspace
// path, and that the run published nothing: the committed inventory still
// holds prior (or stays absent when prior is nil), the committed infra
// requirements keep the bytes they had before the run (or stay absent), and
// neither the .gen aggregate nor the generate result exists.
func assertGenerateFailsWithoutPublishing(t *testing.T, ctx *pctx.Context, projectPath string, code errs.Code, want string, prior []byte) {
	t.Helper()
	infraPath := infra.ProjectRequirementsPath(projectPath)
	infraBefore, infraBeforeErr := os.ReadFile(infraPath)
	if infraBeforeErr != nil && !os.IsNotExist(infraBeforeErr) {
		t.Fatal(infraBeforeErr)
	}

	result, err := doGenerate(ctx, jsonl.New(), projectPath)
	if err == nil || result != nil {
		t.Fatalf("doGenerate = (%+v, %v), want a failure and no result", result, err)
	}
	if got := errs.CodeOf(err); got != code {
		t.Fatalf("error code = %q, want %q (error: %v)", got, code, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), ctx.WorkspaceRoot) {
		t.Fatalf("error %q names the absolute workspace root %q", err, ctx.WorkspaceRoot)
	}
	if prior == nil {
		assertNoCommittedRoutes(t, projectPath)
	} else if got := readCommittedRoutes(t, projectPath); string(got) != string(prior) {
		t.Fatalf("committed inventory changed\ngot:\n%s\nwant:\n%s", got, prior)
	}
	for _, path := range []string{
		emittedRoutesPath(projectPath),
		filepath.Join(projectPath, ".gen", "generate-result.json"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stat %s = %v, want it absent after a failed generate", path, err)
		}
	}
	infraAfter, infraAfterErr := os.ReadFile(infraPath)
	if os.IsNotExist(infraBeforeErr) {
		if !os.IsNotExist(infraAfterErr) {
			t.Fatalf("stat %s = %v, want it absent after a failed generate", infraPath, infraAfterErr)
		}
	} else if infraAfterErr != nil || string(infraAfter) != string(infraBefore) {
		t.Fatalf("committed infra requirements changed (%v)\ngot:\n%s\nwant:\n%s", infraAfterErr, infraAfter, infraBefore)
	}
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

// writeCommittedInfra writes the prior committed infra fixture.
func writeCommittedInfra(t *testing.T, projectPath string) {
	t.Helper()
	path := infra.ProjectRequirementsPath(projectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(priorInfraRequirements), 0o644); err != nil {
		t.Fatal(err)
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

// A hook that evaluated its routes and found none writes an empty fragment:
// the run commits a known-empty canonical inventory with routes [] and
// advertises it. The same complete hook without a fragment is an absence, and
// the next run removes the copy.
func TestDoGenerate_CommitsAKnownEmptyRouteInventory(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "a-complete-empty-fragment-commits-an-empty-inventory")
	mockBunResolution(t)
	ctx, projectPath := newInfraHookProject(t)
	writeCommittedRoutes(t, projectPath, canonicalRouteInventory(t, webRoutesFragment))
	mockRouteHook(t, projectPath, map[string]string{"application": emptyRoutesFragment})

	result, err := doGenerate(ctx, jsonl.New(), projectPath)
	if err != nil {
		t.Fatalf("doGenerate: %v", err)
	}
	committed := readCommittedRoutes(t, projectPath)
	if want := canonicalRouteInventory(t, emptyRoutesFragment); string(committed) != string(want) {
		t.Fatalf("committed inventory is not the canonical empty inventory\ngot:\n%s\nwant:\n%s", committed, want)
	}
	if !strings.Contains(string(committed), `"routes": []`) {
		t.Fatalf("committed inventory does not state routes []:\n%s", committed)
	}
	manifest, diags := httproutes.ParseAndValidateManifest(committed)
	if manifest == nil || len(diags) != 0 || len(manifest.Routes) != 0 {
		t.Fatalf("strict parse of the empty inventory: manifest=%v diagnostics=%v", manifest, diags)
	}
	if got := result.Assets[HTTPRoutesRelPath]; got != emittedRoutesPath(projectPath) {
		t.Fatalf("route inventory asset = %q, want the .gen build asset %q", got, emittedRoutesPath(projectPath))
	}

	mockRouteHook(t, projectPath, map[string]string{})
	if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
		t.Fatalf("doGenerate without a fragment: %v", err)
	}
	assertNoCommittedRoutes(t, projectPath)
}

// When every hook reports a complete evaluation and none wrote a fragment,
// the committed inventory has no producer left and is removed: a hook that ran
// and wrote nothing, a project without hooks, a project whose dependency is a
// plain package without an extension manifest, and a project whose route
// producer was removed from its dependencies.
func TestDoGenerate_RemovesTheRouteInventoryWithoutAProducer(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "complete-evaluations-without-a-fragment-remove-the-committed-copy")

	t.Run("a complete hook that writes no fragment", func(t *testing.T) {
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

	// A readable package.json that declares no hook is a known absence, with or
	// without Bun: no hook can report a route.
	knownAbsence := func(t *testing.T, resolveBun func(*testing.T)) {
		resolveBun(t)
		refuseProcesses(t)
		ctx, root := makeTestCtx(t)
		projectPath := filepath.Join(root, "project")
		writeCommittedRoutes(t, projectPath, canonicalRouteInventory(t, webRoutesFragment))

		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
			t.Fatalf("doGenerate: %v", err)
		}
		assertNoCommittedRoutes(t, projectPath)
	}
	t.Run("a project without hooks", func(t *testing.T) { knownAbsence(t, mockBunResolution) })
	t.Run("a project without hooks when Bun is absent", func(t *testing.T) { knownAbsence(t, failBunResolution) })

	// An installed dependency without putnami.extension.json is a plain package:
	// it declares no hook, which is a known absence, not a gap.
	t.Run("a plain dependency without an extension manifest", func(t *testing.T) {
		mockBunResolution(t)
		refuseProcesses(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, canonicalRouteInventory(t, webRoutesFragment))
		if err := os.Remove(filepath.Join(projectPath, "node_modules", "infra-ext", "putnami.extension.json")); err != nil {
			t.Fatal(err)
		}

		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
			t.Fatalf("doGenerate: %v", err)
		}
		assertNoCommittedRoutes(t, projectPath)
	})

	t.Run("the route producer is removed from the dependencies", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		mockRouteHook(t, projectPath, map[string]string{"application": applicationRoutesFragment})
		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
			t.Fatalf("first doGenerate: %v", err)
		}
		readCommittedRoutes(t, projectPath)

		if err := os.WriteFile(filepath.Join(projectPath, "package.json"), []byte(`{"name":"@test/pkg"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		refuseProcesses(t)
		if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
			t.Fatalf("second doGenerate: %v", err)
		}
		assertNoCommittedRoutes(t, projectPath)
	})
}

// A run whose hook reported its routes unavailable, or whose discovery could
// not read a manifest, fails before it aggregates, reconciles infra
// requirements or commits: neither is evidence that the project serves no
// route.
func TestDoGenerate_FailsWhenARouteProducerIsUnavailable(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "unavailable-route-producers-fail-generate")
	prior := canonicalRouteInventory(t, webRoutesFragment, applicationRoutesFragment)

	t.Run("a hook reports its routes unavailable after writing a fragment and a requirement", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, prior)
		writeCommittedInfra(t, projectPath)
		mockHookRun(t, unavailableRouteRun)

		assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeHTTPRoutesUnavailable,
			"serves: hook infra-ext reported its routes unavailable: the application factory failed.", prior)
	})

	t.Run("an unavailable run creates no committed copy", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		mockHookRun(t, unavailableRouteRun)

		assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeHTTPRoutesUnavailable,
			"hook infra-ext reported its routes unavailable", nil)
	})

	packageJSONCases := map[string]struct {
		write func(t *testing.T, path string)
		want  string
	}{
		"package.json is missing": {
			write: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			want: "serves: package.json is missing.",
		},
		"package.json is malformed": {
			write: writeBody(`{"name":"@test/pkg","dependencies":`),
			want:  "serves: package.json is malformed: ",
		},
		// encoding/json reads null as an empty value: without the check, both
		// would read as a project without hooks and remove the committed copy.
		"package.json is null": {
			write: writeBody(`null`),
			want:  "serves: package.json is malformed: the document is null, not an object.",
		},
		"package.json dependencies are null": {
			write: writeBody(`{"name":"@test/pkg","dependencies":null}`),
			want:  "serves: package.json is malformed: dependencies is null, not an object.",
		},
		"package.json cannot be read": {
			write: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "serves: package.json cannot be read: ",
		},
	}
	for name, tc := range packageJSONCases {
		for bunName, resolveBun := range map[string]func(*testing.T){"Bun present": mockBunResolution, "Bun absent": failBunResolution} {
			t.Run(name+"/"+bunName, func(t *testing.T) {
				resolveBun(t)
				refuseProcesses(t)
				ctx, projectPath := newInfraHookProject(t)
				writeCommittedRoutes(t, projectPath, prior)
				writeCommittedInfra(t, projectPath)
				tc.write(t, filepath.Join(projectPath, "package.json"))

				assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeHTTPRoutesUnavailable, tc.want, prior)
			})
		}
	}

	manifestCases := map[string]struct {
		write func(t *testing.T, path string)
		want  string
	}{
		"a dependency manifest is malformed": {
			write: writeBody(`{"hooks":{"preBuild":`),
			want:  "serves: broken-ext: putnami.extension.json is malformed: ",
		},
		"a dependency manifest is null": {
			write: writeBody(`null`),
			want:  "serves: broken-ext: putnami.extension.json is malformed: the document is null, not an object.",
		},
		"a dependency declares a null preBuild hook": {
			write: writeBody(`{"hooks":{"preBuild":null}}`),
			want:  "serves: broken-ext: putnami.extension.json is malformed: hooks.preBuild is null, not an object.",
		},
		"a dependency manifest cannot be read": {
			write: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "serves: broken-ext: putnami.extension.json cannot be read: ",
		},
	}
	for name, tc := range manifestCases {
		t.Run(name, func(t *testing.T) {
			mockBunResolution(t)
			ctx, projectPath := newInfraHookProject(t)
			writeCommittedRoutes(t, projectPath, prior)
			writeCommittedInfra(t, projectPath)
			depDir := filepath.Join(projectPath, "node_modules", "broken-ext")
			if err := os.MkdirAll(depDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.write(t, filepath.Join(depDir, "putnami.extension.json"))
			if err := os.WriteFile(filepath.Join(projectPath, "package.json"),
				[]byte(`{"name":"@test/pkg","dependencies":{"broken-ext":"^1.0.0","infra-ext":"^1.0.0"}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			// The readable producer runs, writes its fragment and requirement, and
			// reports complete; the failed run must publish none of them.
			mockHookRun(t, completeRouteRun)

			assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeHTTPRoutesUnavailable, tc.want, prior)
		})
	}
}

// writeBody is a fixture writer that puts body at its path.
func writeBody(body string) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A preBuild hook that returns without a complete route evaluation fails the
// run, which names the hook and the remedy, even when the hook wrote a valid
// fragment or runs beside a hook that reported complete: neither its fragment
// nor its silence is evidence of the routes the project serves.
func TestDoGenerate_FailsWithoutACompleteRouteEvaluation(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "a-missing-or-malformed-route-evaluation-fails-naming-the-hook")
	prior := canonicalRouteInventory(t, webRoutesFragment, applicationRoutesFragment)

	t.Run("a legacy hook that wrote no fragment", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, prior)
		writeCommittedInfra(t, projectPath)
		mockRouteHookSummary(t, projectPath, map[string]string{}, legacyHookSummary)

		assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeHTTPRoutesUnavailable,
			"serves: "+missingRouteEvaluationCause("infra-ext"), prior)
	})

	t.Run("a legacy hook that wrote a valid fragment and a requirement", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, prior)
		writeCommittedInfra(t, projectPath)
		mockHookRun(t, legacyRouteRun)

		assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeHTTPRoutesUnavailable,
			"serves: "+missingRouteEvaluationCause("infra-ext"), prior)
	})

	t.Run("a legacy hook beside a hook that reported complete", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		addPreBuildDependency(t, projectPath, "legacy-ext")
		writeCommittedRoutes(t, projectPath, prior)
		writeCommittedInfra(t, projectPath)
		ran := map[string]bool{}
		mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
			hookCtx, err := readMockedHookContext(args)
			if err != nil {
				return nil, err
			}
			ran[hookCtx.Extension] = true
			if hookCtx.Extension == "legacy-ext" {
				return &exec.Result{Success: true, ExitCode: 0, Stdout: legacyHookSummary + "\n"}, nil
			}
			return completeRouteRun(hookCtx.ProjectRoot)
		})

		assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeHTTPRoutesUnavailable,
			"serves: "+missingRouteEvaluationCause("legacy-ext"), prior)
		if !ran["infra-ext"] || !ran["legacy-ext"] {
			t.Fatalf("hooks run = %v, want both", ran)
		}
	})

	t.Run("a malformed route evaluation", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, prior)
		mockRouteHookSummary(t, projectPath, map[string]string{"web": webRoutesFragment}, `{"type":"summary","data":{"httpRoutes":null}}`)

		assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeHTTPRoutesUnavailable,
			"serves: hook infra-ext reported a malformed route evaluation: data.httpRoutes is not an object; upgrade infra-ext", prior)
	})
}

// Solo and batched generate fail the same way: both keep the committed route
// inventory and infra requirements, and the error diagnostic solo emits on its
// stream is the one the batch carries in that member's own diagnostics, byte
// for byte, with the failure's own code, although the two runs live under
// different roots. A sibling in the same batch whose hook reported complete
// still commits its own facts.
func TestDoGenerate_RouteProducerFailureIsBatchEqualToSolo(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "unavailable-route-producers-fail-generate")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "a-missing-or-malformed-route-evaluation-fails-naming-the-hook")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "a-failed-generate-preserves-the-committed-route-and-infra-facts")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "a-batched-generate-commits-the-solo-bytes")
	prior := canonicalRouteInventory(t, webRoutesFragment, applicationRoutesFragment)

	cases := []struct {
		name  string
		run   hookRun
		code  errs.Code
		cause string
	}{
		{"a hook reports its routes unavailable", unavailableRouteRun, errs.CodeHTTPRoutesUnavailable,
			"hook infra-ext reported its routes unavailable: the application factory failed"},
		{"a hook reports no route evaluation", legacyRouteRun, errs.CodeHTTPRoutesUnavailable,
			missingRouteEvaluationCause("infra-ext")},
		{"the Application factory throws", factoryErrorRun, errs.CodeGenerateFailed, factoryErrorHookMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockBunResolution(t)

			soloCtx, soloPath := newInfraHookProject(t)
			writeCommittedRoutes(t, soloPath, prior)
			writeCommittedInfra(t, soloPath)
			mockHookRun(t, tc.run)
			var status string
			var soloErr error
			events := captureEvents(t, func() {
				status, _, soloErr = runBuildGenerate(soloCtx, jsonl.New(), nil)
			})
			if soloErr == nil || status != "FAILED" {
				t.Fatalf("solo generate = (%q, %v), want FAILED", status, soloErr)
			}
			diagnostic := findEvent(events, func(e map[string]any) bool { return e["type"] == "diagnostic" })
			if diagnostic == nil || diagnostic["code"] != tc.code.String() || diagnostic["severity"] != "error" {
				t.Fatalf("solo diagnostic event = %#v, want one %s error", diagnostic, tc.code)
			}
			soloMessage, _ := diagnostic["message"].(string)
			if !strings.Contains(soloMessage, tc.cause) {
				t.Fatalf("solo diagnostic message = %q, want it to contain %q", soloMessage, tc.cause)
			}
			if got := readCommittedRoutes(t, soloPath); string(got) != string(prior) {
				t.Fatalf("solo changed the committed inventory\ngot:\n%s", got)
			}
			if got, err := os.ReadFile(infra.ProjectRequirementsPath(soloPath)); err != nil || string(got) != priorInfraRequirements {
				t.Fatalf("solo changed the committed infra requirements (%v)\ngot:\n%s", err, got)
			}

			batchCtx, batchPath := newInfraHookProject(t)
			writeCommittedRoutes(t, batchPath, prior)
			writeCommittedInfra(t, batchPath)
			siblingPath := addInfraHookProject(t, batchCtx, "sibling")
			mockHookRun(t, func(projectPath string) (*exec.Result, error) {
				if projectPath == siblingPath {
					return completeRouteRun(projectPath)
				}
				return tc.run(projectPath)
			})
			batchCtx.SelectedProjects = []pctx.ProjectRef{
				{ID: "/project", Name: "@test/pkg", Path: "project", FullPath: batchPath},
				{ID: "/sibling", Name: "@test/sibling", Path: "sibling", FullPath: siblingPath},
			}
			status, data, err := runBuildGenerateBatch(batchCtx)
			if err != nil || status != "OK" {
				t.Fatalf("batch generate = (%q, %v), want a per-project failure inside an OK batch", status, err)
			}
			results := batchResultsFor(t, data)
			failed, ok := resultByID(results, "/project")
			if !ok || failed.Status != "FAILED" || failed.Data != nil {
				t.Fatalf("batch results = %+v, want /project FAILED without data", results)
			}
			if got := readCommittedRoutes(t, batchPath); string(got) != string(prior) {
				t.Fatalf("batch changed the committed inventory\ngot:\n%s", got)
			}
			if got, err := os.ReadFile(infra.ProjectRequirementsPath(batchPath)); err != nil || string(got) != priorInfraRequirements {
				t.Fatalf("batch changed the committed infra requirements (%v)\ngot:\n%s", err, got)
			}
			want := buildBatchDiagnostic{Category: tc.code.String(), Severity: "error", Description: soloMessage}
			if got := failed.Diagnostics; len(got) != 1 || got[0] != want {
				t.Fatalf("batch diagnostics = %+v, want [%+v]", got, want)
			}

			sibling, ok := resultByID(results, "/sibling")
			if !ok || sibling.Status != "OK" || len(sibling.Diagnostics) != 0 {
				t.Fatalf("batch results = %+v, want /sibling OK without diagnostics", results)
			}
			if got, want := readCommittedRoutes(t, siblingPath), canonicalRouteInventory(t, webRoutesFragment); string(got) != string(want) {
				t.Fatalf("the sibling did not commit its own inventory\ngot:\n%s\nwant:\n%s", got, want)
			}
			if manifest := readCommittedInfra(t, siblingPath); len(manifest.Databases) != 1 || manifest.Databases[0].Name != "primary" {
				t.Fatalf("sibling databases = %+v, want its own primary requirement", manifest.Databases)
			}
		})
	}
}

// A run whose hooks failed, could not run, or produced an invalid aggregate
// fails and leaves the committed route inventory and infra requirements
// exactly as they stand: an absent or broken producer is never evidence that
// the project serves no route.
func TestDoGenerate_PreservesTheRouteInventoryWhenHooksFailOrDoNotRun(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-route-inventory", "a-failed-generate-preserves-the-committed-route-and-infra-facts")
	prior := canonicalRouteInventory(t, webRoutesFragment, applicationRoutesFragment)

	assertPreserved := func(t *testing.T, projectPath string) {
		t.Helper()
		if got := readCommittedRoutes(t, projectPath); string(got) != string(prior) {
			t.Fatalf("committed inventory changed\ngot:\n%s\nwant:\n%s", got, prior)
		}
	}

	t.Run("bun is not resolvable for a project with a hook", func(t *testing.T) {
		failBunResolution(t)
		refuseProcesses(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, prior)
		writeCommittedInfra(t, projectPath)

		assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeGenerateFailed,
			"pre-build hooks of infra-ext cannot run without Bun: bun not found", prior)
	})

	t.Run("a hook fails", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, prior)
		writeCommittedInfra(t, projectPath)
		mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
			return &exec.Result{Success: false, ExitCode: 1, Stderr: "hook failed"}, nil
		})

		assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeGenerateFailed, "hook failed", prior)
	})

	t.Run("the Application factory throws after a producer wrote a fragment and a requirement", func(t *testing.T) {
		mockBunResolution(t)
		ctx, projectPath := newInfraHookProject(t)
		writeCommittedRoutes(t, projectPath, prior)
		writeCommittedInfra(t, projectPath)
		mockHookRun(t, factoryErrorRun)

		assertGenerateFailsWithoutPublishing(t, ctx, projectPath, errs.CodeGenerateFailed, factoryErrorHookMessage, prior)
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

	for name, resolveBun := range map[string]func(*testing.T){"Bun present": mockBunResolution, "Bun absent": failBunResolution} {
		t.Run("a dependency is not installed/"+name, func(t *testing.T) {
			ctx, projectPath := setupUninstalledDependencyProject(t)
			resolveBun(t)
			writeCommittedRoutes(t, projectPath, prior)

			if _, err := doGenerate(ctx, jsonl.New(), projectPath); err == nil || !strings.Contains(err.Error(), "@putnami/application") {
				t.Fatalf("doGenerate error = %v, want one naming the uninstalled @putnami/application", err)
			}
			assertPreserved(t, projectPath)
		})
	}
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
