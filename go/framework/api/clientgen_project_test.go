package api

import (
	"go.putnami.dev/protocol/features/spectest"

	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// minimalSpec is a one-operation OpenAPI document that ReadOpenAPISpec turns into
// a single service with one method — enough to exercise the full emit path.
const minimalSpec = `{
  "openapi": "3.0.3",
  "info": { "title": "Items", "version": "1.0.0" },
  "paths": {
    "/items/{id}": {
      "get": {
        "operationId": "getItem",
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string" } }
        ],
        "responses": { "204": { "description": "No Content" } }
      }
    }
  }
}`

const goTargetConfig = `{
  "targets": ["go"],
  "thirdParty": true,
  "ts": { "output": "clients/ts", "packageName": "" },
  "go": { "output": "clients/go", "modulePath": "example.com/svc/clients/go", "packageName": "itemsclient", "clientName": "ItemsClient" }
}`

const strictMinimalSpec = `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:items"},"credentials":{}},"paths":{"/items/{id}":{"get":{"operationId":"getItem","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"204":{"description":"No Content"}},"x-putnami-client":{"stream":"unary","transports":[{"protocol":"rest-json","path":"/items/{id}","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"}}}}}}`

const strictEmptySpec = `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:items"},"credentials":{}},"paths":{}}`

// writeProjectFile writes content at projectRoot/rel, creating parent dirs.
func writeProjectFile(t *testing.T, projectRoot, rel, content string) {
	t.Helper()
	path := filepath.Join(projectRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// providerRoot returns the root of a provider project, "svc", inside a
// fresh workspace, so a scaffolded client can name its provider "/svc".
func providerRoot(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	writeProjectFile(t, workspace, workspaceConfigFile, `{"name":"w"}`)
	return filepath.Join(workspace, "svc")
}

func readProjectFile(t *testing.T, projectRoot, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

func TestGenerateProjectClients_EmitsStandaloneGoModule(t *testing.T) {
	root := providerRoot(t)
	writeProjectFile(t, root, ".gen/clientgen/config.json", goTargetConfig)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)

	res, err := GenerateProjectClients(root)
	if err != nil {
		t.Fatalf("GenerateProjectClients: %v", err)
	}
	if !res.Generated {
		t.Fatalf("Generated = false, want true")
	}
	if res.OutputDir != "clients/go" {
		t.Fatalf("OutputDir = %q, want clients/go", res.OutputDir)
	}
	wantFiles := map[string]bool{"clients/go/client.gen.go": true, "clients/go/go.mod": true, "clients/go/putnami.json": true}
	for _, f := range res.Files {
		if !wantFiles[f] {
			t.Errorf("unexpected emitted file %q", f)
		}
		delete(wantFiles, f)
	}
	if len(wantFiles) != 0 {
		t.Errorf("missing emitted files: %v", wantFiles)
	}

	client := readProjectFile(t, root, "clients/go/client.gen.go")
	if !strings.Contains(client, "package itemsclient") {
		t.Errorf("client.gen.go missing package itemsclient:\n%s", client)
	}
	if !strings.Contains(client, "ItemsClient") {
		t.Errorf("client.gen.go missing ItemsClient struct")
	}

	gomod := readProjectFile(t, root, "clients/go/go.mod")
	if !strings.Contains(gomod, "module example.com/svc/clients/go") {
		t.Errorf("go.mod missing module path:\n%s", gomod)
	}
	if !strings.Contains(gomod, "go.putnami.dev/client v0.0.1") {
		t.Errorf("go.mod missing framework client require:\n%s", gomod)
	}
	var project struct {
		Name             string            `json:"name"`
		Extensions       []string          `json:"extensions"`
		Dependencies     []string          `json:"dependencies"`
		FeatureAuthority map[string]string `json:"featureAuthority"`
	}
	if err := json.Unmarshal([]byte(readProjectFile(t, root, "clients/go/putnami.json")), &project); err != nil {
		t.Fatalf("parse generated project metadata: %v", err)
	}
	if want := map[string]string{"provider": "/svc"}; !reflect.DeepEqual(project.FeatureAuthority, want) {
		t.Fatalf("generated featureAuthority = %v, want %v", project.FeatureAuthority, want)
	}
	if project.Name != "example.com/svc/clients/go" || len(project.Extensions) != 1 || project.Extensions[0] != "@putnami/go" {
		t.Fatalf("generated project metadata = %#v", project)
	}
	if len(project.Dependencies) != 0 {
		t.Fatalf("generated external project leaked repository-local dependencies: %#v", project.Dependencies)
	}
}

func TestGenerateProjectClients_EmitsValidatedFirstPartyManifest(t *testing.T) {
	root := providerRoot(t)
	cfg := strings.Replace(goTargetConfig, "\n  \"thirdParty\": true,", "", 1)
	cfg = strings.Replace(cfg, `"modulePath": "example.com/svc/clients/go"`, `"modulePath": ""`, 1)
	writeProjectFile(t, root, ".gen/clientgen/config.json", cfg)
	writeProjectFile(t, root, ".gen/schema/openapi.json", strictMinimalSpec)
	writeProjectFile(t, root, "go.mod", "module example.com/provider\n\ngo 1.25\n")

	result, err := GenerateProjectClients(root)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes := []byte(readProjectFile(t, root, "clients/go/"+clientcontract.GeneratedManifestFile))
	manifest, diagnostics := clientcontract.ParseAndValidateGeneratedManifest(manifestBytes)
	if len(diagnostics) != 0 {
		t.Fatalf("manifest diagnostics = %v", diagnostics)
	}
	wantContractHash := sha256.Sum256([]byte(strictMinimalSpec))
	if manifest.ContractSHA256 != fmt.Sprintf("%x", wantContractHash) {
		t.Fatalf("contract hash = %s", manifest.ContractSHA256)
	}
	if manifest.Binding.ImportPath != "example.com/provider/clients/go" || len(manifest.Binding.Clients) != 1 ||
		manifest.Binding.Clients[0].BindingSymbol != "RegisterItemsClient" {
		t.Fatalf("binding = %#v", manifest.Binding)
	}
	if len(manifest.Operations) != 1 || manifest.Operations[0].OperationID != "getItem" ||
		manifest.Operations[0].MethodSymbol != "GetItem" || manifest.Operations[0].Service != "items" {
		t.Fatalf("operations = %#v", manifest.Operations)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "client.gen.go" {
		t.Fatalf("files = %#v", manifest.Files)
	}
	found := false
	for _, path := range result.Files {
		found = found || path == "clients/go/"+clientcontract.GeneratedManifestFile
	}
	if !found {
		t.Fatalf("result files omitted manifest: %v", result.Files)
	}
}

func TestGenerateProjectClients_PrefersFreshGeneratedContract(t *testing.T) {
	root := providerRoot(t)
	writeProjectFile(t, root, ".gen/clientgen/config.json", goTargetConfig)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)
	fresh := strings.ReplaceAll(minimalSpec, "/items/{id}", "/fresh/{id}")
	writeProjectFile(t, root, ".gen/schema/openapi.json", fresh)
	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatal(err)
	}
	source := readProjectFile(t, root, "clients/go/client.gen.go")
	if !strings.Contains(source, "/fresh/{id}") || strings.Contains(source, "/items/{id}") {
		t.Fatalf("client was not emitted from fresh .gen contract:\n%s", source)
	}
}

// A project that commits its generation contract, such as a provider slice
// with no app, keeps it at clientgen.json: build-describe empties
// .gen/clientgen, so a contract committed there would be gone before the
// clientgen task reads it. A contract a build wrote still wins.
func TestGenerateProjectClients_ReadsTheCommittedContractWhenNoBuildWroteOne(t *testing.T) {
	root := providerRoot(t)
	writeProjectFile(t, root, CommittedClientGenConfigPath, goTargetConfig)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)

	res, err := GenerateProjectClients(root)
	if err != nil {
		t.Fatalf("GenerateProjectClients: %v", err)
	}
	if !res.Generated || res.OutputDir != "clients/go" {
		t.Fatalf("result = %+v, want a client emitted from the committed clientgen.json", res)
	}
	if source := readProjectFile(t, root, "clients/go/client.gen.go"); !strings.Contains(source, "package itemsclient") {
		t.Fatalf("client.gen.go does not follow the committed contract:\n%s", source)
	}

	if err := os.RemoveAll(filepath.Join(root, "clients")); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ".gen/clientgen/config.json",
		`{"targets":["ts"],"ts":{"output":"clients/ts","packageName":"@x/c"},"go":{"output":"clients/go","modulePath":"","packageName":"client","clientName":"Client"}}`)
	res, err = GenerateProjectClients(root)
	if err != nil {
		t.Fatalf("GenerateProjectClients with a built contract: %v", err)
	}
	if res.Generated {
		t.Fatal("the committed clientgen.json won over the contract a build wrote")
	}
}

