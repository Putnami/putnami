package provider

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/gitlab"
)

// ProposalSettings are the settings of a proposals binding.
type ProposalSettings struct {
	// Project is the full path of the project served: namespace/name.
	Project string `json:"project"`
	// Host is the GitLab instance, gitlab.com unless set.
	Host string `json:"host,omitempty"`
	// AllowMerge lets proposals.merge merge. Without it, merge is refused
	// before anything is sent, whoever calls it.
	AllowMerge bool `json:"allowMerge,omitempty"`
	// RemoveSourceBranch asks GitLab to delete the source branch in the merge
	// call itself. Unset, the merge request's own choice applies.
	RemoveSourceBranch bool `json:"removeSourceBranch,omitempty"`
	// AssignAuthor adds the credential's account to the assignees of every
	// merge request an upsert writes.
	AssignAuthor bool `json:"assignAuthor,omitempty"`
	// InheritLabelPrefixes copies, onto every merge request an upsert writes,
	// the labels with one of these prefixes that the issues its description
	// names carry ("Closes #12", "Refs group/project#12").
	InheritLabelPrefixes []string `json:"inheritLabelPrefixes,omitempty"`
}

const proposalSettingNames = "project, host, allowMerge, removeSourceBranch, assignAuthor and inheritLabelPrefixes"

// proposalSession is one proposals call.
type proposalSession struct {
	*session
	settings ProposalSettings
}

func (p *Provider) proposalHandler(run func(*proposalSession, any) (any, *collab.Failure)) collab.Handler {
	return func(ctx context.Context, call collab.Call) (any, *collab.Failure) {
		var settings ProposalSettings
		if failure := decodeSettings(call.Settings, &settings, proposalSettingNames); failure != nil {
			return nil, failure
		}
		for _, prefix := range settings.InheritLabelPrefixes {
			if strings.TrimSpace(prefix) == "" {
				return nil, collab.Fail(collab.OutcomeInvalid, reasonSettings, "settings.inheritLabelPrefixes has an empty prefix")
			}
		}
		ctx, cancel := p.budget(ctx)
		defer cancel()
		s, failure := p.open(ctx, settings.Project, settings.Host)
		if failure != nil {
			return nil, failure
		}
		return run(&proposalSession{session: s, settings: settings}, call.Input)
	}
}

// glMergeRequest is a GitLab merge request as the API answers it.
type glMergeRequest struct {
	ID              int64    `json:"id"`
	IID             int      `json:"iid"`
	ProjectID       int64    `json:"project_id"`
	SourceProjectID int64    `json:"source_project_id"`
	TargetProjectID int64    `json:"target_project_id"`
	Title           string   `json:"title"`
	Description     *string  `json:"description"`
	State           string   `json:"state"`
	Draft           bool     `json:"draft"`
	SourceBranch    string   `json:"source_branch"`
	TargetBranch    string   `json:"target_branch"`
	SHA             *string  `json:"sha"`
	Labels          []string `json:"labels"`
	Assignees       []glUser `json:"assignees"`
	Author          *glUser  `json:"author"`
	WebURL          string   `json:"web_url"`
	UpdatedAt       string   `json:"updated_at"`
	MergeError      string   `json:"merge_error"`
	Squash          bool     `json:"squash"`
	SquashOnMerge   *bool    `json:"squash_on_merge"`
}

// squashes reports whether GitLab squashes the merge request when it merges
// it: squash_on_merge, which folds in the project's and its target branch's
// rules, or the merge request's own flag where GitLab does not report it.
func (m *glMergeRequest) squashes() bool {
	if m.SquashOnMerge != nil {
		return *m.SquashOnMerge
	}
	return m.Squash
}

func (m *glMergeRequest) head() string { return strings.ToLower(deref(m.SHA)) }

func (m *glMergeRequest) labelNames() []string {
	if m.Labels == nil {
		return []string{}
	}
	return slices.Clone(m.Labels)
}

