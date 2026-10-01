package packaging

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/agentartifact"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

const contentExtensionName = "@acme/contributor"

// writeContentExtensionProject creates a content-only extension whose authored
// source is src.
func writeContentExtensionProject(t *testing.T, workspaceRoot, manifest string) string {
	t.Helper()
	projectDir := filepath.Join(workspaceRoot, "mycontent")
	if manifest == "" {
		manifest = `{"name":"` + contentExtensionName + `","agentContent":{"path":"content","source":"src"}}`
	}
	writeProjectFile(t, projectDir, extproto.ManifestFilename, manifest)
	writeProjectFile(t, projectDir, "putnami.json", `{"name":"`+contentExtensionName+`","options":{"agent-artifact":{"forbiddenContent":["password"],"requiredSkills":["acme-plan"]}}}`)
	writeProjectFile(t, projectDir, "src/skills/acme-plan/SKILL.md", "---\nname: acme-plan\ndescription: Plan a change\n---\n\n# Plan\n\nDo the planning.\n")
	writeProjectFile(t, projectDir, "src/agents/acme-planner/AGENT.md", "Plan the change you are given.\n")
	writeProjectFile(t, projectDir, "src/agents/acme-planner/claude.yaml", "name: acme-planner\ndescription: Planner\n")
	writeProjectFile(t, projectDir, "src/agents/acme-planner/codex.toml", "name = \"acme-planner\"\n")
	return projectDir
}

func newContentExtensionCtx(workspaceRoot string) *pctx.Context {
	return &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		Project:       pctx.Project{Name: contentExtensionName, Path: "mycontent"},
		Workspace:     pctx.Workspace{Version: "1.2.3"},
		Params:        pctx.Params{},
	}
}

// tarMembers reads every regular member of a tar.gz by name.
func tarMembers(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(gz)
	members := map[string][]byte{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return members
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = data
	}
}

func TestContentExtensionIsWrittenOncePerPlatformKeyWithIdenticalBytes(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "content-only-extension-packaging", "a-content-only-extension-is-written-once-per-platform-key-with-identical-bytes")
	tmp := t.TempDir()
	projectDir := writeContentExtensionProject(t, tmp, "")
	status, result, err := Content(newContentExtensionCtx(tmp), jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("status = %q, err = %v", status, err)
	}
	if result["name"] != contentExtensionName {
		t.Fatalf("dispatched to the wrong packager: %v", result)
	}

	want, err := agentartifact.PackageExtension(projectDir, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	outputDir := pkgmeta.PackageOutputDir(tmp, "mycontent", "archives")
	for _, platform := range pkgmeta.ArchivePlatformSuffixes() {
		data, err := os.ReadFile(filepath.Join(outputDir, "acme-contributor-"+platform+".tar.gz"))
		if err != nil {
			t.Fatalf("no archive for %s: %v", platform, err)
		}
		if !bytes.Equal(data, want.Archive) {
			t.Fatalf("the %s archive differs from the platform-independent package", platform)
		}
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(pkgmeta.ArchivePlatformSuffixes())+1 {
		t.Fatalf("archives/ holds %d entries, want one archive per platform key and the channel record", len(entries))
	}

	members := tarMembers(t, want.Archive)
	loaded, err := extproto.NegotiateManifest(extproto.ManifestFilename, members[extproto.ManifestFilename])
	if err != nil {
		t.Fatalf("the packaged manifest does not load: %v", err)
	}
	if loaded.CLIContract != protocolcli.AgentContentContract || loaded.Version != "1.2.3" || !loaded.AgentContent.Packaged() {
		t.Fatalf("packaged manifest = %+v", loaded)
	}
	if _, ok := members["content/"+wsproto.AgentArtifactManifestFilename]; !ok {
		t.Fatal("the archive does not carry the built content under agentContent.path")
	}
	for name := range members {
		if strings.HasPrefix(name, "src/") {
			t.Fatalf("the authored source ships in the archive: %s", name)
		}
	}

	metadata, err := pkgmeta.ReadPackageMetadata(tmp, "mycontent")
	if err != nil || metadata == nil {
		t.Fatalf("metadata = %+v, err = %v", metadata, err)
	}
	if metadata.Artifact != "acme-contributor" || metadata.Version != "1.2.3" || metadata.Template || !metadata.HasChannel("archives") {
		t.Fatalf("publication manifest = %+v, want the encoded name on the archives channel, keyed per platform", metadata)
	}

	// A second run over the same source writes the same bytes.
	status, again, err := Content(newContentExtensionCtx(tmp), jsonl.New(), nil)
	if err != nil || status != "OK" || again["archiveSha256"] != result["archiveSha256"] {
		t.Fatalf("second run: status %q, err %v, digest %v vs %v", status, err, again["archiveSha256"], result["archiveSha256"])
	}
}

func TestContentExtensionDryRunReportsWithoutWriting(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "content-only-extension-packaging", "a-content-only-extension-is-written-once-per-platform-key-with-identical-bytes")
	tmp := t.TempDir()
	writeContentExtensionProject(t, tmp, "")
	status, result, err := Content(newContentExtensionCtx(tmp), jsonl.New(), []string{"--dry-run"})
	if err != nil || status != "OK" || result["dryRun"] != true {
		t.Fatalf("status = %q, err = %v, result = %v", status, err, result)
	}
	if got, want := result["archives"], []string{
		"acme-contributor-linux-x64.tar.gz", "acme-contributor-linux-arm64.tar.gz",
		"acme-contributor-darwin-x64.tar.gz", "acme-contributor-darwin-arm64.tar.gz",
		"acme-contributor-windows-x64.tar.gz",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("archives = %v, want %v", got, want)
	}
	if _, err := os.Stat(pkgmeta.PackageOutputDir(tmp, "mycontent", "archives")); !os.IsNotExist(err) {
		t.Fatal("a dry run must not create the archives directory")
	}
}

