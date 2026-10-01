package agentartifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// The builder is exercised here only against synthetic projects. The shipped
// artifacts are checked by the packager that builds them (@putnami/scaffold),
// so this module never reads a sibling project.

var fixtureSkills = []string{"acme-change", "acme-check", "acme-plan", "acme-review"}

const fixturePolicy = `"agent-artifact": {
      "forbiddenContent": ["github", "open a pull request", "model:"],
      "requiredSkills": ["acme-plan", "acme-change", "acme-check", "acme-review"]
    }`

// newArtifactProject writes a four-skill project declaring the fixture policy
// under name, so a test can mutate one input without sharing state.
func newArtifactProject(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	for _, skill := range fixtureSkills {
		writeSource(t, root, "src/skills/"+skill+"/SKILL.md",
			"---\nname: "+skill+"\ndescription: The "+skill+" workflow\n---\n\n# "+skill+"\n\nDo the "+skill+" work locally.\n")
	}
	writeProjectConfig(t, root, `{"name":"`+name+`","options":{`+fixturePolicy+`}}`)
	return root
}

// newMaintainerProject declares the maintainer-style policy, which allows the
// host and model metadata the fixture policy forbids.
func newMaintainerProject(t *testing.T) string {
	t.Helper()
	root := newArtifactProject(t, "@acme/maintainer-workflows")
	writeProjectConfig(t, root, `{"name":"@acme/maintainer-workflows","options":{"agent-artifact":{"forbiddenContent":["password","access token","api key"],"requiredSkills":["acme-plan"]}}}`)
	return root
}

func TestBuildIsByteDeterministic(t *testing.T) {
	root := newArtifactProject(t, "@acme/agent-workflows")
	first, err := Build(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Archive, second.Archive) {
		t.Fatal("two builds over the same source produced different archive bytes")
	}
	if !bytes.Equal(first.Manifest, second.Manifest) || first.ArchiveSHA256 != second.ArchiveSHA256 {
		t.Fatal("two builds over the same source produced different digests")
	}
	if first.Name != "@acme/agent-workflows" {
		t.Fatalf("Result.Name = %q, want the project name", first.Name)
	}
}

func TestBuildHasOnlyTheDeclaredMembers(t *testing.T) {
	result, err := Build(newArtifactProject(t, "@acme/agent-workflows"), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	members, err := ReadArchive(result.Archive)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, 1+2*len(fixtureSkills))
	want = append(want, wsproto.AgentArtifactManifestFilename)
	for _, name := range fixtureSkills {
		want = append(want, ".agents/skills/"+name+"/SKILL.md", ".claude/skills/"+name+"/SKILL.md")
	}
	sort.Strings(want)
	got := make([]string, 0, len(members))
	for name := range members {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("archive members =\n%s\nwant =\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	manifest, diags := wsproto.ParseAndValidateAgentArtifactManifest(result.Manifest)
	if len(diags) != 0 {
		t.Fatalf("manifest diagnostics: %v", diags)
	}
	if manifest.Name != "@acme/agent-workflows" || manifest.ProtocolVersion != 1 || len(manifest.Files) != len(fixtureSkills)*2 {
		t.Fatalf("manifest identity/files = %+v", manifest)
	}
}

// TestBuildEmitsCompleteHostBodies pins the property that no emitted host file
// redirects a reader to another path.
func TestBuildEmitsCompleteHostBodies(t *testing.T) {
	root := newArtifactProject(t, "@acme/agent-workflows")
	result, err := Build(root, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range fixtureSkills {
		source, err := os.ReadFile(filepath.Join(root, "src", "skills", name, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(result.Files[".agents/skills/"+name+"/SKILL.md"], source) {
			t.Errorf(".agents copy of %s differs from its source", name)
		}
		claude := string(result.Files[".claude/skills/"+name+"/SKILL.md"])
		if !strings.HasPrefix(claude, "---\nname: "+name+"\ndescription: ") {
			t.Errorf(".claude/skills/%s/SKILL.md front = %q", name, firstLines(claude, 4))
		}
		if !strings.Contains(claude, "allowed-tools: "+defaultClaudeTools) {
			t.Errorf(".claude/skills/%s/SKILL.md lost the default capability line", name)
		}
		// The default lets a skill load and call the Putnami MCP tools its
		// workspace guidance names as the first discovery step.
		if !strings.Contains(claude, "\nallowed-tools: Bash, Read, Grep, Glob, ToolSearch, mcp__putnami__*\n") {
			t.Errorf(".claude/skills/%s/SKILL.md does not grant the Putnami MCP tools: %q", name, firstLines(claude, 5))
		}
		_, body, err := splitSkillDocument(&skillSource{name: name, document: source})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(claude, "\n\n"+string(body)) {
			t.Errorf(".claude/skills/%s/SKILL.md does not end with the canonical body", name)
		}
	}
}

func TestBuildRejectsUndeclaredSource(t *testing.T) {
	root := newArtifactProject(t, "@acme/agent-workflows")
	writeSource(t, root, "src/skills/maintainer-only/notes.md", "maintainer policy\n")
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("Build error = %v, want undeclared source rejection", err)
	}

	root = newArtifactProject(t, "@acme/agent-workflows")
	writeSource(t, root, "src/agents/worker/notes.md", "stray\n")
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("Build error = %v, want undeclared worker source rejection", err)
	}

	root = newArtifactProject(t, "@acme/agent-workflows")
	writeSource(t, root, "src/README.md", "stray\n")
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("Build error = %v, want undeclared top-level source rejection", err)
	}

	root = newArtifactProject(t, "@acme/agent-workflows")
	writeSource(t, root, "src/skills/Bad_Name/SKILL.md", "---\nname: x\n---\n\nbody\n")
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "invalid skill name") {
		t.Fatalf("Build error = %v, want invalid skill name rejection", err)
	}

	root = newArtifactProject(t, "@acme/agent-workflows")
	writeSource(t, root, "src/agents/Bad_Name/AGENT.md", "Body.\n")
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "invalid worker name") {
		t.Fatalf("Build error = %v, want invalid worker name rejection", err)
	}
}