func (m *glMergeRequest) assigneeLogins() []string {
	logins := make([]string, 0, len(m.Assignees))
	for _, user := range m.Assignees {
		logins = append(logins, user.Username)
	}
	return logins
}

// sameProject reports whether a merge request's source branch lives in the
// project it targets, rather than in a fork.
func (m *glMergeRequest) sameProject() bool {
	return m.SourceProjectID != 0 && m.SourceProjectID == m.TargetProjectID
}

// draftPrefix is a title prefix GitLab reads as marking a draft.
var draftPrefix = regexp.MustCompile(`(?i)^\s*(?:\[draft\]|\(draft\)|draft:|draft\s+-|draft\s*$)`)

// storedDraftPrefix is the prefix a read removes from a draft's title: the
// one upsert writes, and the other spellings GitLab sets a draft by.
var storedDraftPrefix = regexp.MustCompile(`(?i)^(?:\[draft\]|\(draft\)|draft:)\s*`)

// rawTitle is the title GitLab stores for a requested title and draft state.
func rawTitle(title string, draft bool) string {
	if draft {
		return "Draft: " + title
	}
	return title
}

// shownTitle is a merge request's title without the prefix that makes it a
// draft.
func (m *glMergeRequest) shownTitle() string {
	if m.Draft {
		return storedDraftPrefix.ReplaceAllString(m.Title, "")
	}
	return m.Title
}

// proposalRef is the reference of a merge request: GitLab's own !<iid>, so it
// never names an issue, whose iids are counted apart.
func (p project) proposalRef(iid int) collab.Ref {
	return collab.Ref{Source: p.source, ID: "!" + strconv.Itoa(iid)}
}

// mergeRequestIID reads the iid of a proposal reference to this project.
func (p project) mergeRequestIID(ref collab.Ref) (int, bool) {
	if ref.Source != p.source {
		return 0, false
	}
	digits, marked := strings.CutPrefix(ref.ID, "!")
	iid, err := strconv.Atoi(digits)
	if !marked || err != nil || iid <= 0 || strconv.Itoa(iid) != digits {
		return 0, false
	}
	return iid, true
}

// named checks a requested repository against the bound project and answers
// the repository every proposal names.
func (s *proposalSession) named(requested string) (string, *collab.Failure) {
	if requested != "" && !strings.EqualFold(requested, s.project.path) {
		return "", collab.Fail(collab.OutcomeInvalid, reasonRepository,
			"this binding serves %s; repository %s is not served", s.project.path, requested)
	}
	return s.project.path, nil
}

// checkHead refuses a head that is not a branch of the bound project. A git
// branch cannot hold a colon, so owner:branch is GitHub's spelling of a fork.
func checkHead(head string) *collab.Failure {
	if strings.Contains(head, ":") {
		return collab.Fail(collab.OutcomeInvalid, reasonHeadFork,
			"head %s names a fork's branch; this provider serves merge requests from branches of the bound project only, and nothing was sent", head)
	}
	return nil
}

// proposal renders a merge request. Every answer reports its labels and
// assignees, [] when it has none.
func (s *proposalSession) proposal(mr *glMergeRequest) collab.Proposal {
	state, providerState := collab.ProposalStateOpen, mr.State
	switch {
	case mr.State == "merged":
		state = collab.ProposalStateMerged
	case mr.State == "closed":
		state = collab.ProposalStateClosed
	case mr.Draft:
		state, providerState = collab.ProposalStateDraft, "draft"
	}
	key := "!" + strconv.Itoa(mr.IID)
	return collab.Proposal{
		Ref:      s.project.proposalRef(mr.IID),
		Revision: mergeRequestRevision(mr),
		URL:      collabhelper.DisplayURL(mr.WebURL, collab.MaxURLLength),
		Change: collab.Change{Repository: s.project.path, Base: mr.TargetBranch, Head: mr.SourceBranch,
			HeadCommit: token(mr.head())},
		Title:         collabhelper.OneLine(mr.shownTitle(), collab.MaxTitleLength, key),
		Body:          collabhelper.BoundedBody(unescapeBody(deref(mr.Description)), collab.MaxBodyBytes),
		State:         state,
		ProviderState: collabhelper.OneLine(providerState, collab.MaxLabelLength, string(state)),
		Labels:        collabhelper.BoundedLabels(mr.labelNames(), collab.MaxLabels, collab.MaxLabelLength),
		Assignees:     boundedLogins(mr.assigneeLogins()),
		UpdatedAt:     collabhelper.Timestamp(mr.UpdatedAt),
	}
}

