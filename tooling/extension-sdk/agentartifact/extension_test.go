package agentartifact

import (
	"bytes"
	"encoding/json"
	"path"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

const contentExtensionName = "@acme/contributor"

// newContentExtension writes a content-only extension: an authored manifest
// naming its source, a project config declaring the content policy, and one
// skill with a reference and a helper plus one worker for both hosts.
func newContentExtension(t *testing.T, manifest string) string {
	t.Helper()
	root := t.TempDir()
	if manifest == "" {
		manifest = `{"name":"` + contentExtensionName + `","agentContent":{"path":"agent-content","source":"agent-src"}}`
	}
	writeSource(t, root, extproto.ManifestFilename, manifest)
	writeProjectConfig(t, root, `{"name":"`+contentExtensionName+`","options":{"agent-artifact":{"forbiddenContent":["password"],"requiredSkills":["acme-review"]}}}`)
	writeSource(t, root, "agent-src/skills/acme-review/SKILL.md",
		"---\nname: acme-review\ndescription: Review a change\n---\n\n# acme-review\n\nRead references/guide.md, then run scripts/check.sh.\n")
	writeSource(t, root, "agent-src/skills/acme-review/references/guide.md", "Review guide.\n")
	writeSource(t, root, "agent-src/skills/acme-review/scripts/check.sh", "#!/bin/sh\necho checked\n")
	writeSource(t, root, "agent-src/agents/acme-reviewer/AGENT.md", "Review the diff you are given.\n")
	writeSource(t, root, "agent-src/agents/acme-reviewer/claude.yaml", "name: acme-reviewer\ndescription: Reviewer\n")
	writeSource(t, root, "agent-src/agents/acme-reviewer/codex.toml", "name = \"acme-reviewer\"\n")
	return root
}

func TestBuildExtensionContentEmitsEveryHostUnderTheExtensionIdentity(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-packaging", "extension-content-is-built-under-the-extension-identity")
	root := newContentExtension(t, "")
	result, err := BuildExtensionContent(root, "agent-src", contentExtensionName, "1.0.0")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if result.Name != contentExtensionName {
		t.Fatalf("name = %q, want the extension name", result.Name)
	}
	paths := make([]string, 0, len(result.Files))
	for file := range result.Files {
		paths = append(paths, file)
	}
	for _, want := range []string{
		".agents/skills/acme-review/SKILL.md",
		".agents/skills/acme-review/references/guide.md",
		".agents/skills/acme-review/scripts/check.sh",
		".claude/skills/acme-review/SKILL.md",
		".claude/agents/acme-reviewer.md",
		".codex/agents/acme-reviewer.toml",
	} {
		if _, ok := result.Files[want]; !ok {
			t.Errorf("missing %s in %v", want, paths)
		}
	}
	manifest, diags := wsproto.ParseAndValidateAgentArtifactManifest(result.Manifest)
	if len(diags) != 0 {
		t.Fatalf("manifest: %v", diags)
	}
	if manifest.Name != contentExtensionName || manifest.Version != "1.0.0" {
		t.Fatalf("manifest identity = %s@%s", manifest.Name, manifest.Version)
	}

	again, err := BuildExtensionContent(root, "agent-src", contentExtensionName, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Archive, again.Archive) || result.ManifestSHA256 != again.ManifestSHA256 {
		t.Fatal("two builds of one source must be byte-identical")
	}
}

func TestBuildExtensionContentKeepsThePolicyAndTheClosedLayout(t *testing.T) {
	root := newContentExtension(t, "")
	writeSource(t, root, "agent-src/skills/acme-review/references/guide.md", "Never print the password.\n")
	if _, err := BuildExtensionContent(root, "agent-src", contentExtensionName, "1.0.0"); err == nil ||
		!strings.Contains(err.Error(), `forbidden content "password"`) {
		t.Fatalf("error = %v, want the declared content policy to reject the build", err)
	}

	root = newContentExtension(t, "")
	writeSource(t, root, "agent-src/stray.md", "stray\n")
	if _, err := BuildExtensionContent(root, "agent-src", contentExtensionName, "1.0.0"); err == nil ||
		!strings.Contains(err.Error(), "undeclared agent artifact source") {
		t.Fatalf("error = %v, want an undeclared source to fail the build", err)
	}

	root = newContentExtension(t, "")
	writeSource(t, root, "agent-src/agents/acme-reviewer/codex.toml", "[table]\n")
	if _, err := BuildExtensionContent(root, "agent-src", contentExtensionName, "1.0.0"); err == nil ||
		!strings.Contains(err.Error(), "agent-src/agents/acme-reviewer/codex.toml") {
		t.Fatalf("error = %v, want the error to name the authored file under its declared source", err)
	}

	for _, source := range []string{"../escape", "agent-src/", "/abs"} {
		if _, err := BuildExtensionContent(newContentExtension(t, ""), source, contentExtensionName, "1.0.0"); err == nil {
			t.Errorf("source %q must be refused", source)
		}
	}
	if _, err := BuildExtensionContent(t.TempDir(), "agent-src", contentExtensionName, "1.0.0"); err == nil ||
		!strings.Contains(err.Error(), "content policy") {
		t.Fatalf("error = %v, want a missing policy to fail the build", err)
	}
	if _, err := BuildExtensionContent(root, "agent-src", " ", "1.0.0"); err == nil {
		t.Fatal("an unnamed contribution must be refused")
	}
	if _, err := BuildExtensionContent(root, "agent-src", contentExtensionName, " "); err == nil {
		t.Fatal("an unversioned contribution must be refused")
	}
}

func TestPackageExtensionStampsThePackagedFormAndTheAdditiveContract(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-packaging", "a-content-only-extension-packages-with-the-additive-stamp")
	root := newContentExtension(t, "")
	pkg, err := PackageExtension(root, "1.2.0")
	if err != nil {
		t.Fatalf("package: %v", err)
	}

	loaded, err := extproto.NegotiateManifest(extproto.ManifestFilename, pkg.Manifest)
	if err != nil {
		t.Fatalf("the packaged manifest must load: %v", err)
	}
	if loaded.Version != "1.2.0" || loaded.Name != contentExtensionName {
		t.Fatalf("identity = %s@%s", loaded.Name, loaded.Version)
	}
	if loaded.CLIContract != protocolcli.AgentContentContract {
		t.Fatalf("cliContract = %d, want %d", loaded.CLIContract, protocolcli.AgentContentContract)
	}
	want := extproto.AgentContentContribution{Path: "agent-content", ManifestSHA256: pkg.Content.ManifestSHA256}
	if !reflect.DeepEqual(*loaded.AgentContent, want) {
		t.Fatalf("agentContent = %+v, want the packaged form %+v without its source", *loaded.AgentContent, want)
	}

	// The archive carries exactly the manifest, the content manifest and every
	// declared file, under the declared path, and the digest binds them.
	members, err := rawArchiveMembers(pkg.Archive)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(members[extproto.ManifestFilename], pkg.Manifest) {
		t.Fatal("the archive does not carry the stamped manifest")
	}
	contentManifest := members[path.Join("agent-content", wsproto.AgentArtifactManifestFilename)]
	if hash(contentManifest) != pkg.Content.ManifestSHA256 {
		t.Fatal("the archived content manifest does not hash to the stamped digest")
	}
	if len(members) != len(pkg.Content.Files)+2 {
		t.Fatalf("archive has %d members, want the manifest, the content manifest and %d files", len(members), len(pkg.Content.Files))
	}
	for file, content := range pkg.Content.Files {
		if !bytes.Equal(members[path.Join("agent-content", file)], content) {
			t.Errorf("archive member for %s differs from the built file", file)
		}
	}
	if _, ok := members["agent-src/skills/acme-review/SKILL.md"]; ok {
		t.Fatal("the authored source must not ship")
	}

	again, err := PackageExtension(root, "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if again.ArchiveSHA256 != pkg.ArchiveSHA256 {
		t.Fatal("two packages of one source must be byte-identical")
	}
}

func TestPackageExtensionRefusesWhatItCannotCertify(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-packaging", "packaging-refuses-what-it-cannot-certify")
	for name, manifest := range map[string]string{
		"a command surface": `{"name":"` + contentExtensionName + `","agentContent":{"path":"agent-content","source":"agent-src"},` +
			`"commands":{"x":{"run":[{"id":"x","task":"t"}]}},"tasks":{"t":{"kind":"command","command":"echo"}}}`,
		"no source":             `{"name":"` + contentExtensionName + `","agentContent":{"path":"agent-content","manifestSha256":"` + strings.Repeat("a", 64) + `"}}`,
		"an empty contribution": `{"name":"` + contentExtensionName + `","agentContent":{}}`,
		"no contribution":       `{"name":"` + contentExtensionName + `","commands":{}}`,
		"no name":               `{"agentContent":{"path":"agent-content","source":"agent-src"}}`,
		"a future contract":     `{"name":"` + contentExtensionName + `","cliContract":99,"agentContent":{"path":"agent-content","source":"agent-src"}}`,
		"an unknown member":     `{"name":"` + contentExtensionName + `","agentContent":{"path":"agent-content","source":"agent-src","extra":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PackageExtension(newContentExtension(t, manifest), "1.0.0"); err == nil {
				t.Fatalf("a manifest with %s was packaged", name)
			}
		})
	}
	if _, err := PackageExtension(newContentExtension(t, ""), " "); err == nil {
		t.Fatal("an unversioned package must be refused")
	}
	if _, err := PackageExtension(t.TempDir(), "1.0.0"); err == nil {
		t.Fatal("a directory without a manifest must be refused")
	}
}

// A content path that is the manifest's own path, or lies under it, is a valid
// path but has no place in the archive: the package step refuses it by name
// instead of writing a file and a directory at one path.
func TestPackageExtensionRefusesAContentPathTheManifestOccupies(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "agent-content-packaging", "packaging-refuses-what-it-cannot-certify")
	for _, contentPath := range []string{extproto.ManifestFilename, extproto.ManifestFilename + "/content"} {
		t.Run(contentPath, func(t *testing.T) {
			manifest := `{"name":"` + contentExtensionName + `","agentContent":{"path":"` + contentPath + `","source":"agent-src"}}`
			_, err := PackageExtension(newContentExtension(t, manifest), "1.0.0")
			if err == nil || !strings.Contains(err.Error(), `agentContent.path "`+contentPath+`"`) {
				t.Fatalf("a content path at the manifest's own path packaged, or was refused for another reason: %v", err)
			}
		})
	}
}

// A lower authored claim is replaced by the stamp the vocabulary requires: the
// stamp is earned by the package step, never carried over from the author.
func TestPackageExtensionReplacesALowerClaim(t *testing.T) {
	root := newContentExtension(t, `{"name":"`+contentExtensionName+`","cliContract":4,"agentContent":{"path":"agent-content","source":"agent-src"}}`)
	pkg, err := PackageExtension(root, "1.0.0")
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(pkg.Manifest, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["cliContract"] != float64(protocolcli.AgentContentContract) {
		t.Fatalf("cliContract = %v, want %d", raw["cliContract"], protocolcli.AgentContentContract)
	}
}

// The superseded identities are the author's statement about what the content
// replaces, and the published release must carry them: a workspace migrating
// from those artifacts reads them from the installed, packaged manifest.
func TestPackageExtensionKeepsTheSupersededIdentities(t *testing.T) {
	root := newContentExtension(t, `{"name":"`+contentExtensionName+`","agentContent":{"path":"agent-content","source":"agent-src","supersedes":["@acme/workflows","@acme/maintainer-workflows"]}}`)
	pkg, err := PackageExtension(root, "1.0.0")
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	loaded, err := extproto.NegotiateManifest(extproto.ManifestFilename, pkg.Manifest)
	if err != nil {
		t.Fatalf("the packaged manifest must load: %v", err)
	}
	want := extproto.AgentContentContribution{
		Path:           "agent-content",
		ManifestSHA256: pkg.Content.ManifestSHA256,
		Supersedes:     []string{"@acme/workflows", "@acme/maintainer-workflows"},
	}
	if !reflect.DeepEqual(*loaded.AgentContent, want) {
		t.Fatalf("agentContent = %+v, want %+v", *loaded.AgentContent, want)
	}
	// A contribution that names no superseded identity packages without the
	// member, so every existing package keeps its exact bytes.
	plain, err := PackageExtension(newContentExtension(t, ""), "1.0.0")
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	if bytes.Contains(plain.Manifest, []byte("supersedes")) {
		t.Fatalf("a contribution without supersedes gained the member:\n%s", plain.Manifest)
	}
}
