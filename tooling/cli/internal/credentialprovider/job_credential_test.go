package credentialprovider

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// ReadCredential answers the read credential whatever hosts it names, absence
// as nil, a refusal as an error that names the source of the choice, and
// nothing at all for a broker that does not serve the read purpose.
func TestReadCredentialAnswersTheReadPurpose(t *testing.T) {
	t.Parallel()
	var opens, calls atomic.Int32
	clock := &fakeClock{now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}

	serving := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{
		bearer: "b", hosts: []string{"put.putnami.dev"}, lifetime: time.Hour, clock: clock.Now,
	}, &opens), WithClock(clock.Now))
	t.Cleanup(func() { _ = serving.Close() })
	credential, err := serving.ReadCredential(context.Background())
	want := &registry.Credential{Bearer: "b-read", ExpiresAt: "2026-09-29T13:00:00Z", Hosts: []string{"put.putnami.dev"}}
	if err != nil || !reflect.DeepEqual(credential, want) {
		t.Fatalf("ReadCredential = %v, %v; want %v", credential, err, want)
	}

	absent := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{absent: true}, &opens))
	t.Cleanup(func() { _ = absent.Close() })
	if credential, err := absent.ReadCredential(context.Background()); credential != nil || err != nil {
		t.Fatalf("absence: %v, %v", credential, err)
	}

	refusing := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{refuse: "account_suspended"}, &opens), WithSource("--providers"))
	t.Cleanup(func() { _ = refusing.Close() })
	credential, err = refusing.ReadCredential(context.Background())
	var refusal *RefusalError
	if credential != nil || !errors.As(err, &refusal) || !strings.HasPrefix(err.Error(), "--providers: ") {
		t.Fatalf("refusal: %v, %v", credential, err)
	}

	publishOnly := NewBroker([]string{registry.PurposePublish}, inProcess(t, fakeConfig{bearer: "b", calls: &calls}, &opens))
	t.Cleanup(func() { _ = publishOnly.Close() })
	var none *Broker
	for name, broker := range map[string]*Broker{"a publish-only broker": publishOnly, "no broker": none} {
		if credential, err := broker.ReadCredential(context.Background()); credential != nil || err != nil {
			t.Fatalf("%s: %v, %v", name, credential, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("a broker without the read purpose asked its provider %d times", calls.Load())
	}
}

// InstallJobRead makes the broker the source of the credential a hosted fetch
// job receives, until restore runs. A broker without the read purpose installs
// nothing.
func TestInstallJobReadIsTheFetchJobSource(t *testing.T) {
	var opens atomic.Int32
	broker := NewBroker([]string{registry.PurposeRead}, inProcess(t, fakeConfig{
		bearer: "b", hosts: []string{"put.putnami.dev"}, lifetime: time.Hour,
	}, &opens))
	t.Cleanup(func() { _ = broker.Close() })
	t.Cleanup(jobs.InstallJobReadCredential(nil))

	restore := broker.InstallJobRead()
	credential, err := jobs.ReadJobCredential(context.Background())
	if err != nil || credential == nil || credential.Bearer != "b-read" {
		t.Fatalf("installed: %v, %v; want the broker's read credential", credential, err)
	}
	restore()
	if credential, err := jobs.ReadJobCredential(context.Background()); credential != nil || err != nil {
		t.Fatalf("restored: %v, %v; want no source", credential, err)
	}

	publishOnly := NewBroker([]string{registry.PurposePublish}, inProcess(t, fakeConfig{bearer: "b"}, &opens))
	t.Cleanup(func() { _ = publishOnly.Close() })
	restore = publishOnly.InstallJobRead()
	defer restore()
	if credential, err := jobs.ReadJobCredential(context.Background()); credential != nil || err != nil {
		t.Fatalf("a publish-only broker installed %v, %v", credential, err)
	}
}
