package retry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestDelayDoublesWithJitterAndHonorsABoundedRetryAfter(t *testing.T) {
	policy := Default()
	for made, base := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second} {
		for range 100 {
			if got := policy.delay(made, nil); got < base || got >= base+base/2 {
				t.Fatalf("delay after attempt %d = %v, want from %v up to %v", made, got, base, base+base/2)
			}
		}
	}
	policy.Jitter = func(time.Duration) time.Duration { return 0 }
	for retryAfter, want := range map[string]time.Duration{
		"7": 7 * time.Second, "12": 12 * time.Second, "0": time.Second, "30": 30 * time.Second,
		"31": 30 * time.Second, "-5": time.Second, "soon": time.Second, "": time.Second,
	} {
		if got := policy.delay(1, http.Header{"Retry-After": {retryAfter}}); got != want {
			t.Errorf("delay with Retry-After %q = %v, want %v", retryAfter, got, want)
		}
	}
}

func TestNoResponseMarksOnlyAFailureWorthAnotherAttempt(t *testing.T) {
	reset := errors.New("connection reset")
	if marked := NoResponse(context.Background(), reset); !retryable(Answer{}, marked) || !errors.Is(marked, reset) || marked.Error() != reset.Error() {
		t.Fatalf("NoResponse(reset) = %v, want the reset marked for another attempt", marked)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	timeout := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	for name, tc := range map[string]struct {
		ctx context.Context
		err error
	}{
		"an ended context":  {ended, reset},
		"a cancellation":    {context.Background(), context.Canceled},
		"a deadline":        {context.Background(), fmt.Errorf("send: %w", context.DeadlineExceeded)},
		"a network timeout": {context.Background(), timeout},
	} {
		if retryable(Answer{}, NoResponse(tc.ctx, tc.err)) {
			t.Errorf("NoResponse marked %s for another attempt", name)
		}
	}
	if NoResponse(context.Background(), nil) != nil {
		t.Fatal("NoResponse(nil) is not nil")
	}
}

func TestWaitReturnsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wait took %v after its context ended", elapsed)
	}
}

func TestAfterNamesMoreThanOneAttempt(t *testing.T) {
	if After(1) != "" || After(4) != " after 4 attempts" {
		t.Fatalf("After(1) = %q, After(4) = %q", After(1), After(4))
	}
}
