package retry

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDelayDoublesWithJitterAndHonorsABoundedRetryAfter(t *testing.T) {
	policy := Default()
	for made, base := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second} {
		for range 100 {
			if got, stop := policy.delay(made, nil); got < base || got >= base+base/2 || stop != "" {
				t.Fatalf("delay after attempt %d = %v %q, want from %v up to %v", made, got, stop, base, base+base/2)
			}
		}
	}
	policy.Jitter = func(time.Duration) time.Duration { return 0 }
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	policy.Now = func() time.Time { return now }
	for retryAfter, want := range map[string]time.Duration{
		"7": 7 * time.Second, "12": 12 * time.Second, "0": time.Second, "30": 30 * time.Second,
		"-5": time.Second, "soon": time.Second, "": time.Second,
		now.Add(9 * time.Second).Format(http.TimeFormat): 9 * time.Second,
		now.Add(-time.Hour).Format(http.TimeFormat):      time.Second,
	} {
		if got, stop := policy.delay(1, http.Header{"Retry-After": {retryAfter}}); got != want || stop != "" {
			t.Errorf("delay with Retry-After %q = %v %q, want %v", retryAfter, got, stop, want)
		}
	}
}

func TestDelayStopsWhenRetryAfterAsksForMoreThanTheCap(t *testing.T) {
	policy := Default()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	policy.Now = func() time.Time { return now }
	for retryAfter, asked := range map[string]string{
		"31":                   "31s",
		"3600":                 "1h0m0s",
		"99999999999999999999": "99999999999999999999s",
		"9223372036854775807":  "9223372036854775807s",
		now.Add(time.Hour).Format(http.TimeFormat): "1h0m0s",
	} {
		_, stop := policy.delay(1, http.Header{"Retry-After": {retryAfter}})
		if want := "; the registry asked to wait " + asked + ", more than 30s"; stop != want {
			t.Errorf("delay with Retry-After %q stops with %q, want %q", retryAfter, stop, want)
		}
	}
}

func TestDoRetriesATransientAnswerUpToTheBound(t *testing.T) {
	policy := Default()
	var waits []time.Duration
	policy.Wait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	policy.Jitter = func(time.Duration) time.Duration { return 0 }
	calls := 0
	answer, attempts, err := policy.Do(context.Background(), func() (Answer, error) {
		calls++
		return Answer{Status: http.StatusRequestTimeout}, nil
	})
	if err != nil || answer.Status != http.StatusRequestTimeout || attempts != Attempts || calls != Attempts {
		t.Fatalf("Do = %d, %d attempts, %v after %d calls, want 408 after %d", answer.Status, attempts, err, calls, Attempts)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	if note := Note(answer, attempts); note != " after 4 attempts" {
		t.Fatalf("Note = %q", note)
	}
}

func TestDoStopsWhenRetryAfterAsksForMoreThanTheCap(t *testing.T) {
	policy := Default()
	policy.Wait = func(context.Context, time.Duration) error { t.Fatal("Do waited"); return nil }
	calls := 0
	answer, attempts, err := policy.Do(context.Background(), func() (Answer, error) {
		calls++
		return Answer{Status: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"3600"}}}, nil
	})
	if err != nil || attempts != 1 || calls != 1 {
		t.Fatalf("Do = %d attempts, %v after %d calls, want 1 attempt", attempts, err, calls)
	}
	if note := Note(answer, attempts); note != "; the registry asked to wait 1h0m0s, more than 30s" {
		t.Fatalf("Note = %q", note)
	}
}

