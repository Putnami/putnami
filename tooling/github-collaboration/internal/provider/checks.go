package provider

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"

	collab "go.putnami.dev/protocol/collaboration"
)

// maxCheckPages bounds the pages of check runs, and of commit statuses, one
// status read reads.
const maxCheckPages = 10

// checkSeverity orders checks so a truncated list keeps the ones that decide
// the summary: failing first, then pending, passing and skipped.
var checkSeverity = map[collab.CheckState]int{
	collab.CheckStateFailing: 0, collab.CheckStatePending: 1, collab.CheckStatePassing: 2, collab.CheckStateSkipped: 3,
}

// checks reads the check runs and the commit statuses GitHub reports for a
// commit and summarizes them: failing when any fails, pending when any is not
// finished, passing when every one passed or was skipped, none when there is
// none.
func (s *proposalSession) checks(sha string) (collab.Checks, *collab.Failure) {
	var items []collab.Check
	runs, failure := s.checkRuns(sha)
	if failure != nil {
		return collab.Checks{}, failure
	}
	items = append(items, runs...)
	statuses, failure := s.commitStatuses(sha)
	if failure != nil {
		return collab.Checks{}, failure
	}
	items = append(items, statuses...)

	summary := collab.Checks{State: collab.ChecksStateNone, Commit: commit(sha)}
	counts := map[collab.CheckState]int{}
	for _, item := range items {
		counts[item.State]++
	}
	switch {
	case counts[collab.CheckStateFailing] > 0:
		summary.State = collab.ChecksStateFailing
	case counts[collab.CheckStatePending] > 0:
		summary.State = collab.ChecksStatePending
	case len(items) > 0:
		summary.State = collab.ChecksStatePassing
	}
	sort.SliceStable(items, func(i, j int) bool {
		if checkSeverity[items[i].State] != checkSeverity[items[j].State] {
			return checkSeverity[items[i].State] < checkSeverity[items[j].State]
		}
		return items[i].Name < items[j].Name
	})
	if len(items) > collab.MaxListMembers {
		summary.Detail = fmt.Sprintf("%d checks reported; the %d listed are the failing and pending ones first", len(items), collab.MaxListMembers)
		items = items[:collab.MaxListMembers]
	}
	summary.Items = items
	return summary, nil
}

func (s *proposalSession) checkRuns(sha string) ([]collab.Check, *collab.Failure) {
	var checks []collab.Check
	for page := 1; page <= maxCheckPages; page++ {
		var answer ghCheckRuns
		query := url.Values{"per_page": {strconv.Itoa(githubPageSize)}, "page": {strconv.Itoa(page)}}
		if _, err := s.client.Get(s.ctx, s.repo.path("commits", sha, "check-runs"), query, &answer); err != nil {
			return nil, fail(err, "read the check runs of %s", sha)
		}
		for _, run := range answer.CheckRuns {
			state := collab.CheckStatePending
			if run.Status == "completed" {
				switch run.Conclusion {
				case "success":
					state = collab.CheckStatePassing
				case "neutral", "skipped":
					state = collab.CheckStateSkipped
				default:
					state = collab.CheckStateFailing
				}
			}
			checks = append(checks, collab.Check{Name: oneLine(run.Name, collab.MaxTitleLength, "check run"), State: state, URL: displayURL(run.HTMLURL)})
		}
		if len(answer.CheckRuns) < githubPageSize || page*githubPageSize >= answer.TotalCount {
			return checks, nil
		}
	}
	return checks, nil
}

func (s *proposalSession) commitStatuses(sha string) ([]collab.Check, *collab.Failure) {
	var checks []collab.Check
	for page := 1; page <= maxCheckPages; page++ {
		var answer ghCombinedStatus
		query := url.Values{"per_page": {strconv.Itoa(githubPageSize)}, "page": {strconv.Itoa(page)}}
		if _, err := s.client.Get(s.ctx, s.repo.path("commits", sha, "status"), query, &answer); err != nil {
			return nil, fail(err, "read the commit statuses of %s", sha)
		}
		for _, status := range answer.Statuses {
			state := collab.CheckStatePending
			switch status.State {
			case "success":
				state = collab.CheckStatePassing
			case "failure", "error":
				state = collab.CheckStateFailing
			}
			checks = append(checks, collab.Check{Name: oneLine(status.Context, collab.MaxTitleLength, "status"), State: state, URL: displayURL(status.TargetURL)})
		}
		if len(answer.Statuses) < githubPageSize || page*githubPageSize >= answer.TotalCount {
			return checks, nil
		}
	}
	return checks, nil
}
