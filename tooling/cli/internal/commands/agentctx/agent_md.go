package agentctx

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/iox"
)

const (
	ClaudeEntrypointPath = "CLAUDE.md"
	agentEntrypointPath  = "AGENTS.md"
)

// WriteAgentEntrypoints places the Putnami guidance block in the assistant
// entrypoints at the workspace root. Context generation writes these two files
// and nothing else (ADR 0055). The .mcp.json registration is
// a separate step that init, install and upgrade run (RegisterMCPServer, ADR
// 0040); context generation never writes it.
func WriteAgentEntrypoints(wsRoot string) error {
	_, err := writeAgentEntrypoints(wsRoot, false)
	return err
}

type guidanceWriteReport struct {
	Preserved []string
	Changed   bool
}

type managedGuidanceFile struct {
	Path   string
	Target string
	Write  bool
}

// writeAgentEntrypoints is WriteAgentEntrypoints with the force flag that
// reclaims ownership of a block Putnami cannot prove it wrote. It is the
// documented way back for a workspace whose guidance block was edited, or
// written by a release whose exact bytes no longer reconstruct.
func writeAgentEntrypoints(wsRoot string, force bool) (guidanceWriteReport, error) {
	var report guidanceWriteReport

	// One read of the workspace's CI document serves both entrypoints. Putnami
	// owns one delimited block inside each; every other byte belongs to whoever
	// wrote it.
	gate := gateTasksForWorkspace(wsRoot)
	// The workspace's settled decisions travel with the gate: both are derived
	// from committed documents, both are read once, and both are guidance about
	// THIS workspace rather than a hardcoded sentence.
	decisions := decisionsGuidanceForWorkspace(wsRoot)

	files := make([]managedGuidanceFile, 0, 2)
	for _, entrypoint := range []struct {
		path string
		plan func(current string, exists bool, gate, decisions string, force bool) (string, bool, bool)
	}{
		{agentEntrypointPath, planAgentsEntrypoint},
		{ClaudeEntrypointPath, planClaudeEntrypoint},
	} {
		current, exists, err := readOptionalGuidance(filepath.Join(wsRoot, entrypoint.path))
		if err != nil {
			return report, err
		}
		target, write, preserved := entrypoint.plan(current, exists, gate, decisions, force)
		files = append(files, managedGuidanceFile{Path: entrypoint.path, Target: target, Write: write})
		if preserved {
			report.Preserved = append(report.Preserved, entrypoint.path)
		}
	}

	for _, file := range files {
		if !file.Write {
			continue
		}
		if err := shared.AtomicWriteFile(filepath.Join(wsRoot, file.Path), []byte(file.Target)); err != nil {
			return report, err
		}
		report.Changed = true
	}
	for _, path := range report.Preserved {
		iox.Fprintf(os.Stderr, "putnami: warning: preserved the edited Putnami block in %s; run `putnami context generate --force` to rewrite that block (nothing outside it is ever changed)\n", path)
	}
	return report, nil
}

