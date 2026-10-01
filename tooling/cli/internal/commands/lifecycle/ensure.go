package lifecycle

import (
	"context"
	"log/slog"
	"os"
	"sync"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/template"
)

// artifactsEnsuredEnv marks the workspace whose artifacts a parent putnami has
// already materialized, so a nested putnami invocation (an extension job that
// shells out to putnami) skips the redundant ensure pass. It rides the standard
// PUTNAMI_* subprocess inheritance (job subprocesses start from os.Environ).
const artifactsEnsuredEnv = "PUTNAMI_ARTIFACTS_ENSURED"

// ensuredWorkspaces dedupes EnsureArtifacts to once per workspace per process.
var ensuredWorkspaces sync.Map // wsRoot -> struct{}

// EnsureArtifacts materializes the workspace's lock-PINNED registry extensions,
// templates, and agent workflows so a fresh git worktree resolves them with no
// explicit `putnami install` (zero-init worktrees). An extension declared in config but absent from
// the lock is left to explicit install — it is not a zero-init target, and
// materializing it implicitly would re-attempt a download on every command.
//
// It is the implicit, lock-driven half of install: lock-READ only, it never
// writes the lock and never runs deps or context generation, and it touches the
// network only when the machine-global store is missing a pinned digest. The
// warm path (everything present) is a stat plus a keep-warm stamp per artifact.
// Failures are logged, not fatal — a genuinely un-installable extension is
// surfaced by the missing-extension guard in the run plan, which is the single
// terminal exit.
//
// It runs at most once per workspace per process, and skips entirely when a
// parent putnami already ensured this workspace.
func EnsureArtifacts(ctx context.Context, wsRoot string, cfg *wsproto.Config) error {
	return EnsureArtifactsServing(ctx, wsRoot, cfg, nil)
}

// EnsureArtifactsServing is EnsureArtifacts with serve in effect while the
// lock-pinned extensions download, and at no other time. serve makes a
// credential source the first one of registry downloads and returns the
// function that removes it. A nil serve is EnsureArtifacts. A call that
// EnsureArtifacts would skip does not call serve.
func EnsureArtifactsServing(ctx context.Context, wsRoot string, cfg *wsproto.Config, serve func() (restore func())) error {
	if cfg == nil || wsRoot == "" {
		return nil
	}
	if os.Getenv(artifactsEnsuredEnv) == wsRoot {
		return nil // a parent putnami already materialized this workspace
	}
	if _, loaded := ensuredWorkspaces.LoadOrStore(wsRoot, struct{}{}); loaded {
		return nil
	}
	// Mark before doing the work so jobs this run spawns (they inherit
	// os.Environ) skip the redundant pass.
	_ = os.Setenv(artifactsEnsuredEnv, wsRoot)

	lf, lockErr := lockfile.ReadLockFile(wsRoot)
	if lockErr != nil {
		// Tolerant, not silent. A lock this CLI cannot read materializes
		// NOTHING — every entry below resolves to nil and is skipped — and since
		// the format floor moved to v2, a v1 lock reaches this
		// line in every workspace that has not migrated. Without the notice a
		// zero-init worktree just comes up with no extensions, which reads as an
		// empty workspace rather than as a lock that needs converting. Warn, not
		// the Debug the per-artifact `note` below uses: this is the whole pass
		// failing, not one artifact, and it names its own remedy (B6r/F6).
		slog.Warn("lock file could not be read; no artifacts were materialized", "error", lockErr)
	}

	var firstErr error
	note := func(name string, err error) {
		// Debug, not Warn: implicit ensure is best-effort. A locked extension that
		// can't be materialized (e.g. a pinned version the registry no longer
		// serves) is noise on every command if it isn't actually needed — and when
		// it IS needed, the missing-extension guard in the run plan is the
		// authoritative, hard-failing error path.
		slog.Debug("ensure artifact", "name", name, "error", err)
		if firstErr == nil {
			firstErr = err
		}
	}

	ensureLockedExtensions(ctx, wsRoot, cfg, lf, note, serve)

	tplInst := template.NewInstaller(wsRoot)
	for name, constraint := range shared.BuildTemplateMap(cfg) {
		entry := lookupLockEntry(lf, name, false)
		if entry == nil {
			continue
		}
		if err := tplInst.Ensure(ctx, name, constraint, entry); err != nil {
			note(name, err)
		}
	}

	// The agent content of the extensions the lock pins arrives the same way,
	// so a clone runs its skills from the first command. Lock-authoritative and
	// best-effort like the two loops above; a local extension's content is left
	// to its author's explicit `context generate`, and nothing here removes a
	// file. A hosted run writes no assistant content (installWritesWorkspace).
	if !installWritesWorkspace() {
		return firstErr
	}
	if err := ensureAgentWorkflows(ctx, wsRoot, cfg); err != nil {
		note("agent workflows", err)
	}

	return firstErr
}

// ensureLockedExtensions materializes the lock-pinned registry extensions of
// cfg, with serve in effect for the whole pass when it is set, and reports
// each failure to note.
func ensureLockedExtensions(
	ctx context.Context,
	wsRoot string,
	cfg *wsproto.Config,
	lf *lockfile.LockFile,
	note func(name string, err error),
	serve func() (restore func()),
) {
	if serve != nil {
		restore := serve()
		defer restore()
	}
	extInst := extension.NewInstaller(wsRoot)
	for name, constraint := range shared.BuildExtensionMap(cfg) {
		entry := lookupLockEntry(lf, name, true)
		if !shared.IsRegistryArtifactRef(name) || entry == nil {
			// Local-path extensions resolve from the worktree; an unlocked
			// registry extension is not a zero-init target — explicit `putnami
			// install` resolves it and writes the lock first. (Materializing it
			// implicitly would re-attempt a download on every command.)
			continue
		}
		if err := extInst.EnsureExtension(ctx, name, constraint, entry); err != nil {
			note(name, err)
		}
	}
}

// ensureAgentWorkflows is the seam tests replace to observe the agent pass.
var ensureAgentWorkflows = agentctx.EnsureAgentWorkflows

// lookupLockEntry returns a pointer to the lock entry for name (extension when
// isExt, otherwise template), or nil when the lock has no entry for it.
func lookupLockEntry(lf *lockfile.LockFile, name string, isExt bool) *lockfile.LockEntry {
	if lf == nil {
		return nil
	}
	var (
		e  lockfile.LockEntry
		ok bool
	)
	if isExt {
		e, ok = lf.GetExtension(name)
	} else {
		e, ok = lf.GetTemplate(name)
	}
	if !ok {
		return nil
	}
	return &e
}
