package extension

import (
	"slices"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

func TestRuntimeToolchainLocksCountsEveryReferenceTheRuntimeResolves(t *testing.T) {
	t.Parallel()
	declaration := func(lock string, optional bool) extensionproto.RuntimeToolchain {
		return extensionproto.RuntimeToolchain{Lock: lock, Optional: optional}
	}
	description := func(localSource bool) *ExtensionDescription {
		return &ExtensionDescription{
			Name:        "example",
			LocalSource: localSource,
			Runtime: &extensionproto.RuntimeDefinition{
				Executable: "compiled/example",
				Toolchains: map[string]extensionproto.RuntimeToolchain{
					"compiler":  declaration("compiler-lock", false),
					"runner":    declaration("runner-lock", false),
					"formatter": declaration("formatter-lock", true),
					"linker":    declaration("linker-lock", false),
					"unused":    declaration("unused-lock", false),
				},
				RunToolchains: []string{"runner"},
				Prepare:       &extensionproto.RuntimePrepare{Command: "{extensionRoot}/bin/prepare", Toolchains: []string{"compiler"}},
			},
			Tasks: map[string]TaskDefinition{
				"format": {Kind: "command", Command: "fmt", Toolchains: []string{"formatter", "undeclared"}},
			},
			Jobs: map[string]*JobDefinition{
				"link": {Name: "link", Toolchains: []string{"linker"}},
				"nil":  nil,
			},
		}
	}

	if got, want := RuntimeToolchainLocks(description(true)), []string{"compiler-lock", "formatter-lock", "linker-lock", "runner-lock"}; !slices.Equal(got, want) {
		t.Errorf("local source locks = %v, want %v", got, want)
	}
	if got, want := RuntimeToolchainLocks(description(false)), []string{"formatter-lock", "linker-lock", "runner-lock"}; !slices.Equal(got, want) {
		t.Errorf("published runtime locks = %v, want %v: a published runtime never prepares", got, want)
	}

	shared := description(true)
	shared.Runtime.Toolchains["runner"] = declaration("compiler-lock", false)
	if got, want := RuntimeToolchainLocks(shared), []string{"compiler-lock", "formatter-lock", "linker-lock"}; !slices.Equal(got, want) {
		t.Errorf("two aliases of one lock = %v, want %v", got, want)
	}

	for name, ext := range map[string]*ExtensionDescription{
		"nil extension":      nil,
		"no runtime":         {Name: "plain"},
		"runtime, no chains": {Name: "bare", Runtime: &extensionproto.RuntimeDefinition{Executable: "compiled/bare"}},
	} {
		if got := RuntimeToolchainLocks(ext); len(got) != 0 {
			t.Errorf("%s: locks = %v, want none", name, got)
		}
	}
}
