package credentialprovider

import (
	"context"
	"fmt"
	"io"
	"strings"

	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// ProvidersEnv names the invocation providers when --providers is absent. The
// CLI reads it once per process and removes it from its own environment, so
// no process it starts inherits the choice; the provider launch strips it
// too.
const ProvidersEnv = jobs.ProvidersEnv

// New returns the broker for the purposes providers enable, served by the one
// loaded extension that declares registry.CredentialProviderCommand. source
// names where providers came from, for the errors a consumer sees. New
// launches nothing: the provider starts on the first credential a consumer
// asks for, or at Broker.Start. On a hosted run no provider starts once this
// process started repository code (runcredential.StartHolder): the start
// fails instead.
//
// It returns a nil broker when providers enable no purpose or when no loaded
// extension declares the command; every consumer then behaves as without a
// provider. When several extensions declare it, the broker fails the first
// credential a consumer asks for with an error wrapping
// extension.ErrProviderAmbiguous, so a command that needs no credential still
// runs.
func New(workspaceRoot string, extensions []*extension.ExtensionDescription, providers []string, source string, stderr io.Writer, options ...Option) *Broker {
	purposes := Purposes(providers)
	if len(purposes) == 0 {
		return nil
	}
	options = append([]Option{WithSource(source)}, options...)
	provider, err := extension.ResolveReservedProvider(extensions, registry.CredentialProviderCommand)
	if err != nil {
		return NewBroker(purposes, func(context.Context) (*Session, error) {
			return nil, fmt.Errorf("credential provider: %w", err)
		}, options...)
	}
	if provider == nil {
		return nil
	}
	// The opener runs after NewBroker returns, so it reads the options'
	// run credential from the broker it serves.
	var broker *Broker
	open := func(ctx context.Context) (*Session, error) {
		ext := extension.FindExtensionByName(extensions, provider.ExtensionName)
		if ext == nil {
			return nil, fmt.Errorf("credential provider extension %q was not loaded", provider.ExtensionName)
		}
		launch, err := jobs.PrepareProviderLaunch(ctx, workspaceRoot, ext, provider.Command)
		if err != nil {
			return nil, fmt.Errorf("prepare credential provider %s: %w", provider.ExtensionName, err)
		}
		// The provider receives the run credential of a hosted run, so it
		// starts only before repository code, and only as its extension's
		// native runtime.
		holder := "the credential provider of " + provider.ExtensionName
		var session *Session
		err = runcredential.StartHolder(holder, func() error {
			if err := runcredential.RequireNativeHolder(holder, launch.Command, ext.RuntimeExecutable); err != nil {
				return err
			}
			var spawnErr error
			session, spawnErr = Spawn(ctx, LaunchSpec{Command: launch.Command, Args: launch.Args, Dir: launch.Dir, Env: withoutEnv(launch.Env, ProvidersEnv)}, stderr)
			return spawnErr
		})
		if err != nil {
			return nil, err
		}
		initCtx, cancel := context.WithTimeout(ctx, DefaultOpTimeout)
		defer cancel()
		if _, err := session.Initialize(initCtx, broker.runCredential); err != nil {
			_ = session.Close()
			return nil, fmt.Errorf("credential provider %s: %w", provider.ExtensionName, err)
		}
		return session, nil
	}
	broker = NewBroker(purposes, open, options...)
	return broker
}

func withoutEnv(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, name+"=") {
			out = append(out, entry)
		}
	}
	return out
}
