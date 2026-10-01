package workspace

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// The production ProbeProvider: one prepared extension runtime, asked once.
//
// It is deliberately the only place in core that knows a provider is a process.
// RunProbe, the merge, the digest and the snapshot all speak ProbeProvider, so
// the fixture providers the conformance suite uses and the real extension
// runtimes travel the same code path — which is what makes "one batched request
// per extension" checkable without spawning anything.
//
// Every failure here is CLASSIFIED before it leaves. A provider that could not
// be started is not the same problem as a provider that answered with garbage,
// and the recovery a user needs differs: the first is "install/repair the
// extension", the second is "the extension has a bug". Flattening both into an
// error string is what makes a broken workspace unrecoverable.

// DefaultProbeTimeout bounds one provider's answer.
//
// It is generous relative to the measured ~0.18 s fixed process cost
// because a cold provider may read thousands of manifests, and stingy relative
// to a human's patience because an unbounded probe turns every command into a
// hang. A timeout is reported as ProbeFailureTimeout, never as a silently empty
// result: an empty answer drops dependency edges, and a dropped edge is a wrong
// build rather than a slow one.
const DefaultProbeTimeout = 60 * time.Second

// ExecProbeProvider runs one extension executable as a metadata provider.
type ExecProbeProvider struct {
	// Extension is the provider's extension name. It must match the name the
	// answer is attributed to; RunProbe rejects a misrouted result.
	Extension string
	// Executable is the resolved runtime executable.
	Executable string
	// Resolve produces the executable on FIRST probe, when Executable is empty.
	//
	// It is what makes preparation lazy: a workspace whose snapshot validates
	// never calls it, so it never compiles a local extension only to discover
	// nothing changed. Preparation errors travel out unchanged — the runtime
	// primitive already classifies them, and "prepare failed" and "no runtime
	// declared" have different remedies.
	Resolve func() (string, error)
	// Args is the argv tail. Empty means the reserved control invocation.
	Args []string
	// Dir is the process working directory. It must be the workspace root:
	// every path in the exchange is repo-relative, so a provider that resolves
	// them against a different directory answers about a different tree.
	Dir string
	// Env is the child environment. Nil inherits the parent's.
	Env []string
	// Timeout bounds one answer. Zero means DefaultProbeTimeout.
	Timeout time.Duration
	// Context scopes the run. Nil means context.Background().
	Context context.Context
	// FromArtifactStore says the provider's extension is installed from the
	// artifact store (extension.InArtifactStore): its probe runs registry
	// code. Any other provider, a local extension first of all, runs
	// repository code, and Probe records that before the spawn
	// (runcredential.MarkRepositoryCodeStarted).
	FromArtifactStore bool
}

// Name is the provider's extension name.
func (p *ExecProbeProvider) Name() string { return p.Extension }

// Probe spawns the provider, writes one request, and reads one result.
func (p *ExecProbeProvider) Probe(request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	executable, err := p.executable()
	if err != nil {
		return wsproto.ProbeResult{}, err
	}

	var payload bytes.Buffer
	if err := wsproto.EncodeProbeRequest(&payload, request); err != nil {
		return wsproto.ProbeResult{}, wsproto.NewProbeFailure(wsproto.ProbeFailureTransport, p.Extension, "%v", err)
	}

	parent := p.Context
	if parent == nil {
		parent = context.Background()
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	args := p.Args
	if len(args) == 0 {
		args = wsproto.ProbeControlArgs()
	}
	if !p.FromArtifactStore {
		runcredential.MarkRepositoryCodeStarted("workspace probe of " + p.Extension)
	}
	cmd := exec.CommandContext(ctx, executable, args...) //nolint:gosec // executable is a resolved extension runtime
	cmd.Dir = p.Dir
	cmd.Env = p.Env
	cmd.Stdin = &payload

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// The deadline is checked first: a killed process reports a generic
		// signal error, and reporting that instead of the timeout would send
		// the user looking for a crash that never happened.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return wsproto.ProbeResult{}, wsproto.NewProbeFailure(wsproto.ProbeFailureTimeout, p.Extension,
				"probe did not answer within %s", timeout)
		}
		// "The provider could not be STARTED" and "the provider ran and failed"
		// need different remedies — repair the extension versus fix its bug —
		// so they are classified apart. A missing or non-executable file is
		// reported by exec as a lookup error for a bare name and as a path
		// error for a resolved path; both mean the same thing here.
		var notFound *exec.Error
		if errors.As(err, &notFound) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return wsproto.ProbeResult{}, wsproto.NewProbeFailure(wsproto.ProbeFailureUnavailable, p.Extension,
				"could not start %s: %v", executable, err)
		}
		return wsproto.ProbeResult{}, wsproto.NewProbeFailure(wsproto.ProbeFailureTransport, p.Extension,
			"probe exited with an error: %v%s", err, formatProviderStderr(stderr.String()))
	}

	data, err := wsproto.DecodeProbeDocument(&stdout)
	if err != nil {
		return wsproto.ProbeResult{}, wsproto.NewProbeFailure(wsproto.ProbeFailureTransport, p.Extension,
			"%v%s", err, formatProviderStderr(stderr.String()))
	}

	result, diags := wsproto.ParseAndValidateProbeResult(data)
	if result == nil || diag.HasErrors(diags) {
		failure := wsproto.NewProbeFailure(wsproto.ProbeFailureInvalidResult, p.Extension,
			"probe answer rejected by the v%d contract", wsproto.ProbeProtocolVersion)
		failure.Diagnostics = diag.Errors(diags)
		return wsproto.ProbeResult{}, failure
	}
	return *result, nil
}

// executable resolves the provider's runtime, preparing it on first use.
// A resolution failure is classified as provider-unavailable unless it already
// carries a probe classification of its own.
func (p *ExecProbeProvider) executable() (string, error) {
	if resolved := strings.TrimSpace(p.Executable); resolved != "" {
		return resolved, nil
	}
	if p.Resolve == nil {
		return "", wsproto.NewProbeFailure(wsproto.ProbeFailureUnavailable, p.Extension,
			"no prepared runtime executable is resolved for this provider")
	}
	resolved, err := p.Resolve()
	if err != nil {
		var failure *wsproto.ProbeFailure
		if errors.As(err, &failure) {
			return "", failure
		}
		return "", wsproto.NewProbeFailure(wsproto.ProbeFailureUnavailable, p.Extension,
			"could not prepare the provider runtime: %v", err)
	}
	if strings.TrimSpace(resolved) == "" {
		return "", wsproto.NewProbeFailure(wsproto.ProbeFailureUnavailable, p.Extension,
			"runtime preparation produced no executable")
	}
	p.Executable = resolved
	return resolved, nil
}

// formatProviderStderr appends a provider's own diagnostics to a failure
// message, trimmed and bounded. The provider's stderr is usually the ACTUAL
// cause ("cannot find module", "permission denied"); dropping it leaves the
// user with an exit status.
func formatProviderStderr(stderr string) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return ""
	}
	const maxStderr = 4096
	if len(trimmed) > maxStderr {
		trimmed = trimmed[:maxStderr] + "…"
	}
	return ": " + trimmed
}
