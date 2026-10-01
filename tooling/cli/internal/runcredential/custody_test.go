package runcredential

import (
	"errors"
	"testing"
	"time"
)

// The tests below change the credential of the whole process (SetForTest),
// so none of them runs in parallel.

// On a hosted run, a holder asks for the credential after a hook ran: the
// handoff fails, names both, and starts nothing.
func TestCustodyRefusesAHolderAfterRepositoryCode(t *testing.T) {
	restore := SetForTest(testBearer)
	defer restore()

	if err := RequireCustody("cache provider @a/cache"); err != nil {
		t.Fatalf("before repository code: RequireCustody = %v, want nil", err)
	}
	started := false
	if err := StartHolder("cache provider @a/cache", func() error { started = true; return nil }); err != nil || !started {
		t.Fatalf("before repository code: StartHolder = %v, started %v; want nil and started", err, started)
	}

	MarkRepositoryCodeStarted("hook hooks.commands.build.before")
	MarkRepositoryCodeStarted("job build~@a/app")

	want := "--credential-fd: credential-provider @a/registry starts after hook hooks.commands.build.before ran repository code; " +
		"a hosted run hands its credential to no process started after that"
	err := RequireCustody("credential-provider @a/registry")
	var refusal *CustodyError
	if !errors.As(err, &refusal) || err.Error() != want {
		t.Fatalf("after repository code: RequireCustody = %v, want %q", err, want)
	}
	if refusal.Holder != "credential-provider @a/registry" || refusal.Reason != "hook hooks.commands.build.before" {
		t.Errorf("refusal = %+v, want the holder and the first reason", refusal)
	}

	started = false
	err = StartHolder("credential-provider @a/registry", func() error { started = true; return nil })
	if err == nil || err.Error() != want || started {
		t.Errorf("after repository code: StartHolder = %v, started %v; want %q and nothing started", err, started, want)
	}
}

// Without --credential-fd the record does nothing, and a holder starts as
// before.
func TestCustodyWithoutARunCredentialIsInert(t *testing.T) {
	restore := SetForTest("")
	defer restore()

	MarkRepositoryCodeStarted("hook hooks.commands.build.before")
	if err := RequireCustody("cache provider @a/cache"); err != nil {
		t.Errorf("RequireCustody = %v, want nil", err)
	}
	startErr := errors.New("start failed")
	if err := StartHolder("cache provider @a/cache", func() error { return startErr }); !errors.Is(err, startErr) {
		t.Errorf("StartHolder = %v, want the start error", err)
	}

	// A run credential held afterwards starts with nothing recorded.
	hosted := SetForTest(testBearer)
	defer hosted()
	if err := RequireCustody("cache provider @a/cache"); err != nil {
		t.Errorf("hosted after an inert record: RequireCustody = %v, want nil", err)
	}
}

// SetForTest gives the process a credential with nothing recorded, and
// restore brings back the previous credential with its record.
func TestSetForTestResetsCustody(t *testing.T) {
	restore := SetForTest(testBearer)
	defer restore()
	MarkRepositoryCodeStarted("hook hooks.commands.build.before")

	fresh := SetForTest(testBearer)
	if err := RequireCustody("cache provider @a/cache"); err != nil {
		t.Errorf("after SetForTest: RequireCustody = %v, want nil", err)
	}
	fresh()
	if err := RequireCustody("cache provider @a/cache"); err == nil {
		t.Error("after restore: RequireCustody = nil, want the previous record")
	}
}

// Repository code starts only once a holder that is starting runs its own
// image: the record waits for StartHolder to return, and a holder that asks
// afterwards is refused.
func TestRepositoryCodeWaitsForAStartingHolder(t *testing.T) {
	restore := SetForTest(testBearer)
	defer restore()

	starting, release := make(chan struct{}), make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- StartHolder("cache provider @a/cache", func() error {
			close(starting)
			<-release
			return nil
		})
	}()
	<-starting

	marked := make(chan struct{})
	go func() {
		MarkRepositoryCodeStarted("hook hooks.commands.build.before")
		close(marked)
	}()
	select {
	case <-marked:
		t.Fatal("repository code was recorded while a holder was starting")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("the holder that started first: StartHolder = %v, want nil", err)
	}
	<-marked
	if err := RequireCustody("credential-provider @a/registry"); err == nil {
		t.Error("a holder after the record: RequireCustody = nil, want a refusal")
	}
}
