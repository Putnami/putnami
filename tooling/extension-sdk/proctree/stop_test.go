package proctree

import (
	"errors"
	"testing"
	"time"
)

func TestStopReturnsWhatDoneDeliversWithinTheGrace(t *testing.T) {
	done := make(chan int, 1)
	killed := false
	got := Stop(done, time.Hour,
		func() error { done <- 7; return nil },
		func() error { killed = true; return nil })
	if got != 7 || killed {
		t.Fatalf("Stop = %d, killed %v; want 7 without a kill", got, killed)
	}
}

func TestStopKillsOnceTheGraceRunsOut(t *testing.T) {
	done := make(chan int, 1)
	terminated := false
	start := time.Now()
	got := Stop(done, 50*time.Millisecond,
		func() error { terminated = true; return nil },
		func() error { done <- 9; return nil })
	if got != 9 || !terminated {
		t.Fatalf("Stop = %d, terminated %v; want 9 after a terminate", got, terminated)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("Stop killed after %v, before the 50ms grace", elapsed)
	}
}

func TestStopKillsAtOnceWhenTerminateFails(t *testing.T) {
	done := make(chan int, 1)
	start := time.Now()
	got := Stop(done, time.Hour,
		func() error { return errors.New("signal not delivered") },
		func() error { done <- 3; return nil })
	if got != 3 {
		t.Fatalf("Stop = %d, want 3", got)
	}
	if elapsed := time.Since(start); elapsed > time.Minute {
		t.Fatalf("Stop took %v after a failed terminate, want no grace", elapsed)
	}
}

func TestStopReturnsTheZeroValueOfAClosedDone(t *testing.T) {
	done := make(chan struct{})
	close(done)
	Stop(done, time.Hour,
		func() error { return nil },
		func() error { t.Fatal("kill called for a child that exited"); return nil })
}
