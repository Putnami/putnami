package finalize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// buildSkillText reads a file of the build skill's sources, with every run of
// white space folded to one space, so a sentence matches across line breaks.
func buildSkillText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "src", "skills", "build", filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

// TestThePublishedProposalBindsToTheHostSession pins the build skill step
// that shows a proposal change finalize publishes in the host's session: the
// host never sees a pull request a provider opens through its own API, so
// each stage that runs the session's first change finalize binds the printed
// URL when the host offers a bind tool, skips the step otherwise, and binds
// nothing in a push-only run or for an epic's cloud child.
func TestThePublishedProposalBindsToTheHostSession(t *testing.T) {
	t.Parallel()
	required := map[string][]string{
		"references/merge.md": {
			"## Bind the proposal to the session",
			"When the host offers a tool that binds a pull request to its session, such as Claude Code's `bind_pr`, bind the run's own proposal once per session: the first time `change finalize` prints `PROPOSAL_URL=<url>` in the session",
			"A host without such a tool skips this step, and the run goes on as before.",
			"A failed bind is named in the end report and never fails the delivery.",
			"A push-only run publishes nothing and binds nothing",
			"an epic session that publishes a cloud child's proposal does not bind it",
			"whether the proposal was bound to the host session, skipped for want of a bind tool, or failed to bind",
		},
		"references/show.md": {
			"Once the command prints the proposal URL, bind it to the host session when the host offers a tool that binds a pull request to its session, such as Claude Code's `bind_pr`; a host without one skips this step.",
		},
		"references/code.md": {
			"When this is the session's first `change finalize`, such as after a resume, bind the proposal URL it prints to the host session when the host offers a tool to do so; a host without one skips this step",
		},
	}
	for path, sentences := range required {
		text := buildSkillText(t, path)
		for _, sentence := range sentences {
			if !strings.Contains(text, sentence) {
				t.Errorf("%s lacks %q", path, sentence)
			}
		}
	}
	spectest.Proves(t, "tooling/contributor-executable", "session-bound-proposal", "the-published-proposal-binds-to-the-host-session")
}