func TestBuildRejectsNonRegularSource(t *testing.T) {
	root := newArtifactProject(t, "@acme/agent-workflows")
	target := filepath.Join(root, "src", "skills", "acme-plan", "SKILL.md")
	link := filepath.Join(root, "src", "skills", "acme-plan", "references", "linked.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Build error = %v, want non-regular source rejection", err)
	}
}

func TestBuildRejectsMissingInputs(t *testing.T) {
	if _, err := Build(newArtifactProject(t, "@acme/agent-workflows"), " "); err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Fatalf("Build error = %v, want version rejection", err)
	}
	if _, err := Build(t.TempDir(), "0.1.0"); err == nil || !strings.Contains(err.Error(), "no readable putnami.json") {
		t.Fatalf("Build error = %v, want missing project config rejection", err)
	}

	root := newArtifactProject(t, "@acme/agent-workflows")
	writeProjectConfig(t, root, `{"options":{`+fixturePolicy+`}}`)
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "declares no name") {
		t.Fatalf("Build error = %v, want missing name rejection", err)
	}

	root = newArtifactProject(t, "@acme/agent-workflows")
	writeProjectConfig(t, root, `{"name":"@acme/x","tasks":{"test":{"timeoutMs":"soon"}}}`)
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "read agent artifact project config") {
		t.Fatalf("Build error = %v, want invalid project config rejection", err)
	}

	root = newArtifactProject(t, "@acme/agent-workflows")
	if err := os.RemoveAll(filepath.Join(root, "src")); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "validate agent artifact source tree") {
		t.Fatalf("Build error = %v, want missing source root rejection", err)
	}

	root = newArtifactProject(t, "@acme/agent-workflows")
	if err := os.RemoveAll(filepath.Join(root, "src", "skills")); err != nil {
		t.Fatal(err)
	}
	writeSource(t, root, "src/agents/lonely/AGENT.md", "Worker body.\n")
	writeSource(t, root, "src/agents/lonely/claude.yaml", "name: lonely\n")
	writeSource(t, root, "src/agents/lonely/codex.toml", "name = \"lonely\"\n")
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "has no skill") {
		t.Fatalf("Build error = %v, want empty skill tree rejection", err)
	}

	root = newArtifactProject(t, "@acme/agent-workflows")
	if err := os.Remove(filepath.Join(root, "src", "skills", "acme-plan", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	writeSource(t, root, "src/skills/acme-plan/references/note.md", "orphan\n")
	if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "is missing src/skills") {
		t.Fatalf("Build error = %v, want missing skill document rejection", err)
	}

	for _, missing := range []string{"AGENT.md", "claude.yaml", "codex.toml"} {
		root = newArtifactProject(t, "@acme/agent-workflows")
		for _, file := range []struct{ name, content string }{
			{"AGENT.md", "Worker body.\n"},
			{"claude.yaml", "name: lonely\n"},
			{"codex.toml", "name = \"lonely\"\n"},
		} {
			if file.name != missing {
				writeSource(t, root, "src/agents/lonely/"+file.name, file.content)
			}
		}
		if _, err := Build(root, "0.1.0"); err == nil || !strings.Contains(err.Error(), "is missing src/agents/lonely/"+missing) {
			t.Fatalf("Build error = %v, want incomplete worker rejection for %s", err, missing)
		}
	}
}

