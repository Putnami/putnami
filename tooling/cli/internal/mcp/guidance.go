package mcp

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// ExtensionGuidance is an exact extension AI.md together with the lock identity
// that selected it. Content is kept as a string because MCP transports Markdown
// as UTF-8 text; callers read the file bytes once and preserve them verbatim.
type ExtensionGuidance struct {
	Name    string
	Version string
	Content string
}

// GuidanceIssue describes an exact locked guide that is unavailable. It is
// data for a diagnostic resource, never an instruction to resolve a newer
// release.
type GuidanceIssue struct {
	Name    string
	Version string
	Reason  string
}

// The initialize instructions have this layout, in order:
//
//  1. the routing preamble: the active-worktree guard, automatic tool choice,
//     freshness check and one-turn fallback, self-contained so no later part is
//     needed to follow it;
//  2. the settled root decisions, when Options.SettledDecisionLines gives any: a
//     heading, one pointer line to the putnami://workspace/guidance resource,
//     then one compact line per decision ("- D-001 — statement"), as many as
//     fit maxClientInstructionUnits together with the preamble, and one line
//     that counts the decisions left out;
//  3. each extension's AI.md, up to maxInitializeGuidanceBytes in all.
//
// Which host reads which part, measured on Claude Code 2.1.308 and 2.1.310 and
// Codex 0.162 in October 2026: Claude Code puts the instructions into the
// system prompt at session start, and cuts them at 4,096 characters (see
// maxClientInstructionUnits). It keeps the first part and drops the rest, so
// the decisions come before the extension guides, and the guides sit past the
// cut whenever the decisions fill the budget. Codex 0.162 does not put
// instructions into the model's context at start. It shows them whole, and only
// inside a tool-search result, so a Codex model reads the decisions after its
// first tool search, not at session start. The AGENTS.md block, which every host
// reads from disk, is the one text that reaches a model at start with or
// without MCP.
//
// The instructions never repeat that block. Claude Code reads both, so the
// rules and the gate line stay in the block alone, and the instructions add
// what the block no longer carries: the decisions.
//
// The server builds the instructions when a client sends initialize, and sends
// them once. A decision changed later does not alter the instructions of that
// session. The change notice that ends the next tool result names it and points
// at the putnami://workspace/guidance resource, which lists the current
// decisions on every read.

// maxClientInstructionUnits is the length, in UTF-16 code units, at which
// Claude Code cuts the instructions of a stdio MCP server: 4,096 by default in
// 2.1.308 and 2.1.310, which a user can change with
// CLAUDE_CODE_MAX_MCP_DESCRIPTION_LENGTH. The client keeps what comes first, so
// the routing preamble and the settled decisions are held to it together. A
// decision list that does not fit renders its leading decisions whole and one
// line that counts the rest. A longer limit set by a user is never read.
const maxClientInstructionUnits = 4096

// settledDecisionsHeading and settledDecisionsPointer start the decisions in
// the instructions. The pointer comes first, so the model learns where the
// complete list is even when the lines below it are cut.
const (
	settledDecisionsHeading = "--- settled decisions ---"
	settledDecisionsPointer = "Complete list, with each decision's date and enforcement and the project registries: resource " + workspaceGuidanceURI
)

// maxInitializeGuidanceBytes bounds the extension guidance appended to
// initialize across every prepared extension. Each AI.md is already capped at
// 1 MiB on read, but instructions land in the host's system prompt, so the
// number that governs context cost is the total rather than the per-file
// limit. Today's extension guides are 4-13 KB; this leaves an order of
// magnitude of headroom while keeping one oversized guide from consuming a
// session's whole window. The exact bytes of anything omitted stay available as
// a resource.
//
// The settled decisions do not count against this budget: they sit under
// maxClientInstructionUnits with the preamble instead, and come first.
const maxInitializeGuidanceBytes = 128 * 1024

const initializeRoutingPreambleFormat = "MCP root=%s. Match active worktree/cwd before analysis/resume/subagents; mismatch => local Putnami CLI, not MCP. Structure: one advertised Intelligence tool; no workspace arg. Trust freshness=indexed only + revisions covering checkout. Stale/refused => local fallback this turn; no second tool. APIs: putnami.go_docs/typescript_docs. Exact guidance follows."

func (s *Server) initializeRoutingPreamble() string {
	return fmt.Sprintf(initializeRoutingPreambleFormat, initializeWorkspaceIdentity(s.opts.WorkspaceRoot))
}

