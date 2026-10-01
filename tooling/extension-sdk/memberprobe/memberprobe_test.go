package memberprobe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/jsonl"
)

const (
	localDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	otherDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func testSubject() Subject {
	return Subject{
		Ecosystem: "npm", Coordinate: "@acme/widget", Version: "1.4.0",
		Registry: "https://registry.example.test/", ArtifactDigest: localDigest,
	}
}

// The verdict of a held version is decided in one place for every registry
// kind: equal digests are a reuse, anything the dry run cannot prove equal is
// not.
func TestHeldVersionVerdict(t *testing.T) {
	for name, tc := range map[string]struct {
		local, registry string
		wantState       string
		wantReason      string
	}{
		"equal digests are identical":        {localDigest, localDigest, extproto.MemberProbeIdentical, ""},
		"different digests are a conflict":   {localDigest, otherDigest, extproto.MemberProbeConflict, "another digest"},
		"no local artifact is a conflict":    {"", otherDigest, extproto.MemberProbeConflict, "built no artifact to compare"},
		"no local and no registry digest":    {"", "", extproto.MemberProbeConflict, "built no artifact to compare"},
		"no advertised digest is unverified": {localDigest, "", extproto.MemberProbeUnverified, "advertised no digest"},
	} {
		t.Run(name, func(t *testing.T) {
			subject := testSubject()
			subject.ArtifactDigest = tc.local
			probe := subject.Held(tc.registry)
			if probe.State != tc.wantState || !strings.Contains(probe.Reason, tc.wantReason) {
				t.Fatalf("Held(%q) = %+v, want %s with reason %q", tc.registry, probe, tc.wantState, tc.wantReason)
			}
			if diagnostics := extproto.ValidateMemberProbe(&probe); len(diagnostics) != 0 {
				t.Fatalf("verdict %+v is not a valid member-probe: %v", probe, diagnostics)
			}
		})
	}
}

func TestUnreachableReasonNamesTheCauseWithoutTheRequestURL(t *testing.T) {
	cause := errors.New("dial tcp 127.0.0.1:9: connect: connection refused")
	wrapped := &url.Error{Op: "Head", URL: "https://registry.example.test/secret/path?channel=1", Err: cause}
	if got := UnreachableReason(wrapped); got != "the registry could not be reached: "+cause.Error() {
		t.Fatalf("UnreachableReason() = %q", got)
	}
	if got := UnreachableReason(fmt.Errorf("probe: %w", context.DeadlineExceeded)); got != "the registry did not answer in time" {
		t.Fatalf("UnreachableReason(deadline) = %q", got)
	}
	if got := UnreachableReason(nil); got != "the registry could not be reached" {
		t.Fatalf("UnreachableReason(nil) = %q", got)
	}
}

func TestRefusedReasonSaysWhatToDo(t *testing.T) {
	for name, tc := range map[string]struct {
		status    int
		anonymous bool
		excerpt   string
		want      string
	}{
		"anonymous 401":      {http.StatusUnauthorized, true, "", "the registry answered 401 Unauthorized to a request without a credential; sign in or supply the registry token, then run the dry run again"},
		"anonymous 403":      {http.StatusForbidden, true, "", "the registry answered 403 Forbidden to a request without a credential; sign in or supply the registry token, then run the dry run again"},
		"refused credential": {http.StatusForbidden, false, ": token expired", "the registry refused the credential with 403 Forbidden: token expired"},
		"server error":       {http.StatusBadGateway, true, "upstream reset", "the registry answered 502 Bad Gateway: upstream reset"},
	} {
		if got := RefusedReason(tc.status, tc.anonymous, tc.excerpt); got != tc.want {
			t.Errorf("%s: RefusedReason() = %q, want %q", name, got, tc.want)
		}
	}
}

// A registry may echo the request's Authorization in an error body. Whatever a
// probe carries is printed and stored, so the secret never survives.
func TestRedactRemovesCredentialsAndBoundsTheText(t *testing.T) {
	const secret = "pkt_secret/value+1"
	text := "denied for " + secret + " and " + url.QueryEscape(secret) + "\nAuthorization: Bearer eyJhbGciOi.payload.sig\x00 end"
	got := Redact(text, secret, "")
	if strings.Contains(got, secret) || strings.Contains(got, url.QueryEscape(secret)) || strings.Contains(got, "eyJhbGciOi") {
		t.Fatalf("Redact() = %q, still carries a credential", got)
	}
	if strings.ContainsAny(got, "\n\x00") {
		t.Fatalf("Redact() = %q, still carries control characters", got)
	}
	long := Redact(strings.Repeat("é", 1000))
	if runes := []rune(long); len(runes) != reasonLimit+3 || !strings.HasSuffix(long, "...") {
		t.Fatalf("Redact(long) has %d runes, want %d and an ellipsis", len(runes), reasonLimit+3)
	}
}

func TestEndpointIsCredentialFree(t *testing.T) {
	for raw, want := range map[string]string{
		"https://registry.example.test":                     "https://registry.example.test",
		"https://registry.example.test/":                    "https://registry.example.test",
		"https://user:hunter2@registry.example.test/npm/":   "https://registry.example.test/npm",
		"https://registry.example.test/npm?token=hunter2#x": "https://registry.example.test/npm",
		"oci.example.test/team":                             "oci.example.test/team",
		"user:hunter2@oci.example.test/team?x=1":            "oci.example.test/team",
		"https://%zz":                                       "an unparseable registry URL",
	} {
		got := Endpoint(raw)
		if got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", raw, got, want)
		}
		if strings.Contains(got, "hunter2") {
			t.Errorf("Endpoint(%q) = %q, still carries the credential", raw, got)
		}
	}
}

