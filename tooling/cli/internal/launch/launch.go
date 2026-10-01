// Package launch makes the global `putnami` command resolve to the
// workspace-pinned CLI: when putnami.lock.json pins a CLI version and the
// running binary is not it, the launcher resolves that version from the
// machine-global artifact store (downloading once if needed) and re-execs into
// it, so the CLI, putnamiw, and the MCP server all run one engine.
//
// A PIN IS FAIL-CLOSED. A workspace that pins a CLI version has said "run
// exactly this engine"; quietly running a different one is precisely the
// stale-binary drift the pin exists to prevent, and it is invisible — the wrong
// engine produces wrong results, not an error. So every failure to BECOME the
// pinned binary is a hard error naming the command that gets you out: an
// unreadable or too-new lock, no digest for this platform, a failed download,
// a failed verification, a store that would not take the bytes, a failed exec.
//
// It stays fail-OPEN only where there is no pin to betray: no workspace, no lock
// file, a lock that declares no CLI, or a running binary that already IS the pin.
//
// A SOURCE WORKSPACE FAILS CLOSED FOR THE OPPOSITE REASON. When
// the lock's CLI entry is the `source: "workspace"` sentinel
// (lockfile.SourceWorkspace), the workspace has said "the engine is the code in
// this tree". There is no published binary to resolve, so there is nothing to
// exec into — the only correct answers are "you already are that binary" and
// "refuse". Falling through to the running binary would be the same dishonest
// green the pin prevents, one level down: a cold runner's baked CLI or a stale
// global `putnami` would gate a commit whose own CLI source was never compiled.
// The way in is `./putnamiw`, which builds from the tree and names it in
// FromSourceEnv; the way out is `putnami pin <version>`, still launcher-exempt.
//
// One case fails closed for a different reason than the pin: a Go test binary
// that reaches the exec is refused, because replacing it would end the suite
// mid-run and still report success (see isTestBinary).
//
// The recovery story sits OUTSIDE that decision, so it survives a pinned engine
// that is broken, missing, or older than the command you need:
//
//   - IsExemptInvocation (`pin`, `version list/use`, `upgrade --from-source`)
//     never enters the launcher at all;
//   - NoRelaunchEnv covers PUBLISHED pins only: it suppresses the launcher for
//     any pinned version and survives an unreadable lock, but it is NOT an
//     escape from a source workspace — that shape has no pinned binary to be
//     broken, so its ways through are `./putnamiw` and IsExemptInvocation;
//   - LaunchedEnv short-circuits the child of a successful relaunch.
//
// Every hard error below points at one of those three.
package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/clibin"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
)

// LaunchedEnv is set on the re-exec'd child to the pinned binary's SHA. Its
// presence is a hard loop guard for that child and lets the child annotate
// `--version` as launched. App.Run consumes it immediately after the relaunch
// decision so hooks/jobs do not inherit the guard and accidentally suppress
// their own workspace-pin resolution.
const LaunchedEnv = "PUTNAMI_LAUNCHED"

// NoRelaunchEnv, when set to a truthy value (1/true/yes/on), disables the
// launcher for this process. putnamiw sets it so a from-source `./putnamiw`
// build runs as-is instead of being relaunched into a pinned release; users and
// CI can set it to force the locally-selected binary. Distinct from LaunchedEnv:
// it does not annotate --version, since the process was not launched, just left
// alone.
//
// As an exported environment variable it is inherited by child processes, so it
// suppresses the launcher for the whole invocation subtree. That is intended: an
// opt-out applies to the command you ran and everything it spawns.
//
// It does NOT license running a foreign engine in a source workspace: see the
// package doc and relaunch.
const NoRelaunchEnv = "PUTNAMI_NO_RELAUNCH"

// FromSourceEnv carries the ABSOLUTE workspace root whose tree produced the
// running binary. `./putnamiw` exports it immediately before exec'ing the CLI it
// just built (or reused from the tree-keyed build cache); nothing else sets it.
//
// It is compared as a resolved path (symlinks evaluated on both sides) against
// the workspace root the launcher discovered — this repository is used through
// git worktrees and symlinked checkouts, where the same tree has several
// spellings.
//
// It is NOT sufficient on its own. An environment variable is caller-settable,
// so the marker is paired with the session's engine pin: the running binary must
// also BE the blob putnamiw selected. See isPinnedSourceEngine for what the pair
// does and does not defend against.
//
// Like NoRelaunchEnv it is inherited by child processes, so a job or hook that
// re-enters `putnami` inside the SAME workspace runs as-is instead of being
// refused. A child that re-enters a DIFFERENT workspace does not match and is
// refused, which is the intended answer.
const FromSourceEnv = "PUTNAMI_FROM_SOURCE"

