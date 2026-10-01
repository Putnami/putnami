package events

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestConformance_ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			var diags []diag.Diagnostic
			base := filepath.Base(path)
			switch {
			case strings.Contains(base, "envelope"):
				_, diags = ParseAndValidateEnvelope(data)
			case strings.Contains(base, "capabilities"):
				_, diags = ParseAndValidateEventServerCapabilities(data)
			case strings.Contains(base, "error"):
				_, diags = ParseAndValidateEventServerError(data)
			default:
				_, diags = ParseAndValidateEventServerFrame(data)
			}
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
		})
	}
}

func TestConformance_InvalidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			var diags []diag.Diagnostic
			base := filepath.Base(path)
			switch {
			case strings.Contains(base, "envelope"):
				_, diags = ParseAndValidateEnvelope(data)
			case strings.Contains(base, "capabilities"):
				_, diags = ParseAndValidateEventServerCapabilities(data)
			case strings.Contains(base, "error"):
				_, diags = ParseAndValidateEventServerError(data)
			default:
				_, diags = ParseAndValidateEventServerFrame(data)
			}
			if !diag.HasErrors(diags) {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
		})
	}
}

func TestConformance_HandlerOptionDefaults(t *testing.T) {
	opts := NormalizedHandlerOptions(HandlerOptions{})
	if opts.Distribution != DistributionCompeting {
		t.Fatalf("default distribution = %q, want %q", opts.Distribution, DistributionCompeting)
	}
	if opts.MaxRetries != DefaultMaxRetries {
		t.Fatalf("default maxRetries = %d, want %d", opts.MaxRetries, DefaultMaxRetries)
	}
	if opts.DLQ == nil || *opts.DLQ != true {
		t.Fatal("default dlq must be true")
	}
	if diag.HasErrors(ValidateHandlerOptions(opts)) {
		t.Fatalf("normalized defaults should be valid")
	}
}

func TestConformance_GRPCProtoIsNonNormativeReference(t *testing.T) {
	data, err := os.ReadFile("proto/putnami/events/v1/event_server.proto")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// The proto is a non-normative reference: the JSON schemas and fixtures are
	// the canonical source of truth (doc/adr/0001-schemas-are-the-source-of-truth.md).
	// Guard the demotion so the proto cannot silently regress into a second
	// source of truth or get rewired to code generation without a deliberate
	// decision — either would make one endpoint profile's description authoritative
	// over the three that carry the same vocabulary as JSON.
	if !strings.Contains(content, "NON-NORMATIVE") {
		t.Error("event_server.proto must carry the NON-NORMATIVE reference banner")
	}
	if strings.Contains(content, "option go_package") {
		t.Error("event_server.proto must not declare option go_package: the proto is not used for code generation")
	}

	// It still documents the gRPC endpoint profile, so the RPC surface stays
	// available as reference.
	required := []string{
		"package putnami.events.v1;",
		"service EventServer",
		"rpc Capabilities(CapabilitiesRequest) returns (EventServerCapabilities);",
		"rpc Subscribe(SubscribeRequest) returns (stream EventServerFrame);",
		"rpc Publish(PublishRequest) returns (PublishResponse);",
		"rpc Health(HealthRequest) returns (HealthResponse);",
	}
	for _, marker := range required {
		if !strings.Contains(content, marker) {
			t.Errorf("event_server.proto missing %q", marker)
		}
	}
}

func TestConformance_Manifest(t *testing.T) {
	data, err := os.ReadFile("conformance/manifest.json")
	if err != nil {
		t.Fatal(err)
	}

	manifest, diags := ParseConformanceManifestStrict(data)
	if diag.HasErrors(diags) {
		t.Fatalf("conformance manifest produced errors: %v", diags)
	}
	if manifest.Protocol != Protocol {
		t.Fatalf("manifest protocol = %q, want %q", manifest.Protocol, Protocol)
	}
	if manifest.Suite != ConformanceSuite {
		t.Fatalf("manifest suite = %q, want %q", manifest.Suite, ConformanceSuite)
	}

	requiredCases := map[string]bool{
		"discovery.capabilities.valid": false,
		"http.publish.valid":           false,
		"sse.subscribe.realtime":       false,
		"websocket.subscribe.realtime": false,
		"grpc.capabilities.valid":      false,
		"grpc.publish.subscribe":       false,
		"push.receiver.valid":          false,
		"push.receiver.auth-rejected":  false,
	}
	for _, testCase := range manifest.Cases {
		if _, ok := requiredCases[testCase.ID]; ok {
			requiredCases[testCase.ID] = true
		}
	}
	for id, found := range requiredCases {
		if !found {
			t.Fatalf("conformance manifest missing required case %q", id)
		}
	}
}
