package provider

import (
	"encoding/base64"
	"regexp"
	"slices"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/tooling/jira-collaboration/internal/jira"
)

// A find answer walks Jira's search pages of 100, ordered by key, and filters
// what JQL does not. Its cursor is "n<number>", "n<number>.t<token>" or
// "n<number>.b<base64url token>": the number of the last key already
// considered and Jira's token of the page to resume on, absent for the first
// one, kept as it is when it holds only URL-safe characters. Resuming reads that page again and
// skips every key up to that number, so the caller may change the page size
// between pages and an issue created or deleted during a traversal moves no
// other issue across a page boundary.

// jiraPageSize is the page size every search asks for.
const jiraPageSize = 100

// maxFetchesPerPage bounds the Jira pages one find answer reads. When it is
// reached, the answer carries the issues found so far and a cursor.
const maxFetchesPerPage = 5

var cursorPattern = regexp.MustCompile(`^n([0-9]{1,12})(?:\.([tb])([A-Za-z0-9_.=+/-]{1,1000}))?$`)

// plainToken is a Jira page token a cursor carries as it is.
var plainToken = regexp.MustCompile(`^[A-Za-z0-9_.=+/-]{1,1000}$`)

type cursor struct {
	last  int
	token string
}

func parseCursor(value string) (cursor, *collab.Failure) {
	if value == "" {
		return cursor{}, nil
	}
	match := cursorPattern.FindStringSubmatch(value)
	if match == nil {
		return cursor{}, invalidCursor(value)
	}
	last, _ := strconv.Atoi(match[1])
	at := cursor{last: last}
	switch match[2] {
	case "t":
		at.token = match[3]
	case "b":
		token, err := base64.RawURLEncoding.DecodeString(match[3])
		if err != nil || len(token) == 0 {
			return cursor{}, invalidCursor(value)
		}
		at.token = string(token)
	}
	return at, nil
}

func invalidCursor(value string) *collab.Failure {
	return collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "cursor %q was not issued by this provider", value)
}

func (c cursor) String() string {
	value := "n" + strconv.Itoa(c.last)
	switch {
	case c.token == "":
	case plainToken.MatchString(c.token):
		value += ".t" + c.token
	default:
		value += ".b" + base64.RawURLEncoding.EncodeToString([]byte(c.token))
	}
	return value
}

// searchRequest is the body of POST /search/jql.
type searchRequest struct {
	JQL           string   `json:"jql"`
	MaxResults    int      `json:"maxResults"`
	Fields        []string `json:"fields"`
	Properties    []string `json:"properties,omitempty"`
	NextPageToken string   `json:"nextPageToken,omitempty"`
}

type searchResult struct {
	Issues        []jiraIssue `json:"issues"`
	NextPageToken string      `json:"nextPageToken"`
	IsLast        bool        `json:"isLast"`
}

// search reads one page of a JQL search and returns the next page's token,
// empty on the last page.
func (s *session) search(jql, token string, size int) ([]jiraIssue, string, *jira.Error) {
	var result searchResult
	request := searchRequest{JQL: jql, MaxResults: size, Fields: issueFieldNames, Properties: []string{idempotencyProperty}, NextPageToken: token}
	if err := s.client.Search(s.ctx, "/search/jql", request, &result); err != nil {
		return nil, "", err
	}
	next := result.NextPageToken
	if result.IsLast {
		next = ""
	}
	return result.Issues, next, nil
}

// quote renders a JQL string literal: every backslash and double quote is
// escaped, so the value can never end the literal.
func quote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// textOperators are the characters JQL's text search reads as operators.
const textOperators = `+-&|!(){}[]^~*?\:"/'`

// searchText turns a caller's text into the words a text clause matches:
// every operator character becomes a space and the text is lowercased, so
// AND, OR and NOT are words, never operators.
func searchText(text string) string {
	cleaned := strings.Map(func(r rune) rune {
		if strings.ContainsRune(textOperators, r) {
			return ' '
		}
		return r
	}, text)
	return strings.ToLower(strings.Join(strings.Fields(cleaned), " "))
}