func TestContentExtensionLeavesAnExtensionThatRunsSomethingToItsLanguageExtension(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "content-only-extension-packaging", "an-extension-that-runs-something-is-left-to-its-language-extension")
	tmp := t.TempDir()
	writeContentExtensionProject(t, tmp, `{"name":"`+contentExtensionName+`","commands":{"x":{"run":[{"id":"x","task":"x"}]}},"tasks":{"x":{"kind":"command","command":"echo"}},"agentContent":{"path":"content","source":"src"}}`)
	status, _, err := Content(newContentExtensionCtx(tmp), jsonl.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED: an extension with commands is packaged by its language extension", status)
	}
	if _, err := os.Stat(pkgmeta.PackageOutputDir(tmp, "mycontent", "archives")); !os.IsNotExist(err) {
		t.Fatal("a refused package must write nothing")
	}
}

func TestContentDispatchRefusesTwoContentForms(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "source-selects-the-content-form", "the-project-selects-its-packager-and-declaring-both-fails")
	t.Run("a template that declares agent content", func(t *testing.T) {
		tmp := t.TempDir()
		projectDir := writeTemplateProject(t, tmp, validManifest)
		writeProjectFile(t, projectDir, extproto.ManifestFilename, `{"name":"x","agentContent":{"path":"content","source":"agent"}}`)
		if status, _, _ := Content(newCtx(tmp), jsonl.New(), nil); status != "FAILED" {
			t.Fatalf("status = %q, want FAILED", status)
		}
	})
	t.Run("an unparseable extension manifest", func(t *testing.T) {
		tmp := t.TempDir()
		writeContentExtensionProject(t, tmp, `{"agentContent":`)
		if status, _, _ := Content(newContentExtensionCtx(tmp), jsonl.New(), nil); status != "FAILED" {
			t.Fatalf("status = %q, want FAILED", status)
		}
	})
	t.Run("a template whose own extension manifest declares no content", func(t *testing.T) {
		tmp := t.TempDir()
		projectDir := writeTemplateProject(t, tmp, validManifest)
		writeProjectFile(t, projectDir, extproto.ManifestFilename, `{"name":"x","commands":{}}`)
		if status, result, _ := Content(newCtx(tmp), jsonl.New(), []string{"--dry-run"}); status != "OK" || result["name"] != "my-template" {
			t.Fatalf("status = %q, result = %v; want the template packaged", status, result)
		}
	})
}

