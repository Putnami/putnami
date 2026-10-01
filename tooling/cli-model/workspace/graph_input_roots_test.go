package workspace

import (
	"slices"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

func projectWithInputs(id, dir string, layer string, patterns ...string) *Project {
	values := make([]any, 0, len(patterns))
	for _, pattern := range patterns {
		values = append(values, pattern)
	}
	return &Project{
		ID: id, Name: id, Path: dir,
		Config: &wsproto.ProjectConfig{
			Name:    id,
			Options: map[string]map[string]any{layer: {"filePatterns": values}},
		},
	}
}

// TestDeclaredInputRootsReduceAPatternToWhatItCanRead pins the reduction: the
// literal prefix, resolved against the declaring project, is the deepest
// directory a pattern can select from, and the answer never depends on what
// happens to be on disk.
func TestDeclaredInputRootsReduceAPatternToWhatItCanRead(t *testing.T) {
	project := projectWithInputs("scaffold", "tooling/scaffold", "test",
		"internal/packaging/testdata/golden.json",
		"../contributor/src/**",
		"../../go/templates/go-server/**",
		"!../contributor/src/generated/**",
		"../../../outside/**")

	got := DeclaredInputRoots(project)
	want := []string{
		"go/templates/go-server",
		"tooling/contributor/src",
		"tooling/scaffold/internal/packaging/testdata/golden.json",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("roots = %v, want %v (exclusions and out-of-workspace patterns read nothing)", got, want)
	}
}

// TestInputRootReadsProjectCountsBothContainments: a root inside the project is
// a read, and so is a root above it — a glob tail can reach in, and reading a
// declaration too widely leaves a stale edge while reading it too narrowly
// deletes a real dependency.
func TestInputRootReadsProjectCountsBothContainments(t *testing.T) {
	cases := []struct {
		root, project string
		want          bool
	}{
		{"tooling/contributor/src", "tooling/contributor", true},
		{"tooling/contributor", "tooling/contributor", true},
		{"tooling", "tooling/contributor", true},
		{"tooling/scaffold", "tooling/contributor", false},
		{"tooling/contributor-extra", "tooling/contributor", false},
		{".", "tooling/contributor", true},
		{"", "tooling/contributor", false},
	}
	for _, tc := range cases {
		if got := InputRootReadsProject(tc.root, tc.project); got != tc.want {
			t.Errorf("InputRootReadsProject(%q, %q) = %v, want %v", tc.root, tc.project, got, tc.want)
		}
	}
}
