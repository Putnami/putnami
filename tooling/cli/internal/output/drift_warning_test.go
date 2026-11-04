package output

import (
	"bytes"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

func driftWarningResults(t *testing.T) map[string]*jobs.JobResult {
	t.Helper()
	drift := makeRendererDiagnostic(t, runtimeproto.SeverityWarning, "commit the generated files")
	drift.Data["code"] = extension.OutputDriftDiagnosticCode
	other := makeRendererDiagnostic(t, runtimeproto.SeverityWarning, "an unrelated warning")
	return map[string]*jobs.JobResult{
		"build": {Status: "success", Events: []jobs.RawJobEvent{drift, other}},
	}
}

func TestFinishPrintsADriftWarningOfASuccessfulTask(t *testing.T) {
	var out, text, live bytes.Buffer
	textRenderer := NewTextRenderer(&out, &text, TextRendererConfig{})
	textRenderer.Start([]*jobs.ScheduledJob{makeLiveTestJob("app", "build")})
	textRenderer.Finish(driftWarningResults(t), jobs.SessionOutcome{})

	liveRenderer := NewLiveRenderer(&out, &live)
	liveRenderer.Start([]*jobs.ScheduledJob{makeLiveTestJob("app", "build")})
	liveRenderer.Finish(driftWarningResults(t), jobs.SessionOutcome{})

	for name, got := range map[string]string{"text": text.String(), "live": live.String()} {
		if !strings.Contains(got, "warning: commit the generated files") {
			t.Errorf("%s renderer dropped the drift warning: %q", name, got)
		}
		if strings.Contains(got, "an unrelated warning") {
			t.Errorf("%s renderer printed another task diagnostic: %q", name, got)
		}
	}
}
