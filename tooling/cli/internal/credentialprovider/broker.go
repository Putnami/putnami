package credentialprovider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// DefaultOpTimeout bounds one initialize or credential exchange. A provider
// that does not answer within it fails the work that needed the credential.
const DefaultOpTimeout = 30 * time.Second

// FailureBackoff is how long a provider failure answers for its purpose: a
// crash, a timeout, a malformed answer or a failed start. The first consumer
// after it asks the provider again, starting it again when it exited. Absence
// and refusal hold for the process.
const FailureBackoff = 30 * time.Second

// maxRefreshMargin is the longest a credential is renewed before it expires.
// The margin is at most half the lifetime left when the credential arrived,
// so a freshly issued credential is always used at least once.
const maxRefreshMargin = time.Minute

// Purposes maps invocation providers to the credential purposes they enable:
// install enables read, publish enables publish. The result is sorted and
// unique; an unknown provider enables nothing.
func Purposes(providers []string) []string {
	var purposes []string
	for _, provider := range providers {
		switch provider {
		case runner.InvocationProviderInstall:
			purposes = append(purposes, registry.PurposeRead)
		case runner.InvocationProviderPublish:
			purposes = append(purposes, registry.PurposePublish)
		}
	}
	slices.Sort(purposes)
	return slices.Compact(purposes)
}

// Opener starts the provider session. A nil session with a nil error means no
// provider serves this invocation.
type Opener func(ctx context.Context) (*Session, error)

// Broker serves bearers for the purposes a process enabled. It starts the
// provider on the first credential a consumer asks for, and asks it at most
// once per purpose while the answer it holds is valid: absence and refusal
// hold for the rest of the process, a credential until its refresh instant,
// and a provider failure for FailureBackoff. Concurrent consumers of one
// purpose share one exchange. A Broker is safe for concurrent use.
type Broker struct {
	purposes map[string]*purposeCache
	open     Opener
	now      func() time.Time
	timeout  time.Duration
	source   string
	// runCredential is the hosted run's bearer that New's opener hands to the
	// provider at initialize; empty without one. A Broker is never formatted.
	runCredential string

	// connect admits one opener; it also serializes Close with the open.
	connect chan struct{}
	closed  bool
	// absent records that no provider serves this process; it holds for the
	// process.
	absent bool
	// session is the last provider started, live or ended; startedAt is when
	// the last start began and openErr its failure.
	session   *Session
	startedAt time.Time
	openErr   error
}

// purposeCache is one purpose's answer. gate admits one exchange at a time.
type purposeCache struct {
	gate       chan struct{}
	answered   bool
	credential *registry.Credential
	err        error
	// until ends the answer: a credential's refresh instant, or the end of the
	// backoff after a failure. It is zero for absence and refusal, which hold
	// for the process.
	until time.Time
}

// Option adjusts a Broker.
type Option func(*Broker)

// WithClock replaces the clock the broker reads expiry against.
func WithClock(now func() time.Time) Option { return func(b *Broker) { b.now = now } }

// WithOpTimeout replaces DefaultOpTimeout.
func WithOpTimeout(timeout time.Duration) Option { return func(b *Broker) { b.timeout = timeout } }

// WithSource names where the enabled purposes came from — `--providers`,
// PUTNAMI_PROVIDERS or the execution request — in every error Bearer returns.
func WithSource(source string) Option { return func(b *Broker) { b.source = source } }

// WithRunCredential hands bearer, the hosted run's credential, to the provider
// New starts, in its initialize request. The provider's launch environment
// does not change. An empty bearer is the same as no option.
func WithRunCredential(bearer string) Option {
	return func(b *Broker) { b.runCredential = bearer }
}

// NewBroker returns a broker for purposes that opens its provider with open.
// A purpose outside purposes is never asked for.
func NewBroker(purposes []string, open Opener, options ...Option) *Broker {
	broker := &Broker{
		purposes: make(map[string]*purposeCache, len(purposes)),
		open:     open,
		now:      time.Now,
		timeout:  DefaultOpTimeout,
		connect:  make(chan struct{}, 1),
	}
	for _, purpose := range purposes {
		if registry.ValidPurpose(purpose) {
			broker.purposes[purpose] = &purposeCache{gate: make(chan struct{}, 1)}
		}
	}
	for _, option := range options {
		option(broker)
	}
	return broker
}

// Enabled reports whether the broker serves purpose.
func (b *Broker) Enabled(purpose string) bool {
	return b != nil && b.purposes[purpose] != nil
}

// Bearer returns the purpose's bearer when its credential names target's host
// and port. served is false when the purpose is not enabled, when the
// provider holds no credential for it, and when the credential names other
// hosts; the caller then authenticates as it would without a provider. An
// error is a refusal (*RefusalError) or a provider failure, and the caller
// must not fall back to another credential. A refusal fails every target of
// its purpose, whatever its host: it carries no hosts.
func (b *Broker) Bearer(ctx context.Context, purpose string, target *url.URL) (bearer string, served bool, err error) {
	if !b.Enabled(purpose) {
		return "", false, nil
	}
	credential, err := b.credential(ctx, purpose)
	if err != nil && b.source != "" {
		err = fmt.Errorf("%s: %w", b.source, err)
	}
	if err != nil || credential == nil {
		return "", false, err
	}
	if !credential.Serves(target) {
		return "", false, nil
	}
	return credential.Bearer, true, nil
}

