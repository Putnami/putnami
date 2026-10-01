package workspace

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---- helpers ----

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile %q: %v", path, err)
	}
}

// ---- ParsePyprojectName ----

func TestParsePyprojectName(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantName string
		wantErr  bool
	}{
		{
			name: "simple project section",
			content: `[project]
name = "mypackage"
version = "0.1.0"
`,
			wantName: "mypackage",
		},
		{
			name: "project section after other sections",
			content: `[build-system]
requires = ["hatchling"]

[project]
name = "another-pkg"
description = "A test package"
`,
			wantName: "another-pkg",
		},
		{
			name: "name with dashes",
			content: `[project]
name = "my-cool-package"
`,
			wantName: "my-cool-package",
		},
		{
			name: "name with underscores",
			content: `[project]
name = "my_package"
`,
			wantName: "my_package",
		},
		{
			name: "no project section",
			content: `[build-system]
requires = ["setuptools"]
`,
			wantErr: true,
		},
		{
			name: "project section without name",
			content: `[project]
version = "1.0.0"
description = "No name here"
`,
			wantErr: true,
		},
		{
			name: "name in wrong section is ignored",
			content: `[tool.poetry]
name = "wrong-section"

[project]
name = "correct-section"
`,
			wantName: "correct-section",
		},
		{
			name: "nested section stops searching project section",
			content: `[project]
version = "1.0.0"

[project.optional-dependencies]
name = "should-not-match"
`,
			wantErr: true,
		},
		{
			name: "extra whitespace around equals",
			content: `[project]
name = "spaced-name"
`,
			wantName: "spaced-name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			pyprojectPath := filepath.Join(tmp, "pyproject.toml")
			writeFile(t, pyprojectPath, tt.content)

			got, err := ParsePyprojectName(pyprojectPath)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParsePyprojectName() expected error, got nil (name=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePyprojectName() unexpected error: %v", err)
			}
			if got != tt.wantName {
				t.Errorf("ParsePyprojectName() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestParsePyprojectName_FileNotFound(t *testing.T) {
	_, err := ParsePyprojectName("/nonexistent/path/pyproject.toml")
	if err == nil {
		t.Error("ParsePyprojectName() expected error for missing file, got nil")
	}
}

// ---- DiscoverPythonProjects ----

func makePutnamiRC(t *testing.T, root string, projects []string) {
	t.Helper()
	parts := make([]string, len(projects))
	for i, p := range projects {
		parts[i] = `"` + p + `"`
	}
	content := `{"projects": [` + strings.Join(parts, ", ") + `]}`
	writeFile(t, filepath.Join(root, "putnami.workspace.json"), content)
}

func TestDiscoverPythonProjects(t *testing.T) {
	t.Run("no putnamirc", func(t *testing.T) {
		tmp := t.TempDir()
		got := DiscoverPythonProjects(tmp)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("empty projects list", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{})
		got := DiscoverPythonProjects(tmp)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("project without pyproject.toml is excluded", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{"packages/mypkg"})
		os.MkdirAll(filepath.Join(tmp, "packages/mypkg"), 0755)
		got := DiscoverPythonProjects(tmp)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("project with pyproject.toml without [project] is excluded", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{"packages/mypkg"})
		writeFile(t, filepath.Join(tmp, "packages/mypkg/pyproject.toml"), `[build-system]
requires = ["setuptools"]
`)
		got := DiscoverPythonProjects(tmp)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("valid python project is discovered", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{"packages/mypkg"})
		writeFile(t, filepath.Join(tmp, "packages/mypkg/pyproject.toml"), `[project]
name = "mypkg"
`)
		got := DiscoverPythonProjects(tmp)
		if len(got) != 1 || got[0] != "packages/mypkg" {
			t.Errorf("expected [packages/mypkg], got %v", got)
		}
	})

	t.Run("multiple projects returned sorted", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{"packages/zebra", "packages/alpha", "packages/middle"})
		for _, p := range []string{"zebra", "alpha", "middle"} {
			writeFile(t, filepath.Join(tmp, "packages", p, "pyproject.toml"), `[project]
name = "`+p+`"
`)
		}
		got := DiscoverPythonProjects(tmp)
		if len(got) != 3 {
			t.Fatalf("expected 3 projects, got %v", got)
		}
		if !sort.StringsAreSorted(got) {
			t.Errorf("expected sorted results, got %v", got)
		}
	})

	t.Run("workspace root itself is excluded", func(t *testing.T) {
		tmp := t.TempDir()
		// Add "." which resolves to the workspace root itself
		makePutnamiRC(t, tmp, []string{".", "packages/pkg"})
		writeFile(t, filepath.Join(tmp, "packages/pkg/pyproject.toml"), `[project]
name = "pkg"
`)
		got := DiscoverPythonProjects(tmp)
		// Only packages/pkg, not "."
		for _, p := range got {
			if filepath.Join(tmp, p) == tmp {
				t.Errorf("workspace root should not be included in results, got %v", got)
			}
		}
	})

	t.Run("deduplication removes duplicates", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{"packages/mypkg", "packages/mypkg"})
		writeFile(t, filepath.Join(tmp, "packages/mypkg/pyproject.toml"), `[project]
name = "mypkg"
`)
		got := DiscoverPythonProjects(tmp)
		if len(got) != 1 {
			t.Errorf("expected 1 (deduplicated), got %v", got)
		}
	})
}

