package cloudcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/cloud/extension/internal/clicore/hometest"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	regproto "go.putnami.dev/protocol/registry"
)

// These tests pin @putnami/cloud to the registry-token/v1 credential seam
// (go.putnami.dev/protocol/registry). The bearer invocation remains protocol
// backed, while task selection is declared by contract-v3 task inputs rather
// than a marker command, extension version, or cloud-presence probe.

// TestConformance_PublishProviderMarkerNotDeclared prevents a return to the
// retired command-name capability gate. The direct compatibility command stays
// implemented in the binary for old callers, but it is deliberately not a
// manifest contribution and cannot affect planner capability selection.
func TestConformance_PublishProviderMarkerNotDeclared(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest struct {
		Commands map[string]json.RawMessage `json:"commands"`
		Tasks    map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse extension manifest: %v", err)
	}

	if _, ok := manifest.Commands[regproto.PublishProviderCommandName]; ok {
		t.Fatalf("manifest must not declare retired capability marker %q", regproto.PublishProviderCommandName)
	}
	if _, ok := manifest.Tasks["cloud-publish-provider"]; ok {
		t.Fatal("manifest must not declare a retired cloud-publish-provider marker task")
	}
}

// TestConformance_SeamInvocationTokens pins the exact tokens the framework shells
// (`putnami cloud registry-token --host <host>`) so any drift from the protocol
// constants is caught cross-repo, and proves the CLI routes the subcommand to the
// registry-token resolver rather than reporting an unknown command.
func TestConformance_SeamInvocationTokens(t *testing.T) {
	if regproto.SeamParentCommand != "cloud" {
		t.Errorf("SeamParentCommand = %q, want cloud", regproto.SeamParentCommand)
	}
	if regproto.SeamSubcommand != "registry-token" {
		t.Errorf("SeamSubcommand = %q, want registry-token", regproto.SeamSubcommand)
	}
	if regproto.SeamHostFlag != "host" {
		t.Errorf("SeamHostFlag = %q, want host", regproto.SeamHostFlag)
	}
	// The subcommand must dispatch to the registry-token resolver. Invoked with no
	// host data it fails with a usage error (see the required-host test) rather
	// than the "unknown command" default — that proves the wiring.
	err := RunCommand(regproto.SeamSubcommand, IO{
		Env:    hometest.Env(t.TempDir(), nil),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Now:    fixedNow,
	}, nil)
	if err == nil {
		t.Fatalf("%s with no host should error", regproto.SeamSubcommand)
	}
	if strings.Contains(err.Error(), "unknown") {
		t.Fatalf("%s is not wired: %v", regproto.SeamSubcommand, err)
	}
}

// TestConformance_SeamRequiresHostData proves the seam refuses to guess WHICH
// registry it is minting for. The host (or the equivalent `--for` kind) is the
// whole target: it selects one of the four surfaces, and the marker scope the
// token carries binds the bearer to that surface. Resource authority is not a
// flag at all — the registry derives it from the caller's identity — so the
// only thing that can be missing here is the kind.
func TestConformance_SeamRequiresHostData(t *testing.T) {
	err := RunCommand(regproto.SeamSubcommand, IO{
		Env:    hometest.Env(t.TempDir(), nil),
		Stdout: func(string) {},
		Stderr: func(string) {},
		Now:    fixedNow,
	}, nil)
	if err == nil {
		t.Fatal("expected a usage error when the registry kind is omitted")
	}
	assertContains(t, err.Error(), "npm, go, oci, put")
}

// TestConformance_SeamRejectsTargetFlags proves the retired target coordinates
// are refused rather than silently ignored: the seam must not accept a flag it
// no longer honors, because the caller would believe the credential was
// narrowed when it was not.
func TestConformance_SeamRejectsTargetFlags(t *testing.T) {
	for _, flag := range []string{"--owner-workspace", "--package", "--action", "--channel"} {
		err := RunCommand(regproto.SeamSubcommand, IO{
			Env:    hometest.Env(t.TempDir(), nil),
			Stdout: func(string) {},
			Stderr: func(string) {},
			Now:    fixedNow,
		}, []string{"--" + regproto.SeamHostFlag, "npm.putnami.dev", flag, "value"})
		if err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(flag, "--")) {
			t.Fatalf("%s: error = %v, want a refusal naming the retired flag", flag, err)
		}
	}
}

