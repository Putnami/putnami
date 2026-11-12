package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/clientgen/extension/internal/workspaceclient"
)

const clientgenFeature = "tooling/cross-language-rest-clients"

func TestCommandHandlersExposeEveryDeclaredRuntimeAction(t *testing.T) {
	handlers := commandHandlers()
	got := make([]string, 0, len(handlers))
	for name := range handlers {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"generate-go", "generate-module", "generate-ts", "workspace-check"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime handlers = %v, want %v", got, want)
	}
}

// A client whose provider did not resolve fails and names the service it looked
// for, and a binding of the other language is refused, before anything runs.
func TestGenerateClientFailsNamingTheServiceWhenNoProviderResolved(t *testing.T) {
	root := t.TempDir()
	base := pctx.Project{Name: "items-client", Path: "clients/items", FullPath: filepath.Join(root, "clients", "items")}

	status, _, err := generateClient(pctx.GeneratedLanguageGo)(&pctx.Context{WorkspaceRoot: root, Project: base}, nil, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "generatedFrom") {
		t.Fatalf("no binding = status %q err %v, want a failure naming generatedFrom", status, err)
	}

	unresolved := base
	unresolved.GeneratedFrom = &pctx.GeneratedFrom{Service: "catalog.items", Language: pctx.GeneratedLanguageGo}
	status, _, err = generateClient(pctx.GeneratedLanguageGo)(&pctx.Context{WorkspaceRoot: root, Project: unresolved}, nil, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), `"catalog.items"`) {
		t.Fatalf("unresolved provider = status %q err %v, want a failure naming the service", status, err)
	}

	status, _, err = generateClient(pctx.GeneratedLanguageTypeScript)(&pctx.Context{WorkspaceRoot: root, Project: unresolved}, nil, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), `"go" client`) {
		t.Fatalf("language mismatch = status %q err %v", status, err)
	}
}

