package observe

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
)

// HistoryLine is what the summary says of one commit's evidence besides its
// analysis: "" when the run kept no document, else where it went.
type HistoryLine func(sha string) string

// WriteHistoryJSON writes the history as one JSON document.
func WriteHistoryJSON(w io.Writer, h History) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(h)
}

// WriteHistoryText writes the history as a short summary: the repository,
// the branch and the window, one line per observed commit, then the
// warnings of the run and the count of each warning its commits carry.
func WriteHistoryText(w io.Writer, h History, line HistoryLine) error {
	var b strings.Builder
	repository := "this repository"
	if len(h.Commits) > 0 {
		repository = repositoryLabel(h.Commits[0].Observation.Subject)
	}
	branch := h.Branch
	if branch == "" {
		branch = "HEAD"
	}
	fmt.Fprintf(&b, "putnami observe --history: %s, %s\n", repository, branch)
	if !h.Until.IsZero() {
		fmt.Fprintf(&b, "  window   %s to %s\n", h.Since.Format("2006-01-02 15:04"), h.Until.Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintf(&b, "  commits  %s in the window, %d observed\n", count(h.InWindow, "commit"), len(h.Commits))
	warnings := map[string]int{}
	var codes []string
	for _, c := range h.Commits {
		o := c.Observation
		fmt.Fprintf(&b, "    %s %s  %s, %s, %s", short(c.Commit.SHA), c.Commit.Time.Format("2006-01-02"),
			count(len(o.ChangedFiles), "file"), count(changedComponents(o), "changed component"), count(len(o.Analysis.DependencyChanges), "dependency change"))
		if line != nil {
			if where := line(c.Commit.SHA); where != "" {
				fmt.Fprintf(&b, "  %s", where)
			}
		}
		b.WriteString("\n")
		seen := map[string]bool{}
		for _, d := range o.Diagnostics {
			if d.Severity == diag.Info || seen[d.Code] {
				continue
			}
			seen[d.Code] = true
			if warnings[d.Code] == 0 {
				codes = append(codes, d.Code)
			}
			warnings[d.Code]++
		}
	}
	for _, d := range h.Diagnostics {
		fmt.Fprintf(&b, "  %s %s: %s\n", d.Severity, d.Code, d.Message)
	}
	for _, code := range codes {
		fmt.Fprintf(&b, "  warning %s on %s; --output=json lists each\n", code, count(warnings[code], "commit"))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// changedComponents counts the components the commit changes.
func changedComponents(o Observation) int {
	n := 0
	for _, c := range o.Analysis.Components {
		if c.Impact == evidence.ImpactChanged {
			n++
		}
	}
	return n
}