// mergeRequestRevision covers what a proposal shows and a caller, a push or
// the provider's settings change: the stored title, description, state,
// draft flag, base and head, the head commit, labels and assignees.
func mergeRequestRevision(mr *glMergeRequest) string {
	labels := mr.labelNames()
	sort.Strings(labels)
	assignees := mr.assigneeLogins()
	sort.Strings(assignees)
	return revisionOf([]any{mr.Title, deref(mr.Description), mr.State, mr.Draft, mr.TargetBranch, mr.SourceBranch, mr.head(),
		labels, assignees})
}

// mergeRequest reads the merge request a reference names.
func (s *proposalSession) mergeRequest(ref collab.Ref) (*glMergeRequest, *collab.Failure) {
	iid, ours := s.project.mergeRequestIID(ref)
	if !ours {
		return nil, collab.Fail(collab.OutcomeNotFound, "proposal.missing", "%s", s.project.missingAs("proposal", ref))
	}
	mr, failure := s.readMergeRequest(iid)
	if failure == nil && !mr.sameProject() {
		return nil, collab.Fail(collab.OutcomeNotFound, "proposal.missing",
			"merge request !%d of %s comes from a fork; this provider serves merge requests from branches of the project only", iid, s.project.path)
	}
	return mr, failure
}

func (s *proposalSession) readMergeRequest(iid int) (*glMergeRequest, *collab.Failure) {
	var mr glMergeRequest
	if _, err := s.client.Get(s.ctx, s.project.apiPath("merge_requests", strconv.Itoa(iid)), nil, &mr); err != nil {
		return nil, fail(err, "read merge request !%d of %s", iid, s.project.path)
	}
	return &mr, nil
}

// maxProposalPages bounds the pages of merge requests one find reads for an
// exact base and head.
const maxProposalPages = 5

// mergeRequests reads every merge request of the project from branch head
// into base whose source branch is in the project, ordered by iid. state is
// GitLab's state filter.
func (s *proposalSession) mergeRequests(head, base, state string) ([]glMergeRequest, *collab.Failure) {
	var all []glMergeRequest
	for page := 1; ; page++ {
		if page > maxProposalPages {
			return nil, collab.Fail(collab.OutcomeUnavailable, reasonTooManyItems,
				"more than %d merge requests go from %s into %s; this provider reads at most that many", maxProposalPages*gitlabPageSize, head, base)
		}
		query := url.Values{
			"source_branch": {head}, "target_branch": {base}, "state": {state}, "order_by": {"created_at"}, "sort": {"asc"},
			"per_page": {strconv.Itoa(gitlabPageSize)}, "page": {strconv.Itoa(page)},
		}
		var listed []glMergeRequest
		header, err := s.client.Get(s.ctx, s.project.apiPath("merge_requests"), query, &listed)
		if err != nil {
			return nil, fail(err, "list the merge requests of %s", s.project.path)
		}
		for _, mr := range listed {
			if mr.sameProject() && mr.SourceBranch == head && mr.TargetBranch == base {
				all = append(all, mr)
			}
		}
		if !gitlab.HasNext(header) {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].IID < all[j].IID })
	return slices.CompactFunc(all, func(a, b glMergeRequest) bool { return a.IID == b.IID }), nil
}

var proposalCursorPattern = regexp.MustCompile(`^m([0-9]{1,12})$`)