// The spec pairs with the contract's origin. A committed clientgen.json reads
// the committed schema/openapi.json only, even when an earlier build left a
// different .gen/schema/openapi.json: no cache key covers that file in a
// project whose build does not write the contract. A contract a build wrote
// still prefers the fresh .gen spec.
func TestGenerateProjectClients_ACommittedContractReadsOnlyTheCommittedSpec(t *testing.T) {
	root := providerRoot(t)
	writeProjectFile(t, root, CommittedClientGenConfigPath, goTargetConfig)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)
	writeProjectFile(t, root, ".gen/schema/openapi.json", strings.ReplaceAll(minimalSpec, "/items/{id}", "/stale/{id}"))

	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatalf("GenerateProjectClients: %v", err)
	}
	source := readProjectFile(t, root, "clients/go/client.gen.go")
	if !strings.Contains(source, "/items/{id}") || strings.Contains(source, "/stale/{id}") {
		t.Fatalf("a committed contract did not generate from the committed spec:\n%s", source)
	}

	if err := os.Remove(filepath.Join(root, "schema", "openapi.json")); err != nil {
		t.Fatal(err)
	}
	_, err := GenerateProjectClients(root)
	if err == nil || !strings.Contains(err.Error(), "must commit its OpenAPI spec as schema/openapi.json") {
		t.Fatalf("a committed contract without a committed spec: err = %v, want it to name schema/openapi.json", err)
	}

	writeProjectFile(t, root, ".gen/clientgen/config.json", goTargetConfig)
	if _, err = GenerateProjectClients(root); err != nil {
		t.Fatalf("GenerateProjectClients with a built contract: %v", err)
	}
	if source = readProjectFile(t, root, "clients/go/client.gen.go"); !strings.Contains(source, "/stale/{id}") {
		t.Fatalf("a built contract did not generate from the .gen spec:\n%s", source)
	}
}

