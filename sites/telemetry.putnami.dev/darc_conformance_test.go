package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The observability domain's DARC export is a contract about THIS workload's
// ingest behavior. The test joins the declaration to the sanitizer's own
// constants so the manifest cannot promise a provenance or privacy property
// the code stopped enforcing:
//
//   - the ingest is a COMMAND surface — the CLI fire-and-forgets records, the
//     site never exposes a query or replication mode for raw events;
//   - the one exported fact carries no personal data, which is what the
//     sanitize pass and the aggregation layer (no device id, no IP, no
//     argument ever reflected) exist to keep true;
//   - the declared provenance story names the exact origin constant the
//     sanitizer stamps unspoofably at the resource level.
func TestDARCExportMatchesTheIngestItDescribes(t *testing.T) {
	data, err := os.ReadFile("putnami.architecture.json")
	if err != nil {
		t.Fatalf("read the observability domain manifest: %v", err)
	}
	var manifest struct {
		Exports []struct {
			ID          string `json:"id"`
			Status      string `json:"status"`
			Description string `json:"description"`
			Modes       []string
			Facts       []struct {
				Name           string `json:"name"`
				Classification string `json:"classification"`
				PersonalData   string `json:"personalData"`
			} `json:"facts"`
		} `json:"exports"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode the observability domain manifest: %v", err)
	}
	if len(manifest.Exports) != 1 || manifest.Exports[0].ID != "observability.usage-ingest.v1" {
		t.Fatalf("exports = %+v, want exactly observability.usage-ingest.v1", manifest.Exports)
	}
	export := manifest.Exports[0]
	if len(export.Modes) != 1 || export.Modes[0] != "command" {
		t.Errorf("ingest modes = %v, want [command]: raw events are received, never served", export.Modes)
	}
	if len(export.Facts) != 1 || export.Facts[0].PersonalData != "none" || export.Facts[0].Classification != "internal" {
		t.Errorf("ingest facts = %+v, want one internal fact with personalData none — the sanitize and aggregation layers exist to keep that true", export.Facts)
	}
	if !strings.Contains(export.Description, originCLIAnon) {
		t.Errorf("ingest description does not name the stamped origin %q; the declared provenance story must match the sanitizer's constant", originCLIAnon)
	}
}
