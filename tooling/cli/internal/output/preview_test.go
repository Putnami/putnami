package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// TestWritePreviewPlan_EmptyPlanRemainsTyped pins the legitimate no-op shape:
// emptiness is explicit in metrics/tasks and never encoded as an empty stream
// or as a successful zero-task execution.
func TestWritePreviewPlan_EmptyPlanRemainsTyped(t *testing.T) {
	for _, format := range []string{"json", "jsonl"} {
		t.Run(format, func(t *testing.T) {
			var stdout bytes.Buffer
			written, err := WritePreviewPlan(&stdout, format, "build", nil)
			if err != nil {
				t.Fatalf("WritePreviewPlan: %v", err)
			}
			if !written {
				t.Fatal("explicit machine mode fell back to human rendering")
			}
			encoded := []byte(strings.TrimSpace(stdout.String()))
			kind := protocolcli.DocumentResultEnvelope
			var plan *protocolcli.PlanSummary
			if format == "json" {
				var envelope protocolcli.ResultV2
				if err := json.Unmarshal(encoded, &envelope); err != nil {
					t.Fatalf("decode JSON envelope: %v", err)
				}
				if envelope.Run != nil {
					t.Fatal("empty plan was encoded as a run")
				}
				plan = envelope.Plan
			} else {
				kind = protocolcli.DocumentSessionStreamRecord
				var record protocolcli.SessionStreamRecord
				if err := json.Unmarshal(encoded, &record); err != nil {
					t.Fatalf("decode JSONL record: %v", err)
				}
				if record.Record != protocolcli.RecordPlanEnd || record.Run != nil || record.MachineOutput != nil {
					t.Fatalf("preview terminal = %+v, want plan:end without run/session artifact", record)
				}
				plan = record.Plan
			}
			if violations := protocolcli.ValidateDocument(kind, encoded); len(violations) != 0 {
				t.Fatalf("empty preview violates %s: %+v\n%s", kind, violations, encoded)
			}
			if plan == nil || plan.Metrics.Tasks != 0 || len(plan.Tasks) != 0 {
				t.Fatalf("empty plan = %+v, want explicit zero metrics/tasks", plan)
			}
		})
	}
}