// FromSourceKeyEnv carries the CONTENT KEY of the from-source build the running
// process was started from: the `<key>` segment of
// `$PUTNAMI_HOME/artifacts/cli-source/<key>/putnami`, the immutable store blob
// `./putnamiw` built or adopted for this invocation. putnamiw exports it next to
// FromSourceEnv, on the from-source paths only, and leaves it unset on the
// keyless fallback (a checkout with no git or no SHA-256 tool, which has no
// store blob to name).
//
// It is the workspace's EPHEMERAL ENGINE PIN: scoped to the process tree that
// inherits it, and immutable for that tree's whole lifetime because the store is
// content-addressed and first-writer-wins. That is the difference that matters.
// The previous proof was `.putnami/bin/putnami`, a mutable
// workspace-global symlink that `./putnamiw` re-points on every run whose source
// key differs — so any concurrent invocation in the same checkout retroactively
// un-proved a CLI that was still running, and its re-entrant children were
// refused while naming the correct engine. A concurrent run at another tree
// state now resolves another key, publishes another blob, and re-points only the
// symlink, which is no longer part of the proof.
const FromSourceKeyEnv = "PUTNAMI_FROM_SOURCE_KEY"

// putnamiHomeEnv locates the machine-global from-source store, with the user
// home directory (os.UserHomeDir) as its fallback. It is read rather than taken
// from store.ResolveArtifactStoreRoot because that function answers a different
// question: it honors PUTNAMI_ARTIFACT_DIR and ignores PUTNAMI_HOME, while
// putnamiw writes the from-source store under $PUTNAMI_HOME/artifacts. CI sets
// PUTNAMI_HOME to a per-run directory, so resolving the store any other way
// would miss it in the exact environment this proof has to hold in. See
// sourceStoreRoot.
const putnamiHomeEnv = "PUTNAMI_HOME"

// IsExemptCommand reports whether a command is exempt from the launcher by name
// alone, regardless of its flags. `pin` is always exempt so a pin can be changed
// or removed even when the pinned engine is broken — otherwise `putnami pin`
// would relaunch into the very engine it is meant to replace.
func IsExemptCommand(name string) bool {
	return name == "pin"
}

// IsExemptInvocation reports whether a full CLI invocation must run as the
// invoked binary rather than relaunching into a workspace-pinned one. Beyond the
// always-exempt commands (IsExemptCommand), it covers the CLI-binary management
// paths that exist precisely to recover from a broken or outdated pinned engine:
//
//   - `version list` / `version use`     — inspect or switch installed binaries
//   - `upgrade --from-source`            — build and adopt a local CLI
//   - `migrate vnext`                    — convert a lock this engine cannot read
//   - `extensions … --user`              — manage the user scope, not the workspace
//
// These must not relaunch: the pinned binary may be the very thing being
// replaced, or may predate the flag/subcommand entirely — relaunching would then
// fail with "unknown flag/subcommand" before the recovery could even start,
// defeating the escape hatch. (`./putnamiw` additionally sets PUTNAMI_NO_RELAUNCH
// for its from-source builds; this covers the global `putnami` entrypoint, which
// does not.)
//
// `migrate vnext` joined the set later.
// Relaunching READS the lock to resolve the pin, and a lock below the format
// floor no longer parses — so a non-exempt migration would fail on the very
// file it exists to convert, in the one state where it is the only way out. The
// earlier argument for keeping it non-exempt (a pinned pre-v2 engine would
// strip the field the migration writes, finding F2) is void now that such an
// engine cannot run a migrated workspace at all.
func IsExemptInvocation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if IsExemptCommand(args[0]) {
		return true
	}
	switch args[0] {
	case "version":
		switch firstNonFlag(args[1:]) {
		case "list", "use":
			return true
		}
	case "migrate":
		if firstNonFlag(args[1:]) == "vnext" {
			return true
		}
	case "upgrade":
		for _, a := range args[1:] {
			if a == "--from-source" {
				return true
			}
		}
	case "extensions":
		for _, a := range args[1:] {
			if a == "--" {
				break
			}
			if a == UserScopeFlag {
				return true
			}
		}
	}
	return false
}

// UserScopeFlag selects the user scope on `putnami extensions`. Such an
// invocation reads and writes only ~/.putnami/user, never the workspace it runs
// in, so the workspace's CLI pin has no authority over it: a pinned engine that
// predates the flag would otherwise refuse the one command that installs a
// user-scope extension.
const UserScopeFlag = "--user"

// firstNonFlag returns the first argument that does not start with "-", or ""
// when there is none — used to read a subcommand past any leading flags.
func firstNonFlag(args []string) string {
	for _, a := range args {
		if a == "" || a[0] != '-' {
			return a
		}
	}
	return ""
}

// ConsumeLaunched reports whether this process was entered through the launcher
// and removes the loop guard from the environment before any child commands are
// spawned.
func ConsumeLaunched() bool {
	if os.Getenv(LaunchedEnv) == "" {
		return false
	}
	_ = os.Unsetenv(LaunchedEnv)
	return true
}

