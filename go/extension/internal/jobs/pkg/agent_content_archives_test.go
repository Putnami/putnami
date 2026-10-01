package pkg

import (
	"crypto/sha256"
	"encoding/hex"
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
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"

	"go.putnami.dev/go/extension/internal/platform"
)

// agentContentExtensionManifest is a Go extension that ships a runtime, a
// command and its own agent content, authored at the contract a local
// manifest with agent content needs.
const agentContentExtensionManifest = `{
	"name": "@putnami/testext",
	"cliContract": 5,
	"runtime": {
		"executable": "compiled/putnami-go",
		"prepare": {
			"command": "{extensionRoot}/bin/prepare",
			"args": ["--output", "{runtimeOutput}"],
			"inputs": ["cmd/**"]
		}
	},
	"commands": {
		"build": {"run": [{"id": "build", "task": "build"}]}
	},
	"tasks": {
		"build": {"kind": "command", "command": "{extensionRuntime}", "args": ["build"]}
	},
	"agentContent": {"path": "agent-content", "source": "agent-src"}
}`

// agentContentExtensionFixture writes that extension's project: the manifest,
// the content policy, one skill and one worker, and a pre-built runtime per
// archive platform whose bytes name the platform.
func agentContentExtensionFixture(t *testing.T, reference string) (*pctx.Context, string) {
	t.Helper()
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), agentContentExtensionManifest)
	mustWrite(t, filepath.Join(projectRoot, "putnami.json"),
		`{"name":"@putnami/testext","options":{"agent-artifact":{"forbiddenContent":["password"],"requiredSkills":["testext-review"]}}}`)
	mustWrite(t, filepath.Join(projectRoot, "agent-src", "skills", "testext-review", "SKILL.md"),
		"---\nname: testext-review\ndescription: Review with testext\n---\n\n# testext-review\n\nRun `putnami build`, then read references/guide.md.\n")
	mustWrite(t, filepath.Join(projectRoot, "agent-src", "skills", "testext-review", "references", "guide.md"), reference)
	mustWrite(t, filepath.Join(projectRoot, "agent-src", "agents", "testext-reviewer", "AGENT.md"), "Review the build output.\n")
	mustWrite(t, filepath.Join(projectRoot, "agent-src", "agents", "testext-reviewer", "claude.yaml"), "name: testext-reviewer\ndescription: Reviewer\n")
	mustWrite(t, filepath.Join(projectRoot, "agent-src", "agents", "testext-reviewer", "codex.toml"), "name = \"testext-reviewer\"\n")
	mustWrite(t, filepath.Join(projectRoot, "cmd", "putnami-go", "main.go"), "package main\nfunc main() {}\n")
	for _, target := range platform.ArchivePlatforms {
		binary := filepath.Join(outputRoot, "build", "bin", target.Suffix, pkgmeta.ExecutableName(target.GOOS, "putnami-go"))
		mustWrite(t, binary, "runtime-"+target.Suffix)
		if err := os.Chmod(binary, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project:       pctx.Project{Name: "@putnami/testext", FullPath: projectRoot},
	}, outputRoot
}

// contentEntries is every archive entry beneath the agent-content path.
func contentEntries(entries map[string][]byte) map[string]string {
	out := map[string]string{}
	for name, data := range entries {
		if rel, ok := strings.CutPrefix(name, "agent-content/"); ok {
			out[rel] = string(data)
		}
	}
	return out
}

func TestCreateExtensionArchivesShipsTheSameBoundAgentContentOnEveryPlatform(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-agent-content", "every-platform-archive-carries-the-same-bound-content")
	ctx, outputRoot := agentContentExtensionFixture(t, "Guide.\n")
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); !ok {
		t.Fatal("createExtensionArchives failed")
	}
	built, err := agentartifact.BuildExtensionContent(ctx.Project.FullPath, "agent-src", "@putnami/testext", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}

	var first map[string]string
	for _, target := range platform.ArchivePlatforms {
		entries := readArchiveEntries(t, filepath.Join(outputRoot, "archives", "putnami-testext-"+target.Suffix+".tar.gz"))
		if got := string(entries["compiled/"+pkgmeta.ExecutableName(target.GOOS, "putnami-go")]); got != "runtime-"+target.Suffix {
			t.Fatalf("%s runtime = %q: each archive carries its own platform's executable", target.Suffix, got)
		}
		for name := range entries {
			if strings.HasPrefix(name, "agent-src/") {
				t.Fatalf("%s archive ships the authored source %s", target.Suffix, name)
			}
		}

		loaded, err := proto.NegotiateManifest(proto.ManifestFilename, entries[proto.ManifestFilename])
		if err != nil {
			t.Fatalf("%s manifest does not load: %v", target.Suffix, err)
		}
		if loaded.CLIContract != protocolcli.AgentContentContract || loaded.Version != "1.2.3" {
			t.Fatalf("%s manifest = cliContract %d, version %s", target.Suffix, loaded.CLIContract, loaded.Version)
		}
		want := proto.AgentContentContribution{Path: "agent-content", ManifestSHA256: built.ManifestSHA256}
		if !reflect.DeepEqual(*loaded.AgentContent, want) {
			t.Fatalf("%s agentContent = %+v, want the packaged form %+v", target.Suffix, *loaded.AgentContent, want)
		}
		if len(loaded.Commands) != 1 || loaded.Runtime == nil {
			t.Fatalf("%s manifest lost its commands or runtime", target.Suffix)
		}

		content := contentEntries(entries)
		manifestBytes := content[wsproto.AgentArtifactManifestFilename]
		sum := sha256.Sum256([]byte(manifestBytes))
		if hex.EncodeToString(sum[:]) != built.ManifestSHA256 {
			t.Fatalf("%s content manifest does not hash to the bound digest", target.Suffix)
		}
		if len(content) != len(built.Files)+1 {
			t.Fatalf("%s carries %d content files, want the content manifest and %d declared files", target.Suffix, len(content), len(built.Files))
		}
		for file, data := range built.Files {
			if content[file] != string(data) {
				t.Errorf("%s content %s differs from the build", target.Suffix, file)
			}
		}
		if first == nil {
			first = content
		} else if !reflect.DeepEqual(first, content) {
			t.Fatalf("%s carries different agent content bytes than %s", target.Suffix, platform.ArchivePlatforms[0].Suffix)
		}
	}
}

