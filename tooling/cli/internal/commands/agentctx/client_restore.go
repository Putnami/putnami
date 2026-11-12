package agentctx

import (
	"strings"

	"go.putnami.dev/tooling/cli/internal/commands/shared"
)

// The client restore hook: a SessionStart handler the core owns (ADR 0087).
//
// A generated client is built, not committed, so a fresh worktree holds none.
// gopls, `go build` and `tsc` run before any Putnami command and need the
// clients. When the workspace declares a generated client, an explicit
// install, init or upgrade writes a SessionStart handler that runs
// client-generate. No extension declares it and no collaboration binding
// gates it. It keeps the first-use install on, so a fresh worktree installs
// and restores its clients before the agent's first edit. The handler is
// removed when the workspace declares no client.

// clientRestoreTimeoutSeconds is how long a host lets the restore hook run. A
// fresh worktree installs its dependencies first, which can take minutes.
const clientRestoreTimeoutSeconds = 600

// clientRestoreEvent is the host event the restore hook runs at, in both
// hosts.
const clientRestoreEvent = "SessionStart"

// clientRestorePath is the command path the restore hook runs. A handler that
// runs it through a known launcher is Putnami's.
var clientRestorePath = []string{shared.ClientGenerateCommand}

// clientRestoreArgs select every project: the hook runs from the root the host
// provides, and the run must not depend on the directory it starts in.
var clientRestoreArgs = []string{"--all"}

// clientRestoreRedirect sends the command's output to standard error. A host
// adds the standard output of a SessionStart hook to the agent's context, and
// install and generation progress are not context.
const clientRestoreRedirect = " >&2"

// renderClientRestoreGroup renders the restore hook for host, started with
// the workspace's launcher (base, from workspaceLauncher). Unlike an
// extension's hook, it keeps the first-use install on: restoring the clients
// of a fresh worktree needs its dependencies first.
func renderClientRestoreGroup(host, base string) hostHookGroupJSON {
	words := append([]string{hostHookLauncher(host, base)}, clientRestorePath...)
	words = append(words, clientRestoreArgs...)
	command := strings.Join(words, " ") + clientRestoreRedirect + hostHookGuard
	return hostHookGroupJSON{
		Hooks: []hostHookHandlerJSON{{
			Type:    "command",
			Command: command,
			Timeout: clientRestoreTimeoutSeconds,
		}},
	}
}
