package cachekey

import (
	"sort"

	jobmodel "go.putnami.dev/cli/model/jobs"
)

// PolicedOutputs returns the job's declared outputs carrying a drift policy, in
// output-id order.
func PolicedOutputs(job *jobmodel.ScheduledJob) []string {
	declaration := jobmodel.TaskDeclarationOf(job)
	if declaration == nil {
		return nil
	}
	var ids []string
	for id, output := range declaration.Outputs {
		if output.Drift != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
