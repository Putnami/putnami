package cloudcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

// TestExtensionManifestActivatesAndOwnsNativeArchivePublishing guards the
// Cloud-owned half of archive packaging. @putnami/go owns the real package
// producer; Cloud owns immutable Put publication and its ecosystem profile.
func TestExtensionManifestActivatesAndOwnsNativeArchivePublishing(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	manifest, diagnostics := extensionproto.ParseManifest(data)
	if len(diagnostics) != 0 {
		t.Fatalf("strict extension parse diagnostics = %+v", diagnostics)
	}
	if diagnostics := extensionproto.ValidateManifest(manifest); len(diagnostics) != 0 {
		t.Fatalf("extension contract diagnostics = %+v", diagnostics)
	}
	if manifest.Workspace == nil || !slices.Equal(manifest.Workspace.Markers, []string{"putnami.json"}) ||
		!slices.Equal(manifest.Workspace.Inputs, []string{"putnami.extension.json", "putnami.json"}) || manifest.Workspace.SyncTask != "" {
		t.Fatalf("Cloud workspace adapter = %+v", manifest.Workspace)
	}
	requireDeclaredMemberProfile(t, "archive", "putnami/cloud", "0.2.0-main")
	requireDeclaredMemberProfile(t, "put", "cloud/config-control-api", "0.2.0-main")

	pack := manifest.Commands["package"]
	if pack.Activation != "workspace" || !slices.Contains(pack.ActivationFiles, "schema/config.json") {
		t.Fatalf("Config packaging must activate for schema-bearing projects independently of language dependencies: %+v", pack)
	}

	publish := manifest.Commands["publish"]
	if publish.Activation != "workspace" || len(publish.ActivationFiles) != 0 {
		t.Fatalf("publish activation = %q files=%v, want ungated workspace activation", publish.Activation, publish.ActivationFiles)
	}
	for _, retired := range []string{"archive-owner-workspace", "channel", "stable"} {
		if _, exists := publish.Flags[retired]; exists {
			t.Fatalf("publish retained retired archive authority flag %q", retired)
		}
		if _, exists := manifest.Commands["cloud-publish-archives"].Flags[retired]; exists {
			t.Fatalf("cloud-publish-archives retained retired archive authority flag %q", retired)
		}
	}
	var foundArchiveStep bool
	for _, step := range publish.Run {
		if step.ID == cloudArchivePublishStep {
			foundArchiveStep = step.If == "params.archives" && step.Task == "cloud-publish-archives-project"
		}
	}
	if !foundArchiveStep {
		t.Fatalf("publish command does not wire the exact archive step: %+v", publish.Run)
	}

	projectData, err := os.ReadFile(filepath.Join("..", "..", "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var project struct {
		Options map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(projectData, &project); err != nil {
		t.Fatal(err)
	}
	if _, hasCloudAuthorityOverride := project.Options[cloudRuntimeIdentity]; hasCloudAuthorityOverride {
		t.Fatalf("project manifest must not invent cross-workspace archive authority: %s", projectData)
	}
}
