package engine

import (
	"errors"
	"testing"

	"go.putnami.dev/cli/model/extension"
	runner "go.putnami.dev/protocol/runner"
)

func TestPlacementAbsenceAndAmbiguity(t *testing.T) {
	t.Parallel()
	ext := func(name string) *extension.ExtensionDescription {
		return &extension.ExtensionDescription{Name: name, Commands: map[string]string{runner.ProviderCommandName: "portable execution"}}
	}
	for _, where := range []string{"", "local", "remote"} {
		if provider, err := resolvePlacement(where, nil); err != nil || provider != nil {
			t.Errorf("absent provider %q: %v, %v", where, provider, err)
		}
	}
	ambiguous := []*extension.ExtensionDescription{ext("@other/runner"), ext("@fixture/runner")}
	if _, err := resolvePlacement("remote", ambiguous); !errors.Is(err, extension.ErrProviderAmbiguous) {
		t.Fatalf("ambiguous remote provider: %v", err)
	}
	if provider, err := resolvePlacement("local", ambiguous); err != nil || provider != nil {
		t.Fatalf("local placement negotiated remote providers: %v, %v", provider, err)
	}
	if provider, err := resolvePlacement("remote", ambiguous[1:]); err != nil || provider == nil || provider.ExtensionName != "@fixture/runner" {
		t.Fatalf("single provider was not resolved for the seam: %v, %v", provider, err)
	}
	if _, err := resolvePlacement("invalid", nil); err == nil {
		t.Fatal("engine accepted invalid placement from a non-terminal adapter")
	}
}
