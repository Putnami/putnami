package jobs

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// batchProjectLimit resolves how many projects one shared invocation of a
// batchable task may hold. It returns the cap (0 means unbounded) and whether
// the job may batch at all.
//
// A policy can name a parameter (MaxProjectsParam) that a workspace or
// project sets through its options. When the policy names none, or the job's
// resolved parameters do not carry it, the manifest's static MaxProjects
// applies unchanged: a workspace that never sets the option keeps its batch
// keys byte for byte. A positive integer replaces the static cap. A cap of 1
// means one project per invocation, which is the singleton path, so the job
// does not batch. Any other value is an error.
//
// The lookup follows the rest of the CLI: the exact spelling first, then the
// camelCase alias the parameter layers derive from a kebab-case name.
func batchProjectLimit(policy *extension.TaskBatchPolicy, params extension.ParamMap) (int, bool, error) {
	if policy == nil {
		return 0, false, nil
	}
	name := policy.MaxProjectsParam
	if name == "" {
		return policy.MaxProjects, true, nil
	}
	raw, ok := params[name]
	if !ok && strings.Contains(name, "-") {
		raw, ok = params[kebabToCamel(name)]
	}
	if !ok || raw == nil {
		return policy.MaxProjects, true, nil
	}
	limit, ok := positiveIntegerParam(raw)
	if !ok {
		shown, err := json.Marshal(raw)
		if err != nil {
			shown = []byte(fmt.Sprint(raw))
		}
		if _, isString := raw.(string); isString {
			// A quoted number in JSON options, or an undeclared command-line
			// flag, arrives as a string. Say so, or "got "2"" reads as a bug.
			return 0, false, fmt.Errorf("option %s must be a positive integer written as a JSON number in workspace or project options, got the string %s", name, shown)
		}
		return 0, false, fmt.Errorf("option %s must be a positive integer (1 runs every project alone), got %s", name, shown)
	}
	return limit, limit > 1, nil
}

// positiveIntegerParam accepts a whole number from 1 to MaxInt32. Parameters
// decoded from JSON options arrive as float64, so 6.0 is 6 while 6.5, 0, a
// negative number, a string, or a boolean is refused.
func positiveIntegerParam(raw any) (int, bool) {
	switch value := raw.(type) {
	case float64:
		if value >= 1 && value <= math.MaxInt32 && value == math.Trunc(value) {
			return int(value), true
		}
	case int:
		if value >= 1 && value <= math.MaxInt32 {
			return value, true
		}
	}
	return 0, false
}

// validateBatchProjectLimits fails the plan when a job's batch cap option holds
// anything but a positive integer. Checking at plan time turns a typo in
// putnami.workspace.json into an error before any task runs, instead of a
// grouping nobody asked for. The scheduler still refuses to batch such a job
// if a value reaches it by another route.
//
// It reads the cap exactly as Scheduler.readyBatch does: batchProjectLimit over
// deliveredJobParams with jobConfigDefaults. The one input that differs is the
// run's execution-only extension parameters, which the engine overlays after
// planning; they are framework globals, never a batch cap.
func validateBatchProjectLimits(planned []*ScheduledJob, ws *workspace.Workspace, commandParams extension.ParamMap) error {
	for _, job := range planned {
		if job == nil || job.JobDef == nil || job.JobDef.Batchable == nil ||
			job.JobDef.Batchable.MaxProjectsParam == "" || job.Extension == nil || job.Project == nil {
			continue
		}
		params := deliveredJobParams(job, commandParams, jobConfigDefaults(ws, job))
		if _, _, err := batchProjectLimit(job.JobDef.Batchable, params); err != nil {
			return fmt.Errorf("invalid batch size for %s: %w", job.Key(), err)
		}
	}
	return nil
}