// captureEvents runs fn with os.Stdout redirected to a file and returns the
// decoded JSONL events.
func captureEvents(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = file
	fn()
	os.Stdout = original
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decoding %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

// Emit and Decode are the two ends of one event: what a publisher emits is
// exactly what the orchestrator reads back through the strict parser, on both
// stream versions, and the registry never leaves with a credential.
func TestEmitRoundTripsThroughTheStrictReader(t *testing.T) {
	subject := testSubject()
	subject.Registry = "https://user:hunter2@registry.example.test/npm/?token=hunter2"
	subject.Platform = "linux/arm64"
	subject.Anonymous = true
	for name, probe := range map[string]extproto.MemberProbe{
		"absent":     subject.Absent(),
		"identical":  subject.Held(localDigest),
		"conflict":   subject.Held(otherDigest),
		"unverified": subject.Unreachable(errors.New("no route to host")),
	} {
		for _, version := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s v%d", name, version), func(t *testing.T) {
				events := captureEvents(t, func() { Emit(jsonl.NewForVersion(version), probe) })
				if len(events) != 1 {
					t.Fatalf("Emit wrote %d events, want one", len(events))
				}
				event := events[0]
				if event["type"] != "artifact" || event["kind"] != extproto.MemberProbeEventKind {
					t.Fatalf("event = %+v, want a member-probe artifact", event)
				}
				if raw, _ := json.Marshal(event); strings.Contains(string(raw), "hunter2") {
					t.Fatalf("event %s carries the registry credential", raw)
				}
				decoded, err := Decode(event)
				if err != nil {
					t.Fatalf("Decode(%+v): %v", event, err)
				}
				if *decoded != probe {
					t.Fatalf("decoded = %+v, want the emitted probe %+v", *decoded, probe)
				}
				if decoded.Registry != "https://registry.example.test/npm" {
					t.Fatalf("registry = %q, want the credential-free endpoint", decoded.Registry)
				}
			})
		}
	}
}

// A field or a state this build does not know is refused: reading it leniently
// would drop a verdict the publisher stated.
func TestDecodeRefusesWhatTheProtocolDoesNotKnow(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{
			"type": "artifact", "kind": extproto.MemberProbeEventKind, "id": "npm", "name": "@acme/widget", "path": "",
			"ecosystem": "npm", "coordinate": "@acme/widget", "version": "1.4.0",
			"registry": "https://registry.example.test", "state": extproto.MemberProbeAbsent,
		}
	}
	if _, err := Decode(valid()); err != nil {
		t.Fatalf("Decode(valid): %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"unknown field":            func(event map[string]any) { event["tag"] = "latest" },
		"unknown state":            func(event map[string]any) { event["state"] = "present" },
		"conflict without reason":  func(event map[string]any) { event["state"] = extproto.MemberProbeConflict },
		"identical without digest": func(event map[string]any) { event["state"] = extproto.MemberProbeIdentical },
		"registry with credential": func(event map[string]any) { event["registry"] = "https://user:pw@registry.example.test" },
	} {
		event := valid()
		mutate(event)
		if probe, err := Decode(event); err == nil {
			t.Errorf("%s: Decode accepted %+v", name, probe)
		}
	}
}
