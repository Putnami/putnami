package provider

import (
	"regexp"
	"strconv"

	collab "go.putnami.dev/protocol/collaboration"
)

// A list answer walks GitLab's numbered pages of 100, oldest first: GitLab
// orders a project's issues by creation time, then id, and cannot order them
// by iid. Its cursor is "p<page>.t<milliseconds>.i<id>": the GitLab page to
// resume on and the position — creation time and id — of the last issue
// already considered. Resuming skips every issue at or before that position,
// and steps back while the resumed page starts past it, so an issue created,
// closed or deleted during a traversal moves no other issue across a page
// boundary: every issue present throughout is returned exactly once.

// gitlabPageSize is the page size every GitLab list request asks for.
const gitlabPageSize = 100

// maxFetchesPerPage bounds the GitLab pages one list answer reads. When it is
// reached, the answer carries the issues found so far and a cursor.
const maxFetchesPerPage = 5

var cursorPattern = regexp.MustCompile(`^p([1-9][0-9]{0,5})\.t([0-9]{1,15})\.i([0-9]{1,18})$`)

// position orders issues the way GitLab lists them.
type position struct {
	created int64
	id      int64
}

func (p position) after(other position) bool {
	return p.created > other.created || (p.created == other.created && p.id > other.id)
}

func positionOf(issue *glIssue) position {
	return position{created: issue.createdMillis(), id: issue.ID}
}

type cursor struct {
	page int
	last position
	// started is set once an issue was considered.
	started bool
}

func parseCursor(value string) (cursor, *collab.Failure) {
	if value == "" {
		return cursor{page: 1}, nil
	}
	match := cursorPattern.FindStringSubmatch(value)
	if match == nil {
		return cursor{}, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "cursor %q was not issued by this provider", value)
	}
	page, _ := strconv.Atoi(match[1])
	created, _ := strconv.ParseInt(match[2], 10, 64)
	id, _ := strconv.ParseInt(match[3], 10, 64)
	return cursor{page: page, last: position{created: created, id: id}, started: true}, nil
}

func (c cursor) String() string {
	return "p" + strconv.Itoa(c.page) + ".t" + strconv.FormatInt(c.last.created, 10) + ".i" + strconv.FormatInt(c.last.id, 10)
}

// fetchPage reads one GitLab page and reports whether a next one exists.
type fetchPage func(page int) ([]glIssue, bool, *collab.Failure)

// collect fills one page of at most size kept issues, in GitLab's creation
// order, resuming at cursor.
func collect(fetch fetchPage, keep func(*glIssue) bool, size int, from string) ([]glIssue, string, *collab.Failure) {
	at, failure := parseCursor(from)
	if failure != nil {
		return nil, "", failure
	}
	resuming := at.started
	out := make([]glIssue, 0, size)
	for fetches := 0; ; fetches++ {
		if fetches == maxFetchesPerPage {
			return out, at.String(), nil
		}
		issues, more, failure := fetch(at.page)
		if failure != nil {
			return nil, "", failure
		}
		if resuming {
			if at.page > 1 && len(issues) > 0 && positionOf(&issues[0]).after(at.last) {
				at.page--
				continue
			}
			resuming = false
		}
		for i := range issues {
			issue := &issues[i]
			here := positionOf(issue)
			if at.started && !here.after(at.last) {
				continue
			}
			if keep(issue) {
				if len(out) == size {
					return out, at.String(), nil
				}
				out = append(out, *issue)
			}
			at.last, at.started = here, true
		}
		if !more {
			return out, "", nil
		}
		at.page++
	}
}
