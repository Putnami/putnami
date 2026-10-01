package lifecycle

import (
	"context"
	"fmt"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// readPreparationTimeout bounds the complete cold read bootstrap. A registry
// client has a longer general-purpose download timeout, but an analysis command
// must degrade to the workspace's local facts promptly when the registry is
// offline. The warm path is only lock reads and verified stable-link checks.
const readPreparationTimeout = 5 * time.Second

// ReadPreparedExtension is one registry extension that a read command made
// faithful to the workspace lock. Root is the stable, per-worktree path; it is
// safe to hand to later manifest and documentation readers only because
// EnsureExtensionLocked validated its exact version and manifest hash first.
type ReadPreparedExtension struct {
	Name    string
	Version string
	Root    string
}

// ReadPreparationIssue explains why a declared extension was not available to
// a read. Reads continue with local core facts, but callers can surface this
// reason instead of silently substituting an ambient or latest artifact.
type ReadPreparationIssue struct {
	Name    string
	Version string
	Reason  string
}

// ReadPreparationReport is the bounded result used by CLI and MCP reads.
type ReadPreparationReport struct {
	Extensions []ReadPreparedExtension
	Issues     []ReadPreparationIssue
}

type readPreparationContextKey struct{}

// WithReadPreparation carries one invocation's read selection to structured
// CLI handlers. The context is process-local and short-lived; it stores no
// workspace identity and cannot leak one session's root into another.
func WithReadPreparation(ctx context.Context, report ReadPreparationReport) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, readPreparationContextKey{}, report)
}

// ReadPreparationFromContext returns the report selected by the CLI's bounded
// preparation pass, when the current command is one of the analysis reads.
func ReadPreparationFromContext(ctx context.Context) (ReadPreparationReport, bool) {
	if ctx == nil {
		return ReadPreparationReport{}, false
	}
	report, ok := ctx.Value(readPreparationContextKey{}).(ReadPreparationReport)
	return report, ok
}

// PrepareReadOnly materializes only exact, lock-pinned registry extensions
// needed to expose their read surfaces. It never installs templates or agent
// workflows, runs an extension command or workspace-install job, generates
// context files, installs dependencies, invokes Cloud setup, or writes the
// lock. Missing artifacts are fetched through the existing verified CAS under
// one short deadline; failure is recorded and the original read continues.
func PrepareReadOnly(ctx context.Context, wsRoot string, cfg *wsproto.Config) ReadPreparationReport {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, readPreparationTimeout)
	defer cancel()
	return prepareReadOnly(ctx, wsRoot, cfg)
}

func prepareReadOnly(ctx context.Context, wsRoot string, cfg *wsproto.Config) ReadPreparationReport {
	var report ReadPreparationReport
	if cfg == nil || wsRoot == "" {
		return report
	}

	lf, err := lockfile.ReadLockFile(wsRoot)
	if err != nil {
		for _, name := range cfg.Extensions.Names() {
			if shared.IsRegistryArtifactRef(name) {
				report.Issues = append(report.Issues, ReadPreparationIssue{Name: name, Reason: fmt.Sprintf("read %s: %v", lockfile.LockFilename, err)})
			}
		}
		return report
	}
	if lf == nil {
		for _, name := range cfg.Extensions.Names() {
			if shared.IsRegistryArtifactRef(name) {
				report.Issues = append(report.Issues, ReadPreparationIssue{Name: name, Reason: "extension is not pinned because the workspace lock is absent"})
			}
		}
		return report
	}

	installer := extension.NewInstaller(wsRoot)
	installer.AnonymousRegistry = true
	installer.RequireVerified = true
	for _, name := range cfg.Extensions.Names() {
		if !shared.IsRegistryArtifactRef(name) {
			continue
		}
		entry, ok := lf.GetExtension(name)
		if !ok {
			report.Issues = append(report.Issues, ReadPreparationIssue{Name: name, Reason: "extension is not pinned in the workspace lock"})
			continue
		}
		if err := installer.EnsureExtensionLocked(ctx, name, cfg.Extensions.List[name], &entry); err != nil {
			report.Issues = append(report.Issues, ReadPreparationIssue{Name: name, Version: entry.Version, Reason: err.Error()})
			continue
		}
		report.Extensions = append(report.Extensions, ReadPreparedExtension{
			Name: name, Version: entry.Version,
			Root: layout.StableDir(wsRoot, layout.Extensions, name),
		})
	}
	return report
}
