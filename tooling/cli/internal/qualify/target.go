package qualify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	qualifyproto "go.putnami.dev/protocol/qualify"
)

// Target is what a contract runs against.
//
// Describe must answer before Open, without side effects: an unsupported
// contract is recorded without opening anything, and its verdict still names
// the target and the binding it would have proved. Close runs whenever Open was
// called, even when Open failed, so a partially opened target is released.
type Target interface {
	// Describe returns the target identity and the binding it promises. It is
	// read again after Open, which may have learned more (a composition id).
	Describe() (qualifyproto.Target, qualifyproto.Binding)
	// Open makes the target reachable and returns its base URL and the binding
	// it can prove. A *TargetError names the verdict state of a failure.
	Open(ctx context.Context) (baseURL string, binding qualifyproto.Binding, err error)
	// Close releases what Open acquired. A nil report means the target owned
	// nothing to release.
	Close(ctx context.Context) *qualifyproto.Cleanup
}

// WorktreeTarget is a Target whose binding is the worktree it serves (kind
// tree). The version-binding phase asks it to re-read that worktree once the
// workload is ready, and passes only when the tree is still the one the binding
// names. A tree-bound target that cannot re-read its tree fails that phase:
// a verdict never names a tree nobody checked.
type WorktreeTarget interface {
	Target
	// Worktree fingerprints the served worktree now and returns it as a tree
	// binding.
	Worktree(ctx context.Context) (qualifyproto.Binding, error)
}

// StartupTarget is a Target that observes its workload's process, and so can
// report when the workload's application completed its startup. The readiness
// phase waits for that report when the workload declares no readiness route
// (Options.ReadinessRoute is false), instead of polling a route the workload
// may not serve.
type StartupTarget interface {
	Target
	// AwaitStartup returns once the application reported completed startup. It
	// returns ctx's error when ctx is done first, and a *TargetError when the
	// workload can no longer report it (its process exited).
	AwaitStartup(ctx context.Context) error
}

// TargetError classifies a failed Open into a verdict state and a phase code.
type TargetError struct {
	// State is the resolve-target phase state.
	State qualifyproto.State
	// Code is the phase diagnostic code.
	Code string
	// Err is the cause; its message is recorded and must carry no secret.
	Err error
}

func (e *TargetError) Error() string { return e.Err.Error() }
func (e *TargetError) Unwrap() error { return e.Err }

// ProbeTimeout bounds the one HEAD request that classifies a URL target.
const ProbeTimeout = 5 * time.Second

// minExpectSHA is the shortest sha prefix that identifies a build.
const minExpectSHA = 7

var expectSHAPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ParseTargetURL accepts an absolute http or https URL. Credentials, a query
// and a fragment are refused: the URL is recorded in the verdict, and a base
// URL that carries a query cannot be joined with a route path.
func ParseTargetURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("--target %q is not a URL: %w", raw, err)
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return nil, fmt.Errorf("--target must be `local` or an http(s) URL, got %q", raw)
	case parsed.Host == "":
		return nil, fmt.Errorf("--target %q names no host", raw)
	case parsed.User != nil:
		return nil, fmt.Errorf("--target must not carry credentials: the URL is recorded in the verdict")
	case parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "":
		return nil, fmt.Errorf("--target %q must not carry a query or a fragment", raw)
	}
	return parsed, nil
}

// ValidateExpectSHA accepts a lowercase hexadecimal git sha of 7 to 64
// characters: the prefix /version must start with.
func ValidateExpectSHA(sha string) error {
	if !expectSHAPattern.MatchString(sha) {
		return fmt.Errorf("--expect-sha must be a lowercase hexadecimal git sha of at least %d characters, got %q", minExpectSHA, sha)
	}
	return nil
}

// urlTarget is a running deployment. It opens nothing: Open only proves an HTTP
// exchange completes, so a dead or misconfigured URL is target_unreachable
// rather than a readiness timeout.
type urlTarget struct {
	url       *url.URL
	expectSHA string
	client    *http.Client
}

// NewURLTarget returns the target for a deployment at rawURL that must report
// expectSHA on /version.
func NewURLTarget(rawURL, expectSHA string) (Target, error) {
	parsed, err := ParseTargetURL(rawURL)
	if err != nil {
		return nil, err
	}
	if err := ValidateExpectSHA(expectSHA); err != nil {
		return nil, err
	}
	return &urlTarget{url: parsed, expectSHA: expectSHA, client: newClient(ProbeTimeout)}, nil
}

func (t *urlTarget) Describe() (qualifyproto.Target, qualifyproto.Binding) {
	return qualifyproto.Target{Kind: qualifyproto.TargetURL, URL: t.url.String()},
		qualifyproto.Binding{Kind: qualifyproto.BindingArtifact, ExpectedSHA: t.expectSHA}
}

func (t *urlTarget) Open(ctx context.Context) (string, qualifyproto.Binding, error) {
	_, binding := t.Describe()
	request, err := newRequest(ctx, http.MethodHead, t.url.String())
	if err != nil {
		return "", binding, &TargetError{State: qualifyproto.StateTargetUnreachable, Code: qualifyproto.PhaseCodeTargetUnreachable, Err: err}
	}
	response, err := t.client.Do(request) //nolint:gosec // G704: the destination is the --target URL the user named, validated by ParseTargetURL; redirects are never followed
	if err != nil {
		if ctx.Err() != nil {
			return "", binding, ctx.Err()
		}
		return "", binding, &TargetError{
			State: qualifyproto.StateTargetUnreachable,
			Code:  qualifyproto.PhaseCodeTargetUnreachable,
			Err:   fmt.Errorf("no HTTP exchange with %s completed: %w", t.url.Redacted(), err),
		}
	}
	_ = drain(response)
	return t.url.String(), binding, nil
}

func (t *urlTarget) Close(context.Context) *qualifyproto.Cleanup {
	t.client.CloseIdleConnections()
	return nil
}

// newRequest builds every request qualify sends. This file is the package's
// one HTTP adapter, registered in tooling/cli/clientgen.external.json: the
// destination is always the target the user named, joined with a platform path
// or a derived route path, and never carries a body.
func newRequest(ctx context.Context, method, endpoint string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, endpoint, nil)
}

// newClient never follows a redirect: every request stays on the target, and
// a 3xx answer is itself below the 500 bar.
func newClient(timeout time.Duration) *http.Client {
	// A private transport, so CloseIdleConnections releases this run's
	// connections without touching the process-wide default.
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if defaults, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaults.Clone()
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// maxBody caps what a response may make the runner read.
const maxBody = 1 << 20

// drain reads at most maxBody bytes and closes the body, so the connection can
// be reused and a huge answer cannot stall the run.
func drain(response *http.Response) error {
	_, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxBody))
	closeErr := response.Body.Close()
	return errors.Join(err, closeErr)
}
