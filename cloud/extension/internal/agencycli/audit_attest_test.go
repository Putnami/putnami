package agencycli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

const record = `{"repo":"acme/app","project":"/app","group":"security","input_hash":"i1","rubric_hash":"r1","head_sha":"55e2a8e","audit_version":"3","verified_at":"2026-11-19T09:00:00Z","created_at":"2026-11-19T09:00:01Z"}`

func keyFlags(pairs ...any) map[string]any {
	flags := params("repo", "acme/app", "project", "/app", "group", "security", "input-hash", "i1", "rubric-hash", "r1")
	for i := 0; i+1 < len(pairs); i += 2 {
		flags[pairs[i].(string)] = pairs[i+1]
	}
	return flags
}

// TestAuditAttestReadsAndWrites reads, records and prefetches attestations
// through agency-api's generated client, and refuses an incomplete key or
// lookup before anything is sent.
func TestAuditAttestReadsAndWrites(t *testing.T) {
	spectest.Proves(t, feature, "audit-attest-reads-and-writes", "agency-audit-attest")
	s := newStandIn(t)
	s.answer("POST /api/agency/audit/attestations/lookup", http.StatusOK, `{"results":[{"key":"k1","found":true,"record":`+record+`}]}`)
	run := newRun(t, s)
	if err := AuditAttest(keyFlags(), []string{"get"}, run.root, run.env, run.ioctx, nil); err != nil ||
		!strings.Contains(run.text(), "head 55e2a8e verified at 2026-11-19T09:00:00Z (audit 3)") {
		t.Fatalf("get: %v, %q", err, run.text())
	}
	lookups, _ := s.got()[0].body["lookups"].([]any)
	if len(lookups) != 1 || lookups[0].(map[string]any)["input_hash"] != "i1" || s.got()[0].authorization != "Bearer "+run.bearer {
		t.Fatalf("lookup sent %+v", s.got()[0])
	}

	s = newStandIn(t)
	s.answer("PUT /api/agency/audit/attestations", http.StatusCreated, `{"recorded":true,"created":true,"key":"k1","record":`+record+`}`)
	run = newRun(t, s)
	if err := AuditAttest(keyFlags("head-sha", "55e2a8e", "audit-version", "3"), []string{"set"}, run.root, run.env, run.ioctx, nil); err != nil ||
		!strings.Contains(run.text(), "Recorded audit attestation") {
		t.Fatalf("set: %v, %q", err, run.text())
	}
	if body := s.got()[0].body; body["head_sha"] != "55e2a8e" || body["rubric_hash"] != "r1" {
		t.Fatalf("set sent %v", body)
	}

	s = newStandIn(t)
	s.answer("POST /api/agency/audit/attestations/lookup", http.StatusOK,
		`{"results":[{"label":"b","key":"k2","found":false},{"label":"a","key":"k1","found":true,"record":`+record+`}]}`)
	run = newRun(t, s)
	file := filepath.Join(t.TempDir(), "lookups.jsonl")
	lines := `{"label":"b","repo":"acme/app","project":"/app","group":"design","inputHash":"i2","rubricHash":"r1"}` + "\n\n" +
		`{"label":"a","repo":"acme/app","project":"/app","group":"security","inputHash":"i1","rubricHash":"r1"}` + "\n"
	if err := os.WriteFile(file, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AuditAttest(params("file", file), []string{"prefetch"}, run.root, run.env, run.ioctx, noInput{t}); err != nil || len(*run.out) != 2 {
		t.Fatalf("prefetch: %v, %v", err, *run.out)
	}
	var first, second auditPrefetchLine
	_ = json.Unmarshal([]byte((*run.out)[0]), &first)
	_ = json.Unmarshal([]byte((*run.out)[1]), &second)
	if first.Label != "b" || first.Found || second.Label != "a" || !second.Found || second.Record["headSha"] != "55e2a8e" {
		t.Fatalf("prefetch printed %v", *run.out)
	}

	s = newStandIn(t)
	run = newRun(t, s)
	refused := []func() error{
		func() error {
			return AuditAttest(params("repo", "acme/app"), []string{"get"}, run.root, run.env, run.ioctx, nil)
		},
		func() error { return AuditAttest(keyFlags(), []string{"set"}, run.root, run.env, run.ioctx, nil) },
		func() error {
			return AuditAttest(keyFlags("head-sha", "x", "audit-version", "3", "verified-at", "yesterday"), []string{"set"}, run.root, run.env, run.ioctx, nil)
		},
		func() error {
			return AuditAttest(params(), []string{"prefetch"}, run.root, run.env, run.ioctx, strings.NewReader("not json\n"))
		},
		func() error {
			return AuditAttest(params(), []string{"prefetch"}, run.root, run.env, run.ioctx, strings.NewReader(`{"repo":"acme/app"}`+"\n"))
		},
	}
	for i, command := range refused {
		if err := command(); clicore.ExitCode(err) != clicore.ExitUsage {
			t.Fatalf("refused case %d: err = %v", i, err)
		}
	}
	if len(s.got()) != 0 {
		t.Fatalf("agency-api received %+v", s.got())
	}

	s = newStandIn(t)
	s.answer("POST /api/agency/audit/attestations/lookup", http.StatusForbidden, envelope(403))
	run = newRun(t, s)
	if err := AuditAttest(keyFlags(), []string{"get"}, run.root, run.env, run.ioctx, nil); clicore.ExitCode(err) != clicore.ExitAPI {
		t.Fatalf("a refused lookup: %v", err)
	}
}

// TestCommandsUseTheGeneratedClients re-mints the bearer once when a service
// answers 401, through the session every generated call shares, and routes
// each command to its service.
func TestCommandsUseTheGeneratedClients(t *testing.T) {
	spectest.Proves(t, feature, "calls-go-through-generated-clients", "agency-commands-generated-clients")
	s := newStandIn(t)
	s.answer("GET "+triggerRoute("putnami"), http.StatusUnauthorized, envelope(401))
	run := newRun(t, s)
	err := Trigger(params(), []string{"show"}, run.root, run.env, run.ioctx, nil)
	if clicore.ExitCode(err) != clicore.ExitAuth || !strings.Contains(err.Error(), "putnami cloud login") {
		t.Fatalf("a rejected credential: %v", err)
	}
	if got := len(s.got()); got < 1 || got > 2 {
		t.Fatalf("a rejected credential sent %d requests, want the call and at most one re-minted repeat", got)
	}

	if err := Agency(params(), []string{"unknown"}, run.root, run.env, run.ioctx); clicore.ExitCode(err) != clicore.ExitUsage {
		t.Fatalf("an unknown command: %v", err)
	}
	if err := Agency(params(), nil, run.root, run.env, run.ioctx); err != nil || !strings.Contains(run.text(), "agency trigger register|show|remove") {
		t.Fatalf("help: %v, %q", err, run.text())
	}
}
