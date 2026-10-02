package credentialprovider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	distribution "go.putnami.dev/protocol/distribution"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// DefaultReleaseTimeout bounds one release exchange. A release stores the set
// and advances every channel in one transaction, so it gets more time than
// the other ops.
const DefaultReleaseTimeout = 2 * time.Minute

// Publication is the publication-v1 surface of one provider session
// (registry ADR 0003): resolve, open, the publish credential, and release.
// Every op goes to the session the handle was taken from. When that session
// ends, every later op fails: a provider started again has no plan open.
type Publication struct {
	broker         *Broker
	session        *Session
	timeout        time.Duration
	releaseTimeout time.Duration
}

// Publication returns the publication-v1 surface of the broker's provider. It
// returns nil and no error when the broker does not serve the publish
// purpose, when no provider serves this process, and when the provider's
// initialize answer did not echo publication-v1: the echo alone decides. A
// failed start is an error, so a run that enabled publish never falls back to
// another authority because its provider failed.
func (b *Broker) Publication(ctx context.Context) (*Publication, error) {
	if !b.Enabled(registry.PurposePublish) {
		return nil, nil
	}
	session, err := b.sessionFor(ctx)
	if err != nil {
		if b.source != "" {
			err = fmt.Errorf("%s: %w", b.source, err)
		}
		return nil, fmt.Errorf("credential provider: %w", err)
	}
	if session == nil || !session.Publication() {
		return nil, nil
	}
	return &Publication{broker: b, session: session, timeout: b.timeout, releaseTimeout: b.releaseTimeout}, nil
}

// Resolve asks the session for the heads of request's channels, within the op
// timeout. It is not retried.
func (p *Publication) Resolve(ctx context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
	opCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	response, err := p.session.Resolve(opCtx, request)
	if err != nil {
		return nil, p.endUnanswered(ctx, registry.CredentialOpResolve, err, p.timeout)
	}
	return response, nil
}

// Open opens params.Plan. A request the provider did not answer within the op
// timeout is sent once more on the same session; the provider answers a
// second open of the same plan as the first (registry ADR 0003). An answer,
// a refusal included, is final.
func (p *Publication) Open(ctx context.Context, params *registry.OpenParams) error {
	_, err := retryUnanswered(ctx, p, registry.CredentialOpOpen, p.timeout, func(opCtx context.Context) (struct{}, error) {
		return struct{}{}, p.session.Open(opCtx, params)
	})
	if err != nil {
		return fmt.Errorf("open plan %s: %w", planDigestOf(params), err)
	}
	return nil
}

// Release releases the opened plan. A request the provider did not answer
// within the release timeout is sent once more on the same session; the
// provider releases at most once per planDigest and answers a repeated
// release with its first answer. An answer, a refusal included, is final.
// When the second attempt is not answered either, the outcome is unknown: the
// error names the planDigest, and the session ends.
func (p *Publication) Release(ctx context.Context, params *registry.ReleaseParams) (*distribution.ReleaseResponse, error) {
	digest := ""
	if params != nil {
		digest = params.PlanDigest
	}
	response, err := retryUnanswered(ctx, p, registry.CredentialOpRelease, p.releaseTimeout, func(opCtx context.Context) (*distribution.ReleaseResponse, error) {
		return p.session.Release(opCtx, params)
	})
	if err != nil {
		var refusal *PublicationRefusalError
		if errors.As(err, &refusal) {
			return nil, fmt.Errorf("release plan %s: %w", digest, err)
		}
		return nil, fmt.Errorf("release plan %s: the outcome is unknown; read the channel heads before publishing again: %w", digest, err)
	}
	return response, nil
}

// PublishBearer returns the publish bearer for target. The credential is the
// broker's publish credential, which the provider issues only once a plan is
// open. A credential that does not name target's host and port is an error,
// as is a refusal: no upload falls back to another credential.
func (p *Publication) PublishBearer(ctx context.Context, target *url.URL) (string, error) {
	if p.session.ended() {
		return "", fmt.Errorf("the credential provider session that opened the plan ended: %w", p.session.deathErr())
	}
	bearer, served, err := p.broker.Bearer(ctx, registry.PurposePublish, target)
	if err != nil {
		return "", err
	}
	if !served {
		return "", fmt.Errorf("the credential provider holds no publish credential for %s", target.Host)
	}
	return bearer, nil
}

// retryUnanswered runs attempt within timeout, and once more when the first
// attempt was not answered: its own deadline expired while ctx and the
// session are still live. An answer, a refusal or any other error is final.
func retryUnanswered[R any](
	ctx context.Context,
	p *Publication,
	op registry.CredentialOp,
	timeout time.Duration,
	attempt func(context.Context) (R, error),
) (R, error) {
	var result R
	var err error
	for try := 1; try <= 2; try++ {
		opCtx, cancel := context.WithTimeout(ctx, timeout)
		result, err = attempt(opCtx)
		cancel()
		if !unanswered(ctx, p.session, err) {
			return result, err
		}
	}
	return result, p.endUnanswered(ctx, op, err, timeout)
}

// unanswered reports whether err is an op deadline that expired while ctx and
// the session were still live: the request may or may not have been applied.
func unanswered(ctx context.Context, session *Session, err error) bool {
	return err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil && !session.ended()
}

// endUnanswered ends the session when op was not answered within timeout while
// the caller still waited, and returns err. A provider that overstays an op
// timeout is not trusted with the next one.
func (p *Publication) endUnanswered(ctx context.Context, op registry.CredentialOp, err error, timeout time.Duration) error {
	if unanswered(ctx, p.session, err) {
		p.session.markDead(fmt.Errorf("the credential provider did not answer %s within %s", op, timeout))
		return fmt.Errorf("the credential provider did not answer %s within %s: %w", op, timeout, err)
	}
	return err
}

func planDigestOf(params *registry.OpenParams) string {
	if params == nil {
		return ""
	}
	return params.Plan.PlanDigest
}

// InstallPublication makes the broker's publication surface the one the
// release-set path asks for (jobs.InstallPublicationProvider), and returns the
// function that restores the previous source. A broker that does not serve
// the publish purpose installs nothing.
func (b *Broker) InstallPublication() (restore func()) {
	if !b.Enabled(registry.PurposePublish) {
		return func() {}
	}
	return jobs.InstallPublicationProvider(func(ctx context.Context) (jobs.PublicationProvider, error) {
		publication, err := b.Publication(ctx)
		if err != nil || publication == nil {
			return nil, err
		}
		return publication, nil
	})
}
