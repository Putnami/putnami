package provider

import (
	"regexp"
	"strconv"

	collab "go.putnami.dev/protocol/collaboration"
)

// A list answer walks GitHub's numbered pages of 100, in creation order, and
// filters what GitHub cannot. Its cursor is "p<page>.n<number>": the GitHub
// page to resume on and the number of the last item already considered.
// Resuming skips every item up to that number, and steps back while the
// resumed page starts past it, so an item created, closed or deleted during a
// traversal moves no other item across a page boundary: every item present
// throughout is returned exactly once.

// githubPageSize is the page size every GitHub list request asks for.
const githubPageSize = 100

// maxFetchesPerPage bounds the GitHub pages one list answer reads. When it is
// reached, the answer carries the items found so far and a cursor.
const maxFetchesPerPage = 5

var cursorPattern = regexp.MustCompile(`^p([1-9][0-9]{0,5})\.n([0-9]{1,12})$`)

type cursor struct {
	page int
	last int
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
	last, _ := strconv.Atoi(match[2])
	return cursor{page: page, last: last}, nil
}

func (c cursor) String() string {
	return "p" + strconv.Itoa(c.page) + ".n" + strconv.Itoa(c.last)
}

// fetchPage reads one GitHub page and reports whether a next one exists.
type fetchPage[T any] func(page int) ([]T, bool, *collab.Failure)

// collect fills one page of at most size kept items, in GitHub's creation
// order, resuming at cursor.
func collect[T any](fetch fetchPage[T], number func(T) int, keep func(T) bool, size int, from string) ([]T, string, *collab.Failure) {
	at, failure := parseCursor(from)
	if failure != nil {
		return nil, "", failure
	}
	resuming := at.last > 0
	out := make([]T, 0, size)
	for fetches := 0; ; fetches++ {
		if fetches == maxFetchesPerPage {
			return out, at.String(), nil
		}
		items, more, failure := fetch(at.page)
		if failure != nil {
			return nil, "", failure
		}
		if resuming {
			if at.page > 1 && len(items) > 0 && number(items[0]) > at.last {
				at.page--
				continue
			}
			resuming = false
		}
		for _, item := range items {
			n := number(item)
			if n <= at.last {
				continue
			}
			if keep(item) {
				if len(out) == size {
					return out, at.String(), nil
				}
				out = append(out, item)
			}
			at.last = n
		}
		if !more {
			return out, "", nil
		}
		at.page++
	}
}
