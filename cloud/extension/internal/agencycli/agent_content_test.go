package agencycli

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const agentContentFeature = "cloud/agent-content"

// extensionRoot is the @putnami/cloud extension directory, two levels above
// this package.
var extensionRoot = filepath.Join("..", "..")

// agentContentFiles reads every file of the extension's authored agent
// content, keyed by its slash path under agent-src.
func agentContentFiles(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join(extensionRoot, "agent-src")
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestTheExtensionShipsTheAgencySkillsAndWorkers pins the content a
// workspace listing extension:@putnami/cloud receives: the manifest declares
// agentContent from agent-src, and the skills and workers @putnami/intelligence
// shipped are there at the paths a saved routine prompt names.
func TestTheExtensionShipsTheAgencySkillsAndWorkers(t *testing.T) {
	spectest.Proves(t, agentContentFeature, "ships-the-agency-skills-and-workers", "agent-content-ships-agency-skills")

	data, err := os.ReadFile(filepath.Join(extensionRoot, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		AgentContent *struct {
			Path   string `json:"path"`
			Source string `json:"source"`
		} `json:"agentContent"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.AgentContent == nil || manifest.AgentContent.Source != "agent-src" || manifest.AgentContent.Path == "" {
		t.Fatalf("putnami.extension.json must declare agentContent built from agent-src, got %+v", manifest.AgentContent)
	}

	files := agentContentFiles(t)
	for _, path := range []string{
		"skills/audit/SKILL.md",
		"skills/putnami-routine/SKILL.md",
		"skills/putnami-routine/references/dispatch.md",
		"skills/putnami-routine/references/saved-prompt.md",
		"skills/putnami-routine/references/environment-setup.sh",
		"skills/putnami-routine/scripts/dispatch.sh",
		"skills/rebase/SKILL.md",
		"skills/routine-build/SKILL.md",
		"agents/prune-apply/AGENT.md",
		"agents/prune-triage/AGENT.md",
	} {
		if _, ok := files[path]; !ok {
			t.Errorf("agent-src lacks %s", path)
		}
	}
	for _, skill := range []string{"audit", "putnami-routine", "rebase", "routine-build"} {
		if !strings.Contains(files["skills/"+skill+"/SKILL.md"], "\nname: "+skill+"\n") {
			t.Errorf("skills/%s/SKILL.md does not name itself %s", skill, skill)
		}
	}

	dispatch := files["skills/putnami-routine/scripts/dispatch.sh"]
	for _, line := range []string{"ALLOWLIST=(rebase build)", "build) printf 'routine-build\\n' ;;"} {
		if !strings.Contains(dispatch, line) {
			t.Errorf("dispatch.sh lacks %q", line)
		}
	}
	if !strings.Contains(files["skills/putnami-routine/references/saved-prompt.md"], "`.agents/skills/putnami-routine/references/dispatch.md`") {
		t.Error("saved-prompt.md no longer sends the routine to .agents/skills/putnami-routine/references/dispatch.md")
	}
}

// TestTheAgentContentNamesOnlyAgencyCommands pins the commands the content
// names: the agency commands this extension runs, never the intelligence
// group, and the audit settings under options["@putnami/cloud"].
func TestTheAgentContentNamesOnlyAgencyCommands(t *testing.T) {
	spectest.Proves(t, agentContentFeature, "names-only-agency-commands", "agent-content-names-agency-commands")

	files := agentContentFiles(t)
	for path, text := range files {
		folded := strings.Join(strings.Fields(text), " ")
		for _, forbidden := range []string{"putnami intelligence", "@putnami/intelligence"} {
			if strings.Contains(folded, forbidden) {
				t.Errorf("%s names %q", path, forbidden)
			}
		}
	}

	fold := func(path string) string { return strings.Join(strings.Fields(files[path]), " ") }
	requireAll := func(path string, sentences ...string) {
		t.Helper()
		text := fold(path)
		for _, sentence := range sentences {
			if !strings.Contains(text, sentence) {
				t.Errorf("%s lacks %q", path, sentence)
			}
		}
	}
	requireAll("skills/putnami-routine/SKILL.md",
		"`putnami agency build`",
		"`extension:@putnami/cloud`",
		"putnami agency trigger register",
		"`putnami agency trigger show`")
	requireAll("skills/putnami-routine/references/dispatch.md", "`extension:@putnami/cloud` in `agentArtifacts`")
	requireAll("skills/putnami-routine/references/saved-prompt.md", "needs a newer `@putnami/cloud`, listed in `agentArtifacts`.")
	requireAll("skills/rebase/SKILL.md", "putnami agency trigger register --skill rebase")
	requireAll("skills/routine-build/SKILL.md",
		"ran `putnami agency build --issue <n>`",
		"putnami agency trigger register --skill build")
	requireAll("skills/audit/SKILL.md", "`putnami agency audit-attest`", `options["@putnami/cloud"].audit`)
	requireAll("skills/audit/scripts/config.sh", `.options["@putnami/cloud"].audit`)
}

// TestRebaseRegeneratesGeneratedFiles pins how the rebase skill resolves a
// conflicted generated file: by building its project from the replayed
// sources, never by merging the bytes by hand.
func TestRebaseRegeneratesGeneratedFiles(t *testing.T) {
	spectest.Proves(t, agentContentFeature, "rebase-regenerates-generated-files", "agent-content-rebase-regenerates-generated-files")

	text := strings.Join(strings.Fields(agentContentFiles(t)["skills/rebase/SKILL.md"]), " ")
	for _, want := range []string{
		"Never merge a generated file by hand",
		"Resolve the conflicted source files of `C` first",
		`"$PUTNAMI_CLI" build --projects <project>`,
		"A build that fails only with `generated-output-drift` has rewritten the files it names",
		"Any other failure is `command_failed`",
		"A file the build leaves absent, or that still holds a conflict marker, is `conflict_unresolved`",
		"`git add` the conflicted generated files only",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("skills/rebase/SKILL.md lacks %q", want)
		}
	}

	// The resolved sources are staged before the build and the cleanup, or
	// the cleanup drops them and the fix commits a file regenerated from
	// sources it does not hold.
	order := []string{
		"Resolve the conflicted source files of `C` first and `git add` each one",
		`"$PUTNAMI_CLI" build --projects <project>`,
		"`git add` the conflicted generated files only",
		"`git restore --worktree -- .` and `git clean -fdq`",
	}
	at := -1
	for _, step := range order {
		next := strings.Index(text, step)
		if next <= at {
			t.Errorf("skills/rebase/SKILL.md does not run %q after the steps before it", step)
		}
		at = next
	}
}

// TestRoutineBuildAttachesFromItsBranch pins where routine-build attaches the
// session: on the branch the build pushes, which it checks out first, never on
// a detached HEAD, where the binding would name the detached state only.
func TestRoutineBuildAttachesFromItsBranch(t *testing.T) {
	spectest.Proves(t, agentContentFeature, "routine-build-attaches-from-its-branch", "agent-content-routine-build-attaches-from-its-branch")

	text := agentContentFiles(t)["skills/routine-build/SKILL.md"]
	for _, want := range []string{
		"Report to the issue's mission from the branch the build pushes",
		"an attach on a detached HEAD\n   binds that detached state only",
		"git switch <branch> 2>/dev/null || git switch -c <branch>\n   putnami agent attach --issue <issue.number>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("skills/routine-build/SKILL.md lacks %q", want)
		}
	}
}
