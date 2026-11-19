package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultRemoteSourceTimeout     = 5 * time.Second
	defaultRemoteSourceRetryBudget = 30 * time.Second
	remoteRetryInitialDelay        = 100 * time.Millisecond
	remoteRetryMaxDelay            = 2 * time.Second
)

type remoteAttemptFailure struct {
	message   string
	err       error
	retryable bool
}

func retryableRemoteFailure(message string, err error) *remoteAttemptFailure {
	return &remoteAttemptFailure{message: message, err: err, retryable: true}
}

func terminalRemoteFailure(message string, err error) *remoteAttemptFailure {
	return &remoteAttemptFailure{message: message, err: err}
}

func remoteRetryBudget(required bool, configured time.Duration) time.Duration {
	if !required {
		return 0
	}
	if configured < 0 {
		return 0
	}
	if configured == 0 {
		return defaultRemoteSourceRetryBudget
	}
	return configured
}

func remoteRetryDelay(failedAttempts int) time.Duration {
	if failedAttempts <= 1 {
		return remoteRetryInitialDelay
	}
	delay := remoteRetryInitialDelay
	for i := 1; i < failedAttempts; i++ {
		delay *= 2
		if delay >= remoteRetryMaxDelay {
			return remoteRetryMaxDelay
		}
	}
	return delay
}

// waitRemoteRetry sleeps for delay and reports false when ctx ends first.
func waitRemoteRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// remoteLoadCanceled is the error a remote source returns when its caller's
// context ends. It never goes through an optional source's local fallback: a
// canceled boot has no configuration, not an empty one.
func remoteLoadCanceled(source string, err error) error {
	return fmt.Errorf("%s load canceled: %w", source, err)
}

// remoteRefresh fetches a source's tree once, with no retry and no fallback.
type remoteRefresh func(ctx context.Context) (map[string]any, *remoteAttemptFailure)

// remoteLoad shares one completed remote load with every caller. A load that
// its caller's cancellation cut short is not a result: the next caller fetches
// again with its own context instead of inheriting the canceled error.
//
// With a positive period and a refresh function, a successful load older than
// the period is still returned at once, and it starts one background refresh
// that every caller shares. A refresh that answers a tree replaces the held
// tree; a refresh that fails, or answers none, keeps it, logs a warning for each new failure, and the
// next refresh starts once another period has passed.
type remoteLoad struct {
	// period is how long a completed load stays current. Zero keeps the first
	// completed load for the life of the process.
	period time.Duration
	// refresh fetches the tree in the background once period has passed.
	refresh remoteRefresh
	// source names the source in the refresh warnings.
	source string
	// timeout bounds one background refresh.
	timeout time.Duration
	// now returns the current time; nil means time.Now.
	now func() time.Time

	mu         sync.Mutex
	done       bool
	cached     map[string]any
	err        error
	inflight   chan struct{}
	loadedAt   time.Time
	refreshing chan struct{}
	refreshErr string
}

func (l *remoteLoad) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *remoteLoad) load(ctx context.Context, fetch func(context.Context) (map[string]any, error)) (map[string]any, error) {
	for {
		l.mu.Lock()
		if l.done {
			cached, err := l.cached, l.err
			if err == nil {
				l.startRefreshLocked()
			}
			l.mu.Unlock()
			return cached, err
		}
		if wait := l.inflight; wait != nil {
			l.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return nil, err
		}
		wait := make(chan struct{})
		l.inflight = wait
		l.mu.Unlock()
		return l.run(ctx, fetch, wait)
	}
}

func (l *remoteLoad) run(ctx context.Context, fetch func(context.Context) (map[string]any, error), wait chan struct{}) (data map[string]any, err error) {
	defer func() {
		l.mu.Lock()
		l.inflight = nil
		if err == nil || ctx.Err() == nil {
			l.done, l.cached, l.err = true, data, err
			l.loadedAt = l.clock()
		}
		l.mu.Unlock()
		close(wait)
	}()
	return fetch(ctx)
}

// startRefreshLocked starts the background refresh when the held load is older
// than the period and no refresh runs. l.mu must be held by the caller.
func (l *remoteLoad) startRefreshLocked() {
	if l.period <= 0 || l.refresh == nil || l.refreshing != nil || l.clock().Sub(l.loadedAt) < l.period {
		return
	}
	done := make(chan struct{})
	l.refreshing = done
	go l.runRefresh(done)
}

func (l *remoteLoad) runRefresh(done chan struct{}) {
	defer close(done)
	timeout := l.timeout
	if timeout <= 0 {
		timeout = defaultRemoteSourceTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	data, failure := l.refresh(ctx)
	if failure == nil && data == nil {
		failure = terminalRemoteFailure(l.source+" answered no tree", nil)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshing = nil
	l.loadedAt = l.clock()
	if failure == nil {
		l.cached = data
		l.refreshErr = ""
		return
	}
	detail := remoteFailureDetail(failure)
	if detail == l.refreshErr {
		return
	}
	l.refreshErr = detail
	slog.Warn("remote source refresh failed; keeping the configuration it holds",
		slog.String("source", l.source),
		slog.Duration("period", l.period),
		slog.String("error", detail),
	)
}

func retryableRemoteStatus(status int) bool {
	return status == 408 || status == 425 || status == 429 || status >= 500
}

// configServerRefreshFromEnv reads CONFIG_SERVER_REFRESH, the period after
// which a remote source refreshes its tree in the background. Unset, zero or
// invalid keeps the first load for the life of the process.
func configServerRefreshFromEnv() time.Duration {
	return configServerDurationFromEnv("CONFIG_SERVER_REFRESH", 0)
}

func configServerTimeoutFromEnv() time.Duration {
	return configServerDurationFromEnv("CONFIG_SERVER_TIMEOUT", defaultRemoteSourceTimeout)
}

func configServerRetryBudgetFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CONFIG_SERVER_RETRY_BUDGET"))
	if raw == "" {
		return defaultRemoteSourceRetryBudget
	}
	if raw == "0" {
		return -1
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		slog.Warn("invalid config-server duration env var, using default",
			slog.String("env", "CONFIG_SERVER_RETRY_BUDGET"),
			slog.Duration("default", defaultRemoteSourceRetryBudget),
		)
		return defaultRemoteSourceRetryBudget
	}
	if d == 0 {
		return -1
	}
	return d
}

func configServerDurationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		slog.Warn("invalid config-server duration env var, using default",
			slog.String("env", name),
			slog.Duration("default", fallback),
		)
		return fallback
	}
	return d
}
