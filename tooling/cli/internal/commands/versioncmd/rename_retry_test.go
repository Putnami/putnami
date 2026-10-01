package versioncmd

import (
	"errors"
	"testing"
	"time"
)

var (
	errTransientRename = errors.New("held open for a moment")
	errFinalRename     = errors.New("refused for good")
)

// scriptedRename fails with the errors in script, in order, then succeeds.
type scriptedRename struct {
	script []error
	calls  int
}

func (r *scriptedRename) rename(string, string) error {
	r.calls++
	if r.calls <= len(r.script) {
		return r.script[r.calls-1]
	}
	return nil
}

func isTransientForTest(err error) bool { return errors.Is(err, errTransientRename) }

func TestRetryingRename_OutlastsTransientRefusals(t *testing.T) {
	scripted := &scriptedRename{script: []error{errTransientRename, errTransientRename}}
	var slept []time.Duration
	rename := retryingRename(scripted.rename, isTransientForTest, 5, time.Millisecond, func(d time.Duration) { slept = append(slept, d) })

	if err := rename("a", "b"); err != nil {
		t.Fatalf("rename = %v, want success on the third attempt", err)
	}
	if scripted.calls != 3 || len(slept) != 2 {
		t.Fatalf("calls = %d, sleeps = %d, want 3 calls and 2 sleeps", scripted.calls, len(slept))
	}
}

func TestRetryingRename_StopsAtTheBound(t *testing.T) {
	scripted := &scriptedRename{script: []error{errTransientRename, errTransientRename, errTransientRename, errTransientRename}}
	sleeps := 0
	rename := retryingRename(scripted.rename, isTransientForTest, 3, time.Millisecond, func(time.Duration) { sleeps++ })

	if err := rename("a", "b"); !errors.Is(err, errTransientRename) {
		t.Fatalf("rename = %v, want the last transient error", err)
	}
	if scripted.calls != 3 || sleeps != 2 {
		t.Fatalf("calls = %d, sleeps = %d, want 3 calls and 2 sleeps", scripted.calls, sleeps)
	}
}

func TestRetryingRename_ReturnsAnOtherErrorAtOnce(t *testing.T) {
	scripted := &scriptedRename{script: []error{errFinalRename}}
	sleeps := 0
	rename := retryingRename(scripted.rename, isTransientForTest, 5, time.Millisecond, func(time.Duration) { sleeps++ })

	if err := rename("a", "b"); !errors.Is(err, errFinalRename) {
		t.Fatalf("rename = %v, want %v", err, errFinalRename)
	}
	if scripted.calls != 1 || sleeps != 0 {
		t.Fatalf("calls = %d, sleeps = %d, want 1 call and no sleep", scripted.calls, sleeps)
	}
}

// The bound the switch uses keeps a refusal that never clears under 2 s.
func TestRenameRetryBoundStaysUnderTwoSeconds(t *testing.T) {
	if total := time.Duration(renameAttempts-1) * renameRetryDelay; total >= 2*time.Second {
		t.Fatalf("a rename that keeps failing sleeps %v, want under 2s", total)
	}
}
