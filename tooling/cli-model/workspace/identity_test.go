package workspace

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestProjectIDFromPath(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "canonical-identity", "ids-derive-from-workspace-relative-paths")
	tests := []struct {
		path string
		want string
	}{
		{path: "typescript/frameworks/application", want: "/typescript/frameworks/application"},
		{path: "identity/(workloads)/auth-server", want: "/identity/auth-server"},
		{path: "identity/(libs)/identity-client", want: "/identity/identity-client"},
		{path: "commerce/(internal-tools)/catalog-importer", want: "/commerce/catalog-importer"},
		{path: "commerce/(groups)/(internal)/catalog-importer", want: "/commerce/catalog-importer"},
		{path: "identity/work(loads)/auth-server", want: "/identity/work(loads)/auth-server"},
		{path: "identity/(workloads)-legacy/auth-server", want: "/identity/(workloads)-legacy/auth-server"},
		{path: "identity/()/auth-server", want: "/identity/()/auth-server"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := ProjectIDFromPath(tt.path); got != tt.want {
				t.Errorf("ProjectIDFromPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
