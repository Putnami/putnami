package agentartifacts

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Collision is one preserved path in a report, with the reason it was
// preserved. Reason is a stable identifier a tool can branch on; Detail is the
// sentence a human reads.
type Collision struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// Report is the deterministic outcome of one materialization — the SAME
// document whether the run applied, was a no-op, or aborted on a collision, so
// a caller never has to reconcile two shapes.
//
// Every list is sorted by path and every list is non-nil, so the machine
// rendering emits `[]` rather than `null` and two runs over the same inputs
// produce byte-identical bytes. There is no timestamp and no duration: a report
// is compared, and a wall clock makes every comparison fail.
type Report struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	ArchiveDigest string `json:"archiveDigest"`
	ManifestHash  string `json:"manifestHash"`
	// Applied is false when the run aborted on a collision, and true otherwise —
	// including for a no-op run, where the plan was applied and changed nothing.
	Applied   bool        `json:"applied"`
	Created   []string    `json:"created"`
	Updated   []string    `json:"updated"`
	Removed   []string    `json:"removed"`
	Unchanged []string    `json:"unchanged"`
	Collided  []Collision `json:"collided"`
}

// NewReport projects a plan into a report. It is the only place a plan becomes
// a report, so the five plan actions and the five report sections stay in
// one-to-one correspondence.
func NewReport(plan *Plan, applied bool) *Report {
	report := &Report{
		Name:          plan.Name,
		Version:       plan.Version,
		ArchiveDigest: plan.ArchiveDigest,
		ManifestHash:  plan.ManifestHash,
		Applied:       applied,
		Created:       []string{},
		Updated:       []string{},
		Removed:       []string{},
		Unchanged:     []string{},
		Collided:      []Collision{},
	}
	// plan.Entries is already sorted by path, so appending in order keeps every
	// section sorted without a second sort that could disagree with the plan.
	for _, entry := range plan.Entries {
		switch entry.Action {
		case ActionCreate:
			report.Created = append(report.Created, entry.Path)
		case ActionUpdate:
			report.Updated = append(report.Updated, entry.Path)
		case ActionRemove:
			report.Removed = append(report.Removed, entry.Path)
		case ActionUnchanged:
			report.Unchanged = append(report.Unchanged, entry.Path)
		case ActionCollide:
			report.Collided = append(report.Collided, Collision{
				Path:   entry.Path,
				Reason: entry.Reason,
				Detail: entry.Detail,
			})
		}
	}
	return report
}

// JSON renders the machine report: two-space indentation, one trailing
// newline, and no map anywhere in the shape, so the bytes cannot depend on
// iteration order.
func (r *Report) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal agent-artifact report: %w", err)
	}
	return append(data, '\n'), nil
}

// Text renders the human report. It lists the same paths in the same order as
// JSON, and it leads with the collisions when there are any, because that is
// the only case where the run did nothing and the reader has to act.
func (r *Report) Text() string {
	var b strings.Builder
	verb := "materialized"
	if !r.Applied {
		verb = "aborted"
	}
	fmt.Fprintf(&b, "%s %s@%s (%s)\n", verb, r.Name, r.Version, r.ArchiveDigest)

	if len(r.Collided) > 0 {
		fmt.Fprintf(&b, "  collided (%d, preserved):\n", len(r.Collided))
		for _, collision := range r.Collided {
			fmt.Fprintf(&b, "    %s [%s] %s\n", collision.Path, collision.Reason, collision.Detail)
		}
	}
	writeSection(&b, "created", r.Created)
	writeSection(&b, "updated", r.Updated)
	writeSection(&b, "removed", r.Removed)
	writeSection(&b, "unchanged", r.Unchanged)

	if !r.Applied {
		b.WriteString("  nothing was written: resolve every collision above and rerun\n")
	}
	return b.String()
}

func writeSection(b *strings.Builder, label string, paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprintf(b, "  %s (%d):\n", label, len(paths))
	for _, path := range paths {
		fmt.Fprintf(b, "    %s\n", path)
	}
}
