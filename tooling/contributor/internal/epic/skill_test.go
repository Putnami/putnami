package epic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// skillText reads a file of the extension's skill sources, with every run of
// white space folded to one space, so a sentence matches across line breaks.
func skillText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "src", "skills", filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

func requireSentences(t *testing.T, path string, sentences ...string) {
	t.Helper()
	text := skillText(t, path)
	for _, sentence := range sentences {
		if !strings.Contains(text, sentence) {
			t.Errorf("%s lacks %q", path, sentence)
		}
	}
}

// TestAPushOnlyBuildOpensNoProposal pins the build push-only mode a cloud
// child runs: the skill declares the argument, and every stage that would
// publish pushes the branch with Git instead and runs no change finalize.
func TestAPushOnlyBuildOpensNoProposal(t *testing.T) {
	t.Parallel()
	requireSentences(t, "build/SKILL.md",
		"`--push-only` pushes the branch at each checkpoint and opens no proposal",
		"the epic session publishes its proposal")
	requireSentences(t, "build/agents/claude.yaml", "[--local] [--push-only] [--dry-run]")
	requireSentences(t, "build/references/show.md",
		"or push-only, open the proposal as a draft",
		"A push-only run opens no draft and runs no `change finalize`.",
		"It pushes the declarations commit alone with `git push -u origin <branch>`")
	requireSentences(t, "build/references/code.md", "A push-only run pushes with `git push` instead")
	requireSentences(t, "build/references/validate.md", "reads reviews and comments only once the epic session opened its proposal")
	requireSentences(t, "build/references/merge.md",
		"## Push-only A push-only run runs no `change finalize`, ready or draft.",
		"the branch, the pushed head, the base, the title, the body as it stands, the task reference, the projects the gate covered")
	spectest.Proves(t, epicFeature, epicCloudChild, "a-push-only-build-opens-no-proposal")
}

// TestTheEpicSessionProvesAndPublishesACloudChild pins the epic skill's
// cloud recipe: the owner starts the session in push-only mode, and the
// epic session reviews and gates the pushed head in its own worktree before
// it publishes, never on the child's records.
func TestTheEpicSessionProvesAndPublishesACloudChild(t *testing.T) {
	t.Parallel()
	requireSentences(t, "epic/SKILL.md",
		"It opens a proposal only for a cloud child",
		"`cloud`: the owner starts a cloud session on the repository",
		"`/build <task id> --push-only`",
		"do not run `change finalize`",
		"## 6. Publish a cloud child",
		"you never trust them from a distance",
		"`git worktree add -B <branch> ../<epic>-<key> origin/<branch>`",
		"`git reset --hard origin/<branch>`",
		"Its `HEAD` must be the head the child reported",
		"When the child reports its declarations commit pushed, bring the worktree to it and open the draft there",
		"run build's check stage there, without its repair",
		"one fresh independent reviewer, then the final gate as check selects it, with `--fix=false`",
		"publishes nothing: pass it to the child's session",
		"Never fix it in the worktree.",
		"a dossier built from your review and gate (`--verification-file`)",
		"`--gate ci` with a workflow record you write from that gate")
	requireSentences(t, "build/references/code.md", "`git push --force-with-lease` after a rebase")
	requireSentences(t, "build/references/merge.md", "Its final gate is the local `--projects` gate of its last edit, whatever the machine's load")
	requireSentences(t, "build/references/policy.md",
		"`local`, a local workspace the owner opens, or `cloud`, a cloud session whose branch the epic session publishes")
	spectest.Proves(t, epicFeature, epicCloudChild, "the-epic-session-proves-and-publishes-a-cloud-child")
}
