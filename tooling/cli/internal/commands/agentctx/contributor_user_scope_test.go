package agentctx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// contributorUserScope are the skills @putnami/contributor installs in the
// user scope: those that work, or say in one line that they cannot work, in a
// repository without a workspace.
var contributorUserScope = []string{"build", "check", "context", "design", "spec"}

// flat collapses the whitespace of a Markdown document, so a phrase matches
// across its line breaks.
func flat(document string) string {
	return strings.Join(strings.Fields(document), " ")
}

func TestContributorNamesTheWorkspaceFreeSkills(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "user-scope-skills", "the-user-scope-list-names-the-workspace-free-skills")
	m, err := extproto.LoadManifest(filepath.Join(contributorRoot(t), extproto.ManifestFilename))
	if err != nil {
		t.Fatalf("load the contributor manifest: %v", err)
	}
	if !reflect.DeepEqual(m.AgentContent.UserScope, contributorUserScope) {
		t.Fatalf("agentContent.userScope = %v, want %v", m.AgentContent.UserScope, contributorUserScope)
	}
	result := buildContributorContent(t)
	for _, skill := range contributorUserScope {
		for _, host := range []string{".agents/skills/", ".claude/skills/"} {
			if _, ok := result.Files[host+skill+"/SKILL.md"]; !ok {
				t.Errorf("user-scope skill %s does not render as %s%s/SKILL.md", skill, host, skill)
			}
		}
	}
}

func TestContributorUserScopeSkillsDeferToTheWorkspaceCopy(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "user-scope-skills", "each-user-scope-skill-defers-to-the-workspace-copy")
	result := buildContributorContent(t)
	for _, skill := range contributorUserScope {
		for _, host := range []string{".agents/skills/", ".claude/skills/"} {
			document := string(result.Files[host+skill+"/SKILL.md"])
			section := flat(markdownSection(t, document, "Workspace first"))
			for _, want := range []string{
				"look for `putnami.workspace.json` in the current directory and each directory above it",
				"Read `<root>/.claude/skills/" + skill + "/SKILL.md` (Codex: `<root>/.agents/skills/" + skill + "/SKILL.md`) and follow it instead of this file.",
				"`This workspace does not install the " + skill + " skill:",
				"and stop.",
			} {
				if !strings.Contains(section, want) {
					t.Errorf("%s%s: the workspace-first section lacks %q", host, skill, want)
				}
			}
			// The check comes before any instruction that runs a command.
			if first := strings.Index(document, "\n## Workspace first\n"); first < 0 || first != strings.Index(document, "\n## ") {
				t.Errorf("%s%s: workspace first is not the first section", host, skill)
			}
		}
	}
}

func TestContributorBuildCheckAndDesignStopWithoutAWorkspace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "user-scope-skills", "build-check-and-design-stop-without-a-workspace")
	result := buildContributorContent(t)
	for _, skill := range []string{"build", "check", "design"} {
		section := flat(markdownSection(t, string(result.Files[".agents/skills/"+skill+"/SKILL.md"]), "Workspace first"))
		want := "Not found: say `" + skill + " needs a Putnami workspace (putnami.workspace.json), and this repository has none.` and stop. Do not create the file, and do not ask the person to create it."
		if !strings.Contains(section, want) {
			t.Errorf("%s does not stop in one line without a workspace:\n%s", skill, section)
		}
	}
	// No user-scope skill asks for a workspace file.
	for _, skill := range contributorUserScope {
		document := string(result.Files[".agents/skills/"+skill+"/SKILL.md"])
		for _, refused := range []string{"run `putnami init`", "putnami init "} {
			if strings.Contains(flat(document), refused) {
				t.Errorf("%s asks the person to make a workspace: %q", skill, refused)
			}
		}
	}
}

func TestContributorContextSkillReadsObserveWithoutAWorkspace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "context-without-a-workspace", "the-context-skill-reads-observe-without-a-workspace")
	document := string(buildContributorContent(t).Files[".agents/skills/context/SKILL.md"])
	inWorkspace := flat(markdownSection(t, document, "In a workspace"))
	for _, want := range []string{"context map", "context pack <project>", "projects owner <path>", "test --impacted --dry-run"} {
		if !strings.Contains(inWorkspace, want) {
			t.Errorf("the in-workspace section lacks %q", want)
		}
	}
	without := flat(markdownSection(t, document, "Without a workspace"))
	for _, want := range []string{
		"putnami extensions install --user @putnami/observe",
		"putnami observe --base origin/main --no-fetch --output=json",
		"`analysis.components`", "`dependsOn`", "`impact`", "`changed`", "`dependent`",
		"`analysis.tests` and `analysis.coverage`", "`analysis.changedLineCoverage`",
		"`specs`", "`diagnostics`", "git status --short",
	} {
		if !strings.Contains(without, want) {
			t.Errorf("the without-a-workspace section lacks %q", want)
		}
	}
}

func TestContributorSpecSkillWritesTheRepositoryFormatWithoutAWorkspace(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/contributor-workflows", "spec-without-a-workspace", "the-spec-skill-writes-the-repository-format-without-a-workspace")
	document := string(buildContributorContent(t).Files[".agents/skills/spec/SKILL.md"])
	section := flat(markdownSection(t, document, "Without a workspace"))
	for _, want := range []string{
		"putnami observe --base <default branch> --no-fetch --output=json",
		"`speckit`, `openspec`, `kiro` or `gherkin`",
		"`specs/<NNN>-<name>/spec.md` for Spec Kit",
		"`openspec/specs/<capability>/spec.md` for OpenSpec",
		"`.kiro/specs/<name>/requirements.md` for Kiro",
		"a `.feature` file beside the others for Gherkin",
		"never pick the native JSON format",
		"Run `putnami observe` again and check that `specs` lists your requirements",
		"never ask the person to create `putnami.workspace.json`",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the spec skill's without-a-workspace section lacks %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(contributorRoot(t), "src", "skills", "spec", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}
