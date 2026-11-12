package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const itemsContract = `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:items"},"credentials":{}},"paths":{` +
	`"/items/{id}":{"get":{"operationId":"getItem","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"204":{"description":"No Content"}},"x-putnami-client":{"stream":"unary","transports":[{"protocol":"rest-json","path":"/items/{id}","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"}}}}` +
	`}}`

const emptyContract = `{"openapi":"3.0.3","info":{"title":"Empty","version":"1.0.0"},"x-putnami-client":{"protocolVersion":1,"service":{"id":"empty","audience":"urn:empty"},"credentials":{}},"paths":{}}`

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeClientModule writes a replaced module that declares an in-module Go
// client, with its committed contract and no generated file.
func writeClientModule(t *testing.T, dir, contract string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "clientgen.json"),
		`{"targets":["go"],"go":{"output":"go","packageName":"itemsapi","clientName":"Client"}}`)
	writeFile(t, filepath.Join(dir, "schema", "openapi.json"), contract)
}

func readOverlay(t *testing.T, output string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(output, OverlayFile))
	if err != nil {
		t.Fatal(err)
	}
	var document overlay
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document.Replace
}

// A clean checkout holds no generated client. The runtime's clients come from
// each replaced module's committed contract, into the output directory, and the
// overlay places each one where the module's clientgen.json puts it. The source
// tree stays as it was.
func TestGeneratePlacesEachReplacedModulesClientThroughTheOverlay(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "extension")
	writeFile(t, filepath.Join(module, "go.mod"), "module example.test/extension\n\ngo 1.26\n\n"+
		"replace example.test/items => ../clients/items\n\n"+
		"replace (\n\texample.test/empty => ../clients/empty // no operation\n\texample.test/plain => ../plain\n\texample.test/remote => example.test/fork v1.0.0\n)\n")
	writeClientModule(t, filepath.Join(root, "clients", "items"), itemsContract)
	writeClientModule(t, filepath.Join(root, "clients", "empty"), emptyContract)
	writeFile(t, filepath.Join(root, "plain", "go.mod"), "module example.test/plain\n")
	output := t.TempDir()

	if err := Generate(module, output); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	place := filepath.Join(root, "clients", "items", "go", "client.gen.go")
	replace := readOverlay(t, output)
	if len(replace) != 1 || replace[place] == "" {
		t.Fatalf("overlay = %v, want only %s", replace, place)
	}
	generated, err := os.ReadFile(replace[place])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(generated, []byte("package itemsapi\n")) {
		t.Errorf("generated client is not package itemsapi:\n%s", generated)
	}
	if !strings.HasPrefix(replace[place], output+string(filepath.Separator)) {
		t.Errorf("generated client %s lies outside the output %s", replace[place], output)
	}
	if _, err := os.Stat(filepath.Join(root, "clients", "items", "go")); !os.IsNotExist(err) {
		t.Errorf("Generate wrote into the source tree (stat: %v)", err)
	}
}

// A clientgen.json the runtime cannot compile fails the preparation instead of
// building a runtime without that client.
func TestGenerateRefusesAClientgenJSONWithNoInModuleGoClient(t *testing.T) {
	for name, config := range map[string]string{
		"another language":    `{"targets":["ts"],"go":{"output":"go"}}`,
		"a module of its own": `{"targets":["go"],"go":{"output":"go","modulePath":"example.test/c"}}`,
		"outside the module":  `{"targets":["go"],"go":{"output":"../elsewhere"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			module := filepath.Join(root, "extension")
			writeFile(t, filepath.Join(module, "go.mod"), "module example.test/extension\n\nreplace example.test/c => ../c\n")
			writeFile(t, filepath.Join(root, "c", "clientgen.json"), config)
			writeFile(t, filepath.Join(root, "c", "schema", "openapi.json"), itemsContract)
			if err := Generate(module, t.TempDir()); err == nil {
				t.Fatal("Generate accepted the clientgen.json")
			}
		})
	}
}

// A module that declares a client but commits no contract fails the
// preparation.
func TestGenerateFailsWhenTheContractIsMissing(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "extension")
	writeFile(t, filepath.Join(module, "go.mod"), "module example.test/extension\n\nreplace example.test/c => ../c\n")
	writeFile(t, filepath.Join(root, "c", "clientgen.json"), `{"targets":["go"],"go":{"output":"go"}}`)
	err := Generate(module, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "contract") {
		t.Fatalf("Generate = %v, want a missing-contract error", err)
	}
}

func TestRunNeedsAnOutput(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(nil, &stderr); code != 2 {
		t.Fatalf("run() = %d, want 2", code)
	}
	if code := run([]string{"--output", t.TempDir(), "extra"}, &stderr); code != 2 {
		t.Fatalf("run(extra) = %d, want 2", code)
	}
}
