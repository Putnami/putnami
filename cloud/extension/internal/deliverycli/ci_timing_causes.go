package deliverycli

import (
	"fmt"
	"sort"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The causes `timings --against` gives a task that executed instead of being
// served by the cache. Each one names a different owner: an uncached task is a
// task-definition question, a changed key is an input question, and a key that
// both runs share is a cache-store question.
const (
	// ciCauseNoKey: the scheduler computed no cache key, so no run can reuse the
	// task.
	ciCauseNoKey = "no-key"
	// ciCauseNotInOtherRun: the other run has no keyed record of the task.
	ciCauseNotInOtherRun = "not-in-other-run"
	// ciCauseKeyChanged: the two runs keyed the task differently, so an input of
	// the task changed between them.
	ciCauseKeyChanged = "key-changed"
	// ciCauseEntryMissing: the other run ended the task successfully under the
	// same key, and the cache still did not serve it.
	ciCauseEntryMissing = "same-key-entry-missing"
	// ciCauseOtherFailed: the other run held the same key and did not succeed, so
	// it stored nothing to reuse.
	ciCauseOtherFailed = "same-key-other-failed"
)

// ciCauseOrder is the order the classes print in when two weigh the same.
var ciCauseOrder = []string{
	ciCauseKeyChanged, ciCauseEntryMissing, ciCauseOtherFailed, ciCauseNoKey, ciCauseNotInOtherRun,
}

var ciCauseMeaning = map[string]string{
	ciCauseNoKey:         "the scheduler computes no cache key for the task: no run can reuse it",
	ciCauseNotInOtherRun: "the other run has no keyed record of the task",
	ciCauseKeyChanged:    "the two runs keyed the task differently: one of its inputs changed",
	ciCauseEntryMissing:  "the other run succeeded under the same key and the cache did not serve it: evicted, never stored, or the lookup failed",
	ciCauseOtherFailed:   "the other run held the same key and did not succeed: it stored nothing",
}

// CIRunTimingCauses is what `timings <run> --against <other-run>` emits: why
// each task of the run executed, read against a run that could have filled the
// cache for it.
type CIRunTimingCauses struct {
	RunID        string `json:"runId"`
	AgainstRunID string `json:"againstRunId"`
	// Tasks is how many task terminals the run's table holds; Served is how many
	// of them the cache answered.
	Tasks  int `json:"tasks"`
	Served int `json:"served"`
	// Classes weighs each cause, heaviest wall first.
	Classes []CIRunTimingCauseClass `json:"classes"`
	// Executed lists every task that executed, longest wall first.
	Executed []CIRunTimingCause `json:"executed"`
	// ElidedExecuted is how many executed rows a client-side bound left out.
	ElidedExecuted int `json:"elidedExecuted"`
}

// CIRunTimingCauseClass is one cause's weight in the run.
type CIRunTimingCauseClass struct {
	Cause      string `json:"cause"`
	Tasks      int    `json:"tasks"`
	DurationMs int64  `json:"durationMs"`
}

// CIRunTimingCause is one executed task and the reason the cache did not serve
// it.
type CIRunTimingCause struct {
	Key                string `json:"key"`
	Project            string `json:"project"`
	Task               string `json:"task"`
	Status             string `json:"status"`
	DurationMs         int64  `json:"durationMs"`
	Cause              string `json:"cause"`
	InputDigest        string `json:"inputDigest,omitempty"`
	AgainstInputDigest string `json:"againstInputDigest,omitempty"`
	AgainstStatus      string `json:"againstStatus,omitempty"`
	AgainstReuse       string `json:"againstReuse,omitempty"`
}

// ciCompareTimings classifies every executed task of run against baseline.
//
// It refuses a table that carries no cache key at all rather than calling every
// task `no-key`: such a table was derived before the control plane recorded the
// key, and reading it as "nothing here is cacheable" would be a wrong answer
// that looks like a finding.
func ciCompareTimings(run, baseline *CIRunTimingsResponse) (*CIRunTimingCauses, error) {
	for _, table := range []*CIRunTimingsResponse{run, baseline} {
		if len(table.Tasks) > 0 && !ciTimingsCarryKeys(table) {
			return nil, clicore.NewError("run "+ciShortRunID(table.RunID)+" has a timing table without cache keys: the control plane "+
				"derived it before it recorded them, so its tasks cannot be compared", clicore.ExitAPI)
		}
	}
	others := make(map[string]CIRunTimingTask, len(baseline.Tasks))
	for _, row := range baseline.Tasks {
		others[row.Key] = row
	}
	out := &CIRunTimingCauses{
		RunID: run.RunID, AgainstRunID: baseline.RunID,
		Tasks: len(run.Tasks), Executed: []CIRunTimingCause{},
	}
	weights := map[string]*CIRunTimingCauseClass{}
	for _, row := range run.Tasks {
		if !ciTaskExecuted(row) {
			if ciTaskServed(row) {
				out.Served++
			}
			continue
		}
		other, known := others[row.Key]
		cause := ciTimingCause(row, other, known)
		entry := CIRunTimingCause{
			Key: row.Key, Project: row.Project, Task: row.Task, Status: row.Status,
			DurationMs: row.DurationMs, Cause: cause, InputDigest: row.InputDigest,
		}
		if known {
			entry.AgainstInputDigest, entry.AgainstStatus, entry.AgainstReuse = other.InputDigest, other.Status, other.Reuse
		}
		out.Executed = append(out.Executed, entry)
		weight := weights[cause]
		if weight == nil {
			weight = &CIRunTimingCauseClass{Cause: cause}
			weights[cause] = weight
		}
		weight.Tasks++
		weight.DurationMs += row.DurationMs
	}
	sort.SliceStable(out.Executed, func(i, j int) bool {
		if out.Executed[i].DurationMs != out.Executed[j].DurationMs {
			return out.Executed[i].DurationMs > out.Executed[j].DurationMs
		}
		return out.Executed[i].Key < out.Executed[j].Key
	})
	out.Classes = []CIRunTimingCauseClass{}
	for _, cause := range ciCauseOrder {
		if weight := weights[cause]; weight != nil {
			out.Classes = append(out.Classes, *weight)
		}
	}
	sort.SliceStable(out.Classes, func(i, j int) bool { return out.Classes[i].DurationMs > out.Classes[j].DurationMs })
	return out, nil
}

// ciTimingCause names why one executed task was not served.
func ciTimingCause(row, other CIRunTimingTask, known bool) string {
	switch {
	case row.InputDigest == "":
		return ciCauseNoKey
	case !known || other.InputDigest == "":
		return ciCauseNotInOtherRun
	case other.InputDigest != row.InputDigest:
		return ciCauseKeyChanged
	case other.Status != "success":
		return ciCauseOtherFailed
	default:
		return ciCauseEntryMissing
	}
}

// ciTaskExecuted reports whether the task ran in this run. A skipped task did
// no work, and a reused one did none here.
func ciTaskExecuted(row CIRunTimingTask) bool {
	return (row.Reuse == "" || row.Reuse == "none") && row.Status != "skipped"
}

func ciTaskServed(row CIRunTimingTask) bool {
	return row.Reuse != "" && row.Reuse != "none"
}

func ciTimingsCarryKeys(t *CIRunTimingsResponse) bool {
	for _, row := range t.Tasks {
		if row.InputDigest != "" {
			return true
		}
	}
	return false
}

// ciTruncateTimingCauses applies a client-side row bound and accounts for what
// it dropped. The class weights are untouched: they describe the whole run.
func ciTruncateTimingCauses(c *CIRunTimingCauses, limit int) {
	if limit <= 0 || limit >= len(c.Executed) {
		return
	}
	c.ElidedExecuted += len(c.Executed) - limit
	c.Executed = c.Executed[:limit]
}

// ciRenderTimingCauses prints the class weights first, because they answer the
// question — where the uncached time went — and then the tasks behind them.
func ciRenderTimingCauses(ioctx clicore.IO, c *CIRunTimingCauses, limit int) {
	ioctx.Stdout(fmt.Sprintf("Cache causes — run %s against %s", ciShortRunID(c.RunID), ciShortRunID(c.AgainstRunID)))
	ioctx.Stdout(fmt.Sprintf("  tasks     %d recorded, %d served by the cache, %d executed", c.Tasks, c.Served, len(c.Executed)))
	if len(c.Executed) == 0 {
		return
	}
	ioctx.Stdout("Why they executed (heaviest wall first):")
	for _, class := range c.Classes {
		ioctx.Stdout(fmt.Sprintf("  %-22s %5d task(s) %9s  %s",
			class.Cause, class.Tasks, ciDurationMs(class.DurationMs), ciCauseMeaning[class.Cause]))
	}
	shown := len(c.Executed)
	if limit > 0 && limit < shown {
		shown = limit
	}
	ioctx.Stdout(fmt.Sprintf("Tasks (%d of %d, longest wall first):", shown, len(c.Executed)))
	for i := 0; i < shown; i++ {
		row := c.Executed[i]
		out := fmt.Sprintf("  %-22s %-34s %-22s %9s", row.Cause, row.Project, row.Task, ciDurationMs(row.DurationMs))
		if row.Cause == ciCauseKeyChanged {
			out += "  " + ciShortDigest(row.AgainstInputDigest) + " → " + ciShortDigest(row.InputDigest)
		}
		ioctx.Stdout(out)
	}
	if shown < len(c.Executed) {
		ioctx.Stdout(fmt.Sprintf("  … %d further row(s) not printed — raise --limit, or --limit 0 for every executed task.",
			len(c.Executed)-shown))
	}
}

// ciShortDigest keeps the first 12 hex characters of a `sha256:<hex>` key,
// enough to tell two keys apart on one line.
func ciShortDigest(digest string) string {
	hex := digest[strings.IndexByte(digest, ':')+1:]
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}