func TestGenerateProjectClients_RemovesOnlyUnmodifiedManifestOwnedFiles(t *testing.T) {
	root := providerRoot(t)
	cfg := strings.Replace(goTargetConfig, "\n  \"thirdParty\": true,", "", 1)
	writeProjectFile(t, root, ".gen/clientgen/config.json", cfg)
	writeProjectFile(t, root, ".gen/schema/openapi.json", strictMinimalSpec)
	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ".gen/schema/openapi.json", strictEmptySpec)
	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"clients/go/client.gen.go", "clients/go/" + clientcontract.GeneratedManifestFile} {
		if _, err := os.Stat(filepath.Join(root, file)); !os.IsNotExist(err) {
			t.Fatalf("owned generated file remains: %s", file)
		}
	}

	writeProjectFile(t, root, ".gen/schema/openapi.json", strictMinimalSpec)
	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, "clients/go/client.gen.go", "hand edited\n")
	writeProjectFile(t, root, ".gen/schema/openapi.json", strictEmptySpec)
	if _, err := GenerateProjectClients(root); err == nil || !strings.Contains(err.Error(), "refuse to remove modified generated file") {
		t.Fatalf("modified generated file removal error = %v", err)
	}
}

func TestGenerateProjectClients_OutputChangeRemovesPriorOwnedTarget(t *testing.T) {
	root := providerRoot(t)
	cfg := strings.Replace(goTargetConfig, "\n  \"thirdParty\": true,", "", 1)
	writeProjectFile(t, root, ".gen/clientgen/config.json", cfg)
	writeProjectFile(t, root, ".gen/schema/openapi.json", strictMinimalSpec)
	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(cfg, `"output": "clients/go"`, `"output": "clients/new-go"`, 1)
	writeProjectFile(t, root, ".gen/clientgen/config.json", changed)
	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"clients/go/client.gen.go", "clients/go/" + clientcontract.GeneratedManifestFile} {
		if _, err := os.Stat(filepath.Join(root, file)); !os.IsNotExist(err) {
			t.Fatalf("prior owned output remains: %s", file)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "clients/go/go.mod")); err != nil {
		t.Fatalf("scaffold-once go.mod must not be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "clients/new-go/client.gen.go")); err != nil {
		t.Fatalf("new generated output missing: %v", err)
	}
}

