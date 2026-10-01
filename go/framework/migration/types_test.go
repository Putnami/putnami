package migration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDriftReportEmpty(t *testing.T) {
	if !(DriftReport{}).Empty() {
		t.Fatal("zero DriftReport must be empty")
	}

	withHashDrift := DriftReport{HashDrifts: []HashDrift{{Name: "x"}}}
	if withHashDrift.Empty() {
		t.Fatal("DriftReport with HashDrifts must not be empty")
	}

	withMissing := DriftReport{MissingFromStore: []Record{{Name: "y"}}}
	if withMissing.Empty() {
		t.Fatal("DriftReport with MissingFromStore must not be empty")
	}

	withExtra := DriftReport{MissingFromRegistry: []Record{{Name: "z"}}}
	if withExtra.Empty() {
		t.Fatal("DriftReport with MissingFromRegistry must not be empty")
	}
}

func TestRecordJSONOmitsZeroTime(t *testing.T) {
	r := Record{Kind: KindSQL, Name: "x", Status: StatusPending}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "executedAt") {
		t.Fatalf("zero time must be omitted from JSON, got: %s", out)
	}

	r2 := Record{Kind: KindSQL, Name: "x", Status: StatusApplied,
		ExecutedAt: time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)}
	out2, err := json.Marshal(r2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out2), "executedAt") {
		t.Fatalf("non-zero time must appear in JSON, got: %s", out2)
	}
}
