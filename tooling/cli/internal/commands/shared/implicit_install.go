package shared

import "context"

// implicitInstallKey tags a context as an implicit (first-use bootstrap)
// install. installArtifacts reads it (via IsImplicitInstall) to restore
// workspace state WITHOUT rewriting the committed putnami.lock.json —
// persisting this host's freshly-resolved per-platform integrity there is the
// top autonomous-session paper-cut, dirtying the tree on every fresh
// worktree/CI host whose os/arch the committed lock does not already list. A
// context value is used deliberately over a threaded parameter or an env var:
// the signal is scoped to exactly this install call and must NOT be inherited
// by nested/subprocess putnami installs the way a PUTNAMI_* env var would
// (which would wrongly silence an explicit nested install).
type implicitInstallKey struct{}

// WithImplicitInstall marks ctx as an implicit first-use bootstrap install.
func WithImplicitInstall(ctx context.Context) context.Context {
	return context.WithValue(ctx, implicitInstallKey{}, true)
}

// IsImplicitInstall reports whether ctx was marked by WithImplicitInstall.
func IsImplicitInstall(ctx context.Context) bool {
	v, _ := ctx.Value(implicitInstallKey{}).(bool)
	return v
}
