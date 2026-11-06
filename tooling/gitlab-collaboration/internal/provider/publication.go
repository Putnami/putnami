package provider

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
)

// relatedReference is a keyword and the issue reference after it, in
// GitLab's reference syntax: #12 in the bound project, project#12 in a
// project of its namespace, group/project#12 in any project of the instance,
// or an issue URL. A merge request reference (!12) never matches. The parser
// never fetches a URL as an HTTP destination.
var relatedReference = regexp.MustCompile(`(?i)\b(refs?|related to|close[sd]?|closing|fix(?:e[sd]|ing)?|resolve[sd]?|resolving|implement(?:s|ed|ing)?)\s*:?\s+((?:https?://[^\s<>)]+)|(?:[A-Za-z0-9_.][A-Za-z0-9_./-]*)?#[0-9]+)`)

// issueURLPath is the path of an issue's page: the project path, then
// /-/issues/<iid> or the older /issues/<iid>.
var issueURLPath = regexp.MustCompile(`^/(.+?)(?:/-)?/issues/([0-9]+)/?$`)

type relatedIssue struct {
	project project
	iid     int
}

// relatedIssues reads the issues a description names after a keyword,
// once each, in the order it names them.
func (s *proposalSession) relatedIssues(body string) []relatedIssue {
	var issues []relatedIssue
	seen := map[string]bool{}
	for _, indexes := range relatedReference.FindAllStringSubmatchIndex(body, -1) {
		reference := body[indexes[4]:indexes[5]]
		// A reference ends after its number: a following letter, digit,
		// underscore or hyphen makes the token part of a longer value.
		if indexes[5] < len(body) && referenceSuffix(body[indexes[5]]) {
			continue
		}
		related, ok := s.resolveReference(reference)
		if !ok {
			continue
		}
		key := related.project.source + "#" + strconv.Itoa(related.iid)
		if seen[key] {
			continue
		}
		seen[key] = true
		issues = append(issues, related)
	}
	return issues
}

// resolveReference reads one issue reference. A URL is accepted only on the
// bound host, over https, without credentials, query or fragment.
func (s *proposalSession) resolveReference(reference string) (relatedIssue, bool) {
	path, number := "", ""
	switch lower := strings.ToLower(reference); {
	case strings.HasPrefix(lower, "http://"):
		return relatedIssue{}, false
	case strings.HasPrefix(lower, "https://"):
		parsed, err := url.Parse(strings.TrimRight(reference, ".,;:"))
		if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.EqualFold(parsed.Host, s.project.host) {
			return relatedIssue{}, false
		}
		match := issueURLPath.FindStringSubmatch(parsed.Path)
		if match == nil {
			return relatedIssue{}, false
		}
		path, number = match[1], match[2]
	default:
		qualifier, digits, _ := strings.Cut(reference, "#")
		number = digits
		switch {
		case qualifier == "":
			path = s.project.path
		case !strings.Contains(qualifier, "/"):
			namespace := s.project.path[:strings.LastIndex(s.project.path, "/")]
			path = namespace + "/" + qualifier
		default:
			path = qualifier
		}
	}
	iid, err := strconv.Atoi(number)
	if err != nil || iid <= 0 || strconv.Itoa(iid) != number {
		return relatedIssue{}, false
	}
	target, failure := parseProject(path, s.project.host)
	if failure != nil {
		return relatedIssue{}, false
	}
	return relatedIssue{project: target, iid: iid}, true
}

func referenceSuffix(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

// complete applies what the settings add to every written merge request: the
// author's assignment and the inherited labels. Both only add. A failure
// after the merge request itself was written is unresolved with reason
// gitlab.incomplete: repeating the upsert finds the merge request and applies
// the rest, except after a refusal, which every repeat meets again until the
// credential may do it or the setting is off.
func (s *proposalSession) complete(mr *glMergeRequest) (*glMergeRequest, *collab.Failure) {
	partial := func(what, setting string, failure *collab.Failure) *collab.Failure {
		reconcile := "run proposals.find for the same change, then repeat proposals.upsert: it finds the merge request and applies the rest"
		message := "merge request !%d of %s was written, and %s failed: %s"
		if failure.Outcome == collab.OutcomeDenied {
			reconcile = "let the credential's account succeed at " + what + " in " + s.project.path + ", or turn off settings." + setting +
				", then repeat proposals.upsert: until then every repeat is refused the same way"
			message = "merge request !%d of %s was written, and %s was refused, as it will be on every repeat until the credential may do it: %s"
		}
		result := unresolved(reconcile, message, mr.IID, s.project.path, what, failure.Error.Message)
		result.Error.Reason = reasonIncomplete
		return result
	}
	path := s.project.apiPath("merge_requests", strconv.Itoa(mr.IID))
	if s.settings.AssignAuthor {
		me, failure := s.authenticated()
		if failure != nil {
			return nil, partial("reading the account to assign", "assignAuthor", failure)
		}
		if !slices.ContainsFunc(mr.Assignees, func(user glUser) bool { return user.ID == me.ID }) {
			ids := []int64{}
			for _, user := range mr.Assignees {
				ids = append(ids, user.ID)
			}
			var updated glMergeRequest
			if err := s.client.Write(s.ctx, http.MethodPut, path, map[string]any{"assignee_ids": append(ids, me.ID)}, &updated); err != nil {
				return nil, partial("assigning "+me.Username, "assignAuthor", fail(err, "assign %s", me.Username))
			}
			if !slices.ContainsFunc(updated.Assignees, func(user glUser) bool { return user.ID == me.ID }) {
				return nil, partial("assigning "+me.Username, "assignAuthor", collab.Fail(collab.OutcomeDenied, reasonForbidden, "GitLab did not assign %s", me.Username))
			}
			mr = &updated
		}
	}
	if len(s.settings.InheritLabelPrefixes) == 0 {
		return mr, nil
	}
	var wanted []string
	for _, related := range s.relatedIssues(unescapeBody(deref(mr.Description))) {
		var issue glIssue
		name := related.project.path + "#" + strconv.Itoa(related.iid)
		if _, err := s.client.Get(s.ctx, related.project.apiPath("issues", strconv.Itoa(related.iid)), nil, &issue); err != nil {
			return nil, partial("reading the labels of "+name, "inheritLabelPrefixes", fail(err, "read %s", name))
		}
		for _, label := range issue.labelNames() {
			inherited := slices.ContainsFunc(s.settings.InheritLabelPrefixes, func(prefix string) bool { return strings.HasPrefix(label, prefix) })
			present := slices.ContainsFunc(mr.Labels, func(have string) bool { return strings.EqualFold(have, label) })
			if inherited && !present && !strings.Contains(label, ",") && !slices.ContainsFunc(wanted, func(have string) bool { return strings.EqualFold(have, label) }) {
				wanted = append(wanted, label)
			}
		}
	}
	if len(wanted) == 0 {
		return mr, nil
	}
	var updated glMergeRequest
	if err := s.client.Write(s.ctx, http.MethodPut, path, map[string]any{"add_labels": strings.Join(wanted, ",")}, &updated); err != nil {
		return nil, partial("adding labels "+strings.Join(wanted, ", "), "inheritLabelPrefixes", fail(err, "add labels"))
	}
	for _, label := range wanted {
		if !slices.ContainsFunc(updated.Labels, func(have string) bool { return strings.EqualFold(have, label) }) {
			return nil, partial("adding label "+label, "inheritLabelPrefixes", collab.Fail(collab.OutcomeDenied, reasonForbidden, "GitLab did not add %s", label))
		}
	}
	return &updated, nil
}