// find lists the merge requests of an exact base and head by iid. Its cursor
// is m<iid>, the last merge request listed, so every merge request present
// throughout a traversal is listed once. With a headCommit, an open merge
// request behind a branch that already points at it is answered once its
// head caught up.
func (s *proposalSession) find(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalFindInput](input)
	if failure != nil {
		return nil, failure
	}
	if _, failure := s.named(in.Change.Repository); failure != nil {
		return nil, failure
	}
	if failure := checkHead(in.Change.Head); failure != nil {
		return nil, failure
	}
	size, after := collab.DefaultPageSize, 0
	if in.Page != nil {
		if in.Page.Size > 0 {
			size = in.Page.Size
		}
		if in.Page.Cursor != "" {
			match := proposalCursorPattern.FindStringSubmatch(in.Page.Cursor)
			if match == nil {
				return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "cursor %q was not issued by this provider", in.Page.Cursor)
			}
			after, _ = strconv.Atoi(match[1])
		}
	}
	gitlabState := "all"
	isOpen := func(state collab.ProposalState) bool {
		return state == collab.ProposalStateOpen || state == collab.ProposalStateDraft
	}
	switch {
	case len(in.States) > 0 && !slices.ContainsFunc(in.States, func(state collab.ProposalState) bool { return !isOpen(state) }):
		gitlabState = "opened"
	case len(in.States) == 1 && in.States[0] == collab.ProposalStateMerged:
		gitlabState = "merged"
	case len(in.States) == 1 && in.States[0] == collab.ProposalStateClosed:
		gitlabState = "closed"
	}
	all, failure := s.mergeRequests(in.Change.Head, in.Change.Base, gitlabState)
	if failure != nil {
		return nil, failure
	}
	result := &collab.ProposalListResult{Items: []collab.Proposal{}}
	for i := range all {
		mr := &all[i]
		if mr.IID <= after || (len(in.States) > 0 && !slices.Contains(in.States, s.proposal(mr).State)) {
			continue
		}
		if len(result.Items) == size {
			result.Page.Next = "m" + strconv.Itoa(after)
			break
		}
		if in.Change.HeadCommit != "" && mr.State == "opened" {
			if mr, _, failure = s.awaitHead(in.Change.Head, mr, in.Change.HeadCommit); failure != nil {
				return nil, failure
			}
		}
		result.Items = append(result.Items, s.proposal(mr))
		after = mr.IID
	}
	return result, nil
}

// openMergeRequest finds the open merge request of an exact base and head.
// GitLab keeps at most one per source and target branch.
func (s *proposalSession) openMergeRequest(head, base string) (*glMergeRequest, *collab.Failure) {
	open, failure := s.mergeRequests(head, base, "opened")
	if failure != nil || len(open) == 0 {
		return nil, failure
	}
	return &open[0], nil
}

// branchCommit reads the commit a branch of the project points at.
func (s *proposalSession) branchCommit(branch string) (string, *collab.Failure) {
	var answer struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if _, err := s.client.Get(s.ctx, s.project.apiPath("repository", "branches", branch), nil, &answer); err != nil {
		if err.Kind == gitlab.Answered && err.Status == http.StatusNotFound {
			return "", collab.Fail(collab.OutcomeNotFound, reasonHeadMissing, "branch %s does not exist in %s", branch, s.project.path)
		}
		return "", fail(err, "read branch %s of %s", branch, s.project.path)
	}
	return strings.ToLower(answer.Commit.ID), nil
}

// sameCommit reports whether a requested commit, full or abbreviated to at
// least 7 hex digits, is a full commit id.
func sameCommit(full, requested string) bool {
	full, requested = strings.ToLower(full), strings.ToLower(requested)
	return full != "" && len(requested) >= 7 && strings.HasPrefix(full, requested)
}

// headLagReads is how many times awaitHead re-reads a merge request behind
// its branch: with the default pause, for about 10s.
const headLagReads = 4

