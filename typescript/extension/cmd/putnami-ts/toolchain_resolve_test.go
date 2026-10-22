package main

import (
	"bytes"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// The reserved control call is what the CLI spawns: one request on stdin,
// one answer on stdout that the CLI's own checks accept. A committed lock
// version 4 Bun pin of the declared release comes back as a version 5 pin,
// offline.
func TestHandleToolchainResolve_ServesTheReservedInvocation(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-answers-its-toolchain-pin",
		"the-reserved-call-reads-one-request-and-writes-one-answer")
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "package.json"), `{"name":"repo","packageManager":"bun@1.2.9"}`)
	t.Chdir(root)

	integrities := map[string]string{}
	for i, platform := range []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"} {
		integrities[platform] = strings.Repeat(string("abcdef"[i]), 64)
	}
	request := wsproto.ToolchainResolveRequest{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  tsExtensionName,
		Identities: []string{"bun"},
		Locked: map[string]wsproto.LockToolchainEntry{"bun": {
			Version:     "1.2.9",
			Integrities: integrities,
			Source:      "https://github.com/oven-sh/bun/releases/download/bun-v1.2.9/",
		}},
		Platform: "linux/amd64",
		Offline:  true,
	}
	var stdin bytes.Buffer
	if err := wsproto.EncodeToolchainResolveRequest(&stdin, request); err != nil {
		t.Fatal(err)
	}
	in, stdout := redirectStd(t, stdin.Bytes())

	handled, err := handleToolchainResolve(wsproto.ToolchainResolveControlArgs())
	if !handled || err != nil {
		t.Fatalf("handleToolchainResolve = (%v, %v)", handled, err)
	}
	if rest, err := io.ReadAll(in); err != nil || len(rest) != 0 {
		t.Fatalf("the call left %d bytes of stdin unread (%v); it reads the one request document whole", len(rest), err)
	}

	result, diags := wsproto.ParseAndValidateToolchainResolveResult(readAll(t, stdout))
	if result == nil || diag.HasErrors(diags) {
		t.Fatalf("stdout does not carry a conformant result: %s", diag.ErrorText(diags))
	}
	if diags := wsproto.ValidateToolchainResolveAnswer(&request, result); diag.HasErrors(diags) {
		t.Fatalf("the result does not answer the request: %s", diag.ErrorText(diags))
	}
	pin := result.Toolchains["bun"].Pin
	if result.Extension != tsExtensionName || pin == nil || pin.Version != "1.2.9" || pin.Source != "" {
		t.Fatalf("result = %+v, want a version 5 Bun 1.2.9 pin from %s", result, tsExtensionName)
	}
	want := wsproto.LockToolchainArchive{
		URL:        "https://github.com/oven-sh/bun/releases/download/bun-v1.2.9/bun-linux-x64.zip",
		Format:     wsproto.LockArchiveFormatZip,
		Executable: "bun-linux-x64/bun",
	}
	if got := pin.Archives["linux/amd64"]; got != want || pin.Integrities["linux/amd64"] != integrities["linux/amd64"] {
		t.Fatalf("linux/amd64 = %+v with integrity %q, want %+v with %q",
			got, pin.Integrities["linux/amd64"], want, integrities["linux/amd64"])
	}
}

func TestHandleToolchainResolve_DeclinesOtherInvocations(t *testing.T) {
	for _, args := range [][]string{{"build"}, wsproto.ProbeControlArgs()} {
		handled, err := handleToolchainResolve(args)
		if handled || err != nil {
			t.Fatalf("handleToolchainResolve(%v) = (%v, %v), want (false, nil)", args, handled, err)
		}
	}
}

// The manifest's runtime.resolves names exactly the lock identities the
// extension answers for: bun, and not go, which its runtime compiler locks.
func TestManifest_ResolvesTheIdentitiesTheExtensionAnswers(t *testing.T) {
	m := loadExtensionManifest(t)
	if !slices.Equal(m.Runtime.Resolves, toolchain.ResolvedLocks()) {
		t.Fatalf("runtime.resolves = %v, want %v", m.Runtime.Resolves, toolchain.ResolvedLocks())
	}
	if slices.Contains(m.Runtime.Resolves, "go") {
		t.Fatal("runtime.resolves lists go; the extension answers for no Go release")
	}
}