func initializeWorkspaceIdentity(root string) string {
	quoted := fmt.Sprintf("%q", root)
	if len(quoted) <= 152 {
		return quoted
	}
	sum := sha256.Sum256([]byte(root))
	return fmt.Sprintf("sha256:%x", sum)
}

// clientInstructionUnits counts the UTF-16 code units of text, the unit in
// which Claude Code measures the instructions.
func clientInstructionUnits(text string) int {
	units := 0
	for _, r := range text {
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// settledDecisionsSection renders the decisions part of the instructions after
// head, the preamble and its optional full-root line, so that head and the
// section together stay within maxClientInstructionUnits. It returns "" when
// the server has no decision to give.
//
// It keeps the leading decisions that fit whole, never cutting a line, then
// one line that counts the others and names the resource. That line counts
// toward the limit. The pointer and the omission line render even when head
// alone fills the limit, because the model must learn where the list is.
func (s *Server) settledDecisionsSection(head string) string {
	if s.opts.SettledDecisionLines == nil {
		return ""
	}
	lines := s.opts.SettledDecisionLines()
	if len(lines) == 0 {
		return ""
	}
	prefix := "\n\n" + settledDecisionsHeading + "\n\n" + settledDecisionsPointer
	used := clientInstructionUnits(head) + clientInstructionUnits(prefix)
	// For each k, length is the instructions with the first k decisions and,
	// when k < len(lines), the omission line for the other len(lines)-k. fits
	// is the largest k whose length stays within the limit.
	fits := 0
	total := used
	for k := 0; k <= len(lines); k++ {
		length := total
		if k < len(lines) {
			length += clientInstructionUnits(omittedDecisionsLine(len(lines) - k))
		}
		if length <= maxClientInstructionUnits {
			fits = k
		}
		if k < len(lines) {
			total += 1 + clientInstructionUnits(lines[k])
		}
	}
	var out strings.Builder
	out.WriteString(prefix)
	for _, line := range lines[:fits] {
		out.WriteString("\n")
		out.WriteString(line)
	}
	if fits < len(lines) {
		out.WriteString(omittedDecisionsLine(len(lines) - fits))
	}
	return out.String()
}

// omittedDecisionsLine counts the decisions the instructions leave out, with
// its leading newline.
func omittedDecisionsLine(omitted int) string {
	return fmt.Sprintf("\n- ... %d more settled decisions are not listed here; read them at %s", omitted, workspaceGuidanceURI)
}

func (s *Server) initializeInstructions() string {
	guidance := append([]ExtensionGuidance(nil), s.currentSelection().ExtensionGuidance...)
	sort.SliceStable(guidance, func(i, j int) bool {
		if guidance[i].Name == guidance[j].Name {
			return guidance[i].Version < guidance[j].Version
		}
		return guidance[i].Name < guidance[j].Name
	})

	var b strings.Builder
	b.WriteString(s.initializeRoutingPreamble())
	if quoted := fmt.Sprintf("%q", s.opts.WorkspaceRoot); quoted != initializeWorkspaceIdentity(s.opts.WorkspaceRoot) {
		b.WriteString("\n\nFull MCP root for the hash above; compare this path: ")
		b.WriteString(quoted)
	}
	b.WriteString(s.settledDecisionsSection(b.String()))
	budget := maxInitializeGuidanceBytes
	for _, guide := range guidance {
		if guide.Name == "" || guide.Version == "" || guide.Content == "" {
			continue
		}
		if len(guide.Content) > budget {
			// Never truncate guidance: half a document is worse than a pointer
			// to the whole one, because the agent cannot tell where it stopped.
			b.WriteString("\n\n--- exact extension guidance omitted: ")
			b.WriteString(guide.Name)
			b.WriteString("@")
			b.WriteString(guide.Version)
			b.WriteString(" ---\n\nRead the complete exact bytes from resource ")
			b.WriteString(extensionGuidanceURI(guide.Name, guide.Version))
			b.WriteString("\n")
			continue
		}
		budget -= len(guide.Content)
		b.WriteString("\n\n--- exact extension guidance: ")
		b.WriteString(guide.Name)
		b.WriteString("@")
		b.WriteString(guide.Version)
		b.WriteString(" ---\n\n")
		b.WriteString(guide.Content)
	}
	return b.String()
}
