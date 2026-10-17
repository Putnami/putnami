package agentctx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// mcpConfigPath is the workspace-relative MCP registration file that Claude
// Code discovers at the project root. Codex (.codex/config.toml), Cursor
// (.cursor/mcp.json) and VS Code (.vscode/mcp.json) do not read it.
const mcpConfigPath = ".mcp.json"

// mcpServerEntry is the canonical registration for the putnami MCP server in
// a workspace whose server starts with command (see mcpServerCommand).
func mcpServerEntry(command string) map[string]any {
	return map[string]any{
		"command": command,
		"args":    []any{"mcp"},
		// The default keeps compatible hosts on their launch cwd. Claude also
		// injects CLAUDE_PROJECT_DIR directly into stdio servers; the launcher
		// prefers that host-owned value and rejects an unexpanded placeholder.
		"env": map[string]string{
			launch.AgentWorkspaceEnv: "${CLAUDE_PROJECT_DIR:-.}",
		},
	}
}

// mcpServerCommand names the command that starts the MCP server in wsRoot.
// It is the bare binary name, which resolves from PATH on each machine, so the
// file is portable across the team and safe to commit. A source workspace (the
// lock records cli.source "workspace") refuses that global binary (ADR 0028),
// so when its root holds a putnamiw file the server starts through
// `./putnamiw`, which builds the CLI from the tree. Only committed content
// decides: the file mode is not read, because Windows reports no executable
// bit and the same commit would then have two canonical entries. The path is
// relative to the host's launch directory, the workspace root, as in
// .codex/config.toml. A lock that cannot be read is an error: it cannot rule a
// source workspace out.
func mcpServerCommand(wsRoot string) (string, error) {
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		return "", &mcpLockError{err: err}
	}
	if lf == nil {
		return "putnami", nil
	}
	if cli, declared := lf.GetCLI(); !declared || !cli.IsWorkspaceSource() {
		return "putnami", nil
	}
	info, err := os.Stat(filepath.Join(wsRoot, "putnamiw"))
	if err != nil || !info.Mode().IsRegular() {
		return "putnami", nil
	}
	return "./putnamiw", nil
}

// mcpLockError is a putnami.lock.json that cannot be read while choosing the
// entry's command. It names the lock, not .mcp.json, as the file to fix.
type mcpLockError struct{ err error }

func (e *mcpLockError) Error() string {
	msg := e.err.Error()
	if !strings.Contains(msg, lockfile.LockFilename) {
		msg = "read " + lockfile.LockFilename + ": " + msg
	}
	return msg + " (" + mcpConfigPath + " left untouched)"
}

func (e *mcpLockError) Unwrap() error { return e.err }

// mcpConfigStatus reports what ensureMCPConfig did to .mcp.json.
type mcpConfigStatus int

const (
	mcpConfigUnchanged mcpConfigStatus = iota // entry already present; file untouched
	mcpConfigCreated                          // file did not exist; created with the entry
	mcpConfigAdded                            // entry added to an existing file
	mcpConfigUpdated                          // existing entry diverged and was rewritten
	mcpConfigKept                             // existing entry diverged and was kept as written
)

// mcpEntryPolicy says what ensureMCPConfig does with a putnami entry that
// differs from the canonical one. It is the whole difference between the two
// callers (ADR 0040).
type mcpEntryPolicy int

const (
	// mcpRepairDivergedEntry rewrites a diverged entry. `putnami mcp install`
	// uses it: the user asked for this exact write, so it is what repairs a
	// hand-edited or stale registration.
	mcpRepairDivergedEntry mcpEntryPolicy = iota
	// mcpKeepDivergedEntry leaves a diverged entry byte for byte. The implicit
	// registration of init, install and upgrade uses it: an entry that differs
	// was written on purpose, by a person or by another tool, and nobody asked
	// this run to change it.
	mcpKeepDivergedEntry
)

