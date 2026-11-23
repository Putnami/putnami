package workspace

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const retiredRequirement = "a-retired-extension-names-its-successor"

func TestRetiredExtensions_TheRegistryNamesTheSuccessor(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", retiredRequirement, "the-registry-names-the-successor-of-a-retired-extension")
	registry := RetiredExtensions()
	want := RetiredExtension{Name: "@putnami/intelligence", Successor: "@putnami/cloud"}
	if !reflect.DeepEqual(registry, []RetiredExtension{want}) {
		t.Fatalf("the registry is %+v; want %+v", registry, []RetiredExtension{want})
	}
	registry[0].Successor = "changed"
	if RetiredExtensions()[0].Successor != "@putnami/cloud" {
		t.Fatal("a caller changed the registry through its copy")
	}
	notice := "This checkout binds @putnami/intelligence, which is retired; rebase on the default branch or bind @putnami/cloud."
	if got := want.Notice(); got != notice {
		t.Fatalf("the notice is %q; want %q", got, notice)
	}
}

func TestRetiredBindings_AWorkspaceThatBindsARetiredExtensionIsToldItsSuccessor(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", retiredRequirement, "a-workspace-that-binds-a-retired-extension-is-told-its-successor")
	want := []RetiredExtension{{Name: "@putnami/intelligence", Successor: "@putnami/cloud"}}
	for name, document := range map[string]string{
		"extensions list":        `{"extensions":["/go/extension","@putnami/intelligence"]}`,
		"versioned entry":        `{"extensions":["@putnami/intelligence@1.4.0"]}`,
		"extensions object":      `{"extensions":{"@putnami/intelligence":"^1"}}`,
		"agent content opt-in":   `{"agentArtifacts":["extension:@putnami/contributor"," extension: @putnami/intelligence "]}`,
		"missions provider":      `{"options":{"collaboration":{"missions":{"provider":"@putnami/intelligence","version":1}}}}`,
		"every form, named once": `{"extensions":["@putnami/intelligence"],"agentArtifacts":["extension:@putnami/intelligence"],"options":{"collaboration":{"missions":{"provider":"@putnami/intelligence"},"memory":{"provider":"@putnami/intelligence"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(document), &cfg); err != nil {
				t.Fatal(err)
			}
			if got := cfg.RetiredBindings(); !reflect.DeepEqual(got, want) {
				t.Fatalf("RetiredBindings() = %+v; want %+v", got, want)
			}
		})
	}
}

func TestRetiredBindings_AWorkspaceWithoutARetiredExtensionBindsNone(t *testing.T) {
	spectest.Proves(t, "workspace/workspace-contract", retiredRequirement, "a-workspace-without-a-retired-extension-binds-none")
	var none *Config
	if got := none.RetiredBindings(); got != nil {
		t.Fatalf("a nil config binds %+v", got)
	}
	document := `{"extensions":["/cloud/extension","@putnami/cloud"],"agentArtifacts":["extension:@putnami/cloud"],` +
		`"options":{"collaboration":{"missions":{"provider":"@putnami/cloud"},"tasks":"@putnami/intelligence"},"@putnami/intelligence":{"x":1}}}`
	var cfg Config
	if err := json.Unmarshal([]byte(document), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.RetiredBindings(); got != nil {
		t.Fatalf("RetiredBindings() = %+v; want none", got)
	}
}
