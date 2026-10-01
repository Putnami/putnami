package tools

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestManifestContract(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "pinned-lint-tools", "the-pins-file-is-the-declared-source-of-tool-versions")
	manifest := ManifestContract()
	if manifest.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", manifest.SchemaVersion)
	}
	if manifest.GoVersion != "1.25.7" {
		t.Errorf("GoVersion = %q, want 1.25.7", manifest.GoVersion)
	}

	for name, want := range map[string]Tool{
		"golangci-lint": {
			Install: "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1",
			Version: "v2.10.1",
		},
		"staticcheck": {
			Install: "honnef.co/go/tools/cmd/staticcheck@v0.7.0",
			Version: "v0.7.0",
		},
	} {
		got, ok := Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) did not find the pin", name)
			continue
		}
		if got != want {
			t.Errorf("Lookup(%q) = %+v, want %+v", name, got, want)
		}
	}
}