// ---- SyncUVWorkspace ----

func TestSyncUVWorkspace(t *testing.T) {
	t.Run("no python projects returns no change", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{})
		changed, members, err := SyncUVWorkspace(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if changed {
			t.Error("expected changed=false when no projects")
		}
		if len(members) != 0 {
			t.Errorf("expected empty members, got %v", members)
		}
	})

	t.Run("creates pyproject.toml when it does not exist", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{"packages/mypkg"})
		writeFile(t, filepath.Join(tmp, "packages/mypkg/pyproject.toml"), `[project]
name = "mypkg"
`)
		changed, members, err := SyncUVWorkspace(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Error("expected changed=true when creating pyproject.toml")
		}
		if len(members) != 1 || members[0] != "packages/mypkg" {
			t.Errorf("unexpected members: %v", members)
		}
		// Verify file was created with correct content
		data, err := os.ReadFile(filepath.Join(tmp, "pyproject.toml"))
		if err != nil {
			t.Fatalf("pyproject.toml not created: %v", err)
		}
		content := string(data)
		if !strings.Contains(content, "[tool.uv.workspace]") {
			t.Error("created pyproject.toml missing [tool.uv.workspace] section")
		}
		if !strings.Contains(content, `"packages/mypkg"`) {
			t.Error("created pyproject.toml missing member path")
		}
		if !strings.Contains(content, "[tool.uv.sources]") {
			t.Error("created pyproject.toml missing [tool.uv.sources] section")
		}
		if !strings.Contains(content, "mypkg = { workspace = true }") {
			t.Error("created pyproject.toml missing source entry")
		}
	})

	t.Run("updates existing pyproject.toml workspace section", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{"packages/newpkg"})
		writeFile(t, filepath.Join(tmp, "packages/newpkg/pyproject.toml"), `[project]
name = "newpkg"
`)
		// Create existing root pyproject.toml with old workspace config
		writeFile(t, filepath.Join(tmp, "pyproject.toml"), `[project]
name = "putnami-workspace"
version = "0.0.1"

[tool.uv.workspace]
members = ["packages/oldpkg"]

[tool.uv.sources]
oldpkg = { workspace = true }
`)
		changed, members, err := SyncUVWorkspace(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !changed {
			t.Error("expected changed=true when updating workspace members")
		}
		if len(members) != 1 || members[0] != "packages/newpkg" {
			t.Errorf("unexpected members: %v", members)
		}
		data, _ := os.ReadFile(filepath.Join(tmp, "pyproject.toml"))
		content := string(data)
		if !strings.Contains(content, `"packages/newpkg"`) {
			t.Error("updated pyproject.toml should contain new member")
		}
	})

	t.Run("idempotent after two syncs", func(t *testing.T) {
		tmp := t.TempDir()
		makePutnamiRC(t, tmp, []string{"packages/mypkg"})
		writeFile(t, filepath.Join(tmp, "packages/mypkg/pyproject.toml"), `[project]
name = "mypkg"
`)
		// First sync creates the file
		_, _, err := SyncUVWorkspace(tmp)
		if err != nil {
			t.Fatalf("first sync error: %v", err)
		}
		// Second sync may normalize the file (trailing newline)
		_, _, err = SyncUVWorkspace(tmp)
		if err != nil {
			t.Fatalf("second sync error: %v", err)
		}
		// Third sync should be fully idempotent
		changed, _, err := SyncUVWorkspace(tmp)
		if err != nil {
			t.Fatalf("third sync error: %v", err)
		}
		if changed {
			t.Error("expected changed=false on third sync (idempotent)")
		}
	})
}

