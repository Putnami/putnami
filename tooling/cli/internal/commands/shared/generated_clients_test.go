package shared

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A project declares a generated client through generatedFrom or through a
// committed clientgen.json. A directory of that name declares nothing.
func TestDeclaresGeneratedClient(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, dir := range []string{"plain", "provider", "folder/clientgen.json"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "provider", clientGeneratorConfigFile), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := func(path string) *workspace.Project { return &workspace.Project{Path: path} }
	bound := project("client")
	bound.GeneratedClient = &workspace.GeneratedClientBinding{}

	for _, tc := range []struct {
		name string
		ws   *workspace.Workspace
		want bool
	}{
		{name: "no workspace"},
		{name: "no client", ws: &workspace.Workspace{Root: root, Projects: []*workspace.Project{nil, project("plain"), project("folder")}}},
		{name: "a generatedFrom binding", ws: &workspace.Workspace{Root: root, Projects: []*workspace.Project{project("plain"), bound}}, want: true},
		{name: "a client generator config", ws: &workspace.Workspace{Root: root, Projects: []*workspace.Project{project("provider")}}, want: true},
	} {
		if got := DeclaresGeneratedClient(tc.ws); got != tc.want {
			t.Errorf("%s: DeclaresGeneratedClient = %v, want %v", tc.name, got, tc.want)
		}
	}
}
