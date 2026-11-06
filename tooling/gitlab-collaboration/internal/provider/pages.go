package provider

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
)

// A list answer reads GitLab's issues oldest first, from a creation time
// rather than from a page number: GitLab orders a project's issues by
// creation time to the microsecond, then by id descending, shows the time to
// the millisecond only, and moves an offset page whenever an issue before it
// is closed or deleted. Each read asks for the issues created at or after the
// millisecond of the last issue considered. The cursor is that millisecond
// with what was considered at it: "t<millisecond>.i<id>-<id>…", the ids,
// while there are at most maxTiedIDs, and "t<millisecond>.n<count>" past
// them, the count, which the next read skips by position. A read skips every
// id the cursor names wherever it sits in GitLab's answer, and lists at least
// one issue not considered yet, so a traversal always ends and lists every
// issue present throughout it once. Only inside a tie of more than maxTiedIDs
// issues does the cursor count positions, where closing issues of the tie
// during the traversal can hide others of it.

// gitlabPageSize is the page size every GitLab list request asks for.
const gitlabPageSize = 100

// maxFetchesPerPage bounds the GitLab reads one list answer makes. When it is
// reached, the answer carries the issues found so far and a cursor.
const maxFetchesPerPage = 5

// maxTiedIDs bounds the ids a cursor carries, so it stays within the
// contract's cursor length and below a GitLab page: a read anchored at a
// millisecond always holds an issue not considered yet.
const maxTiedIDs = 48

var cursorPattern = regexp.MustCompile(`^t([0-9]{1,15})\.(?:i([0-9]{1,18}(?:-[0-9]{1,18}){0,47})|n([1-9][0-9]{0,8}))$`)

type cursor struct {
	// millis is the creation millisecond of the last issue considered.
	millis int64
	// tied are the ids considered at millis, while counting is false.
	tied []int64
	// counting is set past maxTiedIDs: count issues at millis, the first in
	// GitLab's order, were considered.
	counting bool
	count    int
	// started is set once an issue was considered.
	started bool
}

func parseCursor(value string) (cursor, *collab.Failure) {
	if value == "" {
		return cursor{}, nil
	}
	match := cursorPattern.FindStringSubmatch(value)
	if match == nil {
		return cursor{}, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "cursor %q was not issued by this provider", value)
	}
	millis, _ := strconv.ParseInt(match[1], 10, 64)
	at := cursor{millis: millis, started: true}
	if match[3] != "" {
		at.counting = true
		at.count, _ = strconv.Atoi(match[3])
		return at, nil
	}
	for _, id := range strings.Split(match[2], "-") {
		parsed, _ := strconv.ParseInt(id, 10, 64)
		at.tied = append(at.tied, parsed)
	}
	return at, nil
}

func (c cursor) String() string {
	prefix := "t" + strconv.FormatInt(c.millis, 10)
	if c.counting {
		return prefix + ".n" + strconv.Itoa(c.count)
	}
	ids := make([]string, len(c.tied))
	for i, id := range c.tied {
		ids[i] = strconv.FormatInt(id, 10)
	}
	return prefix + ".i" + strings.Join(ids, "-")
}

// considered reports whether the cursor names an issue of a read anchored at
// its millisecond. In counting mode the read's offset already skips them.
func (c *cursor) considered(issue *glIssue) bool {
	if !c.started {
		return false
	}
	millis := issue.createdMillis()
	return millis < c.millis || (millis == c.millis && !c.counting && slices.Contains(c.tied, issue.ID))
}

// consider records an issue as considered.
func (c *cursor) consider(issue *glIssue) {
	millis := issue.createdMillis()
	switch {
	case !c.started || millis > c.millis:
		*c = cursor{millis: millis, tied: []int64{issue.ID}, started: true}
	case millis == c.millis && c.counting:
		c.count++
	case millis == c.millis:
		c.tied = append(c.tied, issue.ID)
		if len(c.tied) > maxTiedIDs {
			c.counting, c.count, c.tied = true, len(c.tied), nil
		}
	}
}

// fetchPage reads one page of the issues created at or after a millisecond
// (every issue when from is negative) and reports whether a next page exists.
type fetchPage func(from int64, page int) ([]glIssue, bool, *collab.Failure)

// createdAfter renders a millisecond as GitLab's created_after reads it.
func createdAfter(millis int64) string {
	return time.UnixMilli(millis).UTC().Format("2006-01-02T15:04:05.000Z")
}

// collect fills one page of at most size kept issues, in GitLab's creation
// order, resuming at cursor.
func collect(fetch fetchPage, keep func(*glIssue) bool, size int, from string) ([]glIssue, string, *collab.Failure) {
	at, failure := parseCursor(from)
	if failure != nil {
		return nil, "", failure
	}
	out := make([]glIssue, 0, size)
	for fetches := 0; fetches < maxFetchesPerPage; fetches++ {
		anchor, page, skip := int64(-1), 1, 0
		if at.started {
			anchor = at.millis
		}
		if at.counting {
			page, skip = at.count/gitlabPageSize+1, at.count%gitlabPageSize
		}
		issues, more, failure := fetch(anchor, page)
		if failure != nil {
			return nil, "", failure
		}
		progressed := false
		for i := range issues {
			issue := &issues[i]
			if i < skip || at.considered(issue) {
				continue
			}
			if keep(issue) {
				if len(out) == size {
					return out, at.String(), nil
				}
				out = append(out, *issue)
			}
			at.consider(issue)
			progressed = true
		}
		if !more || !progressed {
			return out, "", nil
		}
	}
	return out, at.String(), nil
}
