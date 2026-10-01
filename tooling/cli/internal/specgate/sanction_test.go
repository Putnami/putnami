package specgate

import (
	"strings"
	"testing"

	features "go.putnami.dev/protocol/features"
)

func TestGateActiveFollowsTheCommittedPolicy(t *testing.T) {
	if GateActive(nil) {
		t.Error("a nil workspace reported an active gate")
	}
	if !GateActive(gateWorkspace(nil, nil)) {
		t.Error("the built-in report default did not keep the gate active")
	}
	if GateActive(gateWorkspace(map[string]any{"specs": "off"}, nil)) {
		t.Error("workspace off with no overrides kept the gate active")
	}
	if !GateActive(gateWorkspace(map[string]any{"specs": "off"}, map[string]any{"specs": "report"})) {
		t.Error("a committed project override did not re-arm the gate")
	}
	if GateActive(gateWorkspace(map[string]any{"specs": "off"}, map[string]any{"specs": "off", "features": "enforce"})) {
		t.Error("a reserved sibling domain armed the specs gate")
	}
}

func TestSanctionMessagesAggregatePerProject(t *testing.T) {
	record := &features.SpecVerificationRecord{
		ProtocolVersion: features.SpecVerificationRecordProtocolVersion,
		GeneratedAt:     "2026-08-23T12:00:00Z",
		Groups: []features.SpecVerificationGroup{
			{Project: "/a", Feature: "go/x", Mode: features.VerificationModeEnforce, Blocked: true,
				Counts: features.SpecVerificationCounts{SpecRequirements: 2, Verified: 1, Missing: 1, Executable: 2}},
			{Project: "/a", Feature: "go/y", Mode: features.VerificationModeEnforce, Blocked: true,
				Counts: features.SpecVerificationCounts{SpecRequirements: 1, Contradicted: 1, Executable: 1}},
			{Project: "/b", Feature: "go/z", Mode: features.VerificationModeReport, Blocked: false,
				Counts: features.SpecVerificationCounts{SpecRequirements: 3, Missing: 3, Executable: 3}},
		},
	}
	messages := SanctionMessages(record)
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want exactly the enforce project", messages)
	}
	message := messages["/a"]
	for _, want := range []string{"2 of 3", "missing 1", "contradicted 1", "putnami specs verify"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q does not carry %q", message, want)
		}
	}
	if SanctionMessages(nil) != nil {
		t.Error("a nil record produced sanctions")
	}
	broken := &features.SpecVerificationRecord{Groups: []features.SpecVerificationGroup{
		{Project: "/c", Mode: features.VerificationModeEnforce, Blocked: true},
	}}
	if message := SanctionMessages(broken)["/c"]; !strings.Contains(message, "could not be read") {
		t.Errorf("an input-failure sanction reads %q", message)
	}
}
