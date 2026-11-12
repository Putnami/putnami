package workspace

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandOutputRel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		project, command, want string
	}{
		{"", "test", ".putnami/out/~test"},
		{"/", "build", ".putnami/out/~build"},
		{"a", "test", ".putnami/out/a/~test"},
		{"/a/test", "test", ".putnami/out/a/test/~test"},
		{"apps/(web)/site", "package", ".putnami/out/apps/(web)/site/~package"},
		{"a/~test", "build", ".putnami/out/a/~~test/~build"},
		{"~x/~~y", "lint", ".putnami/out/~~x/~~~y/~lint"},
	}
	for _, tt := range tests {
		if got := CommandOutputRel(tt.project, tt.command); got != tt.want {
			t.Errorf("CommandOutputRel(%q, %q) = %q, want %q", tt.project, tt.command, got, tt.want)
		}
	}
}

func TestCommandOutputDir(t *testing.T) {
	t.Parallel()
	got := CommandOutputDir("/ws", "a/test", "test")
	if want := filepath.Join("/ws", ".putnami", "out", "a", "test", "~test"); got != want {
		t.Errorf("CommandOutputDir = %q, want %q", got, want)
	}
}

// TestCommandOutputRel_NoDirectoryContainsAnother pins #1082: across projects
// that nest, are named after commands, or start with the marker, no command
// output directory equals or contains another one.
func TestCommandOutputRel_NoDirectoryContainsAnother(t *testing.T) {
	t.Parallel()
	projects := []string{"", "a", "a/test", "a/build", "a/lint", "a/test/test", "test", "~test", "a/~test", "a/~~test", "a/~build"}
	commands := []string{"test", "build", "lint", "package"}
	type owner struct{ project, command string }
	seen := map[string]owner{}
	for _, project := range projects {
		for _, command := range commands {
			seen[CommandOutputRel(project, command)] = owner{project, command}
		}
	}
	if len(seen) != len(projects)*len(commands) {
		t.Fatalf("%d distinct directories for %d (project, command) pairs", len(seen), len(projects)*len(commands))
	}
	for dir, a := range seen {
		for other, b := range seen {
			if dir != other && strings.HasPrefix(other, dir+"/") {
				t.Errorf("%v directory %q contains %v directory %q", a, dir, b, other)
			}
		}
	}
}
