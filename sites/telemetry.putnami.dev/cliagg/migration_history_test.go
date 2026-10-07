package cliagg

import (
	"crypto/sha256"
	"fmt"
	"testing"

	protocolmigration "go.putnami.dev/protocol/migration"
)

// Applied migration bodies are immutable: changing an earlier UP file makes a
// new bundle disagree with the hashes already stored for these canonical IDs.
func TestSourcePreservesAppliedMigrationIDsAndUpHashes(t *testing.T) {
	operations, payloads, err := Source().MigrationBundleOperations()
	if err != nil {
		t.Fatalf("build migration bundle operations: %v", err)
	}
	want := []struct {
		id     string
		upHash string
	}{
		{"telemetry:cliagg/20260725120000_create_cli_aggregates", "ff7aba07f5484a88646b37d6c92103062c30156f1d14ee4da90b283ac3ed67a2"},
		{"telemetry:cliagg/20260729103000_bound_cli_aggregates", "b987dbf66d233e85c57e1394253bcdd311096fc461e9c45ccd2eb12c305e755f"},
		{"telemetry:cliagg/20260729170000_harden_cli_retention", "e4b68e3f65eb955f8ff8f96fa9e72a03797e12c7004fb62b3ee11fd138b57613"},
		{"telemetry:cliagg/20260729190000_fix_cli_retention_cron_user", "ceb6efff9058ddc89f363b4abcfb7cc907ae55351e3725528f8972de98abc439"},
		{"telemetry:cliagg/20260930120000_aggregate_run_shape", "be356cadfedf9fc1b41b0b812f5200a6def1c4c5a525d40ceaaca72fb42c4db7"},
	}
	if len(operations) != len(want) {
		t.Fatalf("bundle has %d operations, want %d", len(operations), len(want))
	}
	byPath := make(map[string][]byte, len(payloads))
	for _, payload := range payloads {
		if _, exists := byPath[payload.Path]; exists {
			t.Fatalf("duplicate bundle payload path %q", payload.Path)
		}
		byPath[payload.Path] = payload.Bytes
	}
	for i, operation := range operations {
		id := protocolmigration.CanonicalID(operation.Target, operation.Name)
		if id != want[i].id || operation.Up.Hash != want[i].upHash {
			t.Errorf("operation %d: id %q, up hash %q; want id %q, up hash %q",
				i, id, operation.Up.Hash, want[i].id, want[i].upHash)
		}
		payload, ok := byPath[operation.Up.Path]
		if !ok {
			t.Errorf("operation %d: missing UP payload at %q", i, operation.Up.Path)
			continue
		}
		if actual := fmt.Sprintf("%x", sha256.Sum256(payload)); actual != operation.Up.Hash {
			t.Errorf("operation %d: UP payload hashes to %q, bundle records %q", i, actual, operation.Up.Hash)
		}
	}
}