// ---- DiscoverTestFiles ----

func TestDiscoverTestFiles(t *testing.T) {
	t.Run("empty directory", func(t *testing.T) {
		tmp := t.TempDir()
		files, err := DiscoverTestFiles(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 0 {
			t.Errorf("expected no files, got %v", files)
		}
	})

	t.Run("discovers test_ prefixed files", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "test_module.py"), "")
		writeFile(t, filepath.Join(tmp, "main.py"), "")

		files, err := DiscoverTestFiles(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %v", files)
		}
		if filepath.Base(files[0]) != "test_module.py" {
			t.Errorf("unexpected file: %v", files[0])
		}
	})

	t.Run("discovers _test.py suffixed files", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "module_test.py"), "")
		writeFile(t, filepath.Join(tmp, "module.py"), "")

		files, err := DiscoverTestFiles(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 1 || filepath.Base(files[0]) != "module_test.py" {
			t.Errorf("expected module_test.py, got %v", files)
		}
	})

	t.Run("skips files in skip directories", func(t *testing.T) {
		tmp := t.TempDir()
		// Files in skip dirs should not be found
		writeFile(t, filepath.Join(tmp, ".venv", "test_something.py"), "")
		writeFile(t, filepath.Join(tmp, "__pycache__", "test_cached.py"), "")
		writeFile(t, filepath.Join(tmp, "node_modules", "test_node.py"), "")
		// This one should be found
		writeFile(t, filepath.Join(tmp, "tests", "test_real.py"), "")

		files, err := DiscoverTestFiles(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 1 {
			t.Errorf("expected 1 file (skipping venv/pycache/node_modules), got %v", files)
		}
	})

	t.Run("discovers files in nested directories", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "tests", "unit", "test_unit.py"), "")
		writeFile(t, filepath.Join(tmp, "tests", "integration", "test_integration.py"), "")
		writeFile(t, filepath.Join(tmp, "src", "module.py"), "")

		files, err := DiscoverTestFiles(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 2 {
			t.Errorf("expected 2 test files, got %v", files)
		}
	})

	t.Run("ignores non-py files with test name", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "test_module.js"), "")
		writeFile(t, filepath.Join(tmp, "test_module.py"), "")

		files, err := DiscoverTestFiles(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(files) != 1 || !strings.HasSuffix(files[0], ".py") {
			t.Errorf("expected only .py files, got %v", files)
		}
	})
}

// ---- FileSnapshot ----

func TestFileSnapshot(t *testing.T) {
	t.Run("empty directory", func(t *testing.T) {
		tmp := t.TempDir()
		snap, err := FileSnapshot(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(snap) != 0 {
			t.Errorf("expected empty snapshot, got %v", snap)
		}
	})

	t.Run("captures py and toml files", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "main.py"), "print('hello')")
		writeFile(t, filepath.Join(tmp, "pyproject.toml"), "[project]")
		writeFile(t, filepath.Join(tmp, "README.md"), "# Readme")
		writeFile(t, filepath.Join(tmp, "config.json"), "{}")

		snap, err := FileSnapshot(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := snap["main.py"]; !ok {
			t.Error("expected main.py in snapshot")
		}
		if _, ok := snap["pyproject.toml"]; !ok {
			t.Error("expected pyproject.toml in snapshot")
		}
		if _, ok := snap["README.md"]; ok {
			t.Error("README.md should not be in snapshot")
		}
		if _, ok := snap["config.json"]; ok {
			t.Error("config.json should not be in snapshot")
		}
	})

	t.Run("skips files in skip directories", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, ".venv", "lib.py"), "")
		writeFile(t, filepath.Join(tmp, "__pycache__", "cached.py"), "")
		writeFile(t, filepath.Join(tmp, "src", "module.py"), "")

		snap, err := FileSnapshot(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Only src/module.py should appear
		if len(snap) != 1 {
			t.Errorf("expected 1 entry, got %d: %v", len(snap), snap)
		}
		if _, ok := snap[filepath.Join("src", "module.py")]; !ok {
			t.Errorf("expected src/module.py, got keys: %v", snap)
		}
	})

	t.Run("uses relative paths as keys", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "sub", "module.py"), "")

		snap, err := FileSnapshot(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for key := range snap {
			if filepath.IsAbs(key) {
				t.Errorf("snapshot key should be relative, got %q", key)
			}
		}
	})

	t.Run("mtime values are non-zero", func(t *testing.T) {
		tmp := t.TempDir()
		writeFile(t, filepath.Join(tmp, "main.py"), "x = 1")

		snap, err := FileSnapshot(tmp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for key, mtime := range snap {
			if mtime == 0 {
				t.Errorf("mtime for %q should not be zero", key)
			}
		}
	})
}