func TestDoNeverRetriesWhenTheContextEnds(t *testing.T) {
	policy := Default()
	waited := false
	policy.Wait = func(context.Context, time.Duration) error { waited = true; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, attempts, err := policy.Do(ctx, func() (Answer, error) {
		calls++
		cancel()
		return Answer{Status: http.StatusBadGateway}, nil
	})
	if calls != 1 || attempts != 1 || waited {
		t.Fatalf("Do made %d calls, %d attempts, waited %v, want 1 attempt and no wait", calls, attempts, waited)
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "the last answer was 502") {
		t.Fatalf("Do = %v, want context.Canceled with the last answer 502", err)
	}
}

func TestDoWrapsAFailedWaitWithTheLastAnswer(t *testing.T) {
	policy := Default()
	policy.Wait = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	_, attempts, err := policy.Do(context.Background(), func() (Answer, error) {
		return Answer{Status: http.StatusTooManyRequests}, nil
	})
	if attempts != 1 || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "the last answer was 429") {
		t.Fatalf("Do = %d attempts, %v, want the deadline with the last answer 429", attempts, err)
	}
}

func TestDoRetriesOnlyAMarkedError(t *testing.T) {
	policy := Default()
	policy.Wait = func(context.Context, time.Duration) error { return nil }
	calls := 0
	_, attempts, err := policy.Do(context.Background(), func() (Answer, error) {
		calls++
		return Answer{}, errors.New("refused")
	})
	if calls != 1 || attempts != 1 || err == nil {
		t.Fatalf("Do made %d calls for an unmarked error, want 1", calls)
	}
	calls = 0
	_, attempts, _ = policy.Do(context.Background(), func() (Answer, error) {
		calls++
		return Answer{}, Again(errors.New("reset"))
	})
	if calls != Attempts || attempts != Attempts {
		t.Fatalf("Do made %d calls for a marked error, want %d", calls, Attempts)
	}
}

func TestFailedMarksOnlyATransportFailureWorthAnotherAttempt(t *testing.T) {
	reset := errors.New("connection reset")
	timeout := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	notFound := &net.DNSError{Err: "no such host", Name: "registry.invalid", IsNotFound: true}
	certificate := fmt.Errorf("tls: %w", x509.UnknownAuthorityError{})
	exchange := func(ctx context.Context, connected bool) *Exchange {
		_, e := Watch(ctx)
		e.connected.Store(connected)
		return e
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()

	for name, tc := range map[string]struct {
		ctx       context.Context
		connected bool
		err       error
	}{
		"a reset before the connection":    {context.Background(), false, reset},
		"a reset after the connection":     {context.Background(), true, reset},
		"a timeout before the connection":  {context.Background(), false, timeout},
		"a deadline before the connection": {context.Background(), false, fmt.Errorf("dial: %w", context.DeadlineExceeded)},
	} {
		marked := exchange(tc.ctx, tc.connected).Failed(tc.err, fmt.Errorf("send: %w", tc.err))
		if !retryable(Answer{}, marked) || !errors.Is(marked, tc.err) {
			t.Errorf("Failed did not mark %s for another attempt: %v", name, marked)
		}
	}
	for name, tc := range map[string]struct {
		ctx       context.Context
		connected bool
		err       error
	}{
		"an ended context":                {ended, false, reset},
		"a cancellation":                  {context.Background(), false, context.Canceled},
		"a timeout after the connection":  {context.Background(), true, timeout},
		"a deadline after the connection": {context.Background(), true, fmt.Errorf("read: %w", context.DeadlineExceeded)},
		"an unknown host":                 {context.Background(), false, notFound},
		"a refused certificate":           {context.Background(), false, certificate},
	} {
		if marked := exchange(tc.ctx, tc.connected).Failed(tc.err, tc.err); retryable(Answer{}, marked) {
			t.Errorf("Failed marked %s for another attempt", name)
		}
	}
	if exchange(context.Background(), false).Failed(reset, nil) != nil {
		t.Fatal("Failed with no reported error is not nil")
	}
	if !CertificateRefused(certificate) || CertificateRefused(reset) {
		t.Fatal("CertificateRefused does not tell a refused certificate from a reset")
	}
}

func TestTransientNamesTheRetriedStatuses(t *testing.T) {
	for status := 100; status < 600; status++ {
		want := status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504
		if Transient(status) != want {
			t.Errorf("Transient(%d) = %v, want %v", status, !want, want)
		}
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