func TestGenerateProjectClients_FirstPartyDefaultRejectsUnmarkedSpec(t *testing.T) {
	root := providerRoot(t)
	cfg := strings.Replace(goTargetConfig, "\n  \"thirdParty\": true,", "", 1)
	writeProjectFile(t, root, ".gen/clientgen/config.json", cfg)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)

	_, err := GenerateProjectClients(root)
	if err == nil || !strings.Contains(err.Error(), "requires x-putnami-client") {
		t.Fatalf("GenerateProjectClients error = %v, want strict first-party marker failure", err)
	}
}

func TestGenerateProjectClients_OutputIsGofmtCleanAndParses(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "the-output-is-gofmt-clean-and-compilable")
	root := providerRoot(t)
	writeProjectFile(t, root, ".gen/clientgen/config.json", goTargetConfig)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)

	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatalf("GenerateProjectClients: %v", err)
	}
	src := readProjectFile(t, root, "clients/go/client.gen.go")

	if _, err := parser.ParseFile(token.NewFileSet(), "client.gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("emitted client does not parse as Go:\n%s\n--- error: %v", src, err)
	}
	// The committed client must already be gofmt-clean so a build's lint --fix is a
	// no-op — otherwise the formatter and the clientsync guard fight forever
	// (the bug this whole path was hardened against).
	formatted, err := format.Source([]byte(src))
	if err != nil {
		t.Fatalf("format emitted client: %v", err)
	}
	if string(formatted) != src {
		t.Errorf("emitted client is not gofmt-clean (lint --fix would rewrite it)")
	}
}

func TestGenerateProjectClients_ScaffoldOncePreservesGoMod(t *testing.T) {
	root := providerRoot(t)
	writeProjectFile(t, root, ".gen/clientgen/config.json", goTargetConfig)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)

	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// A hand-edited go.mod (e.g. a pinned require) must survive regeneration, the
	// way the same-language mirror step preserves a committed module file.
	edited := "module example.com/svc/clients/go\n\ngo 1.25\n\nrequire go.putnami.dev/client v1.2.3\n"
	writeProjectFile(t, root, "clients/go/go.mod", edited)

	res, err := GenerateProjectClients(root)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	// go.mod was already present, so it is not re-listed as a written file.
	for _, f := range res.Files {
		if f == "clients/go/go.mod" {
			t.Errorf("go.mod rewritten on second run; want preserved")
		}
	}
	if got := readProjectFile(t, root, "clients/go/go.mod"); got != edited {
		t.Errorf("go.mod not preserved:\ngot:\n%s\nwant:\n%s", got, edited)
	}
	if !strings.Contains(readProjectFile(t, root, "clients/go/client.gen.go"), "package itemsclient") {
		t.Errorf("client.gen.go should still be (re)generated on the second run")
	}
}

func TestGenerateProjectClients_DeterministicAcrossRuns(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "generated-from-contract", "generating-twice-is-byte-identical")
	root := providerRoot(t)
	writeProjectFile(t, root, ".gen/clientgen/config.json", goTargetConfig)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)

	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := readProjectFile(t, root, "clients/go/client.gen.go")
	if _, err := GenerateProjectClients(root); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second := readProjectFile(t, root, "clients/go/client.gen.go"); second != first {
		t.Errorf("client.gen.go not byte-stable across runs")
	}
}

func TestGenerateProjectClients_PackageModeOmitsGoMod(t *testing.T) {
	root := providerRoot(t)
	// Empty modulePath = package mode (the same-language case where the client is a
	// package inside the provider's own module): no go.mod is scaffolded.
	cfg := strings.Replace(goTargetConfig, `"modulePath": "example.com/svc/clients/go"`, `"modulePath": ""`, 1)
	writeProjectFile(t, root, ".gen/clientgen/config.json", cfg)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)

	res, err := GenerateProjectClients(root)
	if err != nil {
		t.Fatalf("GenerateProjectClients: %v", err)
	}
	if !res.Generated {
		t.Fatalf("Generated = false, want true")
	}
	if _, err := os.Stat(filepath.Join(root, "clients/go/go.mod")); !os.IsNotExist(err) {
		t.Errorf("go.mod was scaffolded in package mode (err=%v), want absent", err)
	}
	for _, f := range res.Files {
		if f == "clients/go/go.mod" {
			t.Errorf("go.mod reported in package mode")
		}
	}
}