// The client generates itself: the TypeScript task runs the installed emitter
// on the provider's committed contract, at the path the core resolved, with
// the client's own directory as output and its own package name. Nothing is
// written into the provider.
func TestGenerateClientRunsTheEmitterOnTheProvidersCommittedContract(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "client-owned-generation", "a-generatedfrom-client-generates-itself-from-its-providers-committed-contract")
	spectest.Proves(t, clientgenFeature, "client-owned-generation", "no-project-writes-into-another")
	spectest.Proves(t, clientgenFeature, "spec-mediated-generation", "generation-reads-the-committed-contract-and-builds-no-provider")
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable script")
	}
	root := t.TempDir()
	writeMainTestFile(t, root, "services/catalog/schema/openapi.json", `{"openapi":"3.1.0"}`)
	writeMainTestFile(t, root, "clients/items/package.json", `{"name":"@example/items-client"}`)
	capture := filepath.Join(root, "calls")
	writeMainTestFile(t, root, "node_modules/.bin/putnami-client-generate",
		"#!/bin/sh\nprintf '%s\\n' \"$*\" > \""+capture+"\"\ncat \"$2\" > \""+capture+".contract\"\n")
	if err := os.Chmod(filepath.Join(root, "node_modules", ".bin", "putnami-client-generate"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME", "")
	if err := os.Unsetenv("PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME"); err != nil {
		t.Fatal(err)
	}
	provider := readMainTree(t, filepath.Join(root, "services"))
	client := filepath.Join(root, "clients", "items")
	status, output, err := generateClient(pctx.GeneratedLanguageTypeScript)(&pctx.Context{
		WorkspaceRoot: root,
		Project: pctx.Project{Name: "@example/items-client", Path: "clients/items", FullPath: client,
			GeneratedFrom: &pctx.GeneratedFrom{Service: "catalog.items", Language: pctx.GeneratedLanguageTypeScript,
				ContractPath: "services/catalog/schema/openapi.json", ContractSHA256: sha256Hex(`{"openapi":"3.1.0"}`)}},
	}, nil, nil)
	if err != nil || status != "OK" || len(output) != 0 {
		t.Fatalf("generate = status %q output %v err %v", status, output, err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	contract, rest, found := strings.Cut(strings.TrimPrefix(strings.TrimSpace(string(data)), "--contract "), " ")
	want := "--output " + client + " --package-name @example/items-client --format-project " + client
	if !found || rest != want {
		t.Fatalf("emitter call = %q, want --contract <the keyed contract> %s", data, want)
	}
	if read, err := os.ReadFile(capture + ".contract"); err != nil || string(read) != `{"openapi":"3.1.0"}` {
		t.Fatalf("the emitter read %q (err %v), want the provider's committed contract", read, err)
	}
	if _, err := os.Stat(contract); !os.IsNotExist(err) {
		t.Errorf("the keyed contract copy %s outlived the generation (stat: %v)", contract, err)
	}
	if after := readMainTree(t, filepath.Join(root, "services")); !reflect.DeepEqual(after, provider) {
		t.Fatalf("the provider's tree changed: %v, want %v", after, provider)
	}
}

// The contract edge orders no job, so a provider build may rewrite the
// contract during the session. A client generated from other bytes than its
// key holds is never produced: the generation fails, names the contract, and
// runs no emitter.
func TestGenerateClientRefusesAContractItsKeyDoesNotHold(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "client-owned-generation", "a-client-generates-only-from-the-contract-its-key-holds")
	root := t.TempDir()
	writeMainTestFile(t, root, "services/items/schema/openapi.json", `{"openapi":"3.1.0","paths":{"/new":{}}}`)
	writeMainTestFile(t, root, "clients/items/package.json", `{"name":"@example/items-client"}`)
	capture := filepath.Join(root, "calls")
	writeMainTestFile(t, root, "node_modules/.bin/putnami-client-generate", "#!/bin/sh\ntouch \""+capture+"\"\n")
	if err := os.Chmod(filepath.Join(root, "node_modules", ".bin", "putnami-client-generate"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{pctx.GeneratedLanguageGo, pctx.GeneratedLanguageTypeScript} {
		status, _, err := generateClient(language)(&pctx.Context{
			WorkspaceRoot: root,
			Project: pctx.Project{Name: "items", Path: "clients/items", FullPath: filepath.Join(root, "clients", "items"),
				GeneratedFrom: &pctx.GeneratedFrom{Service: "items", Language: language,
					ContractPath: "services/items/schema/openapi.json", ContractSHA256: sha256Hex(`{"openapi":"3.1.0"}`)}},
		}, nil, nil)
		if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "services/items/schema/openapi.json changed") {
			t.Fatalf("%s: rewritten contract = status %q err %v, want a failure naming the contract", language, status, err)
		}
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatalf("the emitter ran on a contract the key does not hold (stat: %v)", err)
	}
}

func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// The TypeScript package name is the client's own: a package.json without a
// name fails rather than inventing one.
func TestGenerateClientRequiresTheClientsOwnPackageName(t *testing.T) {
	root := t.TempDir()
	writeMainTestFile(t, root, "clients/items/package.json", `{"private":true}`)
	writeMainTestFile(t, root, "services/items/schema/openapi.json", `{"openapi":"3.1.0"}`)
	status, _, err := generateClient(pctx.GeneratedLanguageTypeScript)(&pctx.Context{
		WorkspaceRoot: root,
		Project: pctx.Project{Name: "items", Path: "clients/items", FullPath: filepath.Join(root, "clients", "items"),
			GeneratedFrom: &pctx.GeneratedFrom{Service: "items", Language: pctx.GeneratedLanguageTypeScript,
				ContractPath: "services/items/schema/openapi.json"}},
	}, nil, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "declares no name") {
		t.Fatalf("nameless package = status %q err %v", status, err)
	}
}

// The in-module target refuses a clientgen.json it cannot honor, and an
// output outside the project, before running the emitter.
func TestGenerateModuleTargetRefusesATargetOutsideTheProvidersModule(t *testing.T) {
	cases := map[string]string{
		"a TypeScript target":   `{"targets":["ts"],"ts":{"output":"clients/ts"}}`,
		"a module of its own":   `{"targets":["go"],"go":{"output":"clients/go","modulePath":"example.dev/items-client"}}`,
		"an escaping directory": `{"targets":["go"],"go":{"output":"../elsewhere"}}`,
		"the project root":      `{"targets":["go"],"go":{"output":"."}}`,
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeMainTestFile(t, root, "service/clientgen.json", config)
			writeMainTestFile(t, root, "service/schema/openapi.json", `{"openapi":"3.1.0"}`)
			status, _, err := generateModuleTarget(&pctx.Context{WorkspaceRoot: root,
				Project: pctx.Project{Name: "service", Path: "service", FullPath: filepath.Join(root, "service")}}, nil, nil)
			if status != "FAILED" || err == nil {
				t.Fatalf("status %q err %v, want a failure", status, err)
			}
		})
	}
}

// The provider generates its in-module Go client from its own committed
// contract, into the directory clientgen.json names, and reports the one file
// it owns there.
func TestGenerateModuleTargetWritesInsideTheProviderAndReportsItsFile(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "dynamic-output-ownership", "a-provider-owns-its-in-module-target-directory")
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable script")
	}
	root := t.TempDir()
	capture := filepath.Join(root, "calls")
	installGoEmitterFixture(t, "#!/bin/sh\nprintf '%s\\n' \"$*\" > \""+capture+"\"\n")
	writeMainTestFile(t, root, "service/clientgen.json",
		`{"targets":["go"],"go":{"output":"clients/go","packageName":"itemsclient","clientName":"ItemsClient","omitOperations":["b","a"]}}`)
	writeMainTestFile(t, root, "service/schema/openapi.json", `{"openapi":"3.1.0"}`)
	project := filepath.Join(root, "service")
	status, output, err := generateModuleTarget(&pctx.Context{WorkspaceRoot: root,
		Project: pctx.Project{Name: "service", Path: "service", FullPath: project}}, nil, nil)
	if err != nil || status != "OK" || output["moduleClientOutput"] != "clients/go/client.gen.go" {
		t.Fatalf("generate = status %q output %v err %v", status, output, err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := "--contract " + filepath.Join(project, "schema", "openapi.json") + " --output " + filepath.Join(project, "clients", "go") +
		" --package itemsclient --client ItemsClient --omit a --omit b"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("emitter call = %q, want %q", got, want)
	}
}

