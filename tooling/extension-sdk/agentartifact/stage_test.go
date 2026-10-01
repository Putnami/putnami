package agentartifact

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

// runtimeContentManifest is an authored extension manifest that ships an
// executable beside its agent content: a runtime, a command and a tool, the
// shape a language extension packages.
const runtimeContentManifest = `{
  "name": "` + contentExtensionName + `",
  "cliContract": 5,
  "runtime": {"executable": "compiled/acme"},
  "commands": {"acme": {"description": "Run acme", "run": [{"id": "acme", "task": "acme"}]}},
  "tasks": {"acme": {"kind": "command", "command": "{extensionRuntime}", "args": ["acme"]}},
  "agentContent": {"path": "agent-content", "source": "agent-src", "supersedes": ["@acme/old-workflows"]}
}
`

// stageExtension copies an authored manifest into a fresh stage and stamps the
// version into it, as a packager does before staging agent content.
func stageExtension(t *testing.T, manifest, version string) string {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(manifest), &raw); err != nil {
		t.Fatalf("fixture manifest: %v", err)
	}
	raw["version"] = version
	stamped, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	writeSource(t, stage, extproto.ManifestFilename, string(stamped)+"\n")
	return stage
}

// stagedTree reads every file beneath root by slash path; a symlink is
// recorded by its target, never followed.
func stagedTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			tree[filepath.ToSlash(rel)] = "symlink to " + target
			return err
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		tree[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestStageExtensionContentBindsTheBuiltTreeBesideTheExecutable(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-staging", "a-staged-extension-carries-the-built-content-bound-by-digest")
	root := newContentExtension(t, runtimeContentManifest)
	stage := stageExtension(t, runtimeContentManifest, "2.1.0")

	content, err := StageExtensionContent(root, stage, contentExtensionName, "2.1.0")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	built, err := BuildExtensionContent(root, "agent-src", contentExtensionName, "2.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if content.ManifestSHA256 != built.ManifestSHA256 {
		t.Fatal("staging must build exactly what the shared builder builds")
	}

	stagedManifest, err := os.ReadFile(filepath.Join(stage, extproto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	m, diags := extproto.ParseManifest(stagedManifest)
	if len(diags) != 0 {
		t.Fatalf("staged manifest: %v", diags)
	}
	want := extproto.AgentContentContribution{
		Path:           "agent-content",
		ManifestSHA256: built.ManifestSHA256,
		Supersedes:     []string{"@acme/old-workflows"},
	}
	if !reflect.DeepEqual(*m.AgentContent, want) {
		t.Fatalf("staged agentContent = %+v, want the packaged form %+v", *m.AgentContent, want)
	}
	if m.Runtime == nil || m.Runtime.Executable != "compiled/acme" || len(m.Commands) != 1 || m.Version != "2.1.0" {
		t.Fatalf("staging must keep every other field: %+v", m)
	}

	tree := stagedTree(t, filepath.Join(stage, "agent-content"))
	if got := tree[wsproto.AgentArtifactManifestFilename]; got != string(built.Manifest) {
		t.Fatal("the staged content manifest differs from the built one")
	}
	if len(tree) != len(built.Files)+1 {
		t.Fatalf("staged content has %d files, want the content manifest and %d declared files", len(tree), len(built.Files))
	}
	for file, data := range built.Files {
		if tree[file] != string(data) {
			t.Errorf("staged %s differs from the built file", file)
		}
	}
	// Windows has no mode bits to pin: it reports 0666 for every writable
	// file, and the archive writers record 0644 for a file packaged there.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(stage, "agent-content", ".agents", "skills", "acme-review", "scripts", "check.sh"))
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("staged content mode = %v (err %v), want 0644", info, err)
		}
	}
	if err := VerifyStagedExtensionContent(stage); err != nil {
		t.Fatalf("the staged tree must verify: %v", err)
	}

	// Staging is deterministic: a second stage of the same source is byte for
	// byte the same, which is what makes every platform archive carry the same
	// content.
	second := stageExtension(t, runtimeContentManifest, "2.1.0")
	if _, err := StageExtensionContent(root, second, contentExtensionName, "2.1.0"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stagedTree(t, stage), stagedTree(t, second)) {
		t.Fatal("two stages of one source differ")
	}

	// The contract gate stamps the contract the staged vocabulary requires.
	if got := extproto.RequiredCLIContract(m); got != protocolcli.AgentContentContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.AgentContentContract)
	}
}

