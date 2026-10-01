package extension

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMergeCommandFlags_AllowsIdenticalSharedFlags(t *testing.T) {
	jobs := []*JobDefinition{
		{
			ExtensionName: "@putnami/cloud",
			Name:          "deploy",
			Flags: map[string]FlagDefinition{
				"dry-run": {Type: "boolean", Default: false, Description: "Show what would run"},
				"region":  {Type: "string", Description: "Cloud region"},
			},
		},
		{
			ExtensionName: "@putnami/pulumi",
			Name:          "deploy",
			Flags: map[string]FlagDefinition{
				"dry-run": {Type: "boolean", Default: false, Description: "Show what would run"},
				"stack":   {Type: "string", Description: "Pulumi stack"},
			},
		},
	}

	merged, err := MergeCommandFlags("deploy", jobs)
	if err != nil {
		t.Fatalf("MergeCommandFlags: %v", err)
	}
	for _, name := range []string{"dry-run", "region", "stack"} {
		if _, ok := merged[name]; !ok {
			t.Fatalf("merged flags missing %q: %#v", name, merged)
		}
	}
}

func TestMergeCommandFlags_RejectsConflictingSharedFlags(t *testing.T) {
	jobs := []*JobDefinition{
		{
			ExtensionName: "@putnami/cloud",
			Name:          "deploy",
			Flags: map[string]FlagDefinition{
				"target": {Type: "string", Description: "Cloud target"},
			},
		},
		{
			ExtensionName: "@putnami/pulumi",
			Name:          "deploy",
			Flags: map[string]FlagDefinition{
				"target": {Type: "boolean", Description: "Target all stacks"},
			},
		},
	}

	_, err := MergeCommandFlags("deploy", jobs)
	if err == nil {
		t.Fatal("expected conflicting flag definitions to fail")
	}
	msg := err.Error()
	for _, want := range []string{`command "deploy"`, "--target", "@putnami/cloud", "@putnami/pulumi"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

// Real case: @putnami/cloud and @putnami/go both declare `publish --stable` with
// identical behavior but slightly different wording. That cosmetic drift must
// not fail the whole `publish` command.
func TestMergeCommandFlags_ToleratesDescriptionDrift(t *testing.T) {
	jobs := []*JobDefinition{
		{
			ExtensionName: "@putnami/cloud",
			Name:          "publish",
			Flags: map[string]FlagDefinition{
				"stable": {Type: "boolean", Default: false, Description: "Publish as stable version (strips pre-release suffix, tags as latest)"},
			},
		},
		{
			ExtensionName: "@putnami/go",
			Name:          "publish",
			Flags: map[string]FlagDefinition{
				"stable": {Type: "boolean", Default: false, Description: "Publish stable version (strips pre-release suffix)"},
			},
		},
	}

	merged, err := MergeCommandFlags("publish", jobs)
	if err != nil {
		t.Fatalf("MergeCommandFlags should tolerate description drift, got: %v", err)
	}
	if _, ok := merged["stable"]; !ok {
		t.Fatalf("merged flags missing %q: %#v", "stable", merged)
	}
}

// Behavioral drift (here, a differing default) must still fail even when the
// descriptions are identical — tolerance is limited to help text.
func TestMergeCommandFlags_RejectsBehaviouralDriftDespiteSameDescription(t *testing.T) {
	jobs := []*JobDefinition{
		{
			ExtensionName: "@putnami/a",
			Name:          "publish",
			Flags: map[string]FlagDefinition{
				"stable": {Type: "boolean", Default: false, Description: "same wording"},
			},
		},
		{
			ExtensionName: "@putnami/b",
			Name:          "publish",
			Flags: map[string]FlagDefinition{
				"stable": {Type: "boolean", Default: true, Description: "same wording"},
			},
		},
	}

	if _, err := MergeCommandFlags("publish", jobs); err == nil {
		t.Fatal("expected differing default to still fail despite identical descriptions")
	}
}

func TestCollectCommandFlags_KeepsConflictingNamesVisible(t *testing.T) {
	jobs := []*JobDefinition{
		{
			ExtensionName: "@putnami/cloud",
			Name:          "deploy",
			Flags: map[string]FlagDefinition{
				"target": {Type: "string", Description: "Cloud target"},
				"region": {Type: "string", Description: "Cloud region"},
			},
		},
		{
			ExtensionName: "@putnami/pulumi",
			Name:          "deploy",
			Flags: map[string]FlagDefinition{
				"target": {Type: "boolean", Description: "Target all stacks"},
				"stack":  {Type: "string", Description: "Pulumi stack"},
			},
		},
	}

	merged := CollectCommandFlags(jobs)
	for _, name := range []string{"target", "region", "stack"} {
		if _, ok := merged[name]; !ok {
			t.Fatalf("collected flags missing %q: %#v", name, merged)
		}
	}
	if merged["target"].Description != "Cloud target" {
		t.Fatalf("conflicting flag should keep deterministic first definition, got %#v", merged["target"])
	}
}

func TestFirstPartyPackageFlagsAreCompatible(t *testing.T) {
	repoRoot := findRepoRoot(t)
	manifests := []struct {
		name string
		path string
	}{
		{name: "@putnami/go", path: "go/extension/putnami.extension.json"},
		{name: "@putnami/typescript", path: "typescript/extension/putnami.extension.json"},
		{name: "@putnami/scaffold", path: "tooling/scaffold/putnami.extension.json"},
	}

	jobs := make([]*JobDefinition, 0, len(manifests))
	for _, entry := range manifests {
		manifest, err := LoadManifest(filepath.Join(repoRoot, entry.path))
		if err != nil {
			t.Fatalf("LoadManifest(%s): %v", entry.path, err)
		}
		cmd, ok := manifest.Commands["package"]
		if !ok {
			t.Fatalf("%s has no package command", entry.path)
		}
		jobs = append(jobs, &JobDefinition{
			ExtensionName: entry.name,
			Name:          "package",
			Flags:         cmd.Flags,
		})
	}

	if err := ValidateCommandFlags("package", jobs); err != nil {
		t.Fatalf("first-party package flags are incompatible: %v", err)
	}
}

func TestMergeFlagLayers(t *testing.T) {
	tests := []struct {
		name   string
		layers []map[string]FlagDefinition
		want   map[string]FlagDefinition
	}{
		{
			name:   "nil and empty layers return nil",
			layers: []map[string]FlagDefinition{nil, {}, nil},
			want:   nil,
		},
		{
			name:   "no layers returns nil",
			layers: nil,
			want:   nil,
		},
		{
			name: "single layer passthrough",
			layers: []map[string]FlagDefinition{
				{"env": {Type: "string", Default: "prod"}},
			},
			want: map[string]FlagDefinition{
				"env": {Type: "string", Default: "prod"},
			},
		},
		{
			name: "later layer wins on name collision",
			layers: []map[string]FlagDefinition{
				{"env": {Type: "string", Default: "command"}, "region": {Type: "string"}},
				{"env": {Type: "string", Default: "group"}},
				{"env": {Type: "string", Default: "sub"}},
			},
			want: map[string]FlagDefinition{
				// command < group < subcommand: the subcommand default wins,
				// and the flat command's region survives (no override).
				"env":    {Type: "string", Default: "sub"},
				"region": {Type: "string"},
			},
		},
		{
			name: "group overrides flat command target",
			layers: []map[string]FlagDefinition{
				{"env": {Type: "string", Default: "command"}},
				{"env": {Type: "string", Default: "group"}},
				nil,
			},
			want: map[string]FlagDefinition{
				"env": {Type: "string", Default: "group"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeFlagLayers(tt.layers...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MergeFlagLayers = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("could not find Putnami repo root from %s", start)
		}
		dir = parent
	}
}
