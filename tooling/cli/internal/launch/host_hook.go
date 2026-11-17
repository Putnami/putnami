package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The host hook relay: `putnami host-hook`, which an agent host's hook runs
// (CLI ADR 0098). The host may start a hook in a directory that is not the
// checkout its session works in: the Claude desktop app runs a worktree
// session's hooks in the main checkout. Only the event's cwd names the right
// checkout, so the relay reads the event, finds the workspace of its cwd, and
// runs the hook's command path there, through that workspace's own launcher.

// HostHookCommand is the word that starts the relay: `putnami host-hook <path> [args]`.
const HostHookCommand = "host-hook"

// HostHookEventMax bounds the event the relay reads: the bound `agent hook` reads
// too, wide because a tool response can hold a whole file.
const HostHookEventMax = 64 << 20

// hostHookSourceWrapper is the wrapper a source workspace runs from its root.
const hostHookSourceWrapper = "putnamiw"

// hostHookChildEnvDrop lists the launcher variables the child never inherits. The loop
// guard and the opt-out would keep it from following its own workspace's pin,
// and the from-source proof names the tree of the relay's binary, not the
// child's.
var hostHookChildEnvDrop = []string{
	LaunchedEnv,
	NoRelaunchEnv,
	FromSourceEnv,
	FromSourceKeyEnv,
}

// HostHookEnv holds what the relay reads from its process. Tests replace it.
type HostHookEnv struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Getwd is the relay's own directory, where an event without a workspace
	// runs.
	Getwd func() (string, error)
	// Executable is the running binary, the launcher of every workspace that is
	// not a source workspace.
	Executable func() (string, error)
	// Environ is the environment the child starts from.
	Environ func() []string
	// HomeDir is the user's home, which holds the record of the repositories
	// the relay serves (RecordHostHookRepository).
	HomeDir func() (string, error)
}

// ProcessHostHookEnv returns the HostHookEnv of the running process.
func ProcessHostHookEnv() HostHookEnv {
	return HostHookEnv{
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Getwd:      os.Getwd,
		Executable: os.Executable,
		Environ:    os.Environ,
		HomeDir:    os.UserHomeDir,
	}
}

// RunHostHook relays one hook event: args is the command path and its arguments. It
// exits with the child's exit code. It prints nothing of its own on standard
// output, which the host reads as the hook's answer: a relay that cannot run
// its child says why on standard error and exits 1, which the hook's guard
// turns into 0.
func RunHostHook(ctx context.Context, args []string, env HostHookEnv) int {
	if len(args) == 0 {
		return hostHookFail(env, errors.New("name the command path to run, such as `putnami host-hook agent hook --engine claude-code`"))
	}
	event, err := io.ReadAll(io.LimitReader(env.Stdin, HostHookEventMax+1))
	if err != nil {
		return hostHookFail(env, fmt.Errorf("read the hook event: %w", err))
	}
	if len(event) > HostHookEventMax {
		return hostHookFail(env, fmt.Errorf("the hook event is larger than %d bytes", HostHookEventMax))
	}
	dir, root, err := hostHookTarget(event, env.Getwd)
	if err != nil {
		return hostHookFail(env, err)
	}
	if root != "" {
		// A user hook runs in every session of the person: the relay starts
		// a repository's code only when the person installed it.
		home, err := env.HomeDir()
		if err != nil {
			return hostHookFail(env, fmt.Errorf("find the home directory: %w", err))
		}
		if !HostHookRepositoryRecorded(home, root) {
			_, _ = fmt.Fprintf(env.Stderr, "putnami host-hook: %s is not in a repository recorded in %s; run any putnami command there to report its sessions\n", root, HostHookTrustPath(home))
			return 0
		}
	}
	launcher, err := hostHookLauncher(root, env.Executable)
	if err != nil {
		return hostHookFail(env, err)
	}
	cmd := exec.CommandContext(ctx, launcher, args...)
	cmd.Dir = dir
	cmd.Env = hostHookChildEnv(env.Environ())
	cmd.Stdin = bytes.NewReader(event)
	cmd.Stdout = env.Stdout
	cmd.Stderr = env.Stderr
	// The child is repository code: the workspace's own putnamiw, or a CLI
	// that loads the workspace's extensions.
	runcredential.MarkRepositoryCodeStarted("host-hook runs a hook in the workspace of its event")
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return exitErr.ExitCode()
	default:
		return hostHookFail(env, fmt.Errorf("run %s: %w", strings.Join(args, " "), err))
	}
}

// hostHookTarget is the directory the child runs in and its workspace root: the
// event's cwd when it is an absolute directory inside a workspace, otherwise
// the relay's own directory, whose root may be empty.
func hostHookTarget(event []byte, getwd func() (string, error)) (dir, root string, err error) {
	var fields struct {
		Cwd string `json:"cwd"`
	}
	if json.Unmarshal(event, &fields) == nil && filepath.IsAbs(fields.Cwd) {
		if info, statErr := os.Stat(fields.Cwd); statErr == nil && info.IsDir() {
			if root, _ := workspace.FindRoot(fields.Cwd); root != "" {
				return fields.Cwd, root, nil
			}
		}
	}
	dir, err = getwd()
	if err != nil {
		return "", "", fmt.Errorf("find the relay's directory: %w", err)
	}
	root, _ = workspace.FindRoot(dir)
	return dir, root, nil
}

// hostHookLauncher is the launcher of the workspace at root: the wrapper of a source
// workspace, which builds its own CLI, and otherwise the running binary, which
// follows the workspace's pin. A source workspace whose lock cannot be read,
// or whose wrapper is missing, runs the binary, which then reports why.
func hostHookLauncher(root string, executable func() (string, error)) (string, error) {
	if root != "" {
		if lf, err := lockfile.ReadLockFile(root); err == nil && lf != nil {
			if entry, ok := lf.GetCLI(); ok && entry.IsWorkspaceSource() {
				wrapper := filepath.Join(root, hostHookSourceWrapper)
				if info, err := os.Stat(wrapper); err == nil && info.Mode().IsRegular() {
					return wrapper, nil
				}
			}
		}
	}
	self, err := executable()
	if err != nil {
		return "", fmt.Errorf("find the running putnami: %w", err)
	}
	return self, nil
}

// hostHookChildEnv is environ without the launcher variables the child must not
// inherit (hostHookChildEnvDrop).
func hostHookChildEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		drop := false
		for _, dropped := range hostHookChildEnvDrop {
			if name == dropped {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}

func hostHookFail(env HostHookEnv, err error) int {
	_, _ = fmt.Fprintf(env.Stderr, "putnami host-hook: %v\n", err)
	return 1
}
