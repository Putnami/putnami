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
// each stage that runs the session's first change finalize writes the
// printed URL in the host's pull-request tag, which needs no tool permission,
// never calls Claude Code's bind tool, calls another host's bind tool when it
// offers one, and binds nothing in a push-only run or for an epic's cloud
// child.
func TestThePublishedProposalBindsToTheHostSession(t *testing.T) {
	t.Parallel()
	required := map[string][]string{
		"references/merge.md": {
			"## Bind the proposal to the session",
			"Bind the run's own proposal once per session: the first time `change finalize` prints `PROPOSAL_URL=<url>` in the session",
			"Write `<pr-created><url></pr-created>` as plain text on a line of its own, outside any code span, in your next message: the tag needs no tool permission, and a host that does not read it shows it as text.",
			"On Claude Code, which reads the tag, do not also call `bind_pr`.",
			"On any other host that offers a tool that binds a pull request to its session, also call that tool.",
			"A host that reads no tag and offers no tool binds nothing, and the run goes on as before.",
			"A bind tool that refuses or fails is named in the end report and never fails the delivery.",
			"A push-only run publishes nothing and binds nothing",
			"an epic session that publishes a cloud child's proposal does not bind it",
			"whether the proposal was announced by tag, bound by tool, or the tool refused or failed to bind it",
		},
		"references/show.md": {
			"Once the command prints the proposal URL, bind it to the host session: write `<pr-created><url></pr-created>` as plain text on a line of its own, outside any code span, in your next message, which needs no tool permission. On Claude Code, do not also call `bind_pr`; on any other host that offers a bind tool, also call it.",
		},
		"references/code.md": {
			"When this is the session's first `change finalize`, such as after a resume, bind the proposal URL it prints to the host session: write `<pr-created><url></pr-created>` as plain text on a line of its own, outside any code span, in your next message; on Claude Code, do not also call `bind_pr`; on any other host that offers a bind tool, also call it",
		},
	}
	forbidden := map[string][]string{
		"references/merge.md": {
			"When the host offers a tool that binds a pull request to its session, such as Claude Code's `bind_pr`, bind the run's own proposal",
		},
		"references/show.md": {
			"bind it to the host session when the host offers a tool that binds a pull request to its session",
		},
	}
	for path, sentences := range forbidden {
		text := buildSkillText(t, path)
		for _, sentence := range sentences {
			if strings.Contains(text, sentence) {
				t.Errorf("%s still makes the bind tool the first way to bind: %q", path, sentence)
			}
		}
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

// TestTheEndReportLeadsWithTheChangePage pins the build skill's end report:
// when change finalize printed a CHANGE_URL, the report opens with that change
// page and what waits there for the owner, then the pull request link, and
// without one it stays as it was.
func TestTheEndReportLeadsWithTheChangePage(t *testing.T) {
	t.Parallel()
	required := map[string][]string{
		"references/merge.md": {
			"When `change finalize` printed `CHANGE_URL=<url>`, the end report leads with that change page and what the owner does there: approve the declarations, accept the gaps of the hosted verdict and decide the review findings.",
			"Never derive what waits there from local records: a ready dossier holds no gap and no open finding while the hosted verdict may.",
			"Where the session can read the change, through the Putnami connector's `get_change`, report its pending declarations, verdict gaps and open review findings.",
			"Until the change's verdict is in, its gaps are still to come: never report that nothing waits on an answer without a verdict.",
			"Otherwise say the page was not read, and add only what the run itself knows, such as a change to declarations, which may wait for approval.",
			"Never write that nothing waits without having read the page.",
			"The pull request link follows it. Without a `CHANGE_URL`, the report is unchanged.",
		},
		"SKILL.md": {
			"A `CHANGE_URL` leads the end report, as merge says.",
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
	if text := buildSkillText(t, "references/merge.md"); strings.Contains(text, "the gaps its dossier keeps open") || strings.Contains(text, "or that nothing waits") {
		t.Error("references/merge.md still derives what waits on the change page from local records")
	}
	spectest.Proves(t, "tooling/contributor-executable", "change-page-link", "the-end-report-leads-with-the-change-page")
}