// Restart replaces this process with path and args while explicitly bypassing
// workspace-pin relaunch. It is for a command that has just installed a newer
// verified CLI and must continue under that executable: its own freshly
// installed version is authoritative for the remaining work, not the pin that
// selected the process which performed the update.
//
// The replacement inherits this process's environment, less the relaunch and
// source markers, plus set: each NAME=value entry of set replaces any
// inherited value of NAME.
//
// On Unix a successful restart never returns. On Windows it exits with the
// replacement process's status, matching Relaunch's platform behavior.
func Restart(path string, args []string, set ...string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("restart path is empty")
	}
	argv := append([]string{path}, args...)
	env := restartEnv(os.Environ(), set...)
	return reexec(path, argv, env)
}

func restartEnv(environ []string, set ...string) []string {
	replaced := make(map[string]bool, len(set))
	for _, entry := range set {
		name, _, _ := strings.Cut(entry, "=")
		replaced[name] = true
	}
	env := make([]string, 0, len(environ)+len(set)+1)
	for _, entry := range environ {
		if name, _, _ := strings.Cut(entry, "="); replaced[name] {
			continue
		}
		// FromSourceEnv and FromSourceKeyEnv go too: Restart hands control to a
		// CLI this process just INSTALLED, which by definition did not come from
		// the workspace tree. Inheriting the marker or the engine pin would let a
		// released binary present itself as this workspace's own engine — and the
		// pin must be dropped with the marker, not after it, or the installed CLI
		// would carry a pin naming a blob it is not.
		if strings.HasPrefix(entry, NoRelaunchEnv+"=") || strings.HasPrefix(entry, LaunchedEnv+"=") ||
			strings.HasPrefix(entry, FromSourceEnv+"=") || strings.HasPrefix(entry, FromSourceKeyEnv+"=") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, set...)
	return append(env, NoRelaunchEnv+"=1")
}

// Bootstrap is a credential source the caller started before the relaunch.
// Serve makes it the first credential source of registry downloads and
// returns the function that removes it. Close shuts it down. The zero value
// serves nothing and closes nothing.
type Bootstrap struct {
	Serve func() (restore func())
	Close func()
}

// Relaunch re-execs into the workspace-pinned CLI when the running binary isn't
// it. Under Unix a successful relaunch replaces the process image and does not
// return; otherwise it returns and the caller continues with the current
// binary. wsRoot must be the resolved workspace root ("" disables the launcher).
//
// args are the CLI arguments to hand the pinned binary (os.Args[1:] in
// production); they are taken as a parameter rather than read from os.Args so
// the argv passed on and the invocation echoed back in a recovery hint are the
// ones the caller actually dispatched.
//
// bootstrap serves the pinned CLI's download and nothing else. Relaunch closes
// it before it hands control to the pinned CLI.
//
// A process that holds the run credential hands it only to a pinned CLI that
// advertises runcredential.CustodyLevel or above, and refuses to launch any
// other with ErrPinBelowCustodyLevel (see credentialProbe).
//
// A non-nil error means the workspace declared a CLI this process is not — a
// published pin it could not become, or a source workspace whose tree did not
// build this binary: the caller must NOT continue with the running binary, it
// must report the error (which carries a protocolcli class and a WithNext
// recovery command) and exit.
func Relaunch(ctx context.Context, wsRoot string, args []string, bootstrap Bootstrap) error {
	if wsRoot == "" {
		return nil
	}
	// An unidentifiable running binary is not a reason to skip the pin: self is
	// used only to notice "we already ARE the pinned binary", and an empty path
	// never matches, so the launcher resolves and re-execs instead — at worst one
	// extra exec, which LaunchedEnv stops from recurring.
	self, _ := os.Executable()
	st := artifactstore.New(store.ResolveArtifactStoreRoot(wsRoot))
	resolver := clibin.NewWorkspace(st, wsRoot, self)
	_, err := relaunch(ctx, wsRoot, config{
		self:       self,
		goos:       runtime.GOOS,
		goarch:     runtime.GOARCH,
		args:       args,
		getenv:     os.Getenv,
		homeDir:    os.UserHomeDir,
		environ:    os.Environ,
		resolve:    resolver.Resolve,
		exec:       reexec,
		hosted:     runcredential.Hosted(),
		probe:      credentialProbe(os.Getenv),
		serve:      bootstrap.Serve,
		beforeExec: bootstrap.Close,
	})
	return err
}

