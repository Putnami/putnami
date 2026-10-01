package registrycred

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	registry "go.putnami.dev/protocol/registry"
)

// materializeTimeout bounds the credential call an install makes. It is shorter
// than cloudTokenTimeout because an install must not stall on it: a publish that
// waits 30 s and then works is a good trade, an install that waits 30 s before
// running `bun install` is not.
const materializeTimeout = 10 * time.Second

// SignInHint is the exact instruction shown when the machine has a cloud but no
// session. It is a constant so every caller prints the same words.
const SignInHint = "run putnami cloud login"

// OutcomeKind classifies what EnsureNativeCredential managed to do.
type OutcomeKind int

const (
	// KindSkipped means nothing was attempted: there was no host to ask about.
	KindSkipped OutcomeKind = iota
	// KindMaterialized means the cloud wrote the host's native credential.
	KindMaterialized
	// KindCloudAbsent means there is no cloud extension to ask. A core-only
	// install is a supported configuration, so this is a debug line, not a
	// warning.
	KindCloudAbsent
	// KindNotSignedIn means a cloud is installed but the user has no session.
	KindNotSignedIn
	// KindUnsupported means the cloud rejected the invocation as a usage error:
	// it predates --materialize, or does not accept the host-only form.
	KindUnsupported
	// KindFailed means the call ran and failed for any other reason.
	KindFailed
)

// Outcome is what EnsureNativeCredential did, plus the one line the caller
// should log for it.
//
// Level and Message are empty when there is nothing to say: either nothing was
// attempted, or this process already reported the same host. A caller therefore
// writes `if o.Message != "" { emit.Log(o.Level, o.Message) }` and gets
// at-most-once reporting for free.
type Outcome struct {
	Kind OutcomeKind
	// Level is the log level the message belongs at: "debug" or "warn".
	Level string
	// Message is the human line to log, or "" to log nothing.
	Message string
}

// materialized memoizes one outcome per host for the life of the process, and
// serializes the calls that produce them.
//
// Per-host-per-process is the right granularity twice over. A token is
// short-lived, so caching it across processes would be wrong; and an install
// spawns several extension jobs against the same host, so re-minting inside one
// process would multiply a network round trip and the warning that follows it by
// the number of jobs. Holding the lock across the subprocess also means two
// concurrent jobs in one process queue behind a single mint instead of racing to
// write the same credential file.
var (
	materializedMu sync.Mutex
	materialized   = map[string]Outcome{}
)

// EnsureNativeCredential asks @putnami/cloud to write the native credential for
// host — the `//<host>/:_authToken=` line in the user's .npmrc, the
// `machine <host>` entry in their .netrc — before an installer runs the package
// manager that reads it.
//
// The framework owns no host list and no credential format. It knows one host
// from its own configuration (the @putnami scope registry in the workspace
// .npmrc, the declared Go module origin), passes it as data, and the cloud maps
// it to a registry kind and a file. A host the cloud does not manage, a
// core-only install with no cloud at all, a user who is not signed in, and a
// cloud that predates this shape of the seam are all ordinary states.
//
// It NEVER blocks an install and never fails one. Every path returns an Outcome;
// there is no error return, because there is no caller decision to make. The
// token is never read and never printed: the cloud writes the file, this
// function discards its stdout.
//
// workspaceRoot is the directory the cloud runs in, so it resolves the same
// workspace (and therefore the same account) the install belongs to. ctx bounds
// the call together with the 10 s internal timeout, whichever fires first.
//
// In a job of a hosted run it starts no process and returns KindSkipped
// (hostedJob), in the workspace-fetch that the engine handed an empty job
// credential too.
func EnsureNativeCredential(ctx context.Context, workspaceRoot, host string) Outcome {
	host = strings.ToLower(strings.TrimSpace(host))
	// A job of a hosted run starts no credential child, the workspace-fetch
	// that holds the job credential included (hostedJob): a credential file in
	// the user's home is exactly what a repository process could read.
	if host == "" || hostedJob() {
		return Outcome{Kind: KindSkipped}
	}

	materializedMu.Lock()
	defer materializedMu.Unlock()
	if prior, ok := materialized[host]; ok {
		// Reported once already this process. Keep the classification, drop the
		// line, so a workspace with ten jobs does not print ten identical
		// warnings about one registry.
		return Outcome{Kind: prior.Kind}
	}

	outcome := materializeNativeCredential(ctx, workspaceRoot, host)
	materialized[host] = outcome
	return outcome
}