func TestCheckHandlerRejectsAMissingRootAndAMissingMembership(t *testing.T) {
	emitter := jsonl.NewForVersion(1)
	status, _, err := runCheck(&pctx.Context{}, emitter, nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "workspace root") {
		t.Fatalf("missing root result = status %q err %v", status, err)
	}

	// The check takes the membership from the job context and never from the
	// ignored project index, so a context without one is a finding the run
	// exits on, reported like every other verdict: through the report and the
	// diagnostics, never as a bare error that loses the count. An index on
	// disk does not stand in for it.
	root := t.TempDir()
	writeMainTestFile(t, root, ".putnami/workspace-index.json", `{"version":4,"projects":[{"path":"services/catalog"}]}`)
	writeMainTestFile(t, root, "services/catalog/putnami.json", `{"name":"catalog"}`)
	status, _, err = runCheck(&pctx.Context{WorkspaceRoot: root}, emitter, nil)
	if status != "FAILED" || err == nil {
		t.Fatalf("missing membership result = status %q err %v", status, err)
	}
	report := workspaceclient.InspectCommitted(root, memberPaths(nil))
	membershipReported := false
	for _, finding := range report.Findings {
		if finding.Code == "clientgen.discovery" && strings.Contains(finding.Message, "workspace membership") {
			membershipReported = true
		}
	}
	if !membershipReported {
		t.Fatalf("a missing workspace membership was not reported as a discovery finding: %+v", report.Findings)
	}

	// With the membership the context carries, the same workspace is clean.
	members := []pctx.ProjectRef{{Name: "catalog", Path: "services/catalog"}}
	status, _, err = runCheck(&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: members}, emitter, nil)
	if status != "OK" || err != nil {
		t.Fatalf("context membership result = status %q err %v", status, err)
	}
}

// installGoEmitterFixture places a stand-in for the packaged Go emitter beside
// the running test binary, where the runtime resolves it, and removes it when
// the test ends.
func installGoEmitterFixture(t *testing.T, script string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(executable), "putnami-client-generate-go")
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s already exists; the fixture would replace it", path)
	}
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test-owned fixture
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

func readMainTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path) //nolint:gosec // test-owned temporary tree
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func writeMainTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
