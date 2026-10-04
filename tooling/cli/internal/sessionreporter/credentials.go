// Package sessionreporter delivers persisted engine artifacts through the
// protocol-owned reporting commands: session-reporter and log-reporter.
package sessionreporter

import (
	"context"
	"os"
	"strings"

	"go.putnami.dev/tooling/cli/internal/runcredential"
)

type credentialKey struct{}

// selection is one capability's captured selector and token.
type selection struct{ provider, token string }

// credentials holds every capability's selection, keyed by capability name.
type credentials struct{ selected map[string]selection }

func (*credentials) String() string { return "<session reporter credentials>" }

// Capture removes every capability's explicit selection and token from the
// ambient environment before repository subprocesses run, and keeps them in the
// context. This is not a same-principal or network sandbox. A trusted launcher
// must independently establish provider provenance.
//
// A hosted run keeps no token: its reporters receive the run credential over
// the protocol (startHolder), and the process that captured the run
// credential already removed the tokens from its environment, with a warning
// (runcredential.Capture).
func Capture(ctx context.Context) context.Context {
	captured := &credentials{selected: map[string]selection{}}
	hosted := runcredential.Hosted()
	for _, c := range Capabilities() {
		token := os.Getenv(c.TokenEnv)
		if hosted {
			token = ""
		}
		captured.selected[c.Name] = selection{strings.TrimSpace(os.Getenv(c.SelectorEnv)), token}
		_ = os.Unsetenv(c.SelectorEnv)
		_ = os.Unsetenv(c.TokenEnv)
	}
	if _, ok := ctx.Value(credentialKey{}).(*credentials); ok {
		return ctx
	}
	return context.WithValue(ctx, credentialKey{}, captured)
}

// Provider is the extension the capability's selector named when the context
// was captured, or "" when the capability is not selected.
func (c Capability) Provider(ctx context.Context) string {
	return c.selection(ctx).provider
}

func (c Capability) selection(ctx context.Context) selection {
	if captured, ok := ctx.Value(credentialKey{}).(*credentials); ok {
		return captured.selected[c.Name]
	}
	return selection{}
}

// providerEnv is the capability's provider environment: every capability's
// selector and token are removed, then only this capability's own token is
// added back. A hosted run adds none: no environment of its reporters holds a
// token.
func (c Capability) providerEnv(ctx context.Context, env []string) []string {
	reserved := map[string]bool{}
	for _, other := range Capabilities() {
		reserved[other.SelectorEnv], reserved[other.TokenEnv] = true, true
	}
	clean := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !reserved[key] {
			clean = append(clean, entry)
		}
	}
	if token := c.selection(ctx).token; token != "" && !runcredential.Hosted() {
		clean = append(clean, c.TokenEnv+"="+token)
	}
	return clean
}
