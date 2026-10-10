package cloudcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

func TestReleaseSetProviderManifestAndDispatch(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode extension manifest: %v", err)
	}

	cloud := manifest["commandGroups"].(map[string]any)["cloud"].(map[string]any)
	subcommand, ok := cloud["subcommands"].(map[string]any)["release-set"].(map[string]any)
	if !ok || subcommand["command"] != "cloud-release-set" || subcommand["interactive"] != true {
		t.Fatalf("release-set subcommand = %#v", subcommand)
	}
	commands := manifest["commands"].(map[string]any)
	command, ok := commands["cloud-release-set"].(map[string]any)
	if !ok {
		t.Fatal("manifest is missing cloud-release-set")
	}
	flags, ok := command["flags"].(map[string]any)
	if !ok || len(flags) != 1 {
		t.Fatalf("cloud-release-set flags = %#v, want only request-file", command["flags"])
	}
	requestFile, ok := flags["request-file"].(map[string]any)
	if !ok || requestFile["type"] != "string" {
		t.Fatalf("request-file flag = %#v", flags["request-file"])
	}
	run := command["run"].([]any)[0].(map[string]any)
	if run["task"] != "cloud-release-set" {
		t.Fatalf("cloud-release-set run task = %v", run["task"])
	}
	task := manifest["tasks"].(map[string]any)["cloud-release-set"].(map[string]any)
	args := task["args"].([]any)
	if len(args) != 1 || args[0] != "release-set" {
		t.Fatalf("cloud-release-set task args = %#v", args)
	}

	err = RunCommand("release-set", IO{
		Env: map[string]string{}, Stdout: func(string) {}, Stderr: func(string) {},
	}, []string{"invalid"})
	if err == nil || !strings.Contains(err.Error(), "cloud release-set requires exactly") || strings.Contains(err.Error(), "unknown @putnami/cloud command") {
		t.Fatalf("release-set dispatch error = %v", err)
	}
}

// Check actual publisher output against the profiles the framework discovers,
// so a working provider command cannot hide an undeclared member ecosystem.
func requireDeclaredMemberProfile(t *testing.T, ecosystem, coordinate, version string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest extensionproto.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	profiles, diagnostics := extensionproto.ResolveProfiles([]extensionproto.NamedManifest{{Name: manifest.Name, Manifest: &manifest}}, nil)
	if len(diagnostics) != 0 {
		t.Fatalf("Cloud ecosystem profiles: %+v", diagnostics)
	}
	if err := profiles.ValidateCoordinate(ecosystem, coordinate); err != nil {
		t.Fatal(err)
	}
	if err := profiles.ValidateVersion(ecosystem, version); err != nil {
		t.Fatal(err)
	}
	if !profiles.HasNativeChannel(ecosystem) {
		t.Fatalf("published %s member has no declared native channel", ecosystem)
	}
}
