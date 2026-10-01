package registrycred

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/recorded"
)

// A regression this test guards against: on the first call after
// putnami.lock.json changed, the credential child ran
// the CLI's first-use install and printed its setup lines on stdout ahead of the
// bearer. The seam read that stdout as "no token", the archive download went
// anonymous, and the registry answered 401. Every `upgrade` that rewrote the
// lock failed once.
//
// The replayed CLI answers with the stdout the incident's PATH shim captured
// (testdata/recorded/registry-token/lock-changed) whenever its first-use
// install would run, and with the settled answer when the caller disables that
// install — the one condition the real CLI has.
//
// Red on the tree before the fix: check out the pre-fix commit, copy this
// file, recorded_exchange_test.go, testdata/recorded and the recorded
// package, then run `./putnamiw test --projects putnami-extension-sdk`: the
// PATH seam returns no token and the "non-bearer value on stdout" hint.
func TestCredentialChildAfterALockChangeReturnsTheBearer(t *testing.T) {
	lockChanged := recordedExchange(t, "lock-changed")
	settled := recordedExchange(t, "settled")
	bearer := strings.TrimSpace(string(settled.Stdout()))
	disabled := recorded.Branch{Env: "PUTNAMI_NO_AUTO_INSTALL", Value: "1", Exchange: settled}
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")

	recordedCloudCLI(t, lockChanged, disabled)
	if tok, hint := resolveTokenCLI("put.putnami.dev"); tok != bearer || hint != "" {
		t.Fatalf("PATH seam: token of %d bytes, hint = %q; stdout must be the bare bearer", len(tok), hint)
	}

	executable := recorded.Executable(t, lockChanged, disabled)
	if tok, hint := ResolveTokenWithCLI(context.Background(), "put.putnami.dev", executable); tok != bearer || hint != "" {
		t.Fatalf("bootstrap seam: token of %d bytes, hint = %q; stdout must be the bare bearer", len(tok), hint)
	}
	if os.Getenv("PUTNAMI_NO_AUTO_INSTALL") != "" {
		t.Fatal("obtaining a credential disabled the parent's first-use install")
	}
}
