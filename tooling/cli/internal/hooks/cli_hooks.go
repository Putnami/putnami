package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// AbortCleanupBudget bounds the whole set of "after" hooks once the run context
// is already canceled — Ctrl-C or SIGTERM. After-hooks are cleanup, so they are
// detached from the run's cancellation by the engine and re-bounded with this
// budget instead of their own (much longer) per-hook timeout.
//
// It is short on purpose: cmd/putnami's shutdown handler force-exits 10s after
// the first signal, so a cleanup budget that could outlive that window would be
// a promise the process does not keep. Both after-hook calls (commands, then
// CLI) share ONE budget of this size, keeping the worst case inside the window
// with room for the rest of the unwind.
const AbortCleanupBudget = 5 * time.Second

// getwd is os.Getwd behind a package variable so the no-workspace fallback in
// resolveHookRoot can be tested. Its error used to be discarded.
var getwd = os.Getwd

// workspaceHook is one shell command from putnami.workspace.json together with
// the execution contract of the hook group that declared it. Grouping them here
// keeps the contract attached to the command instead of being re-derived at the
// exec site.
type workspaceHook struct {
	// source is the config path that declared this command, e.g.
	// "hooks.cli.before" or "hooks.commands.build.after". It names the hook in
	// timeout errors and reaches the hook as PUTNAMI_HOOK.
	source     string
	command    string
	timeoutMs  int
	loginShell bool
}

// RunCLIHooks executes CLI-level hooks (hooks.cli.before / hooks.cli.after).
//
// workspaceRoot is the directory the commands run in. It is passed in rather
// than read from the process: the CLI finds its workspace by walking up, so
// invoking from a subdirectory is normal, and resolving `./scripts/pre.sh`
// against that subdirectory made the same config mean different things.
func RunCLIHooks(
	ctx context.Context,
	cfg *wsproto.HooksConfig,
	phase string,
	workspaceRoot string,
	verbose bool,
) error {
	if cfg == nil || cfg.CLI == nil {
		return nil
	}
	return runWorkspaceHooks(ctx, collectHooks(cfg.CLI, "hooks.cli", phase), workspaceRoot, verbose)
}

// RunCommandHooks executes command-level hooks (hooks.commands.<name>.before / .after).
// It matches on wildcard "*" and each command name.
func RunCommandHooks(
	ctx context.Context,
	cfg *wsproto.HooksConfig,
	phase string,
	commandNames []string,
	workspaceRoot string,
	verbose bool,
) error {
	if cfg == nil || cfg.Commands == nil {
		return nil
	}

	var collected []workspaceHook

	// Wildcard hooks apply to all commands
	if star, ok := cfg.Commands["*"]; ok {
		collected = append(collected, collectHooks(star, "hooks.commands.*", phase)...)
	}

	// Command-specific hooks
	seen := make(map[string]struct{})
	for _, name := range commandNames {
		name = strings.TrimSpace(name)
		if name == "" || name == "*" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		if hook, ok := cfg.Commands[name]; ok {
			collected = append(collected, collectHooks(hook, "hooks.commands."+name, phase)...)
		}
	}

	return runWorkspaceHooks(ctx, collected, workspaceRoot, verbose)
}

// collectHooks turns one hook group's phase list into commands carrying that
// group's execution contract.
func collectHooks(hook *wsproto.HookPhaseConfig, source, phase string) []workspaceHook {
	commands := phaseCommands(hook, phase)
	if len(commands) == 0 {
		return nil
	}

	timeoutMs := 0
	if hook.TimeoutMs != nil {
		timeoutMs = *hook.TimeoutMs
	}
	loginShell := hook.LoginShell != nil && *hook.LoginShell

	collected := make([]workspaceHook, 0, len(commands))
	for _, command := range commands {
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		collected = append(collected, workspaceHook{
			source:     source + "." + phase,
			command:    command,
			timeoutMs:  timeoutMs,
			loginShell: loginShell,
		})
	}
	return collected
}

func phaseCommands(hook *wsproto.HookPhaseConfig, phase string) []string {
	if hook == nil {
		return nil
	}
	switch phase {
	case "before":
		return hook.Before
	case "after":
		return hook.After
	default:
		return nil
	}
}

func runWorkspaceHooks(ctx context.Context, collected []workspaceHook, workspaceRoot string, verbose bool) error {
	if len(collected) == 0 {
		return nil
	}

	// Resolve the working directory once, before any command runs, so a bad root
	// fails the whole group with one diagnostic instead of N identical ones.
	root, err := resolveHookRoot(workspaceRoot)
	if err != nil {
		return err
	}

	for _, hook := range collected {
		if err := runShellCommand(ctx, hook, root, verbose); err != nil {
			return err
		}
	}
	return nil
}