// config holds relaunch's dependencies so the decision logic is unit-testable
// without performing a real exec.
type config struct {
	self    string
	goos    string
	goarch  string
	args    []string
	getenv  func(string) string
	homeDir func() (string, error)
	environ func() []string
	resolve func(context.Context, *lockfile.LockEntry, string, string) (string, error)
	exec    func(path string, argv []string, env []string) error
	// hosted reports that this process holds the run credential.
	hosted bool
	// probe reads the custody level the CLI at path advertises. It runs only
	// when hosted.
	probe func(ctx context.Context, path string) (probeResult, error)
	// serve, when set, is in effect while resolve runs and at no other time.
	serve func() (restore func())
	// beforeExec, when set, runs right before exec.
	beforeExec func()
}

// relaunch is the pure decision core. It returns (true, nil) only when it handed
// control to the pinned binary (observable when exec returns — i.e. tests and
// the Windows spawn path); under Unix a real exec never returns. (false, nil)
// means "run the current binary", which happens only when there is no pin to
// honor or this binary already satisfies it. A non-nil error means the workspace
// declared an engine this process is not — the caller must stop rather than run
// some other engine.
//
// Order is load-bearing. The lock is read before NoRelaunchEnv is applied so the
// source-workspace sentinel can be seen at all, but an unreadable lock under
// NoRelaunchEnv still returns fail-open, so the opt-out keeps working as the
// documented escape from a broken published pin.
func relaunch(ctx context.Context, wsRoot string, c config) (bool, error) {
	if c.getenv(LaunchedEnv) != "" {
		return false, nil // already relaunched once: hard loop guard
	}
	optedOut := truthy(c.getenv(NoRelaunchEnv))
	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		if optedOut {
			// The opt-out must survive an unreadable lock, or the one command
			// meant to recover from a broken lock would be bricked by it. This
			// is the ONLY reason the read happens before the opt-out is applied:
			// a source workspace must be recognized before the opt-out, and the
			// sentinel lives in the file.
			return false, nil
		}
		// A lock that exists but cannot be read cannot rule a pin OUT, and
		// "probably unpinned" is not a safe guess.
		return false, lockError(c, err)
	}
	var entry lockfile.LockEntry
	declared := false
	if lf != nil {
		entry, declared = lf.GetCLI()
	}
	if declared && entry.IsWorkspaceSource() {
		// A source workspace is decided BEFORE the opt-out. PUTNAMI_NO_RELAUNCH
		// is the escape from a broken PIN; a source workspace has no pinned
		// binary to be broken, so honoring it here would turn the opt-out into
		// a license to gate a commit with some other engine — exactly what a
		// source workspace's fail-closed rule forbids. `./putnamiw` and
		// IsExemptInvocation are the ways through, and App.Run checks the
		// latter before ever calling Relaunch.
		return false, sourceWorkspaceDecision(wsRoot, c)
	}
	if optedOut {
		return false, nil // explicit opt-out (e.g. putnamiw running its own build)
	}
	if lf == nil {
		return false, nil // no lock file at all: nothing pins this workspace
	}
	if !declared {
		return false, nil // the workspace pins no CLI version
	}
	sha := strings.ToLower(entry.IntegrityFor(c.goos, c.goarch))
	if sha == "" {
		// The pin names a version but nothing this platform can verify against.
		// Running unverified bytes is not an option, and running the current
		// binary would silently defeat the pin.
		return false, noPlatformDigestError(c, entry.Version)
	}

	resolved, err := c.resolveServed(ctx, &entry)
	if err != nil {
		return false, resolveError(c, entry.Version, err)
	}
	if sameFile(resolved, c.self) {
		return false, nil // we already ARE the pinned binary
	}
	if c.hosted {
		// The run credential goes only to a pinned CLI at this CLI's custody
		// level or above, so a pin cannot undo custody this CLI implements.
		// The check runs before the test-binary refusal, so a test can assert
		// it.
		probed, err := c.probe(ctx, resolved)
		if err != nil {
			return false, probeError(c, entry.Version, resolved, err)
		}
		if probed.level < runcredential.CustodyLevel {
			return false, olderPinError(entry.Version, probed)
		}
	}

	if isTestBinary(c.self) {
		return false, testBinaryExecError(c, entry.Version, resolved)
	}

	if c.beforeExec != nil {
		c.beforeExec()
	}
	argv := append([]string{resolved}, c.args...)
	env := append(c.environ(), LaunchedEnv+"="+sha)
	if err := c.exec(resolved, argv, env); err != nil {
		return false, execError(c, entry.Version, resolved, err)
	}
	return true, nil // Unix: unreachable (process replaced); Windows: child ran
}

// resolveServed resolves the pinned CLI with c.serve in effect.
func (c config) resolveServed(ctx context.Context, entry *lockfile.LockEntry) (string, error) {
	if c.serve != nil {
		restore := c.serve()
		defer restore()
	}
	return c.resolve(ctx, entry, c.goos, c.goarch)
}