// TestBuildReadsIdentityFromEachProject proves the builder holds no artifact
// identity of its own: two projects over the same source shape produce two
// distinct, individually reproducible artifacts.
func TestBuildReadsIdentityFromEachProject(t *testing.T) {
	first := newArtifactProject(t, "@acme/first-workflows")
	second := newArtifactProject(t, "@acme/second-workflows")

	firstResult, err := Build(first, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := Build(second, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if firstResult.Name != "@acme/first-workflows" || secondResult.Name != "@acme/second-workflows" {
		t.Fatalf("names = %q / %q", firstResult.Name, secondResult.Name)
	}
	if firstResult.ArchiveSHA256 == secondResult.ArchiveSHA256 {
		t.Fatal("two differently named artifacts produced the same archive digest")
	}
	again, err := Build(first, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if again.ArchiveSHA256 != firstResult.ArchiveSHA256 {
		t.Fatal("rebuilding the same project produced a different archive digest")
	}
}

func TestBuildEmitsSkillAssetsAndHostMetadata(t *testing.T) {
	root := newMaintainerProject(t)
	writeSource(t, root, "src/skills/acme-plan/agents/openai.yaml", "name: acme-plan\nallow_implicit_invocation: false\n")
	writeSource(t, root, "src/skills/acme-plan/agents/claude.yaml", "name: acme-plan\ndescription: Plan\nmodel: sonnet\ndisable-model-invocation: true")
	writeSource(t, root, "src/skills/acme-plan/references/deep.md", "reference body\n")
	writeSource(t, root, "src/skills/acme-plan/scripts/run.sh", "#!/bin/sh\necho hi\n")

	result, err := Build(root, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Files[".agents/skills/acme-plan/agents/openai.yaml"]); !strings.Contains(got, "allow_implicit_invocation: false") {
		t.Errorf("openai.yaml was not copied verbatim: %q", got)
	}
	if got := string(result.Files[".agents/skills/acme-plan/references/deep.md"]); got != "reference body\n" {
		t.Errorf("reference asset = %q", got)
	}
	if got := string(result.Files[".agents/skills/acme-plan/scripts/run.sh"]); got != "#!/bin/sh\necho hi\n" {
		t.Errorf("script asset = %q", got)
	}
	claude := string(result.Files[".claude/skills/acme-plan/SKILL.md"])
	// The declared metadata carried no trailing newline; the builder terminates
	// it so the closing fence stays on its own line.
	if !strings.HasPrefix(claude, "---\nname: acme-plan\ndescription: Plan\nmodel: sonnet\ndisable-model-invocation: true\n---\n\n") {
		t.Errorf("declared Claude metadata was not used: %q", firstLines(claude, 8))
	}
	if strings.Contains(claude, "allowed-tools") {
		t.Error("declared Claude metadata must replace the default frontmatter, not merge with it")
	}
}

func TestBuildEmitsWorkersForBothHosts(t *testing.T) {
	root := newMaintainerProject(t)
	writeSource(t, root, "src/agents/deep-worker/AGENT.md", "# Deep worker\n\nDo the bounded work.")
	writeSource(t, root, "src/agents/deep-worker/claude.yaml", "name: deep-worker\ndescription: Deep worker\nmodel: opus\n")
	writeSource(t, root, "src/agents/deep-worker/codex.toml", "name = \"deep-worker\"\nmodel = \"sol\"")

	result, err := Build(root, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	claude := string(result.Files[".claude/agents/deep-worker.md"])
	if claude != "---\nname: deep-worker\ndescription: Deep worker\nmodel: opus\n---\n\n# Deep worker\n\nDo the bounded work.\n" {
		t.Errorf(".claude/agents/deep-worker.md = %q", claude)
	}
	codex := string(result.Files[".codex/agents/deep-worker.toml"])
	if codex != "name = \"deep-worker\"\nmodel = \"sol\"\n\ndeveloper_instructions = '''\n# Deep worker\n\nDo the bounded work.\n'''\n" {
		t.Errorf(".codex/agents/deep-worker.toml = %q", codex)
	}
}

func TestBuildEnforcesInvocationParity(t *testing.T) {
	openAIOnly := newMaintainerProject(t)
	writeSource(t, openAIOnly, "src/skills/acme-plan/agents/openai.yaml", "name: acme-plan\nallow_implicit_invocation: false\n")
	writeSource(t, openAIOnly, "src/skills/acme-plan/agents/claude.yaml", "name: acme-plan\ndescription: Plan\n")
	if _, err := Build(openAIOnly, "1.0.0"); err == nil || !strings.Contains(err.Error(), "explicit-only invocation declared for one host only") {
		t.Fatalf("Build error = %v, want parity rejection when only OpenAI declares it", err)
	}

	claudeOnly := newMaintainerProject(t)
	writeSource(t, claudeOnly, "src/skills/acme-plan/agents/openai.yaml", "name: acme-plan\n")
	writeSource(t, claudeOnly, "src/skills/acme-plan/agents/claude.yaml", "name: acme-plan\ndescription: Plan\ndisable-model-invocation: true\n")
	if _, err := Build(claudeOnly, "1.0.0"); err == nil || !strings.Contains(err.Error(), "explicit-only invocation declared for one host only") {
		t.Fatalf("Build error = %v, want parity rejection when only Claude declares it", err)
	}

	balanced := newMaintainerProject(t)
	writeSource(t, balanced, "src/skills/acme-plan/agents/openai.yaml", "name: acme-plan\nallow_implicit_invocation: false\n")
	writeSource(t, balanced, "src/skills/acme-plan/agents/claude.yaml", "name: acme-plan\ndescription: Plan\ndisable-model-invocation: true\n")
	if _, err := Build(balanced, "1.0.0"); err != nil {
		t.Fatalf("Build error = %v, want the balanced declaration accepted", err)
	}
}

func TestBuildRejectsMalformedHostMetadata(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "claude metadata carries its own fence",
			files: map[string]string{"src/skills/acme-plan/agents/claude.yaml": "---\nname: acme-plan\n---\n"},
			want:  "contains a --- fence",
		},
		{
			name:  "claude metadata names another skill",
			files: map[string]string{"src/skills/acme-plan/agents/claude.yaml": "name: acme-review\ndescription: Plan\n"},
			want:  "must declare name: acme-plan",
		},
		{
			name: "worker already declares developer instructions",
			files: map[string]string{
				"src/agents/deep-worker/AGENT.md":    "Body.\n",
				"src/agents/deep-worker/claude.yaml": "name: deep-worker\ndescription: Deep worker\n",
				"src/agents/deep-worker/codex.toml":  "name = \"deep-worker\"\ndeveloper_instructions = \"inline\"\n",
			},
			want: "already declares developer_instructions",
		},
		{
			name: "worker body breaks the TOML literal",
			files: map[string]string{
				"src/agents/deep-worker/AGENT.md":    "Body with ''' inside.\n",
				"src/agents/deep-worker/claude.yaml": "name: deep-worker\ndescription: Deep worker\n",
				"src/agents/deep-worker/codex.toml":  "name = \"deep-worker\"\n",
			},
			want: "cannot be embedded in TOML",
		},
		{
			name: "worker body carries a frontmatter fence",
			files: map[string]string{
				"src/agents/deep-worker/AGENT.md":    "---\nname: deep-worker\n---\n\nBody.\n",
				"src/agents/deep-worker/claude.yaml": "name: deep-worker\ndescription: Deep worker\n",
				"src/agents/deep-worker/codex.toml":  "name = \"deep-worker\"\n",
			},
			want: "must carry the body only",
		},
		{
			name: "worker Claude metadata names another worker",
			files: map[string]string{
				"src/agents/deep-worker/AGENT.md":    "Body.\n",
				"src/agents/deep-worker/claude.yaml": "name: other\ndescription: Other\n",
				"src/agents/deep-worker/codex.toml":  "name = \"deep-worker\"\n",
			},
			want: "must declare name: deep-worker",
		},
		{
			name:  "skill document declares an extra key",
			files: map[string]string{"src/skills/acme-plan/SKILL.md": "---\nname: acme-plan\ndescription: Plan\nmodel: opus\n---\n\nBody.\n"},
			want:  "accepts only name and description",
		},
		{
			name:  "skill document declares a key twice",
			files: map[string]string{"src/skills/acme-plan/SKILL.md": "---\nname: acme-plan\nname: acme-plan\ndescription: Plan\n---\n\nBody.\n"},
			want:  "declares name twice",
		},
		{
			name:  "skill document names another skill",
			files: map[string]string{"src/skills/acme-plan/SKILL.md": "---\nname: acme-review\ndescription: Plan\n---\n\nBody.\n"},
			want:  "declares name \"acme-review\"",
		},
		{
			name:  "skill document has no description",
			files: map[string]string{"src/skills/acme-plan/SKILL.md": "---\nname: acme-plan\ndescription: \n---\n\nBody.\n"},
			want:  "declares no description",
		},
		{
			name:  "skill document has no fence",
			files: map[string]string{"src/skills/acme-plan/SKILL.md": "# Plan\n\nBody.\n"},
			want:  "must open with a --- frontmatter fence",
		},
		{
			name:  "skill document leaves the fence open",
			files: map[string]string{"src/skills/acme-plan/SKILL.md": "---\nname: acme-plan\ndescription: Plan\n"},
			want:  "frontmatter is not closed",
		},
		{
			name:  "skill document has no body",
			files: map[string]string{"src/skills/acme-plan/SKILL.md": "---\nname: acme-plan\ndescription: Plan\n---\n\n"},
			want:  "has no body",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := newMaintainerProject(t)
			for path, content := range testCase.files {
				writeSource(t, root, path, content)
			}
			if _, err := Build(root, "1.0.0"); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Build error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestDeclaredContentPolicyRejectsViolations(t *testing.T) {
	root := newArtifactProject(t, "@acme/agent-workflows")
	writeSource(t, root, "src/skills/acme-plan/SKILL.md", "---\nname: acme-plan\ndescription: Plan\n---\n\nOpen a pull request when done.\n")
	_, err := Build(root, "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "forbidden content") {
		t.Fatalf("Build error = %v, want forbidden content rejection", err)
	}
	// Both host copies carry the same body, so both are reported.
	if !strings.Contains(err.Error(), ".agents/skills/acme-plan/SKILL.md") || !strings.Contains(err.Error(), ".claude/skills/acme-plan/SKILL.md") {
		t.Errorf("policy failure must name every emitted file: %v", err)
	}

	root = newArtifactProject(t, "@acme/agent-workflows")
	if err := os.RemoveAll(filepath.Join(root, "src", "skills", "acme-review")); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(root, "1.0.0"); err == nil || !strings.Contains(err.Error(), `required skill "acme-review" has no source`) {
		t.Fatalf("Build error = %v, want required skill rejection", err)
	}
}

func TestDeclaredContentPolicyIsMandatoryAndStrict(t *testing.T) {
	for _, testCase := range []struct {
		config string
		want   string
	}{
		{`{"name":"@acme/no-policy"}`, "declares no options.agent-artifact"},
		{`{"name":"@acme/typo","options":{"agent-artifact":{"forbidenContent":["github"]}}}`, `unknown key "forbidenContent"`},
		{`{"name":"@acme/wrong-type","options":{"agent-artifact":{"forbiddenContent":"github"}}}`, "must be an array of strings"},
		{`{"name":"@acme/blank","options":{"agent-artifact":{"requiredSkills":[" "]}}}`, "entry 0 must be a non-empty string"},
		{`{"name":"@acme/number","options":{"agent-artifact":{"requiredSkills":[1]}}}`, "entry 0 must be a non-empty string"},
	} {
		root := newArtifactProject(t, "@acme/agent-workflows")
		writeProjectConfig(t, root, testCase.config)
		if _, err := Build(root, "1.0.0"); err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("Build(%s) error = %v, want %q", testCase.config, err, testCase.want)
		}
	}
}

func TestReadArchiveRejectsUndeclaredMember(t *testing.T) {
	result, err := Build(newArtifactProject(t, "@acme/agent-workflows"), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	members, err := rawArchiveMembers(result.Archive)
	if err != nil {
		t.Fatal(err)
	}
	members[".codex/config.toml"] = []byte("model = \"host-policy\"\n")
	if _, err := ReadArchive(rawArchive(t, members)); err == nil || !strings.Contains(err.Error(), "undeclared archive member") {
		t.Fatalf("ReadArchive error = %v, want undeclared member rejection", err)
	}
}

func TestReadArchiveRejectsMalformedAndIncompleteArtifacts(t *testing.T) {
	if _, err := ReadArchive([]byte("not a gzip archive")); err == nil || !strings.Contains(err.Error(), "open gzip archive") {
		t.Fatalf("ReadArchive error = %v, want invalid gzip rejection", err)
	}
	if _, err := ReadArchive(rawArchive(t, map[string][]byte{"README.md": []byte("missing manifest\n")})); err == nil || !strings.Contains(err.Error(), "has no") {
		t.Fatalf("ReadArchive error = %v, want missing manifest rejection", err)
	}
	if _, err := ReadArchive(rawArchive(t, map[string][]byte{wsproto.AgentArtifactManifestFilename: []byte("{}\n")})); err == nil || !strings.Contains(err.Error(), "invalid agent artifact manifest") {
		t.Fatalf("ReadArchive error = %v, want invalid manifest rejection", err)
	}

	result, err := Build(newArtifactProject(t, "@acme/agent-workflows"), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	members, err := rawArchiveMembers(result.Archive)
	if err != nil {
		t.Fatal(err)
	}
	missingMember := ".agents/skills/acme-change/SKILL.md"
	delete(members, missingMember)
	if _, err := ReadArchive(rawArchive(t, members)); err == nil || !strings.Contains(err.Error(), "manifest declares") {
		t.Fatalf("ReadArchive error = %v, want missing declared member rejection", err)
	}

	members, err = rawArchiveMembers(result.Archive)
	if err != nil {
		t.Fatal(err)
	}
	members[missingMember] = append(members[missingMember], []byte("tampered\n")...)
	if _, err := ReadArchive(rawArchive(t, members)); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("ReadArchive error = %v, want hash mismatch rejection", err)
	}
}

func rawArchiveMembers(content []byte) (map[string][]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	members := make(map[string][]byte)
	for {
		header, err := tarReader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return members, nil
			}
			return nil, err
		}
		var member bytes.Buffer
		if _, err := member.ReadFrom(tarReader); err != nil {
			return nil, err
		}
		members[header.Name] = member.Bytes()
	}
}

func writeProjectConfig(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "putnami.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSource(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func firstLines(content string, count int) string {
	lines := strings.SplitN(content, "\n", count+1)
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}

func rawArchive(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := members[name]
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestBuildReviewHardening(t *testing.T) {
	t.Run("hidden entries are skipped, not shipped or refused", func(t *testing.T) {
		clean := newArtifactProject(t, "@acme/agent-workflows")
		want, err := Build(clean, "1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		dirty := newArtifactProject(t, "@acme/agent-workflows")
		for _, rel := range []string{"src/.DS_Store", "src/skills/acme-plan/.DS_Store", "src/skills/acme-plan/references/.DS_Store", "src/skills/acme-plan/.cache/x", "src/agents/.git/HEAD"} {
			writeSource(t, dirty, rel, "junk\n")
		}
		got, err := Build(dirty, "1.0.0")
		if err != nil {
			t.Fatalf("hidden files must not fail the build: %v", err)
		}
		if got.ArchiveSHA256 != want.ArchiveSHA256 {
			t.Fatal("hidden files changed the archive bytes")
		}
	})

	t.Run("a codex table would swallow the instructions", func(t *testing.T) {
		root := newMaintainerProject(t)
		writeSource(t, root, "src/agents/deep-worker/AGENT.md", "Body.\n")
		writeSource(t, root, "src/agents/deep-worker/claude.yaml", "name: deep-worker\ndescription: Deep worker\n")
		writeSource(t, root, "src/agents/deep-worker/codex.toml", "model = \"sol\"\n[mcp_servers.putnami]\ncommand = \"putnami\"\n")
		if _, err := Build(root, "1.0.0"); err == nil || !strings.Contains(err.Error(), "declares a table") {
			t.Fatalf("Build error = %v, want a table rejection", err)
		}
	})

	t.Run("invocation parity reads values, not text", func(t *testing.T) {
		root := newMaintainerProject(t)
		writeSource(t, root, "src/skills/acme-plan/agents/openai.yaml", "policy:\n  allow_implicit_invocation: False # explicit only\n")
		writeSource(t, root, "src/skills/acme-plan/agents/claude.yaml", "name: acme-plan\ndescription: Plan\n")
		if _, err := Build(root, "1.0.0"); err == nil || !strings.Contains(err.Error(), "one host only") {
			t.Fatalf("Build error = %v, want a parity rejection for a commented, capitalized value", err)
		}
		writeSource(t, root, "src/skills/acme-plan/agents/claude.yaml", "name: acme-plan\ndescription: Plan\ndisable-model-invocation: 'true'\n")
		if _, err := Build(root, "1.0.0"); err != nil {
			t.Fatalf("balanced values in other spellings must build: %v", err)
		}
	})

	t.Run("the policy states both rules", func(t *testing.T) {
		for _, config := range []string{
			`{"name":"@acme/x","options":{"agent-artifact":{}}}`,
			`{"name":"@acme/x","options":{"agent-artifact":{"forbiddenContent":[]}}}`,
			`{"name":"@acme/x","options":{"agent-artifact":{"requiredSkills":["acme-plan"]}}}`,
		} {
			root := newArtifactProject(t, "@acme/x")
			writeProjectConfig(t, root, config)
			if _, err := Build(root, "1.0.0"); err == nil || !strings.Contains(err.Error(), "declares no") {
				t.Fatalf("Build(%s) error = %v, want a missing-rule rejection", config, err)
			}
		}
	})

	t.Run("host metadata must describe and carry a body", func(t *testing.T) {
		root := newMaintainerProject(t)
		writeSource(t, root, "src/skills/acme-plan/agents/claude.yaml", "name: acme-plan\nmodel: opus\n")
		if _, err := Build(root, "1.0.0"); err == nil || !strings.Contains(err.Error(), "must declare a description") {
			t.Fatalf("Build error = %v, want a missing-description rejection", err)
		}
		root = newMaintainerProject(t)
		writeSource(t, root, "src/agents/deep-worker/AGENT.md", "  \n")
		writeSource(t, root, "src/agents/deep-worker/claude.yaml", "name: deep-worker\ndescription: Deep worker\n")
		writeSource(t, root, "src/agents/deep-worker/codex.toml", "model = \"sol\"\n")
		if _, err := Build(root, "1.0.0"); err == nil || !strings.Contains(err.Error(), "is empty") {
			t.Fatalf("Build error = %v, want an empty-body rejection", err)
		}
	})

	t.Run("CRLF is named", func(t *testing.T) {
		root := newArtifactProject(t, "@acme/agent-workflows")
		writeSource(t, root, "src/skills/acme-plan/SKILL.md", "---\r\nname: acme-plan\r\ndescription: Plan\r\n---\r\n\r\nBody.\r\n")
		if _, err := Build(root, "1.0.0"); err == nil || !strings.Contains(err.Error(), "CRLF") {
			t.Fatalf("Build error = %v, want a CRLF rejection", err)
		}
	})
}