// Status category ids, as JQL's statusCategory clause takes them: ids do not
// depend on the site's language.
var categoryIDs = map[string]int{categoryNone: 1, categoryToDo: 2, categoryDone: 3, categoryInProgress: 4}

// findJQL builds the JQL of a find. ok is false when nothing can match: only
// blocked was asked for, or the text is all operators.
func (s *session) findJQL(in *collab.TaskFindInput) (string, bool) {
	clauses := []string{"project = " + quote(s.settings.Project)}
	if len(in.States) > 0 {
		var ids []int
		for _, state := range in.States {
			switch category := categoryOf(state); category {
			case "":
			case categoryToDo:
				ids = append(ids, categoryIDs[categoryNone], categoryIDs[categoryToDo])
			default:
				ids = append(ids, categoryIDs[category])
			}
		}
		if len(ids) == 0 {
			return "", false
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)
		names := make([]string, len(ids))
		for i, id := range ids {
			names[i] = strconv.Itoa(id)
		}
		clauses = append(clauses, "statusCategory IN ("+strings.Join(names, ", ")+")")
	}
	for _, label := range in.Labels {
		clauses = append(clauses, "labels = "+quote(label))
	}
	if in.Query != "" {
		text := searchText(in.Query)
		if text == "" {
			return "", false
		}
		clauses = append(clauses, "(summary ~ "+quote(text)+" OR description ~ "+quote(text)+")")
	}
	return strings.Join(clauses, " AND ") + " ORDER BY key ASC", true
}

// find lists the bound project's issues by key, oldest first.
func (s *session) find(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskFindInput](input)
	if failure != nil {
		return nil, failure
	}
	if failure := refuseReserved(in.Labels); failure != nil {
		return nil, failure
	}
	size := collab.DefaultPageSize
	from := ""
	if in.Page != nil {
		if in.Page.Size > 0 {
			size = in.Page.Size
		}
		from = in.Page.Cursor
	}
	at, failure := parseCursor(from)
	if failure != nil {
		return nil, failure
	}
	result := &collab.TaskListResult{Items: []collab.Task{}}
	jql, possible := s.findJQL(in)
	if !possible {
		return result, nil
	}
	keep := func(issue *jiraIssue) bool {
		return len(in.States) == 0 || slices.Contains(in.States, issue.state())
	}
	for fetches := 0; ; fetches++ {
		if fetches == maxFetchesPerPage {
			return paged(result, at)
		}
		issues, next, err := s.search(jql, at.token, jiraPageSize)
		if err != nil {
			if err.Kind == jira.Answered && err.Status == 400 && at.token != "" {
				return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid,
					"Jira refused the page token of cursor %q, which expires 7 days after it was issued: %s", from, err.Message)
			}
			return nil, fail(err, "search the issues of project %s", s.settings.Project)
		}
		for i := range issues {
			number := keyNumber(issues[i].Key)
			if number <= at.last {
				continue
			}
			if keep(&issues[i]) {
				if len(result.Items) == size {
					return paged(result, at)
				}
				result.Items = append(result.Items, s.task(&issues[i]))
			}
			at.last = number
		}
		if next == "" {
			return result, nil
		}
		at.token = next
	}
}

// paged ends a page that a cursor continues. A Jira token too long for a
// cursor cannot be continued.
func paged(result *collab.TaskListResult, at cursor) (any, *collab.Failure) {
	next := at.String()
	if len(next) > collab.MaxCursorLength {
		return nil, collab.Fail(collab.OutcomeUnavailable, reasonUnavailable,
			"Jira's page token is too long to carry in a cursor of at most %d characters", collab.MaxCursorLength)
	}
	result.Page.Next = next
	return result, nil
}

// get reads one issue of the site by id.
func (s *session) get(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.TaskRefInput](input)
	if failure != nil {
		return nil, failure
	}
	issue, failure := s.issue(in.Ref)
	if failure != nil {
		return nil, failure
	}
	return &collab.TaskResult{Task: s.task(issue)}, nil
}