// sourceWorkspaceDecision answers "may THIS binary run against a workspace that
// builds its own engine?". There is no binary to exec into, so the answer is
// only ever "run as-is" or a hard error; it never returns launched=true.
//
// Two things prove the running binary came from this tree:
//
//   - FromSourceEnv names this same workspace root. `./putnamiw` sets it after
//     compiling (or reusing its tree-keyed cache of) exactly these sources.
//   - the running binary is the engine that run selected: the immutable store
//     blob FromSourceKeyEnv pins (isPinnedSourceEngine), or `.putnami/bin/putnami`
//     on a checkout that has no content key (isWorkspaceEngine).
//
// A Go test binary is admitted on its own: `go test` produced it from this very
// tree, so it already satisfies the invariant — and refusing it would break every
// suite that runs inside the repository, for no safety gain.
//
// Anything else is a foreign engine and is refused, and the refusal names which
// proof is missing: they fail for unrelated reasons — a marker the process ladder
// dropped, a pin a `Restart` stripped, an engine link some other command
// re-pointed — and a message that says only "at least one" leaves the reader to
// guess which.
func sourceWorkspaceDecision(wsRoot string, c config) error {
	marker := c.getenv(FromSourceEnv)
	if sameResolvedPath(marker, wsRoot) &&
		(isPinnedSourceEngine(c) || isWorkspaceEngine(wsRoot, c.self, c.goos)) {
		return nil
	}
	if isTestBinary(c.self) {
		return nil
	}
	return foreignEngineError(c,
		markerProof(marker, wsRoot),
		pinProof(c),
		engineProof(wsRoot, c.self, c.goos))
}

// markerProof states what FromSourceEnv carries, as one clause of the refusal.
func markerProof(marker, wsRoot string) string {
	switch {
	case strings.TrimSpace(marker) == "":
		return FromSourceEnv + " is unset"
	case sameResolvedPath(marker, wsRoot):
		return FromSourceEnv + " names this workspace root"
	default:
		return fmt.Sprintf("%s names %s, not this workspace root %s", FromSourceEnv, marker, wsRoot)
	}
}

// pinProof states what the session's engine pin says, as one clause of the
// refusal. It distinguishes "no pin at all" — the marker survived but
// FromSourceKeyEnv did not, which is what a Restart or a hand-written
// FromSourceEnv looks like — from "the pin names a blob that is not this
// binary", which is a genuinely foreign engine run under a borrowed key.
func pinProof(c config) string {
	key := strings.TrimSpace(c.getenv(FromSourceKeyEnv))
	if key == "" {
		return "no from-source build is pinned (" + FromSourceKeyEnv + " is unset)"
	}
	root := sourceStoreRoot(c)
	if root == "" {
		return fmt.Sprintf("%s pins %s but the from-source store cannot be located", FromSourceKeyEnv, key)
	}
	blob := filepath.Join(root, key, engineBinary(c.goos))
	if sameFile(c.self, blob) {
		return fmt.Sprintf("%s pins %s and this binary is its blob", FromSourceKeyEnv, key)
	}
	if _, err := os.Stat(blob); err != nil {
		return fmt.Sprintf("%s pins %s, whose blob %s is gone", FromSourceKeyEnv, key, blob)
	}
	return fmt.Sprintf("%s pins %s, whose blob is %s, not this binary", FromSourceKeyEnv, key, blob)
}

// engineProof states what .putnami/bin/putnami is, as the other clause. It
// distinguishes the three ways the link stops being this binary — never
// published, re-pointed at another blob, or pointing at bytes that are gone —
// because each has a different cause and only the first is "you never ran
// ./putnamiw here".
func engineProof(wsRoot, self, goos string) string {
	name := path.Join(".putnami", "bin", engineBinary(goos))
	if strings.TrimSpace(wsRoot) == "" {
		return "the workspace root is unknown, so " + name + " cannot be checked"
	}
	engine := filepath.Join(wsRoot, ".putnami", "bin", engineBinary(goos))
	if sameFile(self, engine) {
		return name + " is this binary"
	}
	info, err := os.Lstat(engine)
	switch {
	case err != nil:
		return name + " does not exist"
	case info.Mode()&os.ModeSymlink != 0:
		target, linkErr := os.Readlink(engine)
		if linkErr != nil {
			return name + " is an unreadable symlink"
		}
		if _, statErr := os.Stat(engine); statErr != nil {
			return fmt.Sprintf("%s points at %s, which does not exist", name, target)
		}
		return fmt.Sprintf("%s points at %s, not this binary", name, target)
	default:
		return name + " is another file, not this binary"
	}
}