func TestStageExtensionContentLeavesAManifestWithoutContentUntouched(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-staging", "a-staged-extension-carries-the-built-content-bound-by-digest")
	for name, manifest := range map[string]string{
		"runtime and commands": `{"name":"@acme/tool","commands":{"x":{"run":[{"id":"x","task":"x"}]}},"tasks":{"x":{"kind":"command","command":"echo"}}}` + "\n",
		"hook only":            `{"commands": {}, "hooks": {"preBuild": {"kind": "command", "command": "echo"}}}`,
		"null agent content":   `{"name":"@acme/tool","agentContent":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			stage := t.TempDir()
			writeSource(t, stage, extproto.ManifestFilename, manifest)
			content, err := StageExtensionContent(t.TempDir(), stage, "@acme/tool", "1.0.0")
			if err != nil || content != nil {
				t.Fatalf("content = %v, err = %v; want nothing staged", content, err)
			}
			if tree := stagedTree(t, stage); !reflect.DeepEqual(tree, map[string]string{extproto.ManifestFilename: manifest}) {
				t.Fatalf("the stage changed: %v", tree)
			}
			if err := VerifyStagedExtensionContent(stage); err != nil {
				t.Fatalf("a manifest without content verifies trivially: %v", err)
			}
		})
	}
}

func TestStageExtensionContentRefusesWhatItCannotBind(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-staging", "staging-refuses-what-it-cannot-bind")
	digest := strings.Repeat("a", 64)
	cases := []struct {
		name     string
		manifest string
		publish  string
		prepare  func(t *testing.T, root, stage string)
		want     string
	}{
		{
			name:     "a committed digest instead of a source",
			manifest: `{"name":"` + contentExtensionName + `","agentContent":{"path":"agent-content","manifestSha256":"` + digest + `"}}`,
			publish:  contentExtensionName,
			want:     "without a source",
		},
		{
			name:     "a name the package does not publish",
			manifest: runtimeContentManifest,
			publish:  "@acme/other",
			want:     "must agree",
		},
		{
			name:     "no name at all",
			manifest: `{"agentContent":{"path":"agent-content","source":"agent-src"}}`,
			want:     "no name",
		},
		{
			name:     "a path the stage already holds",
			manifest: runtimeContentManifest,
			publish:  contentExtensionName,
			prepare: func(t *testing.T, _, stage string) {
				writeSource(t, stage, "agent-content/README.md", "staged by the packager\n")
			},
			want: "already staged",
		},
		{
			name:     "a path below a staged file",
			manifest: strings.Replace(runtimeContentManifest, `"path": "agent-content"`, `"path": "bin/agent-content"`, 1),
			publish:  contentExtensionName,
			prepare: func(t *testing.T, _, stage string) {
				writeSource(t, stage, "bin", "a file, not a directory\n")
			},
			want: "symlink or a file",
		},
		{
			name:     "a path below a staged symlink",
			manifest: strings.Replace(runtimeContentManifest, `"path": "agent-content"`, `"path": "linked/agent-content"`, 1),
			publish:  contentExtensionName,
			prepare: func(t *testing.T, _, stage string) {
				if err := os.Symlink(t.TempDir(), filepath.Join(stage, "linked")); err != nil {
					t.Fatal(err)
				}
			},
			want: "symlink or a file",
		},
		{
			name:     "content the declared policy forbids",
			manifest: runtimeContentManifest,
			publish:  contentExtensionName,
			prepare: func(t *testing.T, root, _ string) {
				writeSource(t, root, "agent-src/skills/acme-review/references/guide.md", "Paste the password here.\n")
			},
			want: "password",
		},
		{
			name:     "a staged version the content is not built at",
			manifest: strings.Replace(runtimeContentManifest, `"cliContract": 5,`, `"cliContract": 5, "version": "9.9.9",`, 1),
			publish:  contentExtensionName,
			want:     "declares version 9.9.9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newContentExtension(t, tc.manifest)
			stage := t.TempDir()
			writeSource(t, stage, extproto.ManifestFilename, tc.manifest)
			if tc.prepare != nil {
				tc.prepare(t, root, stage)
			}
			before := stagedTree(t, stage)
			_, err := StageExtensionContent(root, stage, tc.publish, "1.0.0")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if after := stagedTree(t, stage); !reflect.DeepEqual(before, after) {
				t.Fatal("a refused stage must be left exactly as it was")
			}
		})
	}
}

func TestVerifyStagedExtensionContentRefusesAnyDrift(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-staging", "the-staged-content-is-verified-byte-for-byte")
	cases := []struct {
		name   string
		mutate func(t *testing.T, stage string)
		want   string
	}{
		{
			name: "a tampered file",
			mutate: func(t *testing.T, stage string) {
				writeSource(t, stage, "agent-content/.agents/skills/acme-review/references/guide.md", "tampered\n")
			},
			want: "hashes to",
		},
		{
			name: "a tampered content manifest",
			mutate: func(t *testing.T, stage string) {
				path := filepath.Join(stage, "agent-content", wsproto.AgentArtifactManifestFilename)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				writeSource(t, stage, "agent-content/"+wsproto.AgentArtifactManifestFilename, string(data)+" ")
			},
			want: "agentContent.manifestSha256 binds",
		},
		{
			name: "an undeclared file",
			mutate: func(t *testing.T, stage string) {
				writeSource(t, stage, "agent-content/.agents/skills/acme-review/extra.md", "extra\n")
			},
			want: "does not declare",
		},
		{
			name: "a missing file",
			mutate: func(t *testing.T, stage string) {
				if err := os.Remove(filepath.Join(stage, "agent-content", ".codex", "agents", "acme-reviewer.toml")); err != nil {
					t.Fatal(err)
				}
			},
			want: "lacks .codex/agents/acme-reviewer.toml",
		},
		{
			name: "a symlinked file",
			mutate: func(t *testing.T, stage string) {
				target := filepath.Join(stage, "agent-content", ".codex", "agents", "acme-reviewer.toml")
				data, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				elsewhere := filepath.Join(t.TempDir(), "worker.toml")
				if err := os.WriteFile(elsewhere, data, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, target); err != nil {
					t.Fatal(err)
				}
			},
			want: "symlink",
		},
		{
			name: "the authored form",
			mutate: func(t *testing.T, stage string) {
				writeSource(t, stage, extproto.ManifestFilename, runtimeContentManifest)
			},
			want: "as source",
		},
		{
			name: "a content version the extension does not declare",
			mutate: func(t *testing.T, stage string) {
				path := filepath.Join(stage, extproto.ManifestFilename)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				writeSource(t, stage, extproto.ManifestFilename, strings.Replace(string(data), `"version": "2.1.0"`, `"version": "2.2.0"`, 1))
			},
			want: "is version 2.1.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newContentExtension(t, runtimeContentManifest)
			stage := stageExtension(t, runtimeContentManifest, "2.1.0")
			if _, err := StageExtensionContent(root, stage, contentExtensionName, "2.1.0"); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, stage)
			err := VerifyStagedExtensionContent(stage)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestWriteExtensionArchiveWritesThePackagedBytes(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-packaging", "a-content-only-extension-packages-with-the-additive-stamp")
	pkg, err := PackageExtension(newContentExtension(t, ""), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "nested", "acme-contributor-linux-x64.tar.gz")
	if err := WriteExtensionArchive(target, pkg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != string(pkg.Archive) {
		t.Fatalf("written archive differs from the package (err %v)", err)
	}
	if err := WriteExtensionArchive(target, nil); err == nil {
		t.Fatal("an empty package must be refused")
	}
	if err := WriteExtensionArchive(" ", pkg); err == nil {
		t.Fatal("an empty path must be refused")
	}
}