func TestGenerateProjectClients_ReadsGzippedSpec(t *testing.T) {
	root := providerRoot(t)
	writeProjectFile(t, root, ".gen/clientgen/config.json", goTargetConfig)

	// Only the gzipped companion exists (the openapi output:false / runtime case).
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(minimalSpec)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	gzPath := filepath.Join(root, ".gen", "schema", "openapi.json.gz")
	if err := os.MkdirAll(filepath.Dir(gzPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(gzPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write gz: %v", err)
	}

	res, err := GenerateProjectClients(root)
	if err != nil {
		t.Fatalf("GenerateProjectClients: %v", err)
	}
	if !res.Generated {
		t.Fatalf("Generated = false; expected emission from gzipped spec")
	}
}

func TestGenerateProjectClients_NoopWhenNotConfigured(t *testing.T) {
	t.Run("no config", func(t *testing.T) {
		res, err := GenerateProjectClients(t.TempDir())
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if res.Generated {
			t.Errorf("Generated = true, want false when no clientgen config")
		}
	})

	t.Run("go target disabled", func(t *testing.T) {
		root := providerRoot(t)
		writeProjectFile(t, root, ".gen/clientgen/config.json",
			`{"targets":["ts"],"ts":{"output":"clients/ts","packageName":"@x/c"},"go":{"output":"clients/go","modulePath":"","packageName":"client","clientName":"Client"}}`)
		writeProjectFile(t, root, "schema/openapi.json", minimalSpec)
		res, err := GenerateProjectClients(root)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if res.Generated {
			t.Errorf("Generated = true, want false when go target disabled")
		}
		if _, err := os.Stat(filepath.Join(root, "clients/go")); !os.IsNotExist(err) {
			t.Errorf("clients/go created for a ts-only config")
		}
	})
}

func TestGenerateProjectClients_MissingSpecErrors(t *testing.T) {
	root := providerRoot(t)
	writeProjectFile(t, root, ".gen/clientgen/config.json", goTargetConfig)
	// No spec written.
	if _, err := GenerateProjectClients(root); err == nil {
		t.Fatalf("expected an error when the go target is enabled but no spec exists")
	}
}

func TestGenerateProjectClients_RejectsEscapingOutput(t *testing.T) {
	root := providerRoot(t)
	writeProjectFile(t, root, ".gen/clientgen/config.json",
		`{"targets":["go"],"ts":{"output":"clients/ts","packageName":""},"go":{"output":"../escape","modulePath":"x","packageName":"client","clientName":"Client"}}`)
	writeProjectFile(t, root, "schema/openapi.json", minimalSpec)
	if _, err := GenerateProjectClients(root); err == nil {
		t.Fatalf("expected an error for a go.output that escapes the project root")
	}
}

// A generated client names its provider by workspace project id: the path
// below the workspace root, without transparent group folders. Outside a
// workspace there is no id to name, so generation fails.
func TestProviderProjectID(t *testing.T) {
	workspace := t.TempDir()
	writeProjectFile(t, workspace, workspaceConfigFile, `{"name":"w"}`)
	for _, test := range []struct{ rel, want string }{
		{rel: "svc", want: "/svc"},
		{rel: "go/samples/task-api", want: "/go/samples/task-api"},
		{rel: "(apps)/billing", want: "/billing"},
	} {
		got, err := providerProjectID(filepath.Join(workspace, filepath.FromSlash(test.rel)))
		if err != nil || got != test.want {
			t.Errorf("providerProjectID(%s) = %q, %v; want %q", test.rel, got, err, test.want)
		}
	}
	if _, err := providerProjectID(workspace); err == nil {
		t.Error("the workspace root got a provider id")
	}

	outside := t.TempDir()
	writeProjectFile(t, outside, ".gen/clientgen/config.json", goTargetConfig)
	writeProjectFile(t, outside, "schema/openapi.json", minimalSpec)
	if _, err := GenerateProjectClients(outside); err == nil || !strings.Contains(err.Error(), workspaceConfigFile) {
		t.Fatalf("GenerateProjectClients outside a workspace: err = %v, want it to name %s", err, workspaceConfigFile)
	}
	if _, err := os.Stat(filepath.Join(outside, "clients", "go")); !os.IsNotExist(err) {
		t.Fatalf("a failed generation left clients/go behind: %v", err)
	}
}
