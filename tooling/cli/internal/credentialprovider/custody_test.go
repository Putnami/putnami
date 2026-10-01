package credentialprovider

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/extension"
	registry "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// The hosted tests below change the run credential of the whole process, so
// they do not run in parallel.

const custodyRunCredential = "prc_custody_run_credential"

// hostedBroker is the broker New builds on a hosted run for an out-of-process
// provider, its extension's native runtime, that records its initialize
// request in the returned file.
func hostedBroker(t *testing.T) (*Broker, string) {
	t.Helper()
	t.Cleanup(runcredential.SetForTest(custodyRunCredential))
	initializeFile := filepath.Join(t.TempDir(), "initialize")
	declaring, _ := nativeRuntimeExtension(t, map[string]string{
		fakeProviderEnv: "1", "FAKE_BEARER": "pat", "FAKE_HOSTS": "put.putnami.dev", "FAKE_INITIALIZE_FILE": initializeFile,
	})
	broker := New(t.TempDir(), []*extension.ExtensionDescription{declaring}, []string{runner.InvocationProviderInstall},
		runcredential.Flag, io.Discard, WithRunCredential(custodyRunCredential))
	if broker == nil {
		t.Fatal("New built no broker for one declaring extension")
	}
	t.Cleanup(func() { _ = broker.Close() })
	return broker, initializeFile
}

// On a hosted run the provider receives the run credential, so it starts
// only before repository code. After a hook ran, neither Start nor the first
// credential starts it: both fail with the refusal, and no process received
// an initialize request.
func TestAHostedProviderNeverStartsAfterRepositoryCode(t *testing.T) {
	broker, initializeFile := hostedBroker(t)
	runcredential.MarkRepositoryCodeStarted("hook hooks.cli.before")

	var refusal *runcredential.CustodyError
	if err := broker.Start(context.Background()); !errors.As(err, &refusal) ||
		refusal.Holder != "the credential provider of @fixture/credentials" || refusal.Reason != "hook hooks.cli.before" {
		t.Fatalf("Start = %v, want the refusal that names the provider and the hook", err)
	}
	_, served, err := broker.Bearer(context.Background(), registry.PurposeRead, mustURL(t, "https://put.putnami.dev/a"))
	if served || !errors.As(err, &refusal) {
		t.Fatalf("Bearer = served %v, %v; want the refusal", served, err)
	}
	if _, err := os.Stat(initializeFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a provider received initialize after repository code: %v", err)
	}
}

// Start launches the provider before any credential is asked, with the run
// credential in initialize, and that provider keeps serving after repository
// code ran.
func TestStartLaunchesTheProviderBeforeRepositoryCode(t *testing.T) {
	broker, initializeFile := hostedBroker(t)
	if err := broker.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	initialize, err := os.ReadFile(initializeFile)
	if err != nil || !strings.Contains(string(initialize), `"runCredential":"`+custodyRunCredential+`"`) {
		t.Fatalf("after Start: initialize %q, %v; want the run credential", initialize, err)
	}
	runcredential.MarkRepositoryCodeStarted("hook hooks.cli.before")

	bearer, served, err := broker.Bearer(context.Background(), registry.PurposeRead, mustURL(t, "https://put.putnami.dev/a"))
	if !served || err != nil || bearer != "pat-read" {
		t.Fatalf("Bearer after repository code = %q, served %v, %v; want the provider started first to serve it", bearer, served, err)
	}
}

// A broker without a provider starts nothing.
func TestStartWithoutAProviderStartsNothing(t *testing.T) {
	t.Parallel()
	var broker *Broker
	if err := broker.Start(context.Background()); err != nil {
		t.Errorf("a nil broker: Start = %v, want nil", err)
	}
	absent := NewBroker([]string{registry.PurposeRead}, func(context.Context) (*Session, error) { return nil, nil })
	if err := absent.Start(context.Background()); err != nil {
		t.Errorf("an absent provider: Start = %v, want nil", err)
	}
	if _, served, err := absent.Bearer(context.Background(), registry.PurposeRead, mustURL(t, "https://put.putnami.dev/a")); served || err != nil {
		t.Errorf("an absent provider: Bearer = served %v, %v; want nothing", served, err)
	}
}
