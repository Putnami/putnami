package engine

import (
	"context"
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
)

// A planned run hands the observer the extensions that declare its commands
// public, read from the extensions the run loaded.
func TestRun_ObserverReceivesTheCommandDeclarers(t *testing.T) {
	t.Setenv(protocolcli.ParentSessionEnv, "")
	_, req := reporterFixture(t, fixtureproc.Program{})
	t.Setenv(protocolcli.SessionReporterEnv, "")
	observer := &recordingObserver{}
	req.Observer = observer

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("run: exit=%d err=%v", result.ExitCode, err)
	}
	if len(observer.calls) != 1 {
		t.Fatalf("observer calls = %d, want 1", len(observer.calls))
	}
	want := map[string][]string{"test": {"@test/reporter"}}
	if got := observer.calls[0].Declarers; !reflect.DeepEqual(got, want) {
		t.Fatalf("observed declarers = %v, want %v", got, want)
	}
}
