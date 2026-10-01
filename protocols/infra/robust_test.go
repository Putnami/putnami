package infra

import (
	"errors"
	"testing"
	"time"
)

var (
	errBusy  = errors.New("busy")
	errFatal = errors.New("fatal")
)

func isBusy(err error) bool { return errors.Is(err, errBusy) }

func TestRetryStopsAtTheFirstSuccess(t *testing.T) {
	calls := 0
	err := retry(func() error {
		calls++
		if calls < 3 {
			return errBusy
		}
		return nil
	}, isBusy, time.Minute)
	if err != nil || calls != 3 {
		t.Fatalf("retry = %v after %d calls, want success after 3", err, calls)
	}
}

func TestRetryReturnsAnotherErrorAtOnce(t *testing.T) {
	calls := 0
	err := retry(func() error { calls++; return errFatal }, isBusy, time.Minute)
	if !errors.Is(err, errFatal) || calls != 1 {
		t.Fatalf("retry = %v after %d calls, want the error after 1", err, calls)
	}
}

func TestRetryGivesUpWithinItsBudget(t *testing.T) {
	const budget = 50 * time.Millisecond
	calls := 0
	start := time.Now()
	err := retry(func() error { calls++; return errBusy }, isBusy, budget)
	if !errors.Is(err, errBusy) {
		t.Fatalf("retry = %v, want the last transient error", err)
	}
	if elapsed := time.Since(start); elapsed >= budget+time.Second {
		t.Fatalf("retry ran %v past a %v budget", elapsed, budget)
	}
	if calls < 2 {
		t.Fatalf("retry made %d attempts within %v, want several", calls, budget)
	}
}
