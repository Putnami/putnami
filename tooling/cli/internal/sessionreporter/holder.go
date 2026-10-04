package sessionreporter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// errReporterStart is the core-owned reason a reporter process did not start.
var errReporterStart = errors.New("reporter process could not start")

// noCredentialProtocolError reports a reporter that a hosted run starts
// without the run credential, because it did not accept initialize of
// protocol protocolcli.SessionReportingCredentialVersion: a v1 reporter, or
// one that refused the handshake. It never falls back to an environment
// variable.
type noCredentialProtocolError struct {
	// Holder names the reporter process.
	Holder string
}

func (e *noCredentialProtocolError) Error() string {
	return fmt.Sprintf("%s: %s cannot hold the run credential: it did not accept initialize of session reporting protocol %d",
		runcredential.Flag, e.Holder, protocolcli.SessionReportingCredentialVersion)
}

// credentialRefusedError reports a reporter that refused the run credential.
// Code is the machine code of its answer, never its own words.
type credentialRefusedError struct {
	Holder string
	Code   string
}

func (e *credentialRefusedError) Error() string {
	return fmt.Sprintf("%s: %s refused the run credential: %s", runcredential.Flag, e.Holder, e.Code)
}

// startsWithoutCredential reports whether err keeps a reporter from holding
// the run credential for the whole run, so that it starts without one: its
// command is not its extension's native runtime, or it does not speak the
// handshake. Every other error is a failure to start.
func startsWithoutCredential(err error) bool {
	return errors.As(err, new(*runcredential.NativeHolderError)) || errors.As(err, new(*noCredentialProtocolError))
}

// holderName names the process of capability that provider serves, in a
// custody diagnostic.
func (c Capability) holderName(provider string) string {
	return "the " + c.Label + " of " + provider
}

// startHolder starts the reporter process holder names and hands it the run
// credential over the v2 handshake, each line bounded by timeout. The process
// starts through runcredential.StartHolder, so a hosted run refuses it once
// repository code ran (*runcredential.CustodyError), and only as its
// extension's native runtime (runcredential.RequireNativeHolder). It then
// receives initialize, which carries no credential, and only after it accepts
// initialize, authenticate. A reporter that does not accept initialize is
// closed with a *noCredentialProtocolError and one that refuses authenticate
// with a *credentialRefusedError. It requires a run credential.
func startHolder(ctx context.Context, holder string, launch LaunchSpec, timeout time.Duration) (*process, error) {
	bearer, ok := runcredential.Current()
	if !ok {
		return nil, fmt.Errorf("%s: no run credential to hand", holder)
	}
	var p *process
	err := runcredential.StartHolder(holder, func() error {
		if err := runcredential.RequireNativeHolder(holder, launch.Command, launch.Runtime); err != nil {
			return err
		}
		spawned, err := spawn(launch)
		if err != nil {
			return errReporterStart
		}
		p = spawned
		return nil
	})
	if err != nil {
		return nil, err
	}
	call := func(line protocolcli.SessionReportingHandshake) (*protocolcli.SessionReportingHandshakeResult, error) {
		opCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return p.handshake(opCtx, line)
	}
	if result, err := call(protocolcli.NewSessionReportingInitialize()); err != nil || !result.OK {
		p.close()
		return nil, &noCredentialProtocolError{Holder: holder}
	}
	result, err := call(protocolcli.NewSessionReportingAuthenticate(bearer))
	if err != nil {
		p.close()
		return nil, fmt.Errorf("%s: %s did not answer authenticate: %w", runcredential.Flag, holder, err)
	}
	if !result.OK {
		p.close()
		return nil, &credentialRefusedError{Holder: holder, Code: result.Code}
	}
	return p, nil
}

// Holders are the reporter processes a hosted run starts before its first
// repository code, so that each can hold the run credential: a hosted run
// hands its credential to no process started after repository code
// (runcredential.StartHolder), and a reporter starts lazily, at its first
// chunk, after the hooks and the first jobs started. Start starts them once,
// and the session's Run of each capability adopts its process (StartSelected).
// The zero value is ready to use. Whoever creates it closes it, after the last
// Run that adopts from it started.
type Holders struct {
	mu      sync.Mutex
	started bool
	held    map[string]heldReporter
}

// heldReporter is what Start left for one capability: the process that holds
// the run credential, or plain when the reporter starts without one.
type heldReporter struct {
	provider string
	process  *process
	plain    bool
}

// Start starts, once, the reporter process of every capability in
// capabilities, as a holder (startHolder). It returns, with the reason, each
// resolved reporter that does not hold the credential. One that cannot hold it
// (startsWithoutCredential) starts later without one, as its Run's plain
// process. Any other failure, an unresolved reporter included, leaves the
// start to the Run, which custody then refuses once repository code ran.
// Without a run credential, and after the first call or Close, it starts
// nothing and returns nil.
func (h *Holders) Start(ctx context.Context, capabilities []Capability, resolve Resolver) []Failure {
	if h == nil || !runcredential.Hosted() {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return nil
	}
	h.started = true
	h.held = map[string]heldReporter{}
	var failures []Failure
	for _, capability := range capabilities {
		provider := capability.Provider(ctx)
		if provider == "" {
			continue
		}
		setupCtx, cancel := context.WithTimeout(ctx, providerSetupTimeout)
		launch, err := resolve(capability)(setupCtx)
		cancel()
		if err != nil {
			// The Run resolves the reporter again and reports why it is
			// unavailable.
			continue
		}
		launch.Env = capability.providerEnv(ctx, launch.Env)
		p, err := startHolder(ctx, capability.holderName(provider), launch, operationTimeout)
		switch {
		case err == nil:
			h.held[capability.Name] = heldReporter{provider: provider, process: p}
		case startsWithoutCredential(err):
			h.held[capability.Name] = heldReporter{provider: provider, plain: true}
			failures = append(failures, Failure{capability, fmt.Errorf("%w; it starts without a credential", err)})
		default:
			failures = append(failures, Failure{capability, err})
		}
	}
	return failures
}

// take hands the Run of capability, served by provider, what Start left for
// it: the process that holds the credential, or plain. A process is handed
// once; Close no longer owns it.
func (h *Holders) take(capability Capability, provider string) (p *process, plain bool) {
	if h == nil {
		return nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	held, ok := h.held[capability.Name]
	if !ok || held.provider != provider {
		return nil, false
	}
	delete(h.held, capability.Name)
	return held.process, held.plain
}

// Close closes every process no Run adopted. A later Start starts nothing. A
// nil Holders does nothing.
func (h *Holders) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = true
	for name, held := range h.held {
		if held.process != nil {
			held.process.close()
		}
		delete(h.held, name)
	}
}