func TestCreateExtensionArchivesRefusesAgentContentThePolicyForbids(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-agent-content", "content-the-archive-cannot-bind-fails-packaging")
	ctx, outputRoot := agentContentExtensionFixture(t, "Paste the password here.\n")
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); ok {
		t.Fatal("an extension whose agent content violates its declared policy must not package")
	}
	archives, _ := filepath.Glob(filepath.Join(outputRoot, "archives", "*.tar.gz"))
	if len(archives) != 0 {
		t.Fatalf("a refused package wrote archives: %v", archives)
	}
}

func TestCreateExtensionArchivesRefusesContentUnderAnotherName(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-agent-content", "content-the-archive-cannot-bind-fails-packaging")
	ctx, outputRoot := agentContentExtensionFixture(t, "Guide.\n")
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/renamed", "1.2.3", outputRoot, false); ok {
		t.Fatal("content must be refused when the manifest's name is not the published one")
	}
}

func TestGateAndStampManifestContract_StampsTheAdditiveContractOnlyForAgentContent(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-agent-content", "only-a-manifest-with-agent-content-is-stamped-the-additive-contract")

	// A manifest without agent content earns the base contract, even when its
	// author claimed the additive one: the stamp is what the vocabulary needs.
	claimed := strings.Replace(conformingManifest, `"name": "@putnami/testext",`,
		`"name": "@putnami/testext",
	"cliContract": 5,`, 1)
	plain := stageManifest(t, claimed)
	if err := gateAndStampManifestContract(plain); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if got := stagedContract(t, plain); got != protocolcli.CurrentContract {
		t.Fatalf("a manifest without agent content packages at %d, want %d", got, protocolcli.CurrentContract)
	}

	// A manifest with agent content, staged by the SDK, earns the additive one.
	ctx, _ := agentContentExtensionFixture(t, "Guide.\n")
	stage := t.TempDir()
	if err := copyRel(ctx.Project.FullPath, stage, "putnami.extension.json"); err != nil {
		t.Fatal(err)
	}
	stampManifestVersion(stage, "1.2.3")
	if _, err := agentartifact.StageExtensionContent(ctx.Project.FullPath, stage, "@putnami/testext", "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if err := gateAndStampManifestContract(stage); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if got := stagedContract(t, stage); got != protocolcli.AgentContentContract {
		t.Fatalf("a manifest with agent content packages at %d, want %d", got, protocolcli.AgentContentContract)
	}
}

func TestGateAndStampManifestContract_RefusesAgentContentItCannotVerify(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-agent-content", "content-the-archive-cannot-bind-fails-packaging")
	ctx, _ := agentContentExtensionFixture(t, "Guide.\n")

	// The authored form never ships: a packager that skipped the content step
	// fails the gate instead of publishing a source path.
	authored := t.TempDir()
	if err := copyRel(ctx.Project.FullPath, authored, "putnami.extension.json"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(authored, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = gateAndStampManifestContract(authored)
	if err == nil || !strings.Contains(err.Error(), "as source") {
		t.Fatalf("err = %v, want the authored form refused", err)
	}
	after, err := os.ReadFile(filepath.Join(authored, "putnami.extension.json"))
	if err != nil || string(after) != string(before) {
		t.Fatal("a refused gate must restore the staged manifest")
	}

	// Staged bytes that drift from the digest fail the gate.
	stage := t.TempDir()
	if err := copyRel(ctx.Project.FullPath, stage, "putnami.extension.json"); err != nil {
		t.Fatal(err)
	}
	stampManifestVersion(stage, "1.2.3")
	if _, err := agentartifact.StageExtensionContent(ctx.Project.FullPath, stage, "@putnami/testext", "1.2.3"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(stage, "agent-content", ".agents", "skills", "testext-review", "references", "guide.md"), "tampered\n")
	if err := gateAndStampManifestContract(stage); err == nil || !strings.Contains(err.Error(), "does not match what it binds") {
		t.Fatalf("err = %v, want tampered staged content refused", err)
	}
}
