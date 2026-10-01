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

// initializeRoutingPreambleFormat is deliberately compact. Codex consumes the
// leading 512 instruction bytes, so the active-worktree guard, automatic tool
// choice, freshness check and one-turn fallback cannot depend on the larger
// extension guide that follows.
// maxInitializeGuidanceBytes bounds the guidance appended to initialize across
// every prepared extension. Each AI.md is already capped at 1 MiB on read, but
// instructions land in the host's system prompt, so the number that governs
// context cost is the total rather than the per-file limit. Today's extension
// guides are 4-13 KB; this leaves an order of magnitude of headroom while
// keeping one oversized guide from consuming a session's whole window. The
// exact bytes of anything omitted stay available as a resource.
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

func (s *Server) initializeInstructions() string {
	guidance := append([]ExtensionGuidance(nil), s.opts.ExtensionGuidance...)
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
