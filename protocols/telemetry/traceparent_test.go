package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// proxySafeTraceparentFixture is fixtures/conventions/proxy-safe-traceparent.json,
// which the TypeScript runtime reads too.
type proxySafeTraceparentFixture struct {
	Header string                     `json:"header"`
	Cases  []proxySafeTraceparentCase `json:"cases"`
}

// proxySafeTraceparentCase is one request's two header values and the remote
// parent a server reads from them.
type proxySafeTraceparentCase struct {
	Name               string       `json:"name"`
	Traceparent        string       `json:"traceparent,omitempty"`
	PutnamiTraceparent string       `json:"putnamiTraceparent,omitempty"`
	Source             string       `json:"source"`
	Outside            bool         `json:"outside"`
	Parent             *Traceparent `json:"parent,omitempty"`
}

func readProxySafeTraceparentFixture(t *testing.T) proxySafeTraceparentFixture {
	t.Helper()
	data, err := os.ReadFile("fixtures/conventions/proxy-safe-traceparent.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture proxySafeTraceparentFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture
}

// TestTraceparent_TheRuleMatchesItsFixture keeps the header name and
// ResolveRemoteParent equal to the fixture the TypeScript runtime reads.
func TestTraceparent_TheRuleMatchesItsFixture(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "the-client-span-survives-a-proxy", "the-proxy-safe-rule-matches-its-fixture")
	fixture := readProxySafeTraceparentFixture(t)
	if fixture.Header != HeaderPutnamiTraceparent {
		t.Errorf("header = %q, want %q", fixture.Header, HeaderPutnamiTraceparent)
	}
	sources := map[string]bool{}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			sources[c.Source] = true
			parent, source := ResolveRemoteParent(c.Traceparent, c.PutnamiTraceparent)
			if source != c.Source {
				t.Fatalf("source = %q, want %q", source, c.Source)
			}
			if c.Parent == nil {
				if source != RemoteParentNone {
					t.Fatalf("the case names no parent but source %q", source)
				}
				if parent != (Traceparent{}) {
					t.Fatalf("parent = %+v, want none", parent)
				}
				return
			}
			if parent != *c.Parent {
				t.Fatalf("parent = %+v, want %+v", parent, *c.Parent)
			}
		})
	}
	for _, source := range []string{RemoteParentNone, RemoteParentTraceparent, RemoteParentPutnami} {
		if !sources[source] {
			t.Errorf("no fixture case resolves to %q", source)
		}
	}
}

// TestTraceparent_TheOutsideRuleMatchesItsFixture keeps RemoteParentOutside
// equal to the outside flag of each case the TypeScript runtime reads.
func TestTraceparent_TheOutsideRuleMatchesItsFixture(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "an-outside-parent", "the-outside-rule-matches-its-fixture")
	seen := map[bool]bool{}
	for _, c := range readProxySafeTraceparentFixture(t).Cases {
		seen[c.Outside] = true
		if got := RemoteParentOutside(c.Traceparent, c.PutnamiTraceparent); got != c.Outside {
			t.Errorf("%s: outside = %v, want %v", c.Name, got, c.Outside)
		}
	}
	if !seen[true] || !seen[false] {
		t.Error("the fixture needs an outside and an inside case")
	}
}

func TestTraceparent_ParseRefusesMalformedValues(t *testing.T) {
	valid := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	parsed, ok := ParseTraceparent(valid)
	if !ok || parsed.String() != valid {
		t.Fatalf("ParseTraceparent(%q) = %+v, %v", valid, parsed, ok)
	}
	for _, value := range []string{
		"",
		valid + "-",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00_4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736_00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7_01",
		"00-4bf92f3577b34da6a3ce929d0e0e473g-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902bG-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0z",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
	} {
		if _, ok := ParseTraceparent(value); ok {
			t.Errorf("ParseTraceparent(%q) accepted a malformed value", value)
		}
	}
}