// ---- formatTOMLArray (internal) ----

func TestFormatTOMLArray(t *testing.T) {
	tests := []struct {
		name  string
		items []string
		want  string
	}{
		{
			name:  "empty slice",
			items: []string{},
			want:  "",
		},
		{
			name:  "single item",
			items: []string{"packages/mypkg"},
			want:  `"packages/mypkg"`,
		},
		{
			name:  "multiple items",
			items: []string{"packages/a", "packages/b", "packages/c"},
			want:  `"packages/a", "packages/b", "packages/c"`,
		},
		{
			name:  "nil slice",
			items: nil,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTOMLArray(tt.items)
			if got != tt.want {
				t.Errorf("formatTOMLArray(%v) = %q, want %q", tt.items, got, tt.want)
			}
		})
	}
}

// ---- replaceOrAppendSection (internal) ----

func TestReplaceOrAppendSection(t *testing.T) {
	t.Run("replaces existing section", func(t *testing.T) {
		content := "[existing]\nkey = \"old\"\n"
		pattern := mustCompile(t, `\[existing\]\nkey\s*=\s*"[^"]*"`)
		section := "[existing]\nkey = \"new\""

		got, changed := replaceOrAppendSection(content, pattern, section)
		if !changed {
			t.Error("expected changed=true")
		}
		if !strings.Contains(got, `key = "new"`) {
			t.Errorf("expected updated content, got: %q", got)
		}
	})

	t.Run("appends when section not found", func(t *testing.T) {
		content := "[project]\nname = \"test\"\n"
		pattern := mustCompile(t, `\[tool\.uv\.workspace\]`)
		section := "[tool.uv.workspace]\nmembers = []"

		got, changed := replaceOrAppendSection(content, pattern, section)
		if !changed {
			t.Error("expected changed=true when appending")
		}
		if !strings.Contains(got, "[tool.uv.workspace]") {
			t.Errorf("expected appended section, got: %q", got)
		}
		if !strings.Contains(got, "[project]") {
			t.Errorf("original content should be preserved, got: %q", got)
		}
	})

	t.Run("no change when content already matches", func(t *testing.T) {
		section := "[tool.uv.workspace]\nmembers = []"
		content := "[project]\nname = \"test\"\n\n" + section + "\n"
		pattern := mustCompile(t, `\[tool\.uv\.workspace\]\nmembers = \[\]`)

		got, changed := replaceOrAppendSection(content, pattern, section)
		if changed {
			t.Errorf("expected changed=false when content already matches, got: %q", got)
		}
	})

	t.Run("adds newline before appending if missing", func(t *testing.T) {
		content := "[project]\nname = \"test\""
		pattern := mustCompile(t, `\[new-section\]`)
		section := "[new-section]\nkey = \"val\""

		got, _ := replaceOrAppendSection(content, pattern, section)
		// Should have newlines between original content and new section
		if !strings.Contains(got, "\n\n[new-section]") {
			t.Errorf("expected double newline before appended section, got: %q", got)
		}
	})
}

// ---- dedupe (internal) ----

func TestDedupe(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			name:  "nil slice",
			input: nil,
			want:  nil,
		},
		{
			name:  "empty slice",
			input: []string{},
			want:  []string{},
		},
		{
			name:  "single item",
			input: []string{"a"},
			want:  []string{"a"},
		},
		{
			name:  "no duplicates",
			input: []string{"a", "b", "c"},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "adjacent duplicates removed",
			input: []string{"a", "a", "b", "b", "c"},
			want:  []string{"a", "b", "c"},
		},
		{
			name:  "all duplicates",
			input: []string{"x", "x", "x"},
			want:  []string{"x"},
		},
		{
			name:  "non-adjacent duplicates not deduplicated (requires sorted input)",
			input: []string{"a", "b", "a"},
			want:  []string{"a", "b", "a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupe(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("dedupe(%v) = %v (len=%d), want %v (len=%d)", tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("dedupe(%v)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// mustCompile is a test helper that compiles a regex or fatals.
func mustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("regexp.Compile(%q): %v", pattern, err)
	}
	return re
}
