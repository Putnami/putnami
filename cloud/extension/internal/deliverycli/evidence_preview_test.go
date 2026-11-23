package deliverycli

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// evidencePreview is a level 3 preview as Delivery answers it: one signal
// known with a count, one unknown with its reason.
func evidencePreview(holds int) map[string]any {
	return map[string]any{
		"level":      float64(3),
		"baseBranch": "main",
		"holds":      float64(holds),
		"brokenRules": map[string]any{
			"state": "known", "count": float64(holds),
			"rules": []any{map[string]any{"tool": "biome", "rule": "noUnusedVariables", "violationCount": float64(holds)}},
		},
		"vanishedTests": map[string]any{"state": "unknown", "reason": "no run of main to compare"},
	}
}

func evidenceResultData(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var result struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("result %s: %v", stdout, err)
	}
	return result.Data
}

func TestEvidenceReportAnswersTheLevel3Preview(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "answers-the-level-3-preview", "the-result-carries-the-level-3-preview")
	// The first batch answers an older preview, the second the newer one:
	// the result carries the last.
	fake := &evidenceFakeServer{reportURL: testReportURL, previews: []map[string]any{evidencePreview(1), evidencePreview(2)}}
	stdout, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json"), "--output=json")
	if err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if fake.batches < 2 {
		t.Fatalf("the upload posted %d batches, want at least 2", fake.batches)
	}
	data := evidenceResultData(t, stdout)
	if data["reportUrl"] != testReportURL {
		t.Fatalf("reportUrl = %v, want it beside the preview", data["reportUrl"])
	}
	if got, want := data["level3Preview"], any(evidencePreview(2)); !reflect.DeepEqual(got, want) {
		t.Fatalf("level3Preview = %v, want the last batch's %v", got, want)
	}
}

func TestEvidenceReportWithoutAPreviewAnswersNone(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "answers-the-level-3-preview", "a-run-without-a-preview-answers-none")
	for name, previews := range map[string][]map[string]any{
		"no batch answers one":             nil,
		"only the first batch answers one": {evidencePreview(1)},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &evidenceFakeServer{reportURL: testReportURL, previews: previews}
			stdout, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json"), "--output=json")
			if err != nil {
				t.Fatalf("evidence-report: %v", err)
			}
			got, present := evidenceResultData(t, stdout)["level3Preview"]
			switch {
			case previews == nil && present:
				t.Fatalf("level3Preview = %v, want none", got)
			case previews != nil && !reflect.DeepEqual(got, any(previews[0])):
				t.Fatalf("level3Preview = %v, want the first batch's %v", got, previews[0])
			}
		})
	}
}
