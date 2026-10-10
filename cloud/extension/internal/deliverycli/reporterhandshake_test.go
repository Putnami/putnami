package deliverycli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

const (
	handshakeCredential = "hosted-run-credential-3a7d-held-in-memory"
	handshakeEnvToken   = "env-token-of-a-protocol-1-run"
	handshakeSessionID  = "20261004-110000-abc123"
)

// handshakeLine encodes one engine handshake line with the published
// constructors, as the engine does.
func handshakeLine(t *testing.T, line protocolcli.SessionReportingHandshake) string {
	t.Helper()
	encoded, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func reporterInitializeLine(t *testing.T) string {
	return handshakeLine(t, protocolcli.NewSessionReportingInitialize())
}

func reporterAuthenticateLine(t *testing.T, credential string) string {
	return handshakeLine(t, protocolcli.NewSessionReportingAuthenticate(credential))
}

func eventsFrame(t *testing.T) string {
	return frame(t, handshakeSessionID, "events.jsonl", 0, 0, `{"record":"task:start"}`+"\n", false)
}

// reporterSession is what one reporter process wrote: its stdout lines, its
// stderr, and its posts to the receiver.
type reporterSession struct {
	lines  [][]byte
	stderr string
	posts  int
	err    error
}

// serveReporterLines runs reporter's real entrypoint (the guard, then the
// configuration, then the loop) on lines, against a receiver that accepts only
// accepted as its bearer. env carries the ingest base and envToken.
func serveReporterLines(t *testing.T, reporter reporterUnderTest, accepted, envToken string, lines ...string) reporterSession {
	t.Helper()
	handler, posts := reporter.ingest(accepted)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	env := map[string]string{SessionReporterIngestURLEnv: srv.URL + "/ingest"}
	if envToken != "" {
		env[reporter.tokenEnv] = envToken
	}
	var out, stderr bytes.Buffer
	err := reporter.serve(env, strings.NewReader(strings.Join(lines, "")), &out, &stderr)
	session := reporterSession{stderr: stderr.String(), posts: posts(), err: err}
	if trimmed := bytes.TrimSpace(out.Bytes()); len(trimmed) > 0 {
		session.lines = bytes.Split(trimmed, []byte("\n"))
	}
	// No line the reporter writes, on either stream, ever carries a credential.
	for _, secret := range []string{handshakeCredential, handshakeEnvToken} {
		if strings.Contains(out.String(), secret) || strings.Contains(session.stderr, secret) || (err != nil && strings.Contains(err.Error(), secret)) {
			t.Fatalf("%s wrote a credential: stdout %q stderr %q err %v", reporter.name, out.String(), session.stderr, err)
		}
	}
	return session
}

// requireAnswer parses line as the engine does and checks that it answers op
// with code ("" for acceptance).
func requireAnswer(t *testing.T, line []byte, op, code string) {
	t.Helper()
	result, err := protocolcli.ParseSessionReportingHandshakeResult(line)
	if err != nil {
		t.Fatalf("answer %q does not parse: %v", line, err)
	}
	if !result.Answers(protocolcli.SessionReportingHandshake{ProtocolVersion: protocolcli.SessionReportingCredentialVersion, Op: op}) {
		t.Fatalf("answer %q does not answer %s", line, op)
	}
	if result.OK != (code == "") || result.Code != code {
		t.Fatalf("answer to %s = %+v, want code %q", op, result, code)
	}
}

func requireAck(t *testing.T, line []byte, code string) {
	t.Helper()
	ack, err := protocolcli.ParseSessionReportingAck(line)
	if err != nil {
		t.Fatalf("ack %q does not parse: %v", line, err)
	}
	if !ack.Matches(*mustChunk(t, handshakeSessionID, "events.jsonl", 0, 0, `{"record":"task:start"}`+"\n", false)) {
		t.Fatalf("ack %+v does not echo the frame", ack)
	}
	if ack.OK != (code == "") || ack.Code != code {
		t.Fatalf("ack = %+v, want code %q", ack, code)
	}
}

func requireLines(t *testing.T, session reporterSession, want int) {
	t.Helper()
	if session.err != nil {
		t.Fatalf("reporter: %v", session.err)
	}
	if len(session.lines) != want {
		t.Fatalf("reporter wrote %d lines, want %d: %q", len(session.lines), want, session.lines)
	}
}

// A hosted run hands the credential over the handshake. The reporter accepts
// both lines and posts every chunk under that credential, even when its
// environment also holds a token: the receiver here accepts only the
// handshake's credential.
func TestReportersPostUnderTheCredentialTheHandshakeHanded(t *testing.T) {
	for _, reporter := range reportersUnderTest() {
		t.Run(reporter.name, func(t *testing.T) {
			stubReporterDenyInspection(t, func() error { return nil })
			for _, envToken := range []string{"", handshakeEnvToken} {
				session := serveReporterLines(t, reporter, handshakeCredential, envToken,
					reporterInitializeLine(t), reporterAuthenticateLine(t, handshakeCredential), eventsFrame(t))
				requireLines(t, session, 3)
				requireAnswer(t, session.lines[0], protocolcli.SessionReportingOpInitialize, "")
				requireAnswer(t, session.lines[1], protocolcli.SessionReportingOpAuthenticate, "")
				requireAck(t, session.lines[2], "")
				if session.posts != 1 || session.stderr != "" {
					t.Fatalf("env token %q: posts = %d, stderr = %q", envToken, session.posts, session.stderr)
				}
			}
		})
	}
}

// An engine without a run credential sends no handshake: the first line is a
// chunk, and the reporter posts it under its environment token, as protocol 1
// does. From that chunk on every line is a chunk, so a later handshake line is
// a malformed frame that stops the provider without an answer.
func TestReportersWithoutHandshakeKeepTheEnvironmentToken(t *testing.T) {
	for _, reporter := range reportersUnderTest() {
		t.Run(reporter.name, func(t *testing.T) {
			stubReporterDenyInspection(t, func() error { return nil })
			session := serveReporterLines(t, reporter, handshakeEnvToken, handshakeEnvToken, eventsFrame(t))
			requireLines(t, session, 1)
			requireAck(t, session.lines[0], "")

			late := serveReporterLines(t, reporter, handshakeEnvToken, handshakeEnvToken,
				eventsFrame(t), reporterInitializeLine(t), reporterAuthenticateLine(t, handshakeCredential))
			if late.err == nil || len(late.lines) != 1 || late.posts != 1 {
				t.Fatalf("a handshake after a chunk: err %v, lines %q, posts %d", late.err, late.lines, late.posts)
			}
			requireAck(t, late.lines[0], "")
		})
	}
}

// A malformed or oversize credential is refused with invalid_run_credential
// and never becomes the bearer: the chunk after the refusal still goes out
// under the environment token, which is the only one this receiver accepts.
// The diagnostic names the code, never the credential.
func TestReportersRefuseAMalformedCredential(t *testing.T) {
	oversize := strings.Repeat("c", protocolcli.SessionReportingMaxCredentialBytes+1)
	cases := map[string]string{
		"empty":           `{"protocolVersion":2,"op":"authenticate","runCredential":""}`,
		"absent":          `{"protocolVersion":2,"op":"authenticate"}`,
		"null":            `{"protocolVersion":2,"op":"authenticate","runCredential":null}`,
		"not a string":    `{"protocolVersion":2,"op":"authenticate","runCredential":42}`,
		"space":           `{"protocolVersion":2,"op":"authenticate","runCredential":"hosted run"}`,
		"no-break space":  `{"protocolVersion":2,"op":"authenticate","runCredential":"hosted\u00a0run"}`,
		"wrong case":      `{"protocolVersion":2,"op":"authenticate","RUNCREDENTIAL":"` + handshakeCredential + `"}`,
		"one byte over":   `{"protocolVersion":2,"op":"authenticate","runCredential":"` + oversize + `"}`,
		"over the line":   `{"protocolVersion":2,"op":"authenticate","runCredential":"` + strings.Repeat("c", protocolcli.SessionReportingLineBytes) + `"}`,
		"multi-byte over": `{"protocolVersion":2,"op":"authenticate","runCredential":"` + strings.Repeat("é", protocolcli.SessionReportingMaxCredentialBytes/2+1) + `"}`,
	}
	for _, reporter := range reportersUnderTest() {
		for name, authenticate := range cases {
			t.Run(reporter.name+"/"+name, func(t *testing.T) {
				stubReporterDenyInspection(t, func() error { return nil })
				session := serveReporterLines(t, reporter, handshakeEnvToken, handshakeEnvToken,
					reporterInitializeLine(t), authenticate+"\n", eventsFrame(t))
				requireLines(t, session, 3)
				requireAnswer(t, session.lines[0], protocolcli.SessionReportingOpInitialize, "")
				requireAnswer(t, session.lines[1], protocolcli.SessionReportingOpAuthenticate, handshakeCodeInvalidCredential)
				requireAck(t, session.lines[2], "")
				want := "putnami-cloud " + reporter.name + ": refused authenticate: " + handshakeCodeInvalidCredential + "\n"
				if session.stderr != want {
					t.Fatalf("stderr = %q, want %q", session.stderr, want)
				}
				if strings.Contains(session.stderr, "hosted") || strings.Contains(session.stderr, "ccc") {
					t.Fatalf("stderr quotes the credential: %q", session.stderr)
				}
			})
		}
	}
}

// A handshake line that is not one the engine sends is refused with a code
// and changes nothing: a valid credential beside an unknown member, an
// initialize that carries a credential, a line out of order. Every case ends
// with a chunk the receiver accepts only under the environment token, so a
// refused line never handed its credential over.
func TestReportersRefuseHandshakeLinesTheEngineNeverSends(t *testing.T) {
	type answer struct{ op, code string }
	opInit, opAuth := protocolcli.SessionReportingOpInitialize, protocolcli.SessionReportingOpAuthenticate
	cases := []struct {
		name    string
		lines   []string
		answers []answer
	}{
		{
			name:    "unknown member beside a valid credential",
			lines:   []string{reporterInitializeLine(t), `{"protocolVersion":2,"op":"authenticate","runCredential":"` + handshakeCredential + `","scope":"all"}` + "\n"},
			answers: []answer{{opInit, ""}, {opAuth, handshakeCodeInvalidLine}},
		},
		{
			name:    "initialize carrying a credential",
			lines:   []string{`{"protocolVersion":2,"op":"initialize","runCredential":"` + handshakeCredential + `"}` + "\n", reporterAuthenticateLine(t, handshakeCredential)},
			answers: []answer{{opInit, handshakeCodeInvalidLine}, {opAuth, handshakeCodeOutOfOrder}},
		},
		{
			name:    "authenticate before initialize",
			lines:   []string{reporterAuthenticateLine(t, handshakeCredential)},
			answers: []answer{{opAuth, handshakeCodeOutOfOrder}},
		},
		{
			name:    "initialize twice",
			lines:   []string{reporterInitializeLine(t), reporterInitializeLine(t)},
			answers: []answer{{opInit, ""}, {opInit, handshakeCodeOutOfOrder}},
		},
	}
	for _, reporter := range reportersUnderTest() {
		for _, tc := range cases {
			t.Run(reporter.name+"/"+tc.name, func(t *testing.T) {
				stubReporterDenyInspection(t, func() error { return nil })
				session := serveReporterLines(t, reporter, handshakeEnvToken, handshakeEnvToken, append(tc.lines, eventsFrame(t))...)
				requireLines(t, session, len(tc.answers)+1)
				for i, want := range tc.answers {
					requireAnswer(t, session.lines[i], want.op, want.code)
				}
				requireAck(t, session.lines[len(tc.answers)], "")
			})
		}
	}
}

// A second authenticate is refused, and the first credential stays the
// bearer: the receiver accepts only that one.
func TestReportersKeepTheFirstCredential(t *testing.T) {
	for _, reporter := range reportersUnderTest() {
		t.Run(reporter.name, func(t *testing.T) {
			stubReporterDenyInspection(t, func() error { return nil })
			session := serveReporterLines(t, reporter, handshakeCredential, "",
				reporterInitializeLine(t), reporterAuthenticateLine(t, handshakeCredential), reporterAuthenticateLine(t, "a-later-credential"), eventsFrame(t))
			requireLines(t, session, 4)
			requireAnswer(t, session.lines[1], protocolcli.SessionReportingOpAuthenticate, "")
			requireAnswer(t, session.lines[2], protocolcli.SessionReportingOpAuthenticate, handshakeCodeOutOfOrder)
			requireAck(t, session.lines[3], "")
		})
	}
}

// A process that cannot deny inspection refuses initialize, so the engine
// never sends it the credential and starts it again without one. Were
// authenticate sent anyway, it is refused unread, and no chunk reaches the
// receiver.
func TestReportersThatCannotDenyInspectionRefuseInitialize(t *testing.T) {
	for _, reporter := range reportersUnderTest() {
		t.Run(reporter.name, func(t *testing.T) {
			stubReporterDenyInspection(t, func() error { return errors.New("prctl refused") })
			session := serveReporterLines(t, reporter, handshakeCredential, "",
				reporterInitializeLine(t), reporterAuthenticateLine(t, handshakeCredential), eventsFrame(t))
			requireLines(t, session, 3)
			requireAnswer(t, session.lines[0], protocolcli.SessionReportingOpInitialize, ackCodeInspectionGuard)
			requireAnswer(t, session.lines[1], protocolcli.SessionReportingOpAuthenticate, handshakeCodeOutOfOrder)
			requireAck(t, session.lines[2], ackCodeInspectionGuard)
			if session.posts != 0 {
				t.Fatalf("an unguarded reporter posted %d frames", session.posts)
			}
		})
	}
}

// Only a version 2 line naming one of the two operations opens the
// handshake. Anything else goes to the chunk parser, as in protocol 1.
func TestHandshakeOpRecognizesOnlyHandshakeLines(t *testing.T) {
	for _, line := range []string{
		`{"protocolVersion":2,"op":"initialize"}`,
		`{"protocolVersion":2,"op":"authenticate","runCredential":"x"}`,
	} {
		if _, ok := handshakeOp([]byte(line)); !ok {
			t.Fatalf("handshakeOp(%q) = false, want true", line)
		}
	}
	for _, line := range []string{
		`{"protocolVersion":1,"op":"initialize"}`,
		`{"protocolVersion":2,"op":"shutdown"}`,
		`{"protocolVersion":"2","op":"initialize"}`,
		`not json`,
		strings.TrimSpace(frame(t, handshakeSessionID, "events.jsonl", 0, 0, "a\n", false)),
	} {
		if _, ok := handshakeOp([]byte(line)); ok {
			t.Fatalf("handshakeOp(%q) = true, want false", line)
		}
	}
}

// The configuration that holds the credential prints without it, whatever
// the verb.
func TestReporterConfigFormatsWithoutItsBearer(t *testing.T) {
	cfg := sessionReporterConfig{ingestURL: "https://ingest.invalid", token: handshakeCredential}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		for _, value := range []any{cfg, &cfg} {
			if got := fmt.Sprintf(verb, value); strings.Contains(got, handshakeCredential) || !strings.Contains(got, "<redacted>") {
				t.Fatalf("Sprintf(%s) = %q", verb, got)
			}
		}
	}
	if got := fmt.Sprint(sessionReporterConfig{}); strings.Contains(got, "<redacted>") {
		t.Fatalf("an empty bearer prints as redacted: %q", got)
	}
}