// ensureMCPConfig makes sure .mcp.json at the workspace root registers the
// putnami MCP server. The file is shared with user-managed content (other MCP
// servers), so it is merged, never clobbered: unknown keys and other servers
// are preserved. A file that cannot be parsed is left untouched and reported,
// since rewriting it could destroy the user's other servers. The canonical
// entry is the one this workspace starts its server with (mcpServerCommand).
// policy decides the one remaining case, a putnami entry that differs from it.
func ensureMCPConfig(wsRoot string, policy mcpEntryPolicy) (mcpConfigStatus, error) {
	path := filepath.Join(wsRoot, mcpConfigPath)
	command, err := mcpServerCommand(wsRoot)
	if err != nil {
		return mcpConfigUnchanged, err
	}
	entry := mcpServerEntry(command)

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		root := map[string]any{
			"mcpServers": map[string]any{"putnami": entry},
		}
		return mcpConfigCreated, writeMCPConfig(path, root)
	}
	if err != nil {
		return mcpConfigUnchanged, err
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return mcpConfigUnchanged, fmt.Errorf("parse %s: %w (file left untouched)", mcpConfigPath, err)
	}
	if root == nil {
		root = map[string]any{}
	}

	servers, ok := root["mcpServers"].(map[string]any)
	if !ok {
		if _, exists := root["mcpServers"]; exists {
			return mcpConfigUnchanged, fmt.Errorf("parse %s: %q is not an object (file left untouched)", mcpConfigPath, "mcpServers")
		}
		servers = map[string]any{}
		root["mcpServers"] = servers
	}

	existing, exists := servers["putnami"]
	if exists {
		encoded, err := canonicalJSON(existing)
		if err != nil {
			return mcpConfigUnchanged, fmt.Errorf("read the existing %s putnami entry: %w (file left untouched)", mcpConfigPath, err)
		}
		if encoded == canonicalMCPServerEntry(command) {
			return mcpConfigUnchanged, nil
		}
		if policy == mcpKeepDivergedEntry {
			return mcpConfigKept, nil
		}
	}

	servers["putnami"] = entry
	if exists {
		return mcpConfigUpdated, writeMCPConfig(path, root)
	}
	return mcpConfigAdded, writeMCPConfig(path, root)
}

// canonicalJSON renders a value the one way both sides of every entry
// comparison agree on. A parsed entry and a constructed one hold different Go
// types for the same document; json.Marshal erases that difference and sorts
// object keys, so the comparison is by value and order-independent.
func canonicalJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func canonicalMCPServerEntry(command string) string {
	encoded, err := canonicalJSON(mcpServerEntry(command))
	if err != nil {
		// mcpServerEntry is a literal of JSON-representable types.
		panic("agentctx: canonical MCP entry is not encodable: " + err.Error())
	}
	return encoded
}

func writeMCPConfig(path string, root map[string]any) error {
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}

// MCPInstall is the explicit registration, behind `putnami mcp install`. It is
// the one path that rewrites a hand-edited or diverged putnami entry; the
// implicit registration of init, install and upgrade (RegisterMCPServer) only
// adds a missing one.
func MCPInstall(wsRoot string) error {
	status, err := ensureMCPConfig(wsRoot, mcpRepairDivergedEntry)
	if err != nil {
		return err
	}
	switch status {
	case mcpConfigCreated:
		iox.Fprintf(os.Stdout, "  ✓ %s created — putnami MCP server registered for Claude Code\n", mcpConfigPath)
	case mcpConfigAdded:
		iox.Fprintf(os.Stdout, "  ✓ putnami MCP server added to %s\n", mcpConfigPath)
	case mcpConfigUpdated:
		iox.Fprintf(os.Stdout, "  ✓ putnami MCP server entry rewritten in %s\n", mcpConfigPath)
	default:
		iox.Fprintf(os.Stdout, "  ✓ putnami MCP server already registered in %s\n", mcpConfigPath)
	}
	return nil
}

// RegisterMCPServer is the implicit registration `putnami init`, `putnami
// install` and `putnami upgrade` run (ADR 0040): a new agent session in the
// workspace then finds the putnami MCP server with no manual step. It adds a
// missing putnami entry through the same merge as `putnami mcp install`, and
// differs from it in the two cases where the user did not ask for this write:
//
//   - a putnami entry that differs from the canonical one is kept as written;
//   - a file that cannot be read or parsed is left untouched and reported on
//     warn, naming the file to fix (.mcp.json, or putnami.lock.json when the
//     lock is what failed), and the calling command carries on.
//
// It reports whether .mcp.json was created or gained the entry. A nil warn
// discards the warning.
func RegisterMCPServer(wsRoot string, warn io.Writer) bool {
	status, err := ensureMCPConfig(wsRoot, mcpKeepDivergedEntry)
	if err != nil {
		if warn != nil {
			file := mcpConfigPath
			var lockErr *mcpLockError
			if errors.As(err, &lockErr) {
				file = lockfile.LockFilename
			}
			iox.Fprintf(warn, "putnami: warning: the putnami MCP server is not registered: %v. Fix %s, then run `putnami mcp install`.\n", err, file)
		}
		return false
	}
	return status == mcpConfigCreated || status == mcpConfigAdded
}
