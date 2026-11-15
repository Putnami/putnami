package screens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// sourceText reads a file of the extension's agent sources with every run of
// white space folded to one space, so a sentence matches across line breaks.
func sourceText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "src", filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

func requireText(t *testing.T, path string, sentences ...string) {
	t.Helper()
	text := sourceText(t, path)
	for _, sentence := range sentences {
		if !strings.Contains(text, sentence) {
			t.Errorf("%s lacks %q", path, sentence)
		}
	}
}

// TestTheProveStageCapturesAndReviewsTheScreens pins the build workflow: the
// prove stage serves both sides, captures, has the agent read and fix the
// screens, and merge passes the manifest to change finalize.
func TestTheProveStageCapturesAndReviewsTheScreens(t *testing.T) {
	t.Parallel()
	requireText(t, "skills/build/references/prove.md",
		"Screenshots are the proof of a web change.",
		"Serve the head with `\"$PUTNAMI_CLI\" serve <project> --port <head port>`",
		"Run `\"$PUTNAMI_CLI\" change screenshots --project <dir> --base <base> --base-url <base URL> --head-url <head URL>",
		"Read every capture it lists, before and after, desktop and mobile, light and dark, against the design rules the workspace states",
		"Fix what you find, commit, and capture again.",
		"becomes `--waive-screenshots <reason>` only on their word")
	requireText(t, "skills/build/references/merge.md",
		"A web change: `--screenshots <manifest>` for each web project whose pages it touches")
	spectest.Proves(t, screensFeature, agentReq, "the-prove-stage-captures-and-reviews-the-screens")
}

// TestTheReviewerReceivesTheScreenshots pins the brief check gives the
// independent reviewer and what the reviewer does with it.
func TestTheReviewerReceivesTheScreenshots(t *testing.T) {
	t.Parallel()
	requireText(t, "skills/build/references/check.md",
		"and for a web change the screenshots manifest and the path of each capture it lists.")
	requireText(t, "agents/reviewer/AGENT.md",
		"When the brief lists screenshots, read each capture",
		"is a finding like a code defect")
	requireText(t, "skills/review/SKILL.md", "For a web change, read every screenshot the brief lists")
	spectest.Proves(t, screensFeature, agentReq, "the-reviewer-receives-the-screenshots")
}