// credential returns the cached answer for purpose, asking the provider when
// there is none or when the cached credential reached its refresh instant.
func (b *Broker) credential(ctx context.Context, purpose string) (*registry.Credential, error) {
	cache := b.purposes[purpose]
	select {
	case cache.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-cache.gate }()
	if cache.answered && (cache.until.IsZero() || b.now().Before(cache.until)) {
		return cache.credential, cache.err
	}
	credential, refreshAt, err := b.fetch(ctx, purpose)
	if err != nil && ctx.Err() != nil {
		// The caller gave up; the cache keeps its previous answer and the next
		// caller asks again.
		return nil, err
	}
	var refusal *RefusalError
	cache.answered, cache.credential, cache.err = true, credential, err
	switch {
	case credential != nil:
		cache.until = refreshAt
	case err == nil, errors.As(err, &refusal):
		cache.until = time.Time{}
	default:
		cache.until = b.now().Add(FailureBackoff)
	}
	return credential, err
}

func (b *Broker) fetch(ctx context.Context, purpose string) (*registry.Credential, time.Time, error) {
	session, err := b.sessionFor(ctx)
	if err != nil || session == nil {
		return nil, time.Time{}, err
	}
	opCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	credential, err := session.Credential(opCtx, purpose)
	if err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		// The provider overstayed the op timeout while the caller still
		// waited: the session ends, so the first start after FailureBackoff
		// replaces the hung provider.
		session.markDead(fmt.Errorf("the credential provider did not answer a %s credential within %s", purpose, b.timeout))
	}
	if err != nil {
		var refusal *RefusalError
		if errors.As(err, &refusal) {
			return nil, time.Time{}, err
		}
		return nil, time.Time{}, fmt.Errorf("credential provider %s credential: %w", purpose, err)
	}
	if credential == nil {
		return nil, time.Time{}, nil
	}
	expiry, err := credential.Expiry()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("credential provider %s credential: %w", purpose, err)
	}
	now := b.now()
	lifetime := expiry.Sub(now)
	if lifetime <= 0 {
		return nil, time.Time{}, fmt.Errorf("the credential provider issued a %s credential that expired at %s", purpose, credential.ExpiresAt)
	}
	return credential, expiry.Add(-min(maxRefreshMargin, lifetime/2)), nil
}

// sessionFor returns the live provider session, starting the provider when
// none is live. No provider holds for the process. The provider starts at
// most once per FailureBackoff: until FailureBackoff after the last start
// began, a failed start answers its error and an ended session its end. The
// first caller after that closes the ended session and starts the provider
// again.
func (b *Broker) sessionFor(ctx context.Context) (*Session, error) {
	select {
	case b.connect <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-b.connect }()
	if b.closed {
		return nil, errors.New("the credential provider is closed")
	}
	if b.absent {
		return nil, nil
	}
	if b.session != nil && !b.session.ended() {
		return b.session, nil
	}
	if !b.startedAt.IsZero() && b.now().Before(b.startedAt.Add(FailureBackoff)) {
		if b.session != nil {
			return nil, b.session.deathErr()
		}
		return nil, b.openErr
	}
	if b.session != nil {
		_ = b.session.Close()
		b.session = nil
	}
	startedAt := b.now()
	session, err := b.open(ctx)
	if err != nil && ctx.Err() != nil {
		// The caller gave up; the next caller starts the provider.
		return nil, err
	}
	b.startedAt, b.session, b.openErr = startedAt, session, err
	b.absent = session == nil && err == nil
	return session, err
}

// Start starts the provider now rather than at the first credential a
// consumer asks for, and returns the error of that start, which the first
// consumers within FailureBackoff also receive. A broker without a provider
// starts nothing. A hosted run starts its provider before any repository code,
// after which no provider may start (runcredential.StartHolder).
func (b *Broker) Start(ctx context.Context) error {
	if b == nil {
		return nil
	}
	_, err := b.sessionFor(ctx)
	return err
}

// Close shuts the provider down when it was started. Every later Bearer for
// an enabled purpose whose answer is not cached fails.
func (b *Broker) Close() error {
	if b == nil {
		return nil
	}
	b.connect <- struct{}{}
	defer func() { <-b.connect }()
	b.closed = true
	if b.session == nil {
		return nil
	}
	session := b.session
	b.session = nil
	return session.Close()
}

// InstallRead makes the broker's read credential the first credential source
// of every registry download of this process, and returns the function that
// restores the previous source. A broker that does not serve the read purpose
// installs nothing.
func (b *Broker) InstallRead() (restore func()) {
	if !b.Enabled(registry.PurposeRead) {
		return func() {}
	}
	return extension.InstallRegistryReadCredential(func(ctx context.Context, target *url.URL) (string, bool, error) {
		return b.Bearer(ctx, registry.PurposeRead, target)
	})
}
