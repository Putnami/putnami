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

// remoteLoad shares one completed remote load with every caller. A load that
// its caller's cancellation cut short is not a result: the next caller fetches
// again with its own context instead of inheriting the canceled error.
type remoteLoad struct {
	mu       sync.Mutex
	done     bool
	cached   map[string]any
	err      error
	inflight chan struct{}
}

func (l *remoteLoad) load(ctx context.Context, fetch func(context.Context) (map[string]any, error)) (map[string]any, error) {
	for {
		l.mu.Lock()
		if l.done {
			cached, err := l.cached, l.err
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
		}
		l.mu.Unlock()
		close(wait)
	}()
	return fetch(ctx)
}

func retryableRemoteStatus(status int) bool {
	return status == 408 || status == 425 || status == 429 || status >= 500
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
