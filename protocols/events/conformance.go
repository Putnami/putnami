package events

import (
	"bytes"
	"encoding/json"
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ConformanceSuite is the public Event Server conformance runner contract.
const ConformanceSuite = "putnami.events.conformance.v1"

// ConformanceManifest describes the black-box test suite that public clients
// and private Event Server implementations should agree on.
type ConformanceManifest struct {
	// Protocol is the events protocol identifier (Protocol).
	Protocol string `json:"protocol"`
	// Suite is the conformance suite identifier (ConformanceSuite).
	Suite string `json:"suite"`
	// Defaults are the runner defaults applied to every case.
	Defaults ConformanceDefaults `json:"defaults,omitempty"`
	// Target locates the Event Server endpoints the runner exercises.
	Target ConformanceTarget `json:"target"`
	// Output declares the machine-readable output the runner must emit.
	Output ConformanceOutput `json:"output,omitempty"`
	// Cases is the ordered list of black-box checks to run.
	Cases []ConformanceCase `json:"cases"`
}

// ConformanceDefaults defines runner defaults applied to every case.
type ConformanceDefaults struct {
	// TopicPrefix is prepended to every case's topics to isolate test traffic.
	TopicPrefix string `json:"topicPrefix,omitempty"`
	// TimeoutMs is the default per-case timeout, in milliseconds.
	TimeoutMs int `json:"timeoutMs,omitempty"`
}

// ConformanceTarget defines protocol endpoints the runner must discover or use.
type ConformanceTarget struct {
	// DiscoveryPath is the well-known discovery path the runner GETs
	// (EventServerDiscoveryPath).
	DiscoveryPath string `json:"discoveryPath"`
	// GRPCService is the fully-qualified gRPC service name the runner targets.
	GRPCService string `json:"grpcService"`
}

// ConformanceOutput defines machine-readable runner output expectations.
type ConformanceOutput struct {
	// Formats lists the output formats the runner must produce (e.g. "json").
	Formats []string `json:"formats,omitempty"`
	// RequiredFields lists the fields each result record must contain.
	RequiredFields []string `json:"requiredFields,omitempty"`
}

// ConformanceCase defines one black-box compatibility check.
type ConformanceCase struct {
	// ID is the unique case identifier.
	ID string `json:"id"`
	// Level is the conformance level: required, recommended, or optional.
	Level string `json:"level"`
	// Transport is the wire transport the case exercises (or "discovery"/push).
	Transport string `json:"transport"`
	// Requires gates the case on server capabilities; a case whose requirements
	// are unmet is skipped, not failed.
	Requires ConformanceCaseRequirements `json:"requires,omitempty"`
	// Steps are the ordered actions the runner performs for the case.
	Steps []string `json:"steps"`
	// Expect are the assertions the runner checks after the steps.
	Expect []string `json:"expect"`
}

// ConformanceCaseRequirements controls when a case is applicable.
type ConformanceCaseRequirements struct {
	// Transports restricts the case to servers hosting these transports.
	Transports []EventServerTransport `json:"transports,omitempty"`
	// Features restricts the case to servers advertising these features.
	Features []string `json:"features,omitempty"`
	// Auth restricts the case to servers accepting these auth schemes.
	Auth []AuthScheme `json:"auth,omitempty"`
}

// ParseConformanceManifestStrict decodes a conformance manifest and rejects
// unknown fields.
func ParseConformanceManifestStrict(data []byte) (*ConformanceManifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var manifest ConformanceManifest
	if err := dec.Decode(&manifest); err != nil {
		return nil, []diag.Diagnostic{
			diag.Errorf("parse-error", "", "failed to parse conformance manifest: %v", err),
		}
	}
	return &manifest, ValidateConformanceManifest(&manifest)
}

// ValidateConformanceManifest checks the public runner contract shape.
func ValidateConformanceManifest(manifest *ConformanceManifest) []diag.Diagnostic {
	if manifest == nil {
		return []diag.Diagnostic{diag.Errorf("nil-manifest", "", "conformance manifest is nil")}
	}

	var diags []diag.Diagnostic
	if manifest.Protocol != Protocol {
		diags = append(diags, diag.Errorf("invalid-protocol", "protocol", "expected %q, got %q", Protocol, manifest.Protocol))
	}
	if manifest.Suite != ConformanceSuite {
		diags = append(diags, diag.Errorf("invalid-suite", "suite", "expected %q, got %q", ConformanceSuite, manifest.Suite))
	}
	if manifest.Target.DiscoveryPath != EventServerDiscoveryPath {
		diags = append(diags, diag.Errorf("invalid-target", "target.discoveryPath", "expected %q, got %q", EventServerDiscoveryPath, manifest.Target.DiscoveryPath))
	}
	if manifest.Target.GRPCService != DefaultEventServerGRPCService {
		diags = append(diags, diag.Errorf("invalid-target", "target.grpcService", "expected %q, got %q", DefaultEventServerGRPCService, manifest.Target.GRPCService))
	}
	if len(manifest.Cases) == 0 {
		diags = append(diags, diag.Errorf("required-field", "cases", "conformance manifest requires at least one case"))
	}

	seen := map[string]bool{}
	for i, testCase := range manifest.Cases {
		diags = append(diags, validateConformanceCase(i, testCase, seen)...)
	}

	return diags
}

func validateConformanceCase(index int, testCase ConformanceCase, seen map[string]bool) []diag.Diagnostic {
	var diags []diag.Diagnostic
	path := "cases"

	if testCase.ID == "" {
		diags = append(diags, diag.Errorf("required-field", path, "case at index %d requires id", index))
	} else if seen[testCase.ID] {
		diags = append(diags, diag.Errorf("duplicate-case", path, "duplicate conformance case %q", testCase.ID))
	} else {
		seen[testCase.ID] = true
	}

	if !slices.Contains([]string{"required", "recommended", "optional"}, testCase.Level) {
		diags = append(diags, diag.Errorf("invalid-enum", path, "case %q has unknown level %q", testCase.ID, testCase.Level))
	}
	if !isValidConformanceTransport(testCase.Transport) {
		diags = append(diags, diag.Errorf("invalid-enum", path, "case %q has unknown transport %q", testCase.ID, testCase.Transport))
	}
	if len(testCase.Steps) == 0 {
		diags = append(diags, diag.Errorf("required-field", path, "case %q requires steps", testCase.ID))
	}
	if len(testCase.Expect) == 0 {
		diags = append(diags, diag.Errorf("required-field", path, "case %q requires expect", testCase.ID))
	}

	for _, transport := range testCase.Requires.Transports {
		if !validEventServerTransports[transport] {
			diags = append(diags, diag.Errorf("invalid-enum", path, "case %q requires unknown transport %q", testCase.ID, transport))
		}
	}
	for _, feature := range testCase.Requires.Features {
		if !slices.Contains([]string{"replay", "publish", "ack"}, feature) {
			diags = append(diags, diag.Errorf("invalid-enum", path, "case %q requires unknown feature %q", testCase.ID, feature))
		}
	}
	for _, auth := range testCase.Requires.Auth {
		if !validAuthSchemes[auth] {
			diags = append(diags, diag.Errorf("invalid-enum", path, "case %q requires unknown auth scheme %q", testCase.ID, auth))
		}
	}

	return diags
}

func isValidConformanceTransport(transport string) bool {
	switch transport {
	case "discovery", string(DeliveryProfilePush):
		return true
	}
	return validEventServerTransports[EventServerTransport(transport)]
}
