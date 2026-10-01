package contracts

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRenderMarkdown_IsDeterministicAndContractDerived(t *testing.T) {
	raw, err := os.ReadFile("fixtures/equivalence/contracts.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest, diagnostics := ParseAndValidateManifest(raw)
	if manifest == nil || len(diagnostics) != 0 {
		t.Fatalf("parse manifest: %v", diagnostics)
	}
	first := RenderMarkdown(manifest)
	for range 20 {
		if got := RenderMarkdown(manifest); !bytes.Equal(got, first) {
			t.Fatal("markdown projection is not deterministic")
		}
	}
	for _, want := range []string{
		"## Enum `TokenType`", "## Union `Credential`", "## DTO `TokenBundle`",
		"## Configuration", "## Scopes", "## Capabilities", "## Grants", "## Claims",
	} {
		if !strings.Contains(string(first), want) {
			t.Errorf("markdown projection missing %q", want)
		}
	}

	manifest.Enums[0].Values[0].Value = "changed"
	if bytes.Equal(first, RenderMarkdown(manifest)) {
		t.Fatal("changing the canonical contract did not change its documentation projection")
	}
}
