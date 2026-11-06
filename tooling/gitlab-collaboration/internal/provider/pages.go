package provider

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
)

// A list answer walks GitLab's numbered pages of 100, oldest first. GitLab
// orders a project's issues by creation time to the microsecond, then by id
// descending, and shows the time to the millisecond only, so the order of
// issues created in one millisecond is not visible. A cursor is
// "p<page>.t<millisecond>.i<id>-<id>…": the GitLab page to resume on, the
// millisecond of the last issue considered, and the ids already considered at
// that millisecond. Resuming skips every issue before that millisecond and
// every id the cursor names, wherever it sits in the page, and reads the page
// before while the resumed page is empty or starts on an issue the cursor has
// not seen: an issue closed or deleted during a traversal may have pulled
// unseen issues there. Every issue present throughout a traversal is
// returned, and none twice unless more than maxTiedIDs issues of one
// millisecond straddle a page boundary.

// gitlabPageSize is the page size every GitLab list request asks for.
const gitlabPageSize = 100

// maxFetchesPerPage bounds the GitLab pages one list answer reads. When it is
// reached, the answer carries the issues found so far and a cursor.
const maxFetchesPerPage = 5

// maxTiedIDs bounds the ids a cursor carries, so it stays within the
// contract's cursor length.
const maxTiedIDs = 48

var cursorPattern = regexp.MustCompile(`^p([1-9][0-9]{0,5})\.t([0-9]{1,15})\.i([0-9]{1,18}(?:-[0-9]{1,18}){0,47})$`)

type cursor struct {
	page int
	// millis is the creation millisecond of the last issue considered.
	millis int64
	// tied are the ids considered at millis, oldest first.
	tied []int64
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
	millis, _ := strconv.ParseInt(match[2], 10, 64)
	at := cursor{page: page, millis: millis, started: true}
	for _, id := range strings.Split(match[3], "-") {
		parsed, _ := strconv.ParseInt(id, 10, 64)
		at.tied = append(at.tied, parsed)
	}
	return at, nil
}

func (c cursor) String() string {
	ids := make([]string, len(c.tied))
	for i, id := range c.tied {
		ids[i] = strconv.FormatInt(id, 10)
	}
	if len(ids) == 0 {
		ids = []string{"0"}
	}
	return "p" + strconv.Itoa(c.page) + ".t" + strconv.FormatInt(c.millis, 10) + ".i" + strings.Join(ids, "-")
}

// considered reports whether the cursor already went past an issue.
func (c *cursor) considered(issue *glIssue) bool {
	if !c.started {
		return false
	}
	millis := issue.createdMillis()
	return millis < c.millis || (millis == c.millis && slices.Contains(c.tied, issue.ID))
}

// consider records an issue as considered.
func (c *cursor) consider(issue *glIssue) {
	millis := issue.createdMillis()
	switch {
	case !c.started || millis > c.millis:
		c.millis, c.tied = millis, []int64{issue.ID}
	case millis == c.millis:
		c.tied = append(c.tied, issue.ID)
		if len(c.tied) > maxTiedIDs {
			c.tied = c.tied[len(c.tied)-maxTiedIDs:]
		}
	}
	c.started = true
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
			if at.page > 1 && (len(issues) == 0 || !at.considered(&issues[0])) {
				at.page--
				continue
			}
			resuming = false
		}
		for i := range issues {
			issue := &issues[i]
			if at.considered(issue) {
				continue
			}
			if keep(issue) {
				if len(out) == size {
					return out, at.String(), nil
				}
				out = append(out, *issue)
			}
			at.consider(issue)
		}
		if !more {
			return out, "", nil
		}
		at.page++
	}
}