// awaitHead returns the merge request once its head is commit. GitLab moves
// a branch as soon as a push lands, and the head of its merge request some
// time later: when the branch already points at commit, the merge request is
// re-read until it catches up, and one that does not catch up within
// headLagReads reads is a retryable head.lagging failure. When neither the
// merge request nor its branch is at commit, atCommit is false and the merge
// request is returned as read.
func (s *proposalSession) awaitHead(branch string, mr *glMergeRequest, commit string) (_ *glMergeRequest, atCommit bool, _ *collab.Failure) {
	if sameCommit(mr.head(), commit) {
		return mr, true, nil
	}
	sha, failure := s.branchCommit(branch)
	if failure != nil && failure.Outcome != collab.OutcomeNotFound {
		return nil, false, failure
	}
	if failure != nil || !sameCommit(sha, commit) {
		return mr, false, nil
	}
	pause := s.p.HeadLagPause
	if pause == nil {
		pause = collabhelper.ReconcilePause
	}
	for attempt := 1; attempt <= headLagReads; attempt++ {
		if pause(s.ctx, attempt) != nil {
			break
		}
		read, failure := s.readMergeRequest(mr.IID)
		if failure != nil {
			return nil, false, failure
		}
		if mr = read; sameCommit(mr.head(), commit) {
			return mr, true, nil
		}
	}
	return nil, false, retryable(collab.Fail(collab.OutcomeUnavailable, reasonHeadLagging,
		"branch %s is at %s, and merge request !%d has not caught up with the pushed branch yet: it is still at head %s; retry the call",
		branch, sha, mr.IID, mr.head()))
}

// upsert finds the open merge request of the exact base and head and updates
// it, or opens it. GitLab refuses a second open merge request for one source
// and target branch, so neither a repeat nor a concurrent upsert opens two. A
// requested headCommit must be the commit the branch points at.
// expectedRevision is compared with the merge request read just before the
// write: GitLab has no conditional write, so a writer between the two is not
// detected.
func (s *proposalSession) upsert(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalUpsertInput](input)
	if failure != nil {
		return nil, failure
	}
	repository, failure := s.named(in.Change.Repository)
	if failure != nil {
		return nil, failure
	}
	if failure := checkHead(in.Change.Head); failure != nil {
		return nil, failure
	}
	if draftPrefix.MatchString(in.Title) {
		return nil, collab.Fail(collab.OutcomeInvalid, reasonTitleDraft,
			"title %q starts with a prefix GitLab reads as marking a draft; set draft instead, and nothing was written", in.Title)
	}
	existing, failure := s.openMergeRequest(in.Change.Head, in.Change.Base)
	if failure != nil {
		return nil, failure
	}
	created := false
	if existing == nil {
		if in.ExpectedRevision != "" {
			return nil, conflictAt("", collab.ReasonRevisionConflict,
				"no open proposal exists for %s %s...%s, so revision %s cannot match", repository, in.Change.Base, in.Change.Head, in.ExpectedRevision)
		}
		if in.Change.HeadCommit != "" {
			sha, failure := s.branchCommit(in.Change.Head)
			if failure != nil {
				return nil, failure
			}
			if !sameCommit(sha, in.Change.HeadCommit) {
				return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved, "branch %s is at %s, not %s", in.Change.Head, sha, in.Change.HeadCommit)
			}
		}
		if existing, created, failure = s.openNew(in); failure != nil {
			return nil, failure
		}
	}
	mr := existing
	if !created {
		if current := mergeRequestRevision(mr); in.ExpectedRevision != "" && current != in.ExpectedRevision {
			return nil, conflictAt(current, collab.ReasonRevisionConflict,
				"proposal !%d of %s is at revision %s, not %s", mr.IID, s.project.path, current, in.ExpectedRevision)
		}
		if in.Change.HeadCommit != "" {
			if mr, failure = s.caughtUp(in.Change.Head, mr, in.Change.HeadCommit); failure != nil {
				return nil, failure
			}
		}
		if mr, failure = s.edit(mr, in); failure != nil {
			return nil, failure
		}
	} else if in.Change.HeadCommit != "" {
		if mr, _, failure = s.awaitHead(in.Change.Head, mr, in.Change.HeadCommit); failure != nil {
			return nil, failure
		}
	}
	if mr, failure = s.complete(mr); failure != nil {
		return nil, failure
	}
	return &collab.ProposalUpsertResult{Proposal: s.proposal(mr), Created: created}, nil
}

