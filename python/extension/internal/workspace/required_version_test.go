package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureUVRequiredVersion(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		changed bool
	}{
		{
			name:    "declares the release on the line after the [tool.uv] header",
			content: "[project]\nname = \"ws\"\n\n[tool.uv]\n\n[tool.uv.sources]\na = { workspace = true }\n",
			want:    "[project]\nname = \"ws\"\n\n[tool.uv]\nrequired-version = \"==0.11.32\"\n\n[tool.uv.sources]\na = { workspace = true }\n",
			changed: true,
		},
		{
			name:    "reads a header with spaces and a comment",
			content: "[ tool . uv ] # managed\ndev-dependencies = []\n",
			want:    "[ tool . uv ] # managed\nrequired-version = \"==0.11.32\"\ndev-dependencies = []\n",
			changed: true,
		},
		{
			name:    "keeps CRLF line endings",
			content: "[project]\r\nname = \"ws\"\r\n[tool.uv]\r\n",
			want:    "[project]\r\nname = \"ws\"\r\n[tool.uv]\r\nrequired-version = \"==0.11.32\"\r\n",
			changed: true,
		},
		{
			name:    "a [tool.uv] header on the last line without a newline",
			content: "[project]\nname = \"ws\"\n\n[tool.uv]",
			want:    "[project]\nname = \"ws\"\n\n[tool.uv]\nrequired-version = \"==0.11.32\"\n",
			changed: true,
		},
		{
			name:    "appends a [tool.uv] table when the file has none",
			content: "[project]\nname = \"ws\"\n\n[tool.uv.sources]\na = { workspace = true }",
			want:    "[project]\nname = \"ws\"\n\n[tool.uv.sources]\na = { workspace = true }\n\n[tool.uv]\nrequired-version = \"==0.11.32\"\n",
			changed: true,
		},
		{
			name:    "never changes an exact declaration",
			content: "[tool.uv]\nrequired-version = \"==0.5.18\"\n",
			want:    "[tool.uv]\nrequired-version = \"==0.5.18\"\n",
		},
		{
			name:    "never changes a range declaration",
			content: "[tool.uv]\nrequired-version = \">=0.5\"\n",
			want:    "[tool.uv]\nrequired-version = \">=0.5\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "pyproject.toml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			changed, err := EnsureUVRequiredVersion(root, "0.11.32")
			if err != nil {
				t.Fatalf("EnsureUVRequiredVersion: %v", err)
			}
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.want {
				t.Fatalf("pyproject.toml =\n%q\nwant\n%q", data, tc.want)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode = %v, want the file's own 0600", info.Mode().Perm())
			}
		})
	}

	t.Run("writes nothing when the workspace has no root pyproject.toml", func(t *testing.T) {
		root := t.TempDir()
		changed, err := EnsureUVRequiredVersion(root, "0.11.32")
		if err != nil || changed {
			t.Fatalf("EnsureUVRequiredVersion = %v, %v; want false, nil", changed, err)
		}
		if _, err := os.Stat(filepath.Join(root, "pyproject.toml")); !os.IsNotExist(err) {
			t.Fatalf("pyproject.toml was created: %v", err)
		}
	})

	t.Run("a declaration it writes survives the next workspace sync", func(t *testing.T) {
		root := t.TempDir()
		makePutnamiRC(t, root, []string{"packages/a"})
		writeFile(t, filepath.Join(root, "packages/a/pyproject.toml"), "[project]\nname = \"a\"\n")
		writeFile(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = \"ws\"\n\n[tool.uv.workspace]\nmembers = [\"packages/a\"]\n\n[tool.uv.sources]\na = { workspace = true }\n")
		if changed, err := EnsureUVRequiredVersion(root, "0.11.32"); err != nil || !changed {
			t.Fatalf("EnsureUVRequiredVersion = %v, %v; want true, nil", changed, err)
		}
		changed, _, err := SyncUVWorkspace(root)
		if err != nil {
			t.Fatalf("SyncUVWorkspace: %v", err)
		}
		if changed {
			data, _ := os.ReadFile(filepath.Join(root, "pyproject.toml"))
			t.Fatalf("SyncUVWorkspace rewrote an up-to-date manifest:\n%s", data)
		}
	})
}

func TestSyncUVWorkspaceKeepsTheTableAfterTheSources(t *testing.T) {
	root := t.TempDir()
	makePutnamiRC(t, root, []string{"packages/a", "packages/b"})
	writeFile(t, filepath.Join(root, "packages/a/pyproject.toml"), "[project]\nname = \"a\"\n")
	writeFile(t, filepath.Join(root, "packages/b/pyproject.toml"), "[project]\nname = \"b\"\n")
	writeFile(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = \"ws\"\n\n"+
		"[tool.uv.workspace]\nmembers = [\"packages/a\"]\n\n"+
		"[tool.uv.sources]\na = { workspace = true }\n\n"+
		"[tool.uv]\nrequired-version = \"==0.11.32\"\n")

	changed, _, err := SyncUVWorkspace(root)
	if err != nil {
		t.Fatalf("SyncUVWorkspace: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true: a member was added")
	}
	data, err := os.ReadFile(filepath.Join(root, "pyproject.toml"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[project]\nname = \"ws\"\n\n" +
		"[tool.uv.workspace]\nmembers = [\"packages/a\", \"packages/b\"]\n\n" +
		"[tool.uv.sources]\na = { workspace = true }\nb = { workspace = true }\n\n" +
		"[tool.uv]\nrequired-version = \"==0.11.32\"\n"
	if string(data) != want {
		t.Fatalf("pyproject.toml =\n%q\nwant\n%q", data, want)
	}

	changed, _, err = SyncUVWorkspace(root)
	if err != nil || changed {
		t.Fatalf("second SyncUVWorkspace = %v, %v; want false, nil", changed, err)
	}
}