// writeProjectFile writes one file of a test project, creating its parents.
func writeProjectFile(t *testing.T, projectDir, rel, content string) {
	t.Helper()
	full := filepath.Join(projectDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// TestContentDispatchesOnTheProjectsOwnActivationFile is the invariant that lets
// one task own one archive directory: the project selects its packager, and a
// project that declares no content form is skipped. Agent content ships only
// inside an extension, so a bare src/skills/ tree selects nothing.
func TestContentDispatchesOnTheProjectsOwnActivationFile(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "source-selects-the-content-form", "the-project-selects-its-packager-and-declaring-both-fails")
	t.Run("template", func(t *testing.T) {
		tmp := t.TempDir()
		writeTemplateProject(t, tmp, validManifest)
		status, result, err := Content(newCtx(tmp), jsonl.New(), nil)
		if err != nil || status != "OK" || result["name"] != "my-template" {
			t.Fatalf("status = %q, result = %v, err = %v", status, result, err)
		}
	})
	t.Run("content-only extension", func(t *testing.T) {
		tmp := t.TempDir()
		writeContentExtensionProject(t, tmp, "")
		status, result, err := Content(newContentExtensionCtx(tmp), jsonl.New(), []string{"--dry-run"})
		if err != nil || status != "OK" || result["name"] != contentExtensionName {
			t.Fatalf("status = %q, result = %v, err = %v", status, result, err)
		}
	})
	t.Run("a bare agent source tree", func(t *testing.T) {
		tmp := t.TempDir()
		projectDir := filepath.Join(tmp, "mycontent")
		writeProjectFile(t, projectDir, "putnami.json", `{"name":"`+contentExtensionName+`"}`)
		writeProjectFile(t, projectDir, "src/skills/acme-plan/SKILL.md", "---\nname: acme-plan\ndescription: Plan a change\n---\n\nPlan.\n")
		status, _, err := Content(newContentExtensionCtx(tmp), jsonl.New(), nil)
		if err != nil || status != "SKIP" {
			t.Fatalf("status = %q, err = %v, want SKIP: agent content ships only inside an extension", status, err)
		}
		if _, err := os.Stat(pkgmeta.PackageOutputDir(tmp, "mycontent", "archives")); !os.IsNotExist(err) {
			t.Fatal("a skipped project must write nothing")
		}
	})
	t.Run("neither", func(t *testing.T) {
		tmp := t.TempDir()
		if err := os.MkdirAll(filepath.Join(tmp, "mytemplate"), 0o755); err != nil {
			t.Fatal(err)
		}
		if status, _, err := Content(newCtx(tmp), jsonl.New(), nil); err != nil || status != "SKIP" {
			t.Fatalf("status = %q, err = %v, want SKIP", status, err)
		}
	})
	t.Run("no project context", func(t *testing.T) {
		if status, _, err := Content(&pctx.Context{WorkspaceRoot: t.TempDir()}, jsonl.New(), nil); err != nil || status != "SKIP" {
			t.Fatalf("status = %q, err = %v, want SKIP", status, err)
		}
	})
}

// The archive keys are the SDK's distribution matrix, the one the Go packager
// publishes extension archives under, in the same order.
func TestContentExtensionArchiveKeysAreTheSDKDistributionMatrix(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "content-only-extension-packaging", "the-archive-keys-are-the-sdk-distribution-matrix")
	suffixes := pkgmeta.ArchivePlatformSuffixes()
	if len(suffixes) == 0 || !reflect.DeepEqual(contentExtensionPlatforms(), suffixes) {
		t.Fatalf("content extension keys = %v, SDK matrix = %v", contentExtensionPlatforms(), suffixes)
	}
	tmp := t.TempDir()
	writeContentExtensionProject(t, tmp, "")
	_, result, err := Content(newContentExtensionCtx(tmp), jsonl.New(), []string{"--dry-run"})
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(suffixes))
	for _, suffix := range suffixes {
		want = append(want, "acme-contributor-"+suffix+".tar.gz")
	}
	if !reflect.DeepEqual(result["archives"], want) {
		t.Fatalf("archives = %v, want %v", result["archives"], want)
	}
}

func TestContentExtensionStripsThePreReleaseSuffixForStable(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "resolved-version", "stable-strips-the-pre-release-suffix-for-a-content-only-extension")
	tmp := t.TempDir()
	writeContentExtensionProject(t, tmp, "")
	ctx := newContentExtensionCtx(tmp)
	ctx.Workspace.Version = "2.0.0-rc.1"
	status, result, err := Content(ctx, jsonl.New(), []string{"--stable", "--dry-run"})
	if err != nil || status != "OK" || result["version"] != "2.0.0" {
		t.Fatalf("status = %q, version = %v, err = %v; want 2.0.0", status, result["version"], err)
	}
}