func readOptionalGuidance(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

// The Putnami guidance block. Putnami installs into a workspace that may
// already have its own AGENTS.md and CLAUDE.md, so it owns exactly one
// delimited block inside them rather than the whole file. The begin marker
// carries the SHA-256 of the lines between the markers, so an edit inside the
// block is detectable and preserved; bytes outside the block are never read as
// Putnami's, never rewritten, and survive every run, --force included.
const (
	guidanceBlockBeginPrefix = "<!-- putnami:guidance v2 begin sha256:"
	guidanceBlockEnd         = "<!-- putnami:guidance v2 end -->"
	// claudeImportEntrypoint is the whole CLAUDE.md Putnami writes when none
	// exists: Claude Code imports AGENTS.md, so the block lives in one place.
	claudeImportEntrypoint = "# CLAUDE.md\n\n@AGENTS.md\n"
	agentsEntrypointHead   = "# AGENTS.md\n\n"
)

const guidanceBlockTemplate = "This is a Putnami workspace.\n" +
	"- Build, test, lint, install, add dependencies, and generate with `./putnamiw` when present, otherwise `putnami`. Never call npm, bun, go, pip, or cargo directly for those.\n" +
	"- While iterating, select the projects you changed with `--projects <a>,<b>`; unlike `--impacted`, it skips every project that depends on them. Before declaring work complete, run `putnami {{GATE_TASKS}} --impacted --enforce-coverage` once, when feasible.\n" +
	// The discovery rule is the measured wording: with the former
	// one-line preference, no headless run called a Putnami MCP tool; with this
	// wording, every run made one its first discovery call.
	"- Your first code-discovery step in any task is a Putnami MCP call, not grep, rg, find, or `git show`: `putnami.search` to locate code, `putnami.context` to orient on a project, `putnami.impact` for \"what breaks if I change X\". In Claude Code these are `mcp__putnami__putnami_search`, `mcp__putnami__putnami_context`, and `mcp__putnami__putnami_impact`; load them with ToolSearch `putnami` if they are deferred. Do this even when you already know a keyword, a commit, or a file name. Use grep only for literal text, or after the tool answers stale or unavailable. If the MCP root differs from your worktree, use the local CLI.\n" +
	"- Skills and agents that an extension's agent content installs under `.agents/`, `.claude/` and `.codex/` are managed by Putnami. To customize one, copy it under another name; edited managed files are preserved and reported, never overwritten.\n" +
	"- Read `.agents/constraints.md` when it exists.\n" +
	// The settled decisions of THIS workspace, or nothing at all. A workspace
	// with no decisions.json renders no line here, so its block stays exactly
	// the block it had before the decisions registry existed.
	decisionsPlaceholder

// guidanceBlock is the delimited block for a workspace's gate and decisions.
func guidanceBlock(gate, decisions string) string {
	return wrapGuidanceBlock(guidanceBody(gate, decisions))
}

// guidanceBody fills both workspace-derived placeholders. It is the one place
// the substitutions happen, so the digest in the begin marker and every
// comparison against a committed block are taken over the same bytes.
func guidanceBody(gate, decisions string) string {
	return applyDecisions(applyGate(guidanceBlockTemplate, gate), decisions)
}

// wrapGuidanceBlock delimits body and stamps the begin marker with the digest
// of body, the exact bytes between the two marker lines.
func wrapGuidanceBlock(body string) string {
	sum := sha256.Sum256([]byte(body))
	return guidanceBlockBeginPrefix + hex.EncodeToString(sum[:]) + " -->\n" + body + guidanceBlockEnd + "\n"
}

// findGuidanceBlock locates the first guidance block. [start, end) spans the
// begin line through the end line and its newline. A begin marker with no end
// marker is found but never intact, and end is -1: there is no boundary that
// could be rewritten without guessing where the user's text starts.
func findGuidanceBlock(content string) (start, end int, intact, found bool) {
	start = strings.Index(content, guidanceBlockBeginPrefix)
	for start > 0 && content[start-1] != '\n' {
		next := strings.Index(content[start+1:], guidanceBlockBeginPrefix)
		if next < 0 {
			return 0, 0, false, false
		}
		start += next + 1
	}
	if start < 0 {
		return 0, 0, false, false
	}
	lineEnd := strings.IndexByte(content[start:], '\n')
	if lineEnd < 0 {
		return start, -1, false, true
	}
	beginLine := content[start : start+lineEnd]
	bodyStart := start + lineEnd + 1
	closing := strings.Index(content[bodyStart:], "\n"+guidanceBlockEnd)
	var bodyEnd int
	switch {
	case strings.HasPrefix(content[bodyStart:], guidanceBlockEnd):
		bodyEnd = bodyStart
	case closing >= 0:
		bodyEnd = bodyStart + closing + 1
	default:
		return start, -1, false, true
	}
	end = bodyEnd + len(guidanceBlockEnd)
	if end < len(content) && content[end] == '\n' {
		end++
	}
	digest := strings.TrimSuffix(strings.TrimPrefix(beginLine, guidanceBlockBeginPrefix), " -->")
	sum := sha256.Sum256([]byte(content[bodyStart:bodyEnd]))
	intact = strings.HasSuffix(beginLine, " -->") && digest == hex.EncodeToString(sum[:])
	return start, end, intact, true
}

// upsertGuidanceBlock places block in content and keeps every byte outside it.
// With no block, it goes after a leading "# " heading line, or at the top, with
// one separating newline; deleting the block and that newline restores content
// exactly. An intact block is replaced. An edited block is preserved unless
// force, which rewrites the block and nothing else.
func upsertGuidanceBlock(content, block string, force bool) (out string, changed, preserved bool) {
	start, end, intact, found := findGuidanceBlock(content)
	if found {
		if end < 0 || (!intact && !force) {
			return content, false, true
		}
		out = content[:start] + block + content[end:]
		return out, out != content, false
	}
	if strings.HasPrefix(content, "# ") {
		if newline := strings.IndexByte(content, '\n'); newline >= 0 {
			return content[:newline+1] + "\n" + block + content[newline+1:], true, false
		}
		return content + "\n\n" + block, true, false
	}
	return block + "\n" + content, true, false
}

func planAgentsEntrypoint(current string, exists bool, gate, decisions string, force bool) (string, bool, bool) {
	fresh := generateAgentEntrypoint(gate, decisions)
	if !exists {
		return fresh, current != fresh, false
	}
	return upsertGuidanceBlock(current, guidanceBlock(gate, decisions), force)
}

// planClaudeEntrypoint writes the import form when Putnami owns the file, and
// otherwise places the block inside the user's file. A user CLAUDE.md that
// already imports AGENTS.md already receives the block, so it is left alone
// rather than given a second copy.
func planClaudeEntrypoint(current string, exists bool, gate, decisions string, force bool) (string, bool, bool) {
	fresh := generateClaudeEntrypoint(gate)
	if !exists || current == fresh {
		return fresh, current != fresh, false
	}
	if _, _, _, found := findGuidanceBlock(current); !found && importsAgentsEntrypoint(current) {
		return current, false, false
	}
	return upsertGuidanceBlock(current, guidanceBlock(gate, decisions), force)
}

func importsAgentsEntrypoint(content string) bool {
	for line := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(line) == "@"+agentEntrypointPath {
			return true
		}
	}
	return false
}

// gatePlaceholder is replaced with the workspace's derived gate task list
// wherever the generated guidance names the verification command.
const gatePlaceholder = "{{GATE_TASKS}}"

// applyGate substitutes the derived gate task list into a guidance template.
func applyGate(template string, gate string) string {
	return strings.ReplaceAll(template, gatePlaceholder, gate)
}

// generateClaudeEntrypoint is the CLAUDE.md Putnami writes when it owns the
// whole file: an import of AGENTS.md, where the block lives.
func generateClaudeEntrypoint(string) string {
	return claudeImportEntrypoint
}

// generateAgentEntrypoint is the AGENTS.md Putnami writes when none exists.
func generateAgentEntrypoint(gate, decisions string) string {
	return agentsEntrypointHead + guidanceBlock(gate, decisions)
}
