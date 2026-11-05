package workspace

import "testing"

func TestProjectIDFromPathNamesTheWorkspaceRootSlash(t *testing.T) {
	t.Parallel()
	if got := ProjectIDFromPath(""); got != "/" {
		t.Errorf("ProjectIDFromPath(%q) = %q, want %q", "", got, "/")
	}
}

func TestCleanWorkspacePathCanonicalizesToSlashForm(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":               "",
		"  ":             "",
		".":              "",
		"a/./b":          "a/b",
		"a//b":           "a/b",
		"a/../b":         "b",
		"/tooling/cli/":  "tooling/cli",
		" tooling/cli  ": "tooling/cli",
	}
	for input, want := range cases {
		if got := CleanWorkspacePath(input); got != want {
			t.Errorf("CleanWorkspacePath(%q) = %q, want %q", input, got, want)
		}
	}
}