// materializeNativeCredential runs the seam once and classifies the result.
func materializeNativeCredential(ctx context.Context, workspaceRoot, host string) Outcome {
	ctx, cancel := context.WithTimeout(ctx, materializeTimeout)
	defer cancel()

	executable, advertised := resolveCloudCLIExecutable()
	// The host is a discrete argv element of a fixed command; no shell is
	// involved, and the executable is the CLI-owned absolute path or the
	// package-owned fallback, never caller input.
	cmd := exec.CommandContext(ctx, executable, //nolint:gosec // G702: argv-form exec of a CLI-owned absolute path or fixed fallback
		registry.SeamParentCommand,
		registry.SeamSubcommand,
		"--"+registry.SeamHostFlag, host,
		"--"+registry.SeamMaterializeFlag,
	)
	if dir := strings.TrimSpace(workspaceRoot); dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			cmd.Dir = dir
		}
	}
	// Same reason as resolveTokenWithCLI: a credential child never runs the
	// first-use install. `putnami install` on a fresh checkout has not written
	// the marker yet when it asks for this credential, so without the opt-out
	// the child would start a nested install racing the parent's and time out.
	cmd.Env = append(os.Environ(), "PUTNAMI_NO_AUTO_INSTALL=1")
	if advertised {
		// Same reason as resolveTokenCLIArgs: this child is already the exact
		// CLI its runner selected, so a workspace-pin relaunch would replace its
		// bytes mid-install.
		cmd.Env = append(cmd.Env, "PUTNAMI_NO_RELAUNCH=1")
	}
	// The --materialize shape prints nothing on success, but the bearer shape of
	// the same subcommand prints a token. Discarding stdout makes it impossible
	// for a cloud that ignores the flag to leak one into a build log.
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return Outcome{
			Kind:    KindMaterialized,
			Level:   "debug",
			Message: "refreshed the registry credential for " + host,
		}
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return Outcome{
			Kind:  KindCloudAbsent,
			Level: "debug",
			Message: "no putnami CLI on PATH; leaving the credential for " + host +
				" to whatever this machine already has",
		}
	}
	if ctx.Err() != nil {
		return Outcome{
			Kind:  KindFailed,
			Level: "warn",
			Message: "refreshing the registry credential for " + host +
				" timed out after " + materializeTimeout.String() + "; continuing with the existing credential",
		}
	}
	return classifyMaterializeFailure(err, host, stderr.String())
}

// classifyMaterializeFailure maps a failed seam call to its outcome.
//
// The exit code is the primary signal — the CLI contract in
// go.putnami.dev/protocol/cli gives auth and usage their own codes — and the
// stderr text is the fallback, because a cloud that has not adopted the taxonomy
// yet still says "not signed in" in words. The session wording is read from the
// first stderr line only, so a usage error whose later lines list `cloud login`
// stays a usage error. The core-only markers are read from the whole stderr,
// because the line that identifies a core-only CLI is not its first one.
func classifyMaterializeFailure(err error, host, stderr string) Outcome {
	code := protocolcli.ExitFailure
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	detail := firstLine(stderr)
	lower := strings.ToLower(detail)

	switch {
	case code == protocolcli.ExitAuth || mentionsNoSession(lower):
		return Outcome{
			Kind:    KindNotSignedIn,
			Level:   "warn",
			Message: "not signed in for " + host + "; " + SignInHint,
		}
	case mentionsUnknownCommand(strings.ToLower(stderr)):
		return Outcome{
			Kind:  KindCloudAbsent,
			Level: "debug",
			Message: "this CLI has no cloud extension; leaving the credential for " + host +
				" to whatever this machine already has",
		}
	case code == protocolcli.ExitUsage:
		return Outcome{
			Kind:  KindUnsupported,
			Level: "warn",
			Message: "this putnami cloud cannot write the credential for " + host +
				" yet; continuing with the existing credential" + suffix(detail),
		}
	}
	return Outcome{
		Kind:  KindFailed,
		Level: "warn",
		Message: "could not refresh the registry credential for " + host +
			"; continuing with the existing credential" + suffix(detail),
	}
}

// mentionsNoSession reports whether the cloud's own words say the user has no
// session. It is the fallback for a cloud that exits 1 for everything.
func mentionsNoSession(lower string) bool {
	for _, marker := range []string{"not signed in", "not logged in", "cloud login", "no session"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// mentionsUnknownCommand reports whether the failure is "there is no `cloud`
// command here" rather than "the cloud rejected these flags". Both exit 2, and
// only the second is worth warning about: a core-only install is a supported
// configuration.
//
// A core-only putnami does not say "unknown command". It runs `cloud` as a task
// pipeline, reads `registry-token` as a project selector, and exits 2 with
// "flag --host is not declared by `cloud`" and "no project matched --projects
// "registry-token"" (recorded in testdata/recorded/registry-token/core-only-cli).
// A CLI that has the cloud extension names the subcommand in its flag notice —
// "`cloud registry-token`" — and never reads the subcommand as a project.
func mentionsUnknownCommand(lower string) bool {
	for _, marker := range []string{
		"unknown command", "unknown subcommand", "not a putnami command",
		"is not declared by `" + registry.SeamParentCommand + "`;",
		"no project matched --projects \"" + registry.SeamSubcommand + "\"",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// firstLine returns the first non-empty line of s, bounded so a runaway stderr
// cannot become a log entry of its own size.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 200 {
			return line[:200]
		}
		return line
	}
	return ""
}

// suffix renders a diagnostic as a parenthetical, or nothing when there is none.
func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

// resetMaterializedForTest clears the per-process memo. Tests call it so each
// case starts from an unasked state.
func resetMaterializedForTest() {
	materializedMu.Lock()
	defer materializedMu.Unlock()
	materialized = map[string]Outcome{}
}
