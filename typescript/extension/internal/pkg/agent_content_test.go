package pkg

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/agentartifact"
)

// npmAgentContentManifest is an npm extension that ships a command and its own
// agent content. It names no extension: the package.json name is the name the
// package is published and resolved under.
const npmAgentContentManifest = `{
	"cliContract": 5,
	"commands": {
		"review": {"run": [{"id": "r", "task": "t"}]}
	},
	"tasks": {"t": {"kind": "command", "command": "echo"}},
	"agentContent": {"path": "agent-content", "source": "agent-src"}
}`

const npmAgentContentPackageName = "@acme/npm-tooling"

// npmAgentContentProject writes the extension project and the npm package
// directory as the npm packager has staged it before copyHookFiles runs: the
// rewritten package.json, whose files field is given (omitted when empty).
func npmAgentContentProject(t *testing.T, reference, files string) (string, string) {
	t.Helper()
	projDir := t.TempDir()
	writeFixture(t, projDir, proto.ManifestFilename, npmAgentContentManifest)
	writeFixture(t, projDir, "putnami.json",
		`{"name":"`+npmAgentContentPackageName+`","options":{"agent-artifact":{"forbiddenContent":["password"],"requiredSkills":["npm-review"]}}}`)
	writeFixture(t, projDir, "agent-src/skills/npm-review/SKILL.md",
		"---\nname: npm-review\ndescription: Review with the npm tooling\n---\n\n# npm-review\n\nRun `putnami review`, then read references/guide.md.\n")
	writeFixture(t, projDir, "agent-src/skills/npm-review/references/guide.md", reference)
	writeFixture(t, projDir, "agent-src/agents/npm-reviewer/AGENT.md", "Review the change.\n")
	writeFixture(t, projDir, "agent-src/agents/npm-reviewer/claude.yaml", "name: npm-reviewer\ndescription: Reviewer\n")
	writeFixture(t, projDir, "agent-src/agents/npm-reviewer/codex.toml", "name = \"npm-reviewer\"\n")

	outDir := t.TempDir()
	pkg := `{"name":"` + npmAgentContentPackageName + `","version":"1.4.0"`
	if files != "" {
		pkg += `,"files":` + files
	}
	writeFixture(t, outDir, "package.json", pkg+"}\n")
	return projDir, outDir
}

func writeFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// packageTree reads every regular file beneath root by slash path.
func packageTree(t *testing.T, root string) map[string]string {
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

func TestCopyHookFiles_PackagesBoundAgentContentUnderThePackageName(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "npm-agent-content", "an-npm-package-carries-its-bound-agent-content")
	projDir, outDir := npmAgentContentProject(t, "Guide.\n", "")
	source, err := os.ReadFile(filepath.Join(projDir, proto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err := copyHookFiles(projDir, outDir, "1.4.0"); err != nil {
		t.Fatalf("copyHookFiles: %v", err)
	}

	loaded, err := proto.LoadManifest(filepath.Join(outDir, proto.ManifestFilename))
	if err != nil {
		t.Fatalf("the packaged manifest does not load: %v", err)
	}
	if loaded.CLIContract != protocolcli.AgentContentContract || loaded.Version != "1.4.0" {
		t.Fatalf("packaged manifest = cliContract %d, version %s", loaded.CLIContract, loaded.Version)
	}
	if loaded.AgentContent == nil || !loaded.AgentContent.Packaged() || loaded.AgentContent.Source != "" || loaded.AgentContent.Path != "agent-content" {
		t.Fatalf("agentContent = %+v, want the packaged form", loaded.AgentContent)
	}
	if err := agentartifact.VerifyStagedExtensionContent(outDir); err != nil {
		t.Fatalf("the package does not carry the content it binds: %v", err)
	}
	contentManifest, err := os.ReadFile(filepath.Join(outDir, "agent-content", wsproto.AgentArtifactManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	parsed, diags := wsproto.ParseAndValidateAgentArtifactManifest(contentManifest)
	if len(diags) != 0 {
		t.Fatalf("content manifest: %v", diags)
	}
	if parsed.Name != npmAgentContentPackageName || parsed.Version != "1.4.0" {
		t.Fatalf("content identity = %s@%s, want the package's", parsed.Name, parsed.Version)
	}
	if _, err := os.Stat(filepath.Join(outDir, "agent-src")); !os.IsNotExist(err) {
		t.Fatal("the authored source must not ship in the package")
	}
	after, err := os.ReadFile(filepath.Join(projDir, proto.ManifestFilename))
	if err != nil || string(after) != string(source) {
		t.Fatal("packaging must not touch the source manifest")
	}

	// A second package of the same source carries the same bytes.
	_, again := npmAgentContentProject(t, "Guide.\n", "")
	if err := copyHookFiles(projDir, again, "1.4.0"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(packageTree(t, outDir), packageTree(t, again)) {
		t.Fatal("two packages of one source differ")
	}
}

func TestCopyHookFiles_AFilesAllowlistMustCarryTheAgentContent(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "npm-agent-content", "a-files-allowlist-must-carry-the-content")
	for _, tc := range []struct {
		files string
		want  string
	}{
		{files: `["lib"]`, want: "does not include agentContent.path"},
		{files: `[]`, want: "does not include agentContent.path"},
		{files: `["lib", "agent-content", "!agent-content/.codex"]`, want: "excludes"},
		{files: `["lib", "agent-content"]`},
		{files: `["lib", "./agent-content/"]`},
		{files: `["lib", "agent-content/**"]`},
		{files: `["*"]`},
	} {
		t.Run(tc.files, func(t *testing.T) {
			projDir, outDir := npmAgentContentProject(t, "Guide.\n", tc.files)
			err := copyHookFiles(projDir, outDir, "1.4.0")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("files %s carries the content, but packaging failed: %v", tc.files, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestCopyHookFiles_RefusesAgentContentItCannotBind(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "npm-agent-content", "content-the-package-cannot-bind-fails-packaging")
	t.Run("content the declared policy forbids", func(t *testing.T) {
		projDir, outDir := npmAgentContentProject(t, "Paste the password here.\n", "")
		err := copyHookFiles(projDir, outDir, "1.4.0")
		if err == nil || !strings.Contains(err.Error(), "password") {
			t.Fatalf("err = %v, want the policy violation", err)
		}
		if _, statErr := os.Stat(filepath.Join(outDir, "agent-content")); !os.IsNotExist(statErr) {
			t.Fatal("a refused package must not stage content")
		}
	})
	t.Run("a content path the package already holds", func(t *testing.T) {
		projDir, outDir := npmAgentContentProject(t, "Guide.\n", "")
		writeFixture(t, outDir, "agent-content/index.js", "module.exports = {}\n")
		err := copyHookFiles(projDir, outDir, "1.4.0")
		if err == nil || !strings.Contains(err.Error(), "already staged") {
			t.Fatalf("err = %v, want the collision refused", err)
		}
	})
	t.Run("a manifest without agent content stays at the base contract", func(t *testing.T) {
		projDir := stageSourceManifest(t, conformingCLIManifest)
		outDir := t.TempDir()
		if err := copyHookFiles(projDir, outDir, "1.4.0"); err != nil {
			t.Fatal(err)
		}
		loaded, err := proto.LoadManifest(filepath.Join(outDir, proto.ManifestFilename))
		if err != nil {
			t.Fatal(err)
		}
		if loaded.CLIContract != protocolcli.CurrentContract || loaded.AgentContent != nil {
			t.Fatalf("cliContract = %d, agentContent = %+v; want the base contract and no content", loaded.CLIContract, loaded.AgentContent)
		}
	})
}
