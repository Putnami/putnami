package infra

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// deploymentGolden is the canonical deployment declaration of
// fixtures/valid/aggregated-merged.json: the fixture's manifest without its
// $schema, in struct member order, compact, with no trailing newline.
const deploymentGolden = `{"protocolVersion":2,"workload":"go.putnami.dev/example/api",` +
	`"databases":[{"name":"primary","engine":"postgres","schemas":["audit","iam"],"sources":[{"project":"go.putnami.dev/example/audit","contributor":"manual"},{"project":"go.putnami.dev/example/iam","contributor":"framework:go.putnami.dev/database"}]}],` +
	`"events":{"publishes":[{"name":"workspace.deploy.completed","sources":[{"project":"go.putnami.dev/example/api","contributor":"manual"}]}],"subscribes":[{"name":"billing.invoice.issued","sources":[{"project":"go.putnami.dev/example/billing","contributor":"manual"}]}]},` +
	`"storage":[{"name":"audit-logs","access":"write","public":true,"retention":"90d","sources":[{"project":"go.putnami.dev/example/audit","contributor":"manual"}]}],` +
	`"secrets":[{"name":"jwks_signing_key","sources":[{"project":"go.putnami.dev/example/iam","contributor":"manual"}]}],` +
	`"scheduledJobs":[{"name":"nightly-rollup","schedule":"0 2 * * *","entrypoint":"jobs.NightlyRollup","sources":[{"project":"go.putnami.dev/example/audit","contributor":"manual"}]}],` +
	`"runtime":{"ingress":{"domain":"api.example.com","public":true},"scaling":{"max":100,"concurrency":80},"protocols":{"http2":false}}}`

func mergedFixtureManifest(t *testing.T) *AggregatedManifest {
	t.Helper()
	data, err := os.ReadFile("fixtures/valid/aggregated-merged.json")
	if err != nil {
		t.Fatal(err)
	}
	m, diagnostics := ParseAndValidateAggregatedManifest(data)
	if m == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("aggregated-merged fixture rejected: %v", diagnostics)
	}
	return m
}

// TestMarshalDeploymentWritesTheGoldenBytes pins the byte form: the same
// manifest always encodes to the same bytes, the bytes parse back, and the
// parsed manifest encodes to the same bytes again.
func TestMarshalDeploymentWritesTheGoldenBytes(t *testing.T) {
	m := mergedFixtureManifest(t)
	if m.Schema == "" {
		t.Fatal("the fixture must carry a $schema for this test to prove it is dropped")
	}
	first, diagnostics := MarshalDeployment(m)
	if diag.HasErrors(diagnostics) || string(first) != deploymentGolden {
		t.Fatalf("MarshalDeployment = %s %v\nwant %s", first, diagnostics, deploymentGolden)
	}
	second, _ := MarshalDeployment(mergedFixtureManifest(t))
	if !bytes.Equal(first, second) {
		t.Fatalf("two encodings of one manifest differ:\n%s\n%s", first, second)
	}
	if m.Schema == "" {
		t.Fatal("MarshalDeployment modified its input")
	}

	parsed, diagnostics := ParseDeployment([]byte(deploymentGolden))
	if parsed == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("the golden declaration was refused: %v", diagnostics)
	}
	again, diagnostics := MarshalDeployment(parsed)
	if diag.HasErrors(diagnostics) || string(again) != deploymentGolden {
		t.Fatalf("the round trip drifted: %s %v", again, diagnostics)
	}
}

// TestMarshalDeploymentRefusesAManifestThatDoesNotValidate pins that no bytes
// leave the encoder for a manifest a reader would refuse.
func TestMarshalDeploymentRefusesAManifestThatDoesNotValidate(t *testing.T) {
	if data, diagnostics := MarshalDeployment(nil); data != nil || !findCode(diagnostics, ErrorCodeParseError) {
		t.Fatalf("MarshalDeployment(nil) = %s %v", data, diagnostics)
	}
	m := mergedFixtureManifest(t)
	m.Workload = ""
	if data, diagnostics := MarshalDeployment(m); data != nil || !findCode(diagnostics, ErrorCodeMissingWorkload) {
		t.Fatalf("a manifest without a workload encoded to %s %v", data, diagnostics)
	}
	m = mergedFixtureManifest(t)
	m.ProtocolVersion = 1
	if data, diagnostics := MarshalDeployment(m); data != nil || !findCode(diagnostics, ErrorCodeInvalidProtocolVersion) {
		t.Fatalf("a manifest at protocol version 1 encoded to %s %v", data, diagnostics)
	}
}

// TestParseDeploymentRefusesEveryOtherSpelling pins the reader: only the
// canonical bytes of a valid manifest are a deployment declaration.
func TestParseDeploymentRefusesEveryOtherSpelling(t *testing.T) {
	var indented bytes.Buffer
	if err := json.Indent(&indented, []byte(deploymentGolden), "", "  "); err != nil {
		t.Fatal(err)
	}
	reordered := strings.Replace(deploymentGolden,
		`{"protocolVersion":2,"workload":"go.putnami.dev/example/api",`,
		`{"workload":"go.putnami.dev/example/api","protocolVersion":2,`, 1)
	withSchema := `{"$schema":"https://putnami.dev/schemas/putnami-infra.json",` + strings.TrimPrefix(deploymentGolden, "{")
	unknownField := `{"extra":true,` + strings.TrimPrefix(deploymentGolden, "{")
	duplicate := `{"workload":"go.putnami.dev/example/other",` + strings.TrimPrefix(deploymentGolden, "{")
	unescaped := `{"protocolVersion":2,"workload":"w<b>","runtime":{"scaling":{"max":1}}}`
	invalid := strings.Replace(deploymentGolden, `"engine":"postgres"`, `"engine":"oracle"`, 1)

	cases := []struct {
		name string
		data string
		code string
	}{
		{"indented", indented.String(), ErrorCodeNonCanonical},
		{"reordered", reordered, ErrorCodeNonCanonical},
		{"trailing newline", deploymentGolden + "\n", ErrorCodeNonCanonical},
		{"leading space", " " + deploymentGolden, ErrorCodeNonCanonical},
		{"$schema", withSchema, ErrorCodeNonCanonical},
		{"duplicate member", duplicate, ErrorCodeNonCanonical},
		{"unescaped html", unescaped, ErrorCodeNonCanonical},
		{"unknown field", unknownField, ErrorCodeUnknownField},
		{"invalid engine", invalid, ErrorCodeInvalidEngine},
		{"not json", `{"protocolVersion":`, ErrorCodeParseError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.data == deploymentGolden {
				t.Fatal("the case did not change the golden bytes")
			}
			m, diagnostics := ParseDeployment([]byte(tc.data))
			if m != nil || !findCode(diagnostics, tc.code) {
				t.Fatalf("ParseDeployment(%s) = %#v %v, want %s", tc.name, m, diagnostics, tc.code)
			}
		})
	}

	// The escaped spelling of the same workload is the canonical one.
	escaped := `{"protocolVersion":2,"workload":"w\u003cb\u003e","runtime":{"scaling":{"max":1}}}`
	if m, diagnostics := ParseDeployment([]byte(escaped)); m == nil || diag.HasErrors(diagnostics) {
		t.Fatalf("the escaped spelling was refused: %v", diagnostics)
	}
}
