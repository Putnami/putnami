package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const contract = `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:items"},"credentials":{}},"paths":{` +
	`"/items/{id}":{"get":{"operationId":"getItem","parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{"204":{"description":"No Content"}},"x-putnami-client":{"stream":"unary","transports":[{"protocol":"rest-json","path":"/items/{id}","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"},"producer":{"project":"items-service","feature":"items/read"}}}},` +
	`"/items":{"delete":{"operationId":"deleteItems","responses":{"204":{"description":"No Content"}},"x-putnami-client":{"stream":"unary","transports":[{"protocol":"rest-json","path":"/items","encoding":"json"}],"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"idempotent"}}}}` +
	`}}`

const emptyContract = `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"x-putnami-client":{"protocolVersion":1,"service":{"id":"items","audience":"urn:items"},"credentials":{}},"paths":{}}`

func writeContract(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openapi.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list))
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

func TestRun_WritesOnlyTheClientWithTheRequestedNames(t *testing.T) {
	output := filepath.Join(t.TempDir(), "client")
	code, _, stderr := runCLI(t, "--contract", writeContract(t, contract), "--output", output,
		"--package", "itemsclient", "--client", "ItemsClient", "--omit", "deleteItems", "--putnamiContext", "ignored.json")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := entries(t, output); !reflect.DeepEqual(got, []string{"client.gen.go"}) {
		t.Fatalf("output holds %v, want only client.gen.go", got)
	}
	source, err := os.ReadFile(filepath.Join(output, "client.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"package itemsclient",
		"func (c *ItemsClient) GetItem(",
		`ProducerProject: "items-service", ProducerFeature: "items/read"`,
		"//   - deleteItems (DELETE /items)",
	} {
		if !strings.Contains(string(source), want) {
			t.Errorf("client missing %q:\n%s", want, source)
		}
	}
	if strings.Contains(string(source), "func (c *ItemsClient) DeleteItems(") {
		t.Errorf("the omitted operation has a method:\n%s", source)
	}
	if !strings.Contains(stderr, "left operation deleteItems out") {
		t.Errorf("stderr does not name the omission:\n%s", stderr)
	}
}

func TestRun_DefaultsThePackageAndClientNames(t *testing.T) {
	output := t.TempDir()
	if code, _, stderr := runCLI(t, "--contract", writeContract(t, contract), "--output", output); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	source, err := os.ReadFile(filepath.Join(output, "client.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "package client\n") || !strings.Contains(string(source), "func (c *Client) GetItem(") {
		t.Errorf("client does not use the default names:\n%s", source)
	}
}

func TestRun_Failures(t *testing.T) {
	unmarked := `{"openapi":"3.0.3","info":{"title":"Items","version":"1.0.0"},"paths":{}}`
	tests := []struct {
		name string
		args func(t *testing.T, output string) []string
		want string
	}{
		{"missing contract flag", func(_ *testing.T, output string) []string { return []string{"--output", output} }, "--contract and --output are required"},
		{"missing output flag", func(t *testing.T, _ string) []string { return []string{"--contract", writeContract(t, contract)} }, "--contract and --output are required"},
		{"unreadable contract", func(_ *testing.T, output string) []string {
			return []string{"--contract", filepath.Join(output, "absent.json"), "--output", output}
		}, "read contract"},
		{"no client metadata", func(t *testing.T, output string) []string {
			return []string{"--contract", writeContract(t, unmarked), "--output", output}
		}, "x-putnami-client"},
		{"undeclared omission", func(t *testing.T, output string) []string {
			return []string{"--contract", writeContract(t, contract), "--output", output, "--omit", "deleteItem"}
		}, `"deleteItem", which the contract does not declare`},
		{"stray argument", func(t *testing.T, output string) []string {
			return []string{"--contract", writeContract(t, contract), "--output", output, "extra"}
		}, `unexpected argument "extra"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output := t.TempDir()
			code, _, stderr := runCLI(t, tc.args(t, output)...)
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit %d, stderr %q; want a failure naming %q", code, stderr, tc.want)
			}
			if got := entries(t, output); len(got) != 0 {
				t.Fatalf("a failed run wrote %v", got)
			}
		})
	}
}

func TestRun_AnEmptyContractRemovesTheClient(t *testing.T) {
	output := t.TempDir()
	if code, _, stderr := runCLI(t, "--contract", writeContract(t, contract), "--output", output, "--omit", "deleteItems"); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	code, stdout, stderr := runCLI(t, "--contract", writeContract(t, emptyContract), "--output", output)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := entries(t, output); len(got) != 0 {
		t.Fatalf("output still holds %v", got)
	}
	if !strings.Contains(stdout, "declares no operation") {
		t.Errorf("stdout = %q", stdout)
	}
}