// isPinnedSourceEngine reports whether self IS the store blob this session's
// engine pin names: `<from-source store>/<FromSourceKeyEnv>/putnami`. It is the
// PRIMARY proof, and the one a concurrent `./putnamiw` cannot take away.
//
// It exists because FromSourceEnv alone is caller-settable, and an environment
// variable that a one-line prefix can satisfy is not evidence:
// `PUTNAMI_FROM_SOURCE=$PWD /usr/local/bin/putnami build --all` would otherwise
// gate a commit with a released engine while claiming the tree's own — the exact
// dishonest green this workspace shape removes, restored by a shortcut somebody
// reaches for once.
//
// Naming the key does not weaken that bar, because the key is not the evidence:
// the LOCATION is. `publish_source_cli` is the only writer of the from-source
// store, and `--download` writes to artifacts/cli/<sha> instead, so the released
// binary above is not under any key and the prefix still fails. What the key adds
// is precision — it pins ONE blob, the one whose content key this tree hashes to,
// rather than admitting any from-source blob the machine happens to hold. And it
// stops depending on mutable state.
//
// What the bar actually is, stated honestly: the store root comes from
// PUTNAMI_HOME, which is caller-settable too, so the cheapest bypass is not "plant
// a binary in the machine's real store" but "point PUTNAMI_HOME at a directory you
// control and plant one there" — two deliberate commands plus a third variable,
// where the old link check cost one deliberate write. Neither is a command prefix,
// and neither stops somebody who can already overwrite `.putnami/bin/putnami`.
// This is a guard against the shortcut, not against an adversary with write
// access; ADR 0028 said so of the original pair and it is still true.
//
// Compared by inode (os.SameFile), so /tmp and /private/tmp spellings of the
// same blob, and a symlink that still resolves to it, are the same answer.
func isPinnedSourceEngine(c config) bool {
	key := strings.TrimSpace(c.getenv(FromSourceKeyEnv))
	if key == "" || strings.TrimSpace(c.self) == "" {
		return false
	}
	// A key is a hex digest; rejecting separators keeps it from climbing out of
	// the store it is meant to index.
	if key != filepath.Base(key) || key == "." || key == ".." {
		return false
	}
	root := sourceStoreRoot(c)
	if root == "" {
		return false
	}
	return sameFile(c.self, filepath.Join(root, key, engineBinary(c.goos)))
}

// sourceStoreRoot returns the from-source CLI store: $PUTNAMI_HOME, else
// .putnami under the user home directory (os.UserHomeDir, the rule every other
// machine-global root follows) — then artifacts/cli-source under it. putnamiw
// always exports PUTNAMI_HOME, so a run it started never reaches the fallback.
// Without either, there is no store and the pin proves nothing.
//
// Keeping the two in step is a real coupling, and it is the reason this does not
// call store.ResolveArtifactStoreRoot: that honors PUTNAMI_ARTIFACT_DIR and never
// reads PUTNAMI_HOME, so on CI — which sets PUTNAMI_HOME to a per-run directory
// and leaves PUTNAMI_ARTIFACT_DIR unset — it would resolve a store putnamiw never
// wrote to, and the proof would fail everywhere it matters most.
func sourceStoreRoot(c config) string {
	home := strings.TrimSpace(c.getenv(putnamiHomeEnv))
	if home == "" && c.homeDir != nil {
		if userHome, err := c.homeDir(); err == nil && strings.TrimSpace(userHome) != "" {
			home = filepath.Join(userHome, ".putnami")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "artifacts", "cli-source")
}

// isWorkspaceEngine reports whether self IS the binary this workspace's
// ./putnamiw selects — .putnami/bin/putnami, which putnamiw links at the blob it
// just built or adopted from the tree-keyed store.
//
// It is the FALLBACK proof, kept for the keyless path: a checkout with no git or
// no SHA-256 tool gets no content key, so putnamiw builds a regular file straight
// into .putnami/bin/putnami and there is no store blob to pin. Dropping this
// would refuse those checkouts outright.
//
// On that path the link is still mutable, so a concurrent rebuild still races it
// — narrowed from every checkout to one that has neither git nor a
// hashing tool, where a content key cannot be computed at all.
//
// Compared by inode (os.SameFile), so the symlink putnamiw publishes and the
// store blob it resolves to are the same answer.
func isWorkspaceEngine(wsRoot, self, goos string) bool {
	if strings.TrimSpace(self) == "" || strings.TrimSpace(wsRoot) == "" {
		return false
	}
	return sameFile(self, filepath.Join(wsRoot, ".putnami", "bin", engineBinary(goos)))
}

// engineBinary is the file name of a CLI engine built for goos: the store blob
// a from-source pin names and the workspace's .putnami/bin link.
func engineBinary(goos string) string {
	return pkgmeta.ExecutableName(goos, "putnami")
}

// foreignEngineError reports a binary that is not this workspace's own engine.
// It is InvalidConfig — the machine is fine, the invocation is wrong — and it
// names the file, the sentinel, and the one command that produces a legitimate
// engine, so the message is actionable without reading this package.
func foreignEngineError(c config, marker, pin, engine string) error {
	self := c.self
	if strings.TrimSpace(self) == "" {
		self = "this binary"
	}
	return protocolcli.WithNext(
		protocolcli.InvalidConfigf(
			"%s declares this workspace the source of its own CLI (cli.source = %q), so only a putnami "+
				"built from this tree may run here; %s is not it. A from-source run carries two things: "+
				"%s naming this workspace root, and the binary being the engine that run selected — the "+
				"immutable store blob %s pins, or .putnami/bin/putnami on a checkout with no content key. "+
				"Here %s, %s, and %s, so running this would gate the commit with an engine the commit "+
				"does not contain",
			lockfile.LockFilename, lockfile.SourceWorkspace, self, FromSourceEnv, FromSourceKeyEnv,
			marker, pin, engine),
		putnamiwCommand(c.args))
}

// putnamiwCommand renders the recovery for a source workspace: the committed
// wrapper, which builds the CLI from the tree and then runs this exact
// invocation with it. Arguments are quoted like optOutCommand's, so a copy-paste
// reproduces the command verbatim.
func putnamiwCommand(args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "./putnamiw")
	for _, a := range args {
		parts = append(parts, ShellQuote(a))
	}
	return strings.Join(parts, " ")
}

