package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
)

// toolchainResolveWorkspace makes the working directory a workspace whose
// go.work declares Go 1.26.1, with a Putnami home of its own, and returns the
// request core sends for it: the committed lock version 4 Go pin of that
// release, offline, so the answer needs no network.
func toolchainResolveWorkspace(t *testing.T) wsproto.ToolchainResolveRequest {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.26.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	return wsproto.ToolchainResolveRequest{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  goExtensionName,
		Identities: []string{"go"},
		Locked: map[string]wsproto.LockToolchainEntry{"go": {
			Version: "1.26.1",
			Integrities: map[string]string{
				"darwin/arm64":  strings.Repeat("a", 64),
				"linux/amd64":   strings.Repeat("b", 64),
				"windows/amd64": strings.Repeat("c", 64),
			},
			Source: "https://go.dev/dl/",
		}},
		Platform: "linux/amd64",
		Offline:  true,
	}
}

// The reserved control call is answered through the entrypoint before
// ordinary dispatch, and stdout carries one result that passes both of the
// protocol's checks against the request.
func TestRunEntrypointAnswersToolchainResolve(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "go-answers-its-toolchain-pin",
		"the-reserved-call-is-answered-before-command-dispatch")
	request := toolchainResolveWorkspace(t)
	stdin := filepath.Join(t.TempDir(), "request.json")
	var encoded bytes.Buffer
	if err := wsproto.EncodeToolchainResolveRequest(&encoded, request); err != nil {
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
	err = runEntrypoint(wsproto.ToolchainResolveControlArgs(), &stdout, os.Getenv,
		func(map[string]cli.JobFunc) { t.Fatal("toolchain-resolve must not enter normal command dispatch") })
	if err != nil {
		t.Fatalf("runEntrypoint(toolchain-resolve): %v", err)
	}

	result, diags := wsproto.ParseAndValidateToolchainResolveResult(stdout.Bytes())
	if result == nil || diag.HasErrors(diags) {
		t.Fatalf("stdout does not carry a conformant result: %v\n%s", diags, stdout.String())
	}
	if diags := wsproto.ValidateToolchainResolveAnswer(&request, result); diag.HasErrors(diags) {
		t.Fatalf("the result does not answer the request: %v", diags)
	}
	pin := result.Toolchains["go"].Pin
	if result.Extension != goExtensionName || pin == nil || pin.Version != "1.26.1" {
		t.Fatalf("result = %+v", result)
	}
	if archive := pin.Archives["linux/amd64"]; archive.URL != "https://go.dev/dl/go1.26.1.linux-amd64.tar.gz" {
		t.Fatalf("linux/amd64 archive = %+v", archive)
	}
}

// A request addressed to another extension is refused, and stdout stays
// empty.
func TestHandleToolchainResolve_RefusesAMisroutedRequest(t *testing.T) {
	request := toolchainResolveWorkspace(t)
	request.Extension = "@putnami/typescript"
	var stdin, stdout bytes.Buffer
	if err := wsproto.EncodeToolchainResolveRequest(&stdin, request); err != nil {
		t.Fatal(err)
	}
	handled, err := handleToolchainResolve(wsproto.ToolchainResolveControlArgs(), &stdin, &stdout, os.Getenv)
	if !handled || err == nil {
		t.Fatalf("handleToolchainResolve = (%v, %v), want a refusal", handled, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("a refused answer wrote stdout: %s", stdout.String())
	}
}

// The manifest lists exactly the lock identity the answer resolves, so the
// core asks this extension for the Go pin and for nothing it would refuse.
func TestManifestResolvesTheGoLockIdentity(t *testing.T) {
	resolves := loadExtensionManifest(t).Runtime.Resolves
	if len(resolves) != 1 || resolves[0] != workspacejob.GoLockIdentity {
		t.Fatalf("runtime.resolves = %v, want [%s]", resolves, workspacejob.GoLockIdentity)
	}
}

func TestHandleToolchainResolve_DeclinesOrdinarySubcommands(t *testing.T) {
	handled, err := handleToolchainResolve([]string{"build"}, &bytes.Buffer{}, &bytes.Buffer{}, os.Getenv)
	if handled || err != nil {
		t.Fatalf("handleToolchainResolve(build) = (%v, %v), want (false, nil)", handled, err)
	}
}
