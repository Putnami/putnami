package security

import (
	"expvar"
	"strconv"
	"strings"
	"testing"
	"time"
)

// keyringEventValue reads the current value of one keyring-event counter.
// Counters are process-global (expvar), so tests assert on the delta around an
// action rather than an absolute value.
func keyringEventValue(t *testing.T, e keyringEvent) int64 {
	t.Helper()
	v := keyringEventCounters().Get(string(e))
	if v == nil {
		return 0
	}
	n, err := strconv.ParseInt(v.String(), 10, 64)
	if err != nil {
		t.Fatalf("counter %q value %q not an int: %v", e, v.String(), err)
	}
	return n
}

// TestKeyringEvents_PublishedUnderScrapableName pins that the counters are
// published under a stable, scrapable expvar name so an operator (or
// /debug/vars) can read rotation cadence.
func TestKeyringEvents_PublishedUnderScrapableName(t *testing.T) {
	keyringEventCounters() // ensure published
	if expvar.Get("security.keyring_events") == nil {
		t.Fatal("expected security.keyring_events to be published via expvar")
	}
}

// TestKeyringEvents_CountRotateAndRevoke proves a successful Rotate and Revoke
// each move their counter exactly once, and that a REJECTED revoke (of the
// active key) moves nothing — the counter tracks committed lifecycle events, not
// attempts.
func TestKeyringEvents_CountRotateAndRevoke(t *testing.T) {
	beforeRotate := keyringEventValue(t, keyringEventRotate)
	beforeRevoke := keyringEventValue(t, keyringEventRevoke)

	p, err := NewRotatingSigningKeyProvider(ProviderConfig{
		OverlapWindow: time.Hour,
		Keys:          []SigningKey{{Kid: "k1", EC: newECKey(t)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Rotate(SigningKey{Kid: "k2", EC: newECKey(t)}); err != nil {
		t.Fatal(err)
	}
	if err := p.Revoke("k1"); err != nil {
		t.Fatal(err)
	}
	if got := keyringEventValue(t, keyringEventRotate); got != beforeRotate+1 {
		t.Errorf("rotate counter = %d, want %d", got, beforeRotate+1)
	}
	if got := keyringEventValue(t, keyringEventRevoke); got != beforeRevoke+1 {
		t.Errorf("revoke counter = %d, want %d", got, beforeRevoke+1)
	}

	// A rejected revoke (the active key cannot be revoked) must not move the counter.
	beforeRejected := keyringEventValue(t, keyringEventRevoke)
	if err := p.Revoke("k2"); err == nil {
		t.Fatal("expected error revoking the active key")
	}
	if got := keyringEventValue(t, keyringEventRevoke); got != beforeRejected {
		t.Errorf("revoke counter moved on a rejected revoke: %d, want %d", got, beforeRejected)
	}
}

// TestKeyringEvents_NoKeyMaterialInSerializedMetrics asserts, on the serialized
// expvar bytes, that the keyring metrics never carry a private JWK field name or
// the active key's private scalar. The counters are label+count only, so this is
// the redaction guarantee made trivially checkable.
func TestKeyringEvents_NoKeyMaterialInSerializedMetrics(t *testing.T) {
	ec := newECKey(t)
	p, err := NewRotatingSigningKeyProvider(ProviderConfig{
		OverlapWindow: time.Hour,
		Keys:          []SigningKey{{Kid: "k1", EC: ec}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Rotate(SigningKey{Kid: "k2", EC: newECKey(t)}); err != nil {
		t.Fatal(err)
	}
	if err := p.Revoke("k1"); err != nil {
		t.Fatal(err)
	}

	raw := keyringEventCounters().String()
	if scalar := b64u(ec.D.Bytes()); strings.Contains(raw, scalar) {
		t.Errorf("keyring metrics leak the private scalar: %s", raw)
	}
	for _, f := range []string{`"d"`, `"p"`, `"q"`, `"dp"`, `"dq"`, `"qi"`, `"k"`} {
		if strings.Contains(raw, f) {
			t.Errorf("keyring metrics carry a private JWK field %s: %s", f, raw)
		}
	}
}