// caughtUp returns the merge request once its head is commit. A branch at
// another commit is a conflict.
func (s *proposalSession) caughtUp(branch string, mr *glMergeRequest, commit string) (*glMergeRequest, *collab.Failure) {
	mr, atCommit, failure := s.awaitHead(branch, mr, commit)
	if failure != nil {
		return nil, failure
	}
	if !atCommit {
		return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved, "proposal !%d is at head %s, not %s", mr.IID, mr.head(), commit)
	}
	return mr, nil
}

// openNew opens a merge request. When GitLab answers that one already exists,
// or the outcome is unknown, it reads the open merge request of the base and
// head back: created is true only when that one carries exactly what was
// sent.
func (s *proposalSession) openNew(in *collab.ProposalUpsertInput) (*glMergeRequest, bool, *collab.Failure) {
	draft := in.Draft != nil && *in.Draft
	title, description := rawTitle(in.Title, draft), escapeBody(in.Body)
	request := map[string]any{"source_branch": in.Change.Head, "target_branch": in.Change.Base, "title": title, "description": description}
	var mr glMergeRequest
	err := s.client.Write(s.ctx, http.MethodPost, s.project.apiPath("merge_requests"), request, &mr)
	if err == nil && mr.IID > 0 {
		return &mr, true, nil
	}
	if err != nil && err.Kind != gitlab.Uncertain && err.Status != http.StatusConflict {
		return nil, false, fail(err, "open a merge request in %s", s.project.path)
	}
	var found *glMergeRequest
	if !s.reconcile(func() (bool, *collab.Failure) {
		open, failure := s.openMergeRequest(in.Change.Head, in.Change.Base)
		found = open
		return open != nil, failure
	}) {
		message := "GitLab acknowledged the merge request without its iid"
		if err != nil {
			message = err.Message
		}
		return nil, false, unresolved("run proposals.find for the same change: GitLab keeps one open merge request per source and target branch, so a repeated upsert never opens a second",
			"opening the merge request %s...%s in %s may have happened (%s), and reading it back did not find it", in.Change.Base, in.Change.Head, s.project.path, message)
	}
	ours := found.Title == title && deref(found.Description) == description
	return found, ours, nil
}

// edit writes the title, description and draft state the request names onto
// an open merge request in one request, changing only what differs. GitLab
// sets a draft by its title prefix, so an upsert without draft keeps the
// merge request's draft state.
func (s *proposalSession) edit(mr *glMergeRequest, in *collab.ProposalUpsertInput) (*glMergeRequest, *collab.Failure) {
	draft := mr.Draft
	if in.Draft != nil {
		draft = *in.Draft
	}
	title, description := rawTitle(in.Title, draft), escapeBody(in.Body)
	patch := map[string]any{}
	if mr.Title != title || mr.Draft != draft {
		patch["title"] = title
	}
	if deref(mr.Description) != description {
		patch["description"] = description
	}
	if len(patch) == 0 {
		return mr, nil
	}
	path := s.project.apiPath("merge_requests", strconv.Itoa(mr.IID))
	var updated glMergeRequest
	err := s.client.Write(s.ctx, http.MethodPut, path, patch, &updated)
	if err == nil {
		return &updated, nil
	}
	if err.Kind != gitlab.Uncertain {
		return nil, fail(err, "update merge request !%d of %s", mr.IID, s.project.path)
	}
	var current *glMergeRequest
	if s.reconcile(func() (bool, *collab.Failure) {
		read, failure := s.readMergeRequest(mr.IID)
		current = read
		return failure == nil && read.Title == title && read.Draft == draft && deref(read.Description) == description, failure
	}) {
		return current, nil
	}
	return nil, unresolved("read the proposal with proposals.status and compare it with the request; repeating proposals.upsert is safe",
		"the update of merge request !%d may have happened (%s), and reading it back did not show it", mr.IID, err.Message)
}