// resolveHookRoot returns the directory every workspace hook command runs in.
//
// The caller passes the resolved workspace root. Only when there is none — a
// hook declared in the global ~/.putnami/config.json and fired outside any
// workspace — does the process working directory stand in, and then its error
// is RETURNED. The old code assigned `cmd.Dir, _ = os.Getwd()`, and on failure
// cmd.Dir == "" silently means "inherit whatever the process has", which is the
// invocation-relative behavior this replaces.
func resolveHookRoot(workspaceRoot string) (string, error) {
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		cwd, err := getwd()
		if err != nil {
			return "", fmt.Errorf("resolve hook working directory: %w", err)
		}
		root = cwd
	}

	// filepath.Abs consults the process working directory for a relative root,
	// and its error is surfaced here for the same reason.
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve hook working directory %q: %w", root, err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("hook working directory %q is unusable: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("hook working directory %q is not a directory", abs)
	}
	return abs, nil
}

func runShellCommand(ctx context.Context, hook workspaceHook, root string, verbose bool) error {
	if verbose {
		iox.Fprintf(os.Stderr, "  hook: %s\n", hook.command)
	}

	shell, err := exec.LookPath("sh")
	if err != nil {
		return shellNotFound(err)
	}

	// Same rule as an extension hook: unset or non-positive means the default.
	timeoutMs := hook.timeoutMs
	if timeoutMs <= 0 {
		timeoutMs = DefaultHookTimeoutMs
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, shell, shellFlag(hook.loginShell), hook.command)
	tree := configureShellCancellation(cmd)
	cmd.Dir = root
	// Workspace hooks get the CLI's environment plus the two variables that are
	// theirs to know: where the workspace is (the hook may cd away from the root
	// it starts in) and which hook it is (one script can be wired to several
	// phases). They deliberately do NOT get PUTNAMI_PROJECT_ROOT or
	// PUTNAMI_EXTENSION_ROOT — a workspace hook brackets a whole invocation and
	// has neither a project nor an extension to name.
	//
	// A hosted run's hook downloads nothing and holds no framework credential.
	cmd.Env = runcredential.ChildEnv(append(os.Environ(),
		"PUTNAMI_WORKSPACE_ROOT="+root,
		"PUTNAMI_HOOK="+hook.source,
	), false)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// A hosted run hands its credential to no process started after this one.
	runcredential.MarkRepositoryCodeStarted("hook " + hook.source)
	if err := tree.Run(); err != nil {
		switch {
		case parent.Err() != nil:
			// The run itself was stopped (Ctrl-C, or the after-hook cleanup
			// budget). Reporting the per-hook timeout here would name a bound
			// that never fired.
			return fmt.Errorf("hook %s stopped before it finished: %s", hook.source, hook.command)
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fmt.Errorf("hook %s timed out after %dms: %s", hook.source, timeoutMs, hook.command)
		default:
			return fmt.Errorf("hook command failed: %s: %w", hook.command, err)
		}
	}
	return nil
}

// configureShellCancellation starts the shell as the root of its own process
// tree and makes context cancellation kill that whole tree. Start it with the
// returned tree. Killing only `sh` can leave a child such as `sleep`, `curl`,
// or a build tool alive; on Linux cmd.Wait then remains blocked on the orphan
// and the timeout is not a bound at all.
//
// A hard kill matches exec.CommandContext's default cancellation contract. The
// important difference is its target: the tree reaches the shell and every
// process it spawned, so neither a hook timeout nor the after-hook cleanup
// budget can be outlived by a child process.
func configureShellCancellation(cmd *exec.Cmd) *proctree.Tree {
	tree := proctree.New(cmd)
	cmd.Cancel = tree.Kill
	return tree
}

// shellFlag returns the `sh` invocation flags for a hook group.
//
// `-c`, not `-lc`: a login shell sources /etc/profile and ~/.profile before the
// hook, so the same repo and the same putnami.workspace.json behave differently
// per developer dotfile and local diverges from CI. A group that
// genuinely needs a profile-installed PATH — nvm, asdf, mise — opts back in
// with loginShell, which records that reproducibility cost in the config.
func shellFlag(loginShell bool) string {
	if loginShell {
		return "-lc"
	}
	return "-c"
}
