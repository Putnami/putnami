package upload

import (
	"context"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestTheReporterAnswersTheLevel3Preview(t *testing.T) {
	spectest.Proves(t, feature, "upload-answers-the-level-3-preview", "the-reporter-answers-the-level-3-preview")
	const report = "https://putnami.app/w/acme/changes/acme%2Fshop%2342"
	data := `{"reportUrl":"` + report + `","level3Preview":{"level":3,"baseBranch":"main","holds":1,` +
		`"brokenRules":{"state":"known","count":2},"vanishedTests":{"state":"unknown","reason":"no run of main to compare"}}}`
	url, preview, diags := Run(context.Background(), Request{
		Path:   document(t),
		Getenv: env(map[string]string{"PUTNAMI_CLI_EXECUTABLE": answeringCLI(t, resultJSON("success", data), "0")}),
	})
	if len(diags) != 0 || url != report || preview == nil {
		t.Fatalf("url %q, preview %v, diagnostics %v", url, preview, diags)
	}
	if preview.Level != 3 || preview.BaseBranch != "main" || len(preview.Held()) != 1 || preview.Held()[0].Count != 2 {
		t.Fatalf("preview %+v", preview)
	}
	if unknown := preview.Unknown(); len(unknown) != 4 || unknown[len(unknown)-1].Reason != "no run of main to compare" {
		t.Fatalf("unknown %+v", unknown)
	}

	// A reporter that answers none, or a failure, keeps no preview.
	for name, stdout := range map[string]string{
		"no preview":    resultJSON("success", `{"reportUrl":"`+report+`"}`),
		"a null one":    resultJSON("success", `{"reportUrl":"`+report+`","level3Preview":null}`),
		"not an object": resultJSON("success", `{"reportUrl":"`+report+`","level3Preview":[1]}`),
		"a failure":     resultJSON("error", data),
	} {
		t.Run(name, func(t *testing.T) {
			if _, preview, _ := Run(context.Background(), Request{Path: document(t), Getenv: env(map[string]string{"PUTNAMI_CLI_EXECUTABLE": answeringCLI(t, stdout, "0")})}); preview != nil {
				t.Fatalf("preview %+v, want none", preview)
			}
		})
	}
}
