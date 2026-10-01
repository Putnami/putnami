package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AgentWorkspaceEnv is the active project directory supplied by an agent host
// when it starts Putnami's MCP server. Claude Code also supplies its stable
// project root directly as CLAUDE_PROJECT_DIR; Codex starts the server in the
// session's working directory and therefore does not need to set either one.
const AgentWorkspaceEnv = "PUTNAMI_AGENT_WORKSPACE"

const claudeProjectDirEnv = "CLAUDE_PROJECT_DIR"

// EnterAgentWorkspace moves an agent-started MCP process into the active
// session directory before workspace discovery and pin relaunch. Moving first
// is load-bearing: an older lock-pinned CLI does not know AgentWorkspaceEnv,
// but it inherits this process's cwd and therefore still opens the right
// worktree after Relaunch replaces the current binary.
//
// The variable is consumed after a successful move so it cannot make a nested
// Putnami process reinterpret its own command. It applies only to the bare MCP
// server invocation installed in host configuration; the interactive
// `mcp install` command continues to use the caller's cwd.
func EnterAgentWorkspace(args []string) error {
	return enterAgentWorkspace(args, os.Getenv, os.Unsetenv, os.Chdir)
}

func enterAgentWorkspace(
	args []string,
	getenv func(string) string,
	unsetenv func(string) error,
	chdir func(string) error,
) error {
	if len(args) != 1 || args[0] != "mcp" {
		return nil
	}
	// Claude's direct stdio-server variable is authoritative. Configuration
	// expansion occurs before that server-only variable exists, so an env entry
	// such as ${CLAUDE_PROJECT_DIR:-.} can legitimately resolve to the launch
	// cwd instead. It remains the fallback used by compatible non-Claude hosts.
	//
	// The configured variable is consumed whenever it is present, including when
	// the direct value won: leaving it set is exactly the leak into nested
	// processes that consuming it exists to prevent.
	source := claudeProjectDirEnv
	raw := strings.TrimSpace(getenv(claudeProjectDirEnv))
	consume := strings.TrimSpace(getenv(AgentWorkspaceEnv)) != ""
	if raw == "" {
		source = AgentWorkspaceEnv
		raw = strings.TrimSpace(getenv(AgentWorkspaceEnv))
	}
	if raw == "" {
		return nil
	}
	if strings.Contains(raw, "${CLAUDE_PROJECT_DIR") {
		return fmt.Errorf("%s was not expanded by the agent host; update or reconnect the host before starting Putnami MCP", source)
	}

	dir, err := filepath.Abs(raw)
	if err != nil {
		return fmt.Errorf("resolve agent workspace %q: %w", raw, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("resolve agent workspace %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("resolve agent workspace %q: not a directory", dir)
	}
	if err := chdir(dir); err != nil {
		return fmt.Errorf("enter agent workspace %q: %w", dir, err)
	}
	if consume {
		if err := unsetenv(AgentWorkspaceEnv); err != nil {
			return fmt.Errorf("consume %s after entering %q: %w", AgentWorkspaceEnv, dir, err)
		}
	}
	return nil
}
