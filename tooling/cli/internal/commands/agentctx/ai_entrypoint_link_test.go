package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A CLAUDE.md that links to AGENTS.md shows the block AGENTS.md holds: the
// link stays a link, and the block is written once, in AGENTS.md. The same
// holds the other way round.
func TestWriteAgentEntrypointsKeepsALinkBetweenTheEntrypoints(t *testing.T) {
	for name, c := range map[string]struct{ link, file string }{
		"CLAUDE.md links to AGENTS.md": {link: ClaudeEntrypointPath, file: agentEntrypointPath},
		"AGENTS.md links to CLAUDE.md": {link: agentEntrypointPath, file: ClaudeEntrypointPath},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			user := "# Our rules\n\nBe brief.\n"
			writeGuidance(t, dir, c.file, user)
			if err := os.Symlink(c.file, filepath.Join(dir, c.link)); err != nil {
				t.Fatal(err)
			}
			if _, err := writeAgentEntrypoints(dir, false); err != nil {
				t.Fatal(err)
			}
			if target, err := os.Readlink(filepath.Join(dir, c.link)); err != nil || target != c.file {
				t.Fatalf("%s is no longer a link to %s: %q, %v", c.link, c.file, target, err)
			}
			got := readGuidance(t, dir, c.file)
			if strings.Count(got, guidanceBlockBeginPrefix) != 1 || withoutBlock(t, got) != user {
				t.Fatalf("%s:\n%s", c.file, got)
			}
			if report, err := writeAgentEntrypoints(dir, false); err != nil || report.Changed {
				t.Fatalf("a second run changed a file: %+v, %v", report, err)
			}
		})
	}
}
