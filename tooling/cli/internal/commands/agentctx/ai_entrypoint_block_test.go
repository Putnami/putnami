package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func readGuidance(t *testing.T, dir, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeGuidance(t *testing.T, dir, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// withoutBlock removes the guidance block and the one separator newline
// upsertGuidanceBlock adds, which must give back the user's original bytes.
func withoutBlock(t *testing.T, content string) string {
	t.Helper()
	start, end, intact, found := findGuidanceBlock(content)
	if !found || !intact || end < 0 {
		t.Fatalf("no intact guidance block in:\n%s", content)
	}
	if start > 0 && content[start-1] == '\n' && start >= 2 && content[start-2] == '\n' {
		return content[:start-1] + content[end:]
	}
	return content[:start] + strings.TrimPrefix(content[end:], "\n")
}

func TestGuidanceBlockRoundTrips(t *testing.T) {
	block := guidanceBlock("lint,test", "")
	start, end, intact, found := findGuidanceBlock(block)
	if !found || !intact || start != 0 || end != len(block) {
		t.Fatalf("find(block) = %d, %d, %v, %v", start, end, intact, found)
	}
	for _, line := range []string{
		"This is a Putnami workspace.",
		"`putnami lint,test --impacted --enforce-coverage` once",
		"While iterating, select the projects you changed with `--projects <a>,<b>`",
		"If the MCP root differs from your worktree, use the local CLI.",
		"Skills and agents that an extension's agent content installs",
		"Read `.agents/constraints.md` when it exists.",
	} {
		if !strings.Contains(block, line) {
			t.Errorf("block missing %q", line)
		}
	}
	body := guidanceBody("lint,test", "")
	if lines := strings.Count(body, "\n"); lines != 6 {
		t.Fatalf("block body has %d lines, want exactly 6", lines)
	}
}

func TestFindGuidanceBlockRejectsEditsAndMalformedMarkers(t *testing.T) {
	block := guidanceBlock("lint", "")
	edited := strings.Replace(block, "This is a Putnami workspace.", "This is my workspace.", 1)
	if _, _, intact, found := findGuidanceBlock(edited); !found || intact {
		t.Fatalf("an edited block must be found and not intact (found=%v intact=%v)", found, intact)
	}
	unclosed := strings.TrimSuffix(block, guidanceBlockEnd+"\n")
	if _, end, intact, found := findGuidanceBlock(unclosed); !found || intact || end != -1 {
		t.Fatalf("an unclosed block = end %d intact %v found %v", end, intact, found)
	}
	if _, _, _, found := findGuidanceBlock("text mentioning " + guidanceBlockBeginPrefix + " inline\n"); found {
		t.Fatal("a marker that does not start a line is not a block")
	}
	if _, _, _, found := findGuidanceBlock("# Nothing here\n"); found {
		t.Fatal("found a block in a file without one")
	}
}

func TestUpsertGuidanceBlockPreservesEveryOtherByte(t *testing.T) {
	block := guidanceBlock("lint", "")
	for name, original := range map[string]string{
		"heading and blank line": "# My project\n\nOur own rules.\n",
		"heading without blank":  "# My project\nOur own rules.\n",
		"heading only":           "# My project",
		"no heading":             "Our own rules.\n",
		"empty file":             "",
	} {
		t.Run(name, func(t *testing.T) {
			out, changed, preserved := upsertGuidanceBlock(original, block, false)
			if !changed || preserved {
				t.Fatalf("changed=%v preserved=%v", changed, preserved)
			}
			if got := withoutBlock(t, out); got != original && !(original == "# My project" && got == "# My project\n") {
				t.Fatalf("bytes outside the block changed:\n--- original ---\n%q\n--- without block ---\n%q", original, got)
			}
			again, changed, _ := upsertGuidanceBlock(out, block, false)
			if changed || again != out {
				t.Fatal("a second upsert with the same block must change nothing")
			}
		})
	}

	// A new block replaces an intact one in place.
	withOld := "# P\n\n" + guidanceBlock("lint", "") + "tail\n"
	out, changed, _ := upsertGuidanceBlock(withOld, guidanceBlock("lint,test", ""), false)
	if !changed || out != "# P\n\n"+guidanceBlock("lint,test", "")+"tail\n" {
		t.Fatalf("intact block not replaced in place:\n%s", out)
	}

	// An edited block is preserved; force rewrites the block only.
	edited := "# P\n\n" + strings.Replace(block, "This is a Putnami workspace.", "Ours.", 1) + "tail\n"
	if out, changed, preserved := upsertGuidanceBlock(edited, block, false); changed || !preserved || out != edited {
		t.Fatal("an edited block must be preserved without force")
	}
	if out, _, preserved := upsertGuidanceBlock(edited, block, true); preserved || out != "# P\n\n"+block+"tail\n" {
		t.Fatalf("force must rewrite the block and keep the rest:\n%s", out)
	}

	// An unclosed block is never rewritten, even with force.
	unclosed := "# P\n\n" + strings.TrimSuffix(block, guidanceBlockEnd+"\n") + "tail\n"
	if out, changed, preserved := upsertGuidanceBlock(unclosed, block, true); changed || !preserved || out != unclosed {
		t.Fatal("an unclosed block must be preserved even with force")
	}
}

// A user's own 40-line AGENTS.md gains the block after its heading and keeps
// every other byte; the next run changes nothing.
func TestWriteAgentEntrypointsInsertsTheBlockIntoAUserAgentsFile(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "generated-context", "an-existing-entrypoint-keeps-every-byte-outside-the-block")
	dir := t.TempDir()
	var user strings.Builder
	user.WriteString("# Team agents guide\n\n")
	for i := 1; i <= 38; i++ {
		user.WriteString("- Team rule number " + strings.Repeat("x", i%7) + "\n")
	}
	original := user.String()
	writeGuidance(t, dir, agentEntrypointPath, original)

	if _, err := writeAgentEntrypoints(dir, false); err != nil {
		t.Fatal(err)
	}
	got := readGuidance(t, dir, agentEntrypointPath)
	if !strings.HasPrefix(got, "# Team agents guide\n\n"+guidanceBlockBeginPrefix) {
		t.Fatalf("block is not directly after the heading:\n%s", firstLines(got, 4))
	}
	if withoutBlock(t, got) != original {
		t.Fatal("bytes outside the block changed")
	}

	report, err := writeAgentEntrypoints(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Changed || readGuidance(t, dir, agentEntrypointPath) != got {
		t.Fatal("a second run changed the file")
	}
}

func TestWriteAgentEntrypointsClaudeEntrypointForms(t *testing.T) {
	// Absent: Putnami writes the import form, and leaves it alone afterwards.
	dir := t.TempDir()
	if _, err := writeAgentEntrypoints(dir, false); err != nil {
		t.Fatal(err)
	}
	if got := readGuidance(t, dir, ClaudeEntrypointPath); got != "# CLAUDE.md\n\n@AGENTS.md\n" {
		t.Fatalf("absent CLAUDE.md became %q", got)
	}
	if got := readGuidance(t, dir, agentEntrypointPath); got != generateAgentEntrypoint(gateTasksForWorkspace(dir), decisionsGuidanceForWorkspace(dir)) {
		t.Fatalf("absent AGENTS.md became %q", got)
	}

	// Present without an import: the block goes inside, no import is added.
	dir = t.TempDir()
	writeGuidance(t, dir, ClaudeEntrypointPath, "# Our Claude rules\n\nBe brief.\n")
	if _, err := writeAgentEntrypoints(dir, false); err != nil {
		t.Fatal(err)
	}
	got := readGuidance(t, dir, ClaudeEntrypointPath)
	if importsAgentsEntrypoint(got) {
		t.Fatal("Putnami must not presume the user wants the import")
	}
	if withoutBlock(t, got) != "# Our Claude rules\n\nBe brief.\n" {
		t.Fatalf("user CLAUDE.md changed outside the block:\n%s", got)
	}

	// Present with an import: already receives the block through AGENTS.md.
	dir = t.TempDir()
	importing := "# Our Claude rules\n\n@AGENTS.md\n\nBe brief.\n"
	writeGuidance(t, dir, ClaudeEntrypointPath, importing)
	if _, err := writeAgentEntrypoints(dir, false); err != nil {
		t.Fatal(err)
	}
	if readGuidance(t, dir, ClaudeEntrypointPath) != importing {
		t.Fatal("a CLAUDE.md that imports AGENTS.md must be left alone")
	}
}

// An edited block is preserved and reported; nothing is written.
func TestWriteAgentEntrypointsPreservesAnEditedBlock(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "generated-context", "an-edited-block-is-preserved")
	dir := t.TempDir()
	if _, err := writeAgentEntrypoints(dir, false); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(readGuidance(t, dir, agentEntrypointPath), "This is a Putnami workspace.", "This is our workspace.", 1)
	writeGuidance(t, dir, agentEntrypointPath, edited)

	report, err := writeAgentEntrypoints(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(report.Preserved, ",") != agentEntrypointPath {
		t.Fatalf("preserved = %v, want only %s", report.Preserved, agentEntrypointPath)
	}
	if readGuidance(t, dir, agentEntrypointPath) != edited {
		t.Fatal("an edited block was overwritten")
	}
}

func firstLines(content string, count int) string {
	lines := strings.SplitN(content, "\n", count+1)
	if len(lines) > count {
		lines = lines[:count]
	}
	return strings.Join(lines, "\n")
}