// sameResolvedPath reports whether two path spellings name the same directory.
// Both sides are made absolute and have their symlinks evaluated, because this
// repository is worked in through git worktrees and symlinked checkouts where
// the same tree is reachable under several names — comparing the raw strings
// would refuse a legitimate from-source run (the worktree-symlink-cwd class of
// bug). An empty marker never matches, so "unset" is not "matches everything".
func sameResolvedPath(a, b string) bool {
	ra, rb := resolvePath(a), resolvePath(b)
	return ra != "" && ra == rb
}

// resolvePath returns the canonical form of p: absolute, with its directory
// links followed, a junction on Windows included. A path that cannot be
// resolved (it does not exist yet, or a parent is unreadable) degrades to its
// absolute form rather than to "", so two spellings of the same non-existent
// path still compare equal.
func resolvePath(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := dirlink.Resolve(abs); err == nil {
		return resolved
	}
	return abs
}

// lockError reports a putnami.lock.json this binary could not read. The two
// version errors are called out separately because neither recovery is the
// generic one: for a lock written by a NEWER CLI the file is fine and this
// binary is too old, and for one written by an OLDER CLI the binary is fine and
// the file needs converting. Only a genuinely unreadable lock — corrupt,
// unreadable bytes — falls through to "opt out of the pin for this run".
func lockError(c config, err error) error {
	var unsupported *lockfile.UnsupportedVersionError
	if errors.As(err, &unsupported) {
		// Plain `putnami upgrade` is NOT exempt, so it would re-enter the
		// launcher and hit this same wall; the opt-out form is the one that runs.
		return protocolcli.WithNext(
			protocolcli.InvalidConfigf("cannot honor the workspace CLI pin: %w", err),
			NoRelaunchEnv+"=1 putnami upgrade")
	}
	var outdated *lockfile.OutdatedVersionError
	if errors.As(err, &outdated) {
		// `migrate vnext` is exempt from the launcher (IsExemptInvocation), so
		// this next step runs instead of re-entering the same wall — and unlike
		// the generic opt-out it actually repairs the workspace rather than
		// running one command with the pin ignored.
		return protocolcli.WithNext(
			protocolcli.InvalidConfigf("cannot honor the workspace CLI pin: %w", err),
			"putnami migrate vnext --apply")
	}
	return protocolcli.WithNext(
		protocolcli.InvalidConfigf(
			"cannot read %s, so a workspace CLI pin cannot be ruled out: %w",
			lockfile.LockFilename, err),
		optOutCommand(c.args))
}

// noPlatformDigestError reports a pin with no digest for the running platform —
// typically a lock pinned on one OS/arch and used on another. `putnami pin` is
// exempt from the launcher and appends this platform's digest to the same entry,
// so re-running it here completes the pin instead of replacing it.
func noPlatformDigestError(c config, version string) error {
	return protocolcli.WithNext(
		protocolcli.InvalidConfigf(
			"%s pins CLI %q but records no integrity digest for %s/%s, so the pinned binary cannot be verified on this machine",
			lockfile.LockFilename, version, c.goos, c.goarch),
		pinCommand(version))
}

// pinCommand names the `pin` invocation that repairs this pin. A pin carrying no
// version at all — only a hand-edited lock produces one — cannot be re-pinned,
// only dropped, and suggesting a bare `putnami pin` would just print it back.
func pinCommand(version string) string {
	if strings.TrimSpace(version) == "" {
		return "putnami pin --remove"
	}
	return "putnami pin " + version
}

