package credentialprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	distribution "go.putnami.dev/protocol/distribution"
	extproto "go.putnami.dev/protocol/extension"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/jobs/publication"
)

// DefaultReleaseTimeout bounds one release exchange. A release stores the set
// and advances every channel in one transaction, so it gets more time than
// the other ops.
const DefaultReleaseTimeout = 2 * time.Minute

// DefaultUploadTimeout is the base bound of one upload exchange. Each upload
// gets one more minute per uploadBytesPerMinute of the files its member
// declares (uploadTimeout).
const DefaultUploadTimeout = 10 * time.Minute

// uploadBytesPerMinute is the declared size that adds one minute to the bound
// of an upload: 256 MiB.
const uploadBytesPerMinute = 256 << 20

// UploaderRequiredError refuses a credential provider session that serves
// publication-v1 without uploader-v1. This CLI sends every publication upload
// to its credential provider, so the session cannot publish; upgrading the
// provider's extension fixes it. It is an invalid configuration
// (protocolcli.ErrInvalidConfig).
type UploaderRequiredError struct {
	// Extension names the extension that declares the provider; empty when
	// it is not known.
	Extension string
}

func (e *UploaderRequiredError) Error() string {
	provider, upgrade := "the credential provider", "the extension that declares it"
	if e.Extension != "" {
		provider, upgrade = "the credential provider of "+e.Extension, e.Extension
	}
	return fmt.Sprintf("%s serves %s without %s, and this CLI sends every publication upload to its credential provider: upgrade %s to a version that serves %s",
		provider, registry.CapabilityPublicationV1, registry.CapabilityUploaderV1, upgrade, registry.CapabilityUploaderV1)
}

// Is classifies the refusal as an invalid configuration.
func (e *UploaderRequiredError) Is(target error) bool {
	return target == protocolcli.ErrInvalidConfig
}

// Publication is the publication-v1 and uploader-v1 surface of one provider
// session (registry ADR 0003): resolve, open, upload and release. Every op
// goes to the session the handle was taken from. When that session ends,
// every later op fails: a provider started again has no plan open.
type Publication struct {
	session        *Session
	timeout        time.Duration
	releaseTimeout time.Duration
	uploadTimeout  time.Duration
}

// Publication returns the publication-v1 surface of the broker's provider. It
// returns nil and no error when the broker does not serve the publish
// purpose, when no provider serves this process, and when the provider's
// initialize answer did not echo publication-v1: the echo alone decides. A
// failed start is an error, so a run that enabled publish never falls back to
// another authority because its provider failed. A session that echoed
// publication-v1 without uploader-v1 is an *UploaderRequiredError.
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
	if err := b.requireUploader(session); err != nil {
		return nil, err
	}
	return &Publication{session: session, timeout: b.timeout, releaseTimeout: b.releaseTimeout, uploadTimeout: b.uploadTimeout}, nil
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

// Upload sends params, one packed member of the opened plan, to the session,
// and returns what the registry stores. The provider uploads with a
// credential it holds itself; the engine asks for none. The exchange is
// bounded by uploadTimeout and is not retried: an upload the provider did not
// answer may have reached the registry, so the session ends instead. A
// refusal is a *PublicationRefusalError.
func (p *Publication) Upload(ctx context.Context, params *registry.UploadParams) (*registry.UploadResult, error) {
	if params == nil {
		return nil, errors.New("credential provider upload: no member")
	}
	timeout := uploadTimeout(p.uploadTimeout, params.Member)
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := p.session.Upload(opCtx, params)
	if err != nil {
		return nil, p.endUnanswered(ctx, registry.CredentialOpUpload, err, timeout)
	}
	return result, nil
}

// uploadTimeout bounds the upload of member: base, plus one minute per whole
// uploadBytesPerMinute of the files member declares. An OCI member declares
// its layout directory and no file, so it gets base. The outbox contract
// bounds every declared size, so the sum cannot overflow.
func uploadTimeout(base time.Duration, member extproto.OutboxMember) time.Duration {
	var declared int64
	switch {
	case member.NPM != nil:
		declared = member.NPM.Tarball.Size + member.NPM.Manifest.Size
	case member.Go != nil:
		declared = member.Go.Zip.Size + member.Go.Mod.Size + member.Go.Info.Size
	case member.Put != nil:
		declared = member.Put.Manifest.Size
		for _, blob := range member.Put.Blobs {
			declared += blob.Size
		}
	}
	return base + time.Duration(declared/uploadBytesPerMinute)*time.Minute
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
// release-set path asks for (publication.InstallPublicationProvider), and
// returns the function that restores the previous source. A broker that does
// not serve the publish purpose installs nothing.
func (b *Broker) InstallPublication() (restore func()) {
	if !b.Enabled(registry.PurposePublish) {
		return func() {}
	}
	return publication.InstallPublicationProvider(func(ctx context.Context) (publication.PublicationProvider, error) {
		surface, err := b.Publication(ctx)
		if err != nil || surface == nil {
			return nil, err
		}
		return surface, nil
	})
}
