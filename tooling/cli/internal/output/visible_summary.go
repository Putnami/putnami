package output

import (
	"io"
	"sort"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/iox"
)

func renderVisibleSummaries(w io.Writer, results map[string]*jobs.JobResult) {
	keys := make([]string, 0, len(results))
	for key := range results {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		result := results[key]
		for _, ev := range result.Events {
			if ev.Type != jobs.EventTypeSummary {
				continue
			}
			if visibility, _ := ev.Data["visibility"].(string); visibility != "always" {
				continue
			}
			msg, _ := ev.Data["message"].(string)
			if msg == "" {
				continue
			}
			iox.Fprintf(w, "\n%s\n", msg)
		}
	}
}