// resolveError turns a clibin failure into the one message that names what
// actually broke and what to run next. The three modes have three recoveries:
// a download failure is transient or offline (opt out for this run), an
// integrity failure means the registry bytes no longer match the pin (re-pin,
// the only trust-establishing action), and a store failure is local (opt out
// while the disk/permissions are fixed).
func resolveError(c config, version string, err error) error {
	switch {
	case errors.Is(err, clibin.ErrNoPlatformDigest):
		return noPlatformDigestError(c, version)
	case errors.Is(err, clibin.ErrDownload):
		return protocolcli.WithNext(
			protocolcli.APIf("could not download pinned CLI %s for %s/%s: %w",
				version, c.goos, c.goarch, err),
			optOutCommand(c.args))
	case errors.Is(err, clibin.ErrIntegrity):
		return protocolcli.WithNext(
			fmt.Errorf("pinned CLI %s does not match the digest recorded in %s: %w",
				version, lockfile.LockFilename, err),
			pinCommand(version))
	case errors.Is(err, clibin.ErrStore):
		return protocolcli.WithNext(
			fmt.Errorf("could not admit pinned CLI %s into the machine-global artifact store: %w",
				version, err),
			optOutCommand(c.args))
	default:
		return protocolcli.WithNext(
			fmt.Errorf("could not resolve pinned CLI %s for %s/%s: %w",
				version, c.goos, c.goarch, err),
			optOutCommand(c.args))
	}
}

// execError reports a resolved binary that would not run — a store entry
// truncated or corrupted after admission, a noexec mount, a binary for the wrong
// platform. The path is named so the offending store entry can be removed.
func execError(c config, version, path string, err error) error {
	return protocolcli.WithNext(
		fmt.Errorf("could not launch pinned CLI %s from %s: %w", version, path, err),
		optOutCommand(c.args))
}

// testBinaryExecError reports a Go test binary that reached the exec. It is a
// hard error rather than a silent skip because the caller is a test that would
// otherwise be measuring the wrong process, and rather than an exec because
// syscall.Exec would end the suite. See isTestBinary for why that is worse than
// any failure: the run exits 0 and prints "ok" with most tests never executed.
//
// The fix belongs in the test, not here: give it its own workspace with
// t.Chdir(t.TempDir()) so no pin applies, or set NoRelaunchEnv when the test
// genuinely means to run against a pinned workspace.
// It is InvalidConfig, not a generic error, so it stays distinguishable from a
// genuine exec failure (execError) when triaging a CI log: this one is always a
// defect in the test, never in the machine.
func testBinaryExecError(c config, version, path string) error {
	return protocolcli.WithNext(
		protocolcli.InvalidConfigf(
			"refusing to replace a Go test binary with pinned CLI %s from %s: "+
				"exec would end the test run mid-suite; give the test its own workspace "+
				"(t.Chdir(t.TempDir())) or set %s", version, path, NoRelaunchEnv),
		optOutCommand(c.args))
}

// optOutCommand renders the escape hatch for this exact invocation. NoRelaunchEnv
// is read before the lock file is, so this command runs no matter which wall the
// launcher hit — and rendering the real arguments makes the hint copy-pasteable
// instead of a generic "set an environment variable".
func optOutCommand(args []string) string {
	parts := make([]string, 0, len(args)+2)
	parts = append(parts, NoRelaunchEnv+"=1", "putnami")
	for _, a := range args {
		parts = append(parts, ShellQuote(a))
	}
	return strings.Join(parts, " ")
}

// ShellQuote single-quotes an argument that would not survive a copy-paste
// unquoted, so the suggested command reproduces the invocation verbatim.
func ShellQuote(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`&|;<>()*?[]{}#~!") {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

// isTestBinary reports whether the running executable is a Go test binary.
// `go test` names it <package>.test, so the base name is the identity.
//
// A test binary must never be re-execed. syscall.Exec REPLACES the process, so
// every test after the one that triggered it silently disappears while the run
// still exits 0 and prints "ok", and the coverage profile is never written — the
// whole package then measures 0%. That is a vacuous pass: the suite reports
// success precisely because it stopped running.
//
// NoRelaunchEnv is the manual way out and `./putnamiw` sets it, which is why the
// truncation stayed invisible locally. The global `putnami` entrypoint does not,
// so CI ran the truncated suite. Detecting the test binary makes the guarantee
// independent of who invoked the run, rather than a property of one wrapper.
//
// The check sits at the exec and nowhere earlier, so every decision a test may
// want to assert — an unreadable lock, a pin with no digest for this platform, a
// resolve failure — still runs and still fails closed. Only the irreversible
// step is refused.
func isTestBinary(self string) bool {
	base := filepath.Base(self)
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
}

// truthy reports whether an environment value means "on" (1/true/yes/on,
// case-insensitive). Anything else — including "", "0", "false" — is off, so a
// stray PUTNAMI_NO_RELAUNCH=0 does not silently disable the launcher.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// sameFile reports whether a and b are the same on-disk file by inode identity
// (following symlinks), so a worktree symlink into the store counts as
// already-the-pinned-binary and avoids a needless relaunch.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