// The packager reports the SDK's refusal rather than shipping the archive
// anyway, and leaves nothing behind.
func TestContentExtensionFailsOnADeclaredPolicyViolation(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "content-only-extension-packaging", "a-declared-policy-violation-fails-the-job")
	tmp := t.TempDir()
	projectDir := writeContentExtensionProject(t, tmp, "")
	writeProjectFile(t, projectDir, "src/skills/acme-plan/SKILL.md", "---\nname: acme-plan\ndescription: Plan a change\n---\n\nUse the password admin123.\n")
	status, _, err := Content(newContentExtensionCtx(tmp), jsonl.New(), nil)
	if err != nil || status != "FAILED" {
		t.Fatalf("status = %q, err = %v, want FAILED", status, err)
	}
	if _, err := os.Stat(pkgmeta.PackageOutputDir(tmp, "mycontent", "archives")); !os.IsNotExist(err) {
		t.Fatal("a refused package must leave no archive directory behind")
	}
}

func TestEncodeArtifactNameMatchesTheRegistryKey(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "content-only-extension-packaging", "the-file-key-encodes-the-extension-name-the-registry-uses")
	for _, testCase := range []struct{ name, want string }{
		{"@putnami/contributor", "putnami-contributor"},
		{"@acme/a/b", "acme-a/b"},
		{"plain", "plain"},
		{"@noslash", "noslash"},
	} {
		if got := encodeArtifactName(testCase.name); got != testCase.want {
			t.Errorf("encodeArtifactName(%q) = %q, want %q", testCase.name, got, testCase.want)
		}
	}
}

// contributorRoot is the committed @putnami/contributor extension. Its
// manifest, content policy and source are this extension's test inputs, so
// the cache key follows the real content.
func contributorRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "contributor"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestTheContributorExtensionPackagesThroughThePackageJob(t *testing.T) {
	spectest.Proves(t, "tooling/project-scaffolding", "content-only-extension-packaging", "the-contributor-extension-packages-through-the-package-job")
	root := contributorRoot(t)
	workspaceRoot := filepath.Dir(filepath.Dir(root))
	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		Project:       pctx.Project{Name: "@putnami/contributor", Path: filepath.Join("tooling", "contributor")},
		Workspace:     pctx.Workspace{Version: "0.1.0"},
		Params:        pctx.Params{},
	}
	// A dry run packages the real source through the job's dispatch without
	// writing into the repository.
	status, result, err := Content(ctx, jsonl.New(), []string{"--dry-run"})
	if err != nil || status != "OK" {
		t.Fatalf("status = %q, err = %v", status, err)
	}
	if result["name"] != "@putnami/contributor" {
		t.Fatalf("the contributor was not packaged as an extension: %v", result)
	}
	archives, _ := result["archives"].([]string)
	if len(archives) != len(pkgmeta.ArchivePlatformSuffixes()) || archives[0] != "putnami-contributor-linux-x64.tar.gz" {
		t.Fatalf("archives = %v", archives)
	}

	pkg, err := agentartifact.PackageExtension(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if result["archiveSha256"] != pkg.ArchiveSHA256 || result["manifestSha256"] != pkg.Content.ManifestSHA256 {
		t.Fatal("the job and the SDK package step disagree about the contributor's bytes")
	}
	loaded, err := extproto.NegotiateManifest(extproto.ManifestFilename, pkg.Manifest)
	if err != nil {
		t.Fatalf("the packaged contributor manifest does not load: %v", err)
	}
	if loaded.Name != "@putnami/contributor" || loaded.Version != "0.1.0" || loaded.CLIContract != protocolcli.AgentContentContract {
		t.Fatalf("packaged identity = %s@%s at contract %d", loaded.Name, loaded.Version, loaded.CLIContract)
	}
	if loaded.AgentContent == nil || loaded.AgentContent.Path != "content" || loaded.AgentContent.ManifestSHA256 != pkg.Content.ManifestSHA256 {
		t.Fatalf("agentContent = %+v", loaded.AgentContent)
	}
	contentManifest, diags := wsproto.ParseAndValidateAgentArtifactManifest(pkg.Content.Manifest)
	if len(diags) != 0 {
		t.Fatalf("content manifest: %v", diags)
	}
	declared := map[string]bool{}
	for _, file := range contentManifest.Files {
		declared[file.Path] = true
	}
	for _, skill := range []string{"plan", "execute", "fix", "epic", "check", "code-review"} {
		for _, host := range []string{".agents", ".claude"} {
			if !declared[host+"/skills/"+skill+"/SKILL.md"] {
				t.Errorf("the packaged contributor lacks %s/skills/%s/SKILL.md", host, skill)
			}
		}
	}
}
