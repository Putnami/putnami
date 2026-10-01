package api

import (
	"go.putnami.dev/protocol/features/spectest"

	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Phase 3 / Task 4 — cross-language integration coverage (the Go half).
//
// The TypeScript service-to-service provider declares targets:['ts','go'] and
// commits its cross-language Go client. Here we regenerate that client from the
// provider's committed OpenAPI spec and assert client.gen.go matches the
// committed file. A TS-side API change that lands without re-running
// `putnami clientgen` must fail this test rather than ship a stale Go client.
//
// (The TS half — a Go provider's committed clients/ts — is guarded by a sibling
// bun test: typescript/framework/client/test/generator/cross-language-sync.test.ts.)
func TestCrossLanguage_TSProviderGoClientInSync(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "shared-contract-source", "committed-spec-and-committed-generated-clients-stay-in-sync")
	ws := repoRoot(t)
	sampleRoot := filepath.Join(ws, "typescript", "samples", "10-service-to-service")
	committed := filepath.Join(sampleRoot, "clients", "go", "client.gen.go")
	if _, err := os.Stat(committed); err != nil {
		t.Fatalf("expected committed cross-language client at %s: %v", committed, err)
	}

	// Mirror the provider's generate inputs (committed spec + the clientgen
	// contract its build emits) in a throwaway project, then regenerate.
	root := t.TempDir()
	spec, err := os.ReadFile(filepath.Join(sampleRoot, "schema", "openapi.json"))
	if err != nil {
		t.Fatalf("read provider spec: %v", err)
	}
	writeProjectFile(t, root, "schema/openapi.json", string(spec))
	writeProjectFile(t, root, ".gen/clientgen/config.json", crossLanguageClientgenConfig(t, spec))

	want, err := os.ReadFile(committed)
	if err != nil {
		t.Fatalf("read committed client: %v", err)
	}

	// The provider is a Putnami TypeScript service, so it is first-party by
	// nature. Until it publishes x-putnami-client, GenerateProjectClients refuses
	// provider generation rather than emitting an untyped client from a document
	// that declares no contract. The drift guard still runs: the committed client
	// is compared against what the emitter produces from the committed spec.
	var got []byte
	if firstParty := bytes.Contains(spec, []byte(`"x-putnami-client"`)); !firstParty {
		res, genErr := GenerateProjectClients(root)
		if genErr == nil {
			t.Fatalf("GenerateProjectClients accepted a provider spec with no first-party contract: %#v", res)
		}
		if !strings.Contains(genErr.Error(), "requires x-putnami-client") {
			t.Fatalf("GenerateProjectClients error = %v, want the missing-contract refusal", genErr)
		}
		got = regenerateLegacyProjectClient(t, root, spec)
	} else {
		res, genErr := GenerateProjectClients(root)
		if genErr != nil {
			t.Fatalf("GenerateProjectClients: %v", genErr)
		}
		if !res.Generated {
			t.Fatalf("Generated = false, want true")
		}
		// Compare client.gen.go (the generated source). go.mod is excluded: its
		// `go` directive tracks the building toolchain, not the spec.
		regenerated, readErr := os.ReadFile(filepath.Join(root, "clients", "go", "client.gen.go"))
		if readErr != nil {
			t.Fatalf("read regenerated client: %v", readErr)
		}
		got = regenerated
	}
	if string(got) != string(want) {
		t.Errorf("committed typescript/samples/10-service-to-service/clients/go is out of sync with the spec;\nrun `putnami clientgen` and commit the result")
	}
}

// regenerateLegacyProjectClient drives the same emission GenerateProjectClients
// performs, minus the ownership manifest that only a first-party contract can
// name. It exists so the committed cross-language client keeps a drift guard
// while its TypeScript provider is still migrating to the first-party contract.
func regenerateLegacyProjectClient(t *testing.T, root string, spec []byte) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ".gen", ClientGenConfigPath))
	if err != nil {
		t.Fatalf("read clientgen config: %v", err)
	}
	var cfg clientGenConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse clientgen config: %v", err)
	}
	ir, err := ReadOpenAPISpec(spec)
	if err != nil {
		t.Fatalf("ReadOpenAPISpec: %v", err)
	}
	source, err := GenerateClientFromIR(ir, ClientGenOptions{
		PackageName: cfg.Go.PackageName,
		ClientName:  cfg.Go.ClientName,
		Design:      clientDesignOptions(cfg.Design),
	})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	return []byte(source)
}

// repoRoot walks up from the working directory to the workspace root (the dir
// containing go.work) so the cross-language fixtures are located without a
// brittle relative path.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.work")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.work not found above the test working directory")
		}
		dir = parent
	}
}

// crossLanguageClientgenConfig builds the clientgen contract the provider's
// build emits, with the design table derived from the committed spec rather
// than copied into this file. A literal table silently stops describing the
// provider the moment it declares one more route, and the guard then compares
// the wrong contract — the defect this file carried twice already.
func crossLanguageClientgenConfig(t *testing.T, spec []byte) string {
	t.Helper()
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(spec, &document); err != nil {
		t.Fatalf("read provider spec paths: %v", err)
	}
	type designOperation struct {
		Method          string `json:"method"`
		Path            string `json:"path"`
		ProducerProject string `json:"producerProject"`
		ProducerFeature string `json:"producerFeature"`
	}
	operations := make([]designOperation, 0, len(document.Paths))
	for path, methods := range document.Paths {
		for method := range methods {
			operations = append(operations, designOperation{
				Method:          strings.ToUpper(method),
				Path:            path,
				ProducerProject: "@example/10-service-to-service",
				ProducerFeature: "items/manage",
			})
		}
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].Method != operations[j].Method {
			return operations[i].Method < operations[j].Method
		}
		return operations[i].Path < operations[j].Path
	})
	config := map[string]any{
		"targets": []string{"ts", "go"},
		"ts":      map[string]any{"output": "clients/ts", "packageName": "@example/items-client"},
		"go": map[string]any{
			"output": "clients/go", "modulePath": "go.putnami.dev/examples/ts-items-client",
			"packageName": "itemsclient", "clientName": "ItemsClient",
		},
		"design": map[string]any{"operations": operations},
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("encode clientgen contract: %v", err)
	}
	return string(encoded)
}