// TestConformance_SeamStdoutPurity proves the success path emits exactly one line
// — a bare bearer that satisfies regproto.ValidBearer — and nothing else. The
// framework sends this line verbatim as `Authorization: Bearer`, so a second line
// or any interior whitespace would be a malformed header.
func TestConformance_SeamStdoutPurity(t *testing.T) {
	home := t.TempDir()
	fake := newFakeTransport(t)
	env := hometest.Env(home, nil)
	if err := RunCommand("login", IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--auth-url", testBaseURL, "--no-open"}); err != nil {
		t.Fatalf("login: %v", err)
	}

	var out []string
	if err := RunCommand(regproto.SeamSubcommand, IO{
		Env:    env,
		Stdout: func(line string) { out = append(out, line) },
		Stderr: func(string) {},
		Client: fake.client(),
		Now:    fixedNow,
	}, []string{"--" + regproto.SeamHostFlag, "npm.putnami.dev"}); err != nil {
		t.Fatalf("registry-token seam: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("seam wrote %d stdout lines, want exactly 1: %q", len(out), out)
	}
	if !regproto.ValidBearer(out[0]) {
		t.Fatalf("seam stdout %q is not a valid bare bearer", out[0])
	}
}

// TestConformance_SeamRejectsMalformedBearer proves the seam fails closed: when
// resolution yields a value carrying whitespace (a human status line written to
// the wrong stream, which regproto.ValidBearer rejects), the command exits
// non-zero with nothing on stdout so the publisher falls back to native
// credentials rather than sending a malformed Authorization header.
func TestConformance_SeamRejectsMalformedBearer(t *testing.T) {
	env := hometest.Env(t.TempDir(), nil)
	state := &distributioncli.RegistriesState{
		Version: 1,
		Keys: []distributioncli.KeyRef{{
			Registry: distributioncli.RegistryNPM,
			Host:     "npm.putnami.dev",
			URL:      "https://npm.putnami.dev",
		}},
		Auth: map[string]string{"npm.putnami.dev": "resolved token for host"},
	}
	if err := distributioncli.WriteRegistriesState(env, state); err != nil {
		t.Fatalf("write registries state: %v", err)
	}

	var out []string
	err := RunCommand(regproto.SeamSubcommand, IO{
		Env:    env,
		Stdout: func(line string) { out = append(out, line) },
		Stderr: func(string) {},
		Now:    fixedNow,
	}, []string{"--" + regproto.SeamHostFlag, "npm.putnami.dev", "--opaque"})
	if err == nil {
		t.Fatalf("expected a failure for a malformed (whitespace) bearer")
	}
	if len(out) != 0 {
		t.Fatalf("seam wrote to stdout on failure: %q", out)
	}
}

// TestConformance_PublishProviderCompatibilityCommandRuns keeps the direct
// compatibility command available without letting it become a manifest marker
// or planner input. A direct invocation exits 0 and names the credential seam.
func TestConformance_PublishProviderCompatibilityCommandRuns(t *testing.T) {
	var out []string
	if err := RunCommand(regproto.PublishProviderCommandName, IO{
		Env:    hometest.Env(t.TempDir(), nil),
		Stdout: func(line string) { out = append(out, line) },
		Stderr: func(string) {},
		Now:    fixedNow,
	}, nil); err != nil {
		t.Fatalf("%s: %v", regproto.PublishProviderCommandName, err)
	}
	if len(out) != 1 || strings.TrimSpace(out[0]) == "" {
		t.Fatalf("%s printed %q, want one non-empty line", regproto.PublishProviderCommandName, out)
	}
	assertContains(t, out[0], regproto.SeamSubcommand)
}
