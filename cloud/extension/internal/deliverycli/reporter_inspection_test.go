package deliverycli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// reporterUnderTest is one reporter entrypoint with the receiver it posts to.
type reporterUnderTest struct {
	name     string
	tokenEnv string
	serve    func(env map[string]string, in io.Reader, out, logw io.Writer) error
	ingest   func(token string) (http.Handler, func() int)
}

func reportersUnderTest() []reporterUnderTest {
	return []reporterUnderTest{
		{
			name: protocolcli.SessionReporterCommand, tokenEnv: protocolcli.SessionReporterTokenEnv, serve: serveSessionReporter,
			ingest: func(token string) (http.Handler, func() int) {
				ingest := &fakeIngest{token: token}
				return ingest, func() int { ingest.mu.Lock(); defer ingest.mu.Unlock(); return len(ingest.posts) }
			},
		},
		{
			name: protocolcli.LogReporterCommand, tokenEnv: protocolcli.LogReporterTokenEnv, serve: serveLogReporter,
			ingest: func(token string) (http.Handler, func() int) {
				ingest := &fakeLogIngest{token: token}
				return ingest, func() int { ingest.mu.Lock(); defer ingest.mu.Unlock(); return len(ingest.posts) }
			},
		},
	}
}

// stubReporterDenyInspection replaces the reporters' inspection guard for one
// test. The seam is a package variable, so these tests do not run in parallel.
func stubReporterDenyInspection(t *testing.T, deny func() error) {
	t.Helper()
	previous := reporterDenyInspection
	reporterDenyInspection = deny
	t.Cleanup(func() { reporterDenyInspection = previous })
}

func serveOneReporterFrame(t *testing.T, reporter reporterUnderTest, env map[string]string) ([]protocolcli.SessionReportingAck, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	in := strings.NewReader(frame(t, "20261003-110000-abc123", "events.jsonl", 0, 0, `{"record":"task:start"}`+"\n", false))
	if err := reporter.serve(env, in, &out, &stderr); err != nil {
		t.Fatalf("%s: %v", reporter.name, err)
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	acks := make([]protocolcli.SessionReportingAck, 0, len(lines))
	for _, line := range lines {
		ack, err := protocolcli.ParseSessionReportingAck(line)
		if err != nil {
			t.Fatalf("%s ack %q: %v", reporter.name, line, err)
		}
		acks = append(acks, *ack)
	}
	return acks, stderr.String()
}

// The guard runs before the reporter reads its configuration. The stub writes
// the ingest base and the token into env only when it runs, so a reporter that
// read them first would hold no destination and a stale token, and the
// receiver would never accept its frame.
func TestReportersDenyInspectionBeforeReadingTheirCredential(t *testing.T) {
	for _, reporter := range reportersUnderTest() {
		t.Run(reporter.name, func(t *testing.T) {
			handler, posts := reporter.ingest("run-secret")
			srv := httptest.NewServer(handler)
			defer srv.Close()
			env := map[string]string{reporter.tokenEnv: "stale-before-the-guard"}
			denied := 0
			stubReporterDenyInspection(t, func() error {
				denied++
				env[SessionReporterIngestURLEnv] = srv.URL + "/ingest"
				env[reporter.tokenEnv] = "run-secret"
				return nil
			})

			acks, stderr := serveOneReporterFrame(t, reporter, env)
			if denied != 1 {
				t.Fatalf("inspection guard ran %d times, want 1", denied)
			}
			if len(acks) != 1 || !acks[0].OK {
				t.Fatalf("acks = %+v; the reporter read its configuration before the guard", acks)
			}
			if posts() != 1 || stderr != "" {
				t.Fatalf("posts = %d, stderr = %q", posts(), stderr)
			}
		})
	}
}

// A reporter that cannot deny inspection keeps serving, like the cache
// provider, but does not use its token: no frame reaches the receiver, each
// one gets a terminal inspection_guard_unavailable NACK, and the diagnostic
// does not quote the token.
func TestReportersRefuseTheirCredentialWhenInspectionCannotBeDenied(t *testing.T) {
	for _, reporter := range reportersUnderTest() {
		t.Run(reporter.name, func(t *testing.T) {
			handler, posts := reporter.ingest("run-secret")
			srv := httptest.NewServer(handler)
			defer srv.Close()
			env := map[string]string{SessionReporterIngestURLEnv: srv.URL + "/ingest", reporter.tokenEnv: "run-secret"}
			denied := 0
			stubReporterDenyInspection(t, func() error {
				denied++
				return errors.New("prctl refused")
			})

			acks, stderr := serveOneReporterFrame(t, reporter, env)
			if denied != 1 {
				t.Fatalf("inspection guard ran %d times, want 1", denied)
			}
			if len(acks) != 1 || acks[0].OK || acks[0].Code != ackCodeInspectionGuard || acks[0].Retryable {
				t.Fatalf("acks = %+v, want one terminal %s NACK", acks, ackCodeInspectionGuard)
			}
			if posts() != 0 {
				t.Fatalf("an unguarded reporter posted %d frames", posts())
			}
			if !strings.Contains(stderr, "putnami-cloud "+reporter.name+": cannot deny process inspection (prctl refused)") ||
				strings.Contains(stderr, "run-secret") {
				t.Fatalf("stderr = %q", stderr)
			}
		})
	}
}
