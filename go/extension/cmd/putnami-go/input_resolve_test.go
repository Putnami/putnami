package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	"go.putnami.dev/sdk/extension/goembed"
)

// inputResolveWorkspace makes the working directory a workspace with a Go
// project at app/ whose main.go embeds static/a.txt, and returns the request
// the core sends for both selectors and an unknown one.
func inputResolveWorkspace(t *testing.T) wsproto.InputResolveRequest {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"app/go.mod":       "module example.com/app\n",
		"app/main.go":      "package main\n\nimport _ \"embed\"\n\n//go:embed static/a.txt\nvar a string\n",
		"app/static/a.txt": "a",
	}
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	return wsproto.InputResolveRequest{
		Version:   wsproto.InputResolveProtocolVersion,
		Extension: goExtensionName,
		Inputs: []wsproto.InputResolveQuery{
			{Project: "app", Selector: goembed.BuildSelector, Patterns: []string{"**/*.go", "!**/*_test.go"}},
			{Project: "app", Selector: goembed.TestSelector},
			{Project: "app", Selector: "go-embed:other"},
		},
	}
}

// The reserved control call is answered through the entrypoint before
// ordinary dispatch, and stdout carries one result that passes both of the
// protocol's checks against the request.
func TestRunEntrypointAnswersInputResolve(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-input-selectors",
		"the-reserved-call-is-answered-before-command-dispatch")
	request := inputResolveWorkspace(t)
	stdin := filepath.Join(t.TempDir(), "request.json")
	var encoded bytes.Buffer
	if err := wsproto.EncodeInputResolveRequest(&encoded, request); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stdin, encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(stdin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	previous := os.Stdin
	os.Stdin = file
	t.Cleanup(func() { os.Stdin = previous })

	var stdout bytes.Buffer
	err = runEntrypoint(wsproto.InputResolveControlArgs(), &stdout, os.Getenv,
		func(map[string]cli.JobFunc) { t.Fatal("input-resolve must not enter normal command dispatch") })
	if err != nil {
		t.Fatalf("runEntrypoint(input-resolve): %v", err)
	}

	result, diags := wsproto.ParseAndValidateInputResolveResult(stdout.Bytes())
	if result == nil || diag.HasErrors(diags) {
		t.Fatalf("stdout does not carry a conformant result: %v\n%s", diags, stdout.String())
	}
	if diags := wsproto.ValidateInputResolveAnswer(&request, result); diag.HasErrors(diags) {
		t.Fatalf("the result does not answer the request: %v", diags)
	}
	if result.Extension != goExtensionName || len(result.Inputs) != 3 {
		t.Fatalf("result = %+v", result)
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"main.go", "static/a.txt"}
	for _, answer := range result.Inputs[:2] {
		if answer.Error != "" || !reflect.DeepEqual(answer.Files, want) {
			t.Fatalf("%s answer = %q %v, want %v", answer.Selector, answer.Error, answer.Files, want)
		}
		digest, err := wsproto.InputBasisDigest(filepath.Join(root, "app"), answer.Basis)
		if err != nil || digest == "" {
			t.Fatalf("%s basis digest = %q, %v", answer.Selector, digest, err)
		}
	}
	if result.Inputs[2].Error == "" {
		t.Fatalf("an unknown selector was answered: %+v", result.Inputs[2])
	}
}

// A request addressed to another extension is refused, and stdout stays
// empty.
func TestHandleInputResolve_RefusesAMisroutedRequest(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "first-party-extensions-answer-the-reserved-calls", "the-go-extension-refuses-a-misrouted-request")
	request := inputResolveWorkspace(t)
	request.Extension = "@putnami/typescript"
	var stdin, stdout bytes.Buffer
	if err := wsproto.EncodeInputResolveRequest(&stdin, request); err != nil {
		t.Fatal(err)
	}
	handled, err := handleInputResolve(wsproto.InputResolveControlArgs(), &stdin, &stdout)
	if !handled || err == nil {
		t.Fatalf("handleInputResolve = (%v, %v), want a refusal", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("a refused answer wrote stdout: %s", stdout.String())
	}
}

func TestHandleInputResolve_DeclinesOrdinarySubcommands(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", "first-party-extensions-answer-the-reserved-calls", "the-go-extension-claims-only-the-reserved-calls")
	handled, err := handleInputResolve([]string{"build"}, &bytes.Buffer{}, &bytes.Buffer{})
	if handled || err != nil {
		t.Fatalf("handleInputResolve(build) = (%v, %v), want (false, nil)", handled, err)
	}
}

// The manifest lists exactly the selectors the answer expands, so the core
// asks this extension for both go-embed selectors and for nothing it would
// refuse.
func TestManifestListsBothInputSelectors(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-input-selectors",
		"the-manifest-lists-both-selectors")
	selectors := loadExtensionManifest(t).Runtime.InputSelectors
	if want := []string{goembed.BuildSelector, goembed.TestSelector}; !reflect.DeepEqual(selectors, want) {
		t.Fatalf("runtime.inputSelectors = %v, want %v", selectors, want)
	}
}
