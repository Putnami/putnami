package doctor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// sampleReport builds a representative production-profile report exercising
// every type: a finding with evidence and no waiver (critical), a finding with
// evidence and a matching waiver (high), and a finding with neither evidence
// nor waiver (warning). It is the shared input for the byte-stability and
// canonical-form guards.
func sampleReport() *Report {
	return &Report{
		Schema:          "https://putnami.dev/schemas/putnami-doctor.json",
		ProtocolVersion: ProtocolVersion,
		Profile:         ProfileProduction,
		Findings: []Finding{
			{
				Code:     CheckEphemeralSigningKey,
				Severity: SeverityCritical,
				Profile:  ProfileProduction,
				Project:  "go.putnami.dev/example/iam",
				Message:  "the auth signing key is generated in-process and will not survive a restart",
				Evidence: []Evidence{
					{Path: "example/iam/auth.go", Field: "security.jwt.signingKey"},
				},
				Remediation: CheckRemediations[CheckEphemeralSigningKey],
			},
			{
				Code:     CheckVolatilePersistence,
				Severity: SeverityHigh,
				Profile:  ProfileProduction,
				Project:  "go.putnami.dev/example/iam",
				Message:  "datasource \"default\" is bound to an in-memory store",
				Evidence: []Evidence{
					{Path: "example/iam/config.yaml", Field: "database.default.driver"},
				},
				Remediation: CheckRemediations[CheckVolatilePersistence],
				WaivedBy: &WaiverProvenance{
					Owner:   "platform-team",
					Reason:  "staging bake still uses the in-memory store; tracked in PUT-1234",
					Expires: "2027-06-30",
				},
			},
			{
				Code:        CheckInsecureTransport,
				Severity:    SeverityWarning,
				Profile:     ProfileProduction,
				Project:     "go.putnami.dev/example/gateway",
				Message:     "the metrics endpoint is served over plaintext HTTP",
				Remediation: CheckRemediations[CheckInsecureTransport],
			},
		},
		Summary: Summary{
			Findings: 3,
			Info:     0,
			Warning:  1,
			High:     1,
			Critical: 1,
			Waived:   1,
		},
	}
}

// sampleWaiverFile builds a representative waiver file exercising every Waiver
// field: a project-scoped waiver and a workspace-wide (no project) waiver.
func sampleWaiverFile() *WaiverFile {
	return &WaiverFile{
		Schema:          "https://putnami.dev/schemas/putnami-doctor.json",
		ProtocolVersion: ProtocolVersion,
		Waivers: []Waiver{
			{
				Code:    CheckVolatilePersistence,
				Project: "go.putnami.dev/example/iam",
				Owner:   "platform-team",
				Reason:  "staging bake still uses the in-memory store; tracked in PUT-1234",
				Expires: "2027-06-30",
			},
			{
				Code:    CheckLocalRateLimit,
				Owner:   "gateway-team",
				Reason:  "single-replica preview deploy; distributed limiter lands in PUT-1300",
				Expires: "2027-12-31",
			},
		},
	}
}

// canonical returns the canonical serialization of v: json.MarshalIndent with
// two-space indentation and a trailing newline. This is the exact byte form
// every doctor producer must reproduce.
func canonical(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(data, '\n')
}

// TestSerialization_Stable100 verifies the canonical serialization of the same
// report and waiver file is byte-identical across 100 marshals — no map
// iteration or other nondeterminism leaks into the wire form.
func TestSerialization_Stable100(t *testing.T) {
	cases := map[string]any{
		"report":     sampleReport(),
		"waiverFile": sampleWaiverFile(),
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			first := canonical(t, v)
			for i := range 100 {
				if got := canonical(t, v); !bytes.Equal(got, first) {
					t.Fatalf("iteration %d: serialization changed\nfirst: %s\ngot:   %s", i, first, got)
				}
			}
		})
	}
}

// TestSerialization_CanonicalByteForm pins the exact canonical bytes: the
// committed fixtures must equal the Go serialization of the sample builders.
// This is the contract every producer of a doctor report or waiver file must
// reproduce byte-for-byte.
func TestSerialization_CanonicalByteForm(t *testing.T) {
	cases := []struct {
		path string
		v    any
	}{
		{filepath.Join("fixtures", "valid", "full.json"), sampleReport()},
		{filepath.Join("fixtures", "waivers", "valid", "full.json"), sampleWaiverFile()},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			want, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			got := canonical(t, tc.v)
			if !bytes.Equal(got, want) {
				t.Fatalf("canonical serialization does not match %s.\n--- got ---\n%s\n--- want ---\n%s", tc.path, got, want)
			}
		})
	}
}

// TestRoundTrip_Idempotent verifies parse → marshal → parse → marshal is
// byte-stable for every valid fixture: re-serializing a parsed document yields
// the same bytes the second time, so the wire form is a fixed point.
func TestRoundTrip_Idempotent(t *testing.T) {
	reportFiles, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(reportFiles) == 0 {
		t.Fatal("no valid report fixtures found")
	}
	for _, path := range reportFiles {
		t.Run("report/"+filepath.Base(path), func(t *testing.T) {
			assertRoundTrip(t, path, func(b []byte) (any, bool) {
				m, _ := ParseReport(b)
				return m, m != nil
			})
		})
	}

	waiverFiles, err := filepath.Glob("fixtures/waivers/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(waiverFiles) == 0 {
		t.Fatal("no valid waiver fixtures found")
	}
	for _, path := range waiverFiles {
		t.Run("waiver/"+filepath.Base(path), func(t *testing.T) {
			assertRoundTrip(t, path, func(b []byte) (any, bool) {
				w, _ := ParseWaiverFile(b)
				return w, w != nil
			})
		})
	}
}

func assertRoundTrip(t *testing.T, path string, parse func([]byte) (any, bool)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v1, ok := parse(data)
	if !ok {
		t.Fatalf("parse %s produced nil document", path)
	}
	once := canonical(t, v1)
	v2, ok := parse(once)
	if !ok {
		t.Fatalf("re-parse %s produced nil document", path)
	}
	twice := canonical(t, v2)
	if !bytes.Equal(once, twice) {
		t.Fatalf("%s: round-trip not idempotent\nonce:  %s\ntwice: %s", path, once, twice)
	}
}
