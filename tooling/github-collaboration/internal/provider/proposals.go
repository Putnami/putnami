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
	"go.putnami.dev/tooling/github-collaboration/internal/github"
)

// ProposalSettings are the settings of a proposals binding.
type ProposalSettings struct {
	repositorySettings
	// AllowMerge lets proposals.merge merge. Without it, merge is refused
	// before anything is sent, whoever calls it.
	AllowMerge bool `json:"allowMerge,omitempty"`
	// AssignAuthor assigns the credential's account to every proposal an
	// upsert writes.
	AssignAuthor bool `json:"assignAuthor,omitempty"`
	// InheritLabelPrefixes copies, onto every proposal an upsert writes, the
	// labels with one of these prefixes that the issues its body closes carry
	// ("Closes #12").
	InheritLabelPrefixes []string `json:"inheritLabelPrefixes,omitempty"`
}

const proposalSettingNames = "repository, host, allowMerge, assignAuthor and inheritLabelPrefixes"

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
		s, failure := p.open(ctx, settings.repositorySettings)
		if failure != nil {
			return nil, failure
		}
		return run(&proposalSession{session: s, settings: settings}, call.Input)
	}
}

// head is a requested head: a branch of the repository, or "owner:branch" of
// a fork.
type head struct {
	// requested is the spelling the request used, which every answer echoes.
	requested string
	owner     string
	branch    string
}

func (s *proposalSession) parseHead(value string) head {
	owner, branch, fork := strings.Cut(value, ":")
	if !fork {
		return head{requested: value, owner: s.repo.owner, branch: value}
	}
	return head{requested: value, owner: owner, branch: branch}
}

// sameRepository reports whether the head is a branch of the served
// repository rather than of a fork.
func (h head) sameRepository(repo repository) bool {
	return strings.EqualFold(h.owner, repo.owner)
}

// param is the head as GitHub's create and filter parameters take it.
func (h head) param() string {
	return h.owner + ":" + h.branch
}

// matches reports whether a pull request is about exactly this base and head.
func (h head) matches(pull *ghPull, repo repository, base string) bool {
	if pull.Base.Ref != base || pull.Head.Ref != h.branch {
		return false
	}
	if pull.Head.Repo == nil {
		return false
	}
	if h.sameRepository(repo) {
		return strings.EqualFold(pull.Head.Repo.FullName, repo.owner+"/"+repo.name)
	}
	return strings.EqualFold(pull.Head.Label, h.param())
}

// proposal renders a pull request. change names the repository and head
// spelling the request used; status passes the canonical ones. Every answer
// reports the pull request's labels and assignees, as [] when it has none.
func (s *proposalSession) proposal(pull *ghPull, repository, headSpelling string) collab.Proposal {
	body := deref(pull.Body)
	state := collab.ProposalStateOpen
	switch {
	case pull.MergedAt != nil:
		state = collab.ProposalStateMerged
	case pull.State == "closed":
		state = collab.ProposalStateClosed
	case pull.Draft:
		state = collab.ProposalStateDraft
	}
	return collab.Proposal{
		Ref:      s.repo.ref(pull.Number),
		Revision: pullRevision(pull),
		URL:      displayURL(pull.HTMLURL),
		Change: collab.Change{Repository: repository, Base: pull.Base.Ref, Head: headSpelling,
			HeadCommit: commit(pull.Head.SHA)},
		Title:         oneLine(pull.Title, collab.MaxTitleLength, "#"+strconv.Itoa(pull.Number)),
		Body:          boundedBody(body),
		State:         state,
		ProviderState: string(state),
		Labels:        boundedLabels(pull.labelNames()),
		Assignees:     boundedLogins(pull.assigneeLogins()),
		UpdatedAt:     timestamp(pull.UpdatedAt),
	}
}

// canonicalHead is the head spelling of a pull request read by reference: the
// branch for the repository's own, owner:branch for a fork's.
func (s *proposalSession) canonicalHead(pull *ghPull) string {
	if pull.Head.Repo != nil && strings.EqualFold(pull.Head.Repo.FullName, s.repo.owner+"/"+s.repo.name) {
		return pull.Head.Ref
	}
	if pull.Head.Label != "" {
		return pull.Head.Label
	}
	return pull.Head.Ref
}

// pullRevision covers what a proposal shows and a caller, a push or the
// provider's settings change: title, body, state, draft, base and head, the
// head commit, labels and assignees.
func pullRevision(pull *ghPull) string {
	labels := pull.labelNames()
	sort.Strings(labels)
	assignees := pull.assigneeLogins()
	sort.Strings(assignees)
	return revisionOf([]any{pull.Title, deref(pull.Body), pull.State, pull.Draft, pull.MergedAt != nil, pull.Base.Ref, pull.Head.Label, pull.Head.SHA,
		labels, assignees})
}

// pull reads the pull request a reference names.
func (s *proposalSession) pull(ref collab.Ref) (*ghPull, *collab.Failure) {
	number, ours := s.repo.number(ref)
	if !ours {
		return nil, collab.Fail(collab.OutcomeNotFound, "proposal.missing", "no proposal %s in source %s", ref.ID, ref.Source)
	}
	var pull ghPull
	if _, err := s.client.Get(s.ctx, s.repo.path("pulls", strconv.Itoa(number)), nil, &pull); err != nil {
		return nil, fail(err, "read pull request #%d of %s", number, s.repo.display)
	}
	return &pull, nil
}

// listPulls pages the pull requests of one base and head, oldest first.
func (s *proposalSession) listPulls(h head, base, state string) fetchPage[ghPull] {
	return func(page int) ([]ghPull, bool, *collab.Failure) {
		query := url.Values{
			"state": {state}, "head": {h.param()}, "base": {base}, "sort": {"created"}, "direction": {"asc"},
			"per_page": {strconv.Itoa(githubPageSize)}, "page": {strconv.Itoa(page)},
		}
		var pulls []ghPull
		header, err := s.client.Get(s.ctx, s.repo.path("pulls"), query, &pulls)
		if err != nil {
			return nil, false, fail(err, "list the pull requests of %s", s.repo.display)
		}
		return pulls, github.HasNext(header), nil
	}
}

func (s *proposalSession) find(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalFindInput](input)
	if failure != nil {
		return nil, failure
	}
	repository, failure := s.repo.named(in.Change.Repository)
	if failure != nil {
		return nil, failure
	}
	h := s.parseHead(in.Change.Head)
	size := collab.DefaultPageSize
	cursor := ""
	if in.Page != nil {
		if in.Page.Size > 0 {
			size = in.Page.Size
		}
		cursor = in.Page.Cursor
	}
	githubState := "all"
	isOpen := func(state collab.ProposalState) bool {
		return state == collab.ProposalStateOpen || state == collab.ProposalStateDraft
	}
	switch {
	case len(in.States) > 0 && !slices.ContainsFunc(in.States, func(state collab.ProposalState) bool { return !isOpen(state) }):
		githubState = "open"
	case len(in.States) > 0 && !slices.ContainsFunc(in.States, isOpen):
		githubState = "closed"
	}
	keep := func(pull ghPull) bool {
		if !h.matches(&pull, s.repo, in.Change.Base) {
			return false
		}
		return len(in.States) == 0 || slices.Contains(in.States, s.proposal(&pull, repository, h.requested).State)
	}
	pulls, next, failure := collect(s.listPulls(h, in.Change.Base, githubState), func(pull ghPull) int { return pull.Number }, keep, size, cursor)
	if failure != nil {
		return nil, failure
	}
	result := &collab.ProposalListResult{Items: make([]collab.Proposal, 0, len(pulls)), Page: collab.Page{Next: next}}
	for i := range pulls {
		result.Items = append(result.Items, s.proposal(&pulls[i], repository, h.requested))
	}
	return result, nil
}

// openPull finds the open pull request of an exact base and head. GitHub
// keeps at most one open pull request per base and head.
func (s *proposalSession) openPull(h head, base string) (*ghPull, *collab.Failure) {
	pulls, _, failure := collect(s.listPulls(h, base, "open"), func(pull ghPull) int { return pull.Number },
		func(pull ghPull) bool { return h.matches(&pull, s.repo, base) }, 1, "")
	if failure != nil || len(pulls) == 0 {
		return nil, failure
	}
	return &pulls[0], nil
}

// headCommit reads the commit a head branch points at. A fork's branch is
// read in the fork that keeps this repository's name: GitHub names no fork
// by its owner alone, so a fork renamed away from it cannot be read.
func (s *proposalSession) headCommit(h head) (string, *collab.Failure) {
	segments := append([]string{"repos", h.owner, s.repo.name, "git", "ref", "heads"}, strings.Split(h.branch, "/")...)
	var ref ghRef
	if _, err := s.client.Get(s.ctx, github.Path(segments...), nil, &ref); err != nil {
		if err.Kind == github.Answered && err.Status == http.StatusNotFound {
			if !h.sameRepository(s.repo) {
				return "", collab.Fail(collab.OutcomeNotFound, reasonHeadMissing,
					"branch %s does not exist in %s/%s, the fork of %s that headCommit is checked in; a fork with another name cannot be checked: upsert without headCommit",
					h.branch, h.owner, s.repo.name, s.repo.display)
			}
			return "", collab.Fail(collab.OutcomeNotFound, reasonHeadMissing, "branch %s does not exist in %s/%s", h.branch, h.owner, s.repo.name)
		}
		return "", fail(err, "read branch %s", h.branch)
	}
	return ref.Object.SHA, nil
}

// upsert finds the open pull request of the exact base and head and updates
// it, or opens it. GitHub refuses a second open pull request for one base and
// head, so neither a repeat nor a concurrent upsert opens two. A requested
// headCommit must be the commit the head points at. expectedRevision is
// compared with the pull request read just before the write: GitHub has no
// conditional write, so a writer between the two is not detected.
func (s *proposalSession) upsert(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalUpsertInput](input)
	if failure != nil {
		return nil, failure
	}
	repository, failure := s.repo.named(in.Change.Repository)
	if failure != nil {
		return nil, failure
	}
	h := s.parseHead(in.Change.Head)
	existing, failure := s.openPull(h, in.Change.Base)
	if failure != nil {
		return nil, failure
	}
	created := false
	if existing == nil {
		if in.ExpectedRevision != "" {
			return nil, conflictAt("", collab.ReasonRevisionConflict,
				"no open proposal exists for %s %s...%s, so revision %s cannot match", repository, in.Change.Base, h.requested, in.ExpectedRevision)
		}
		if in.Change.HeadCommit != "" {
			sha, failure := s.headCommit(h)
			if failure != nil {
				return nil, failure
			}
			if !sameCommit(sha, in.Change.HeadCommit) {
				return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved, "branch %s is at %s, not %s", h.requested, sha, in.Change.HeadCommit)
			}
		}
		existing, created, failure = s.openPullRequest(h, in)
		if failure != nil {
			return nil, failure
		}
	}
	pull := existing
	if !created {
		if current := pullRevision(pull); in.ExpectedRevision != "" && current != in.ExpectedRevision {
			return nil, conflictAt(current, collab.ReasonRevisionConflict,
				"proposal #%d of %s is at revision %s, not %s", pull.Number, s.repo.display, current, in.ExpectedRevision)
		}
		if in.Change.HeadCommit != "" {
			if pull, failure = s.caughtUp(h, pull, in.Change.HeadCommit); failure != nil {
				return nil, failure
			}
		}
		if pull, failure = s.edit(pull, in); failure != nil {
			return nil, failure
		}
	}
	if pull, failure = s.complete(pull); failure != nil {
		return nil, failure
	}
	return &collab.ProposalUpsertResult{Proposal: s.proposal(pull, repository, h.requested), Created: created}, nil
}

// headLagReads is how many times caughtUp re-reads a pull request behind its
// branch: with the default pause, for about 10s.
const headLagReads = 4

// caughtUp returns the pull request once its head is commit. GitHub moves a
// branch as soon as a push lands, and the head of its pull request seconds
// later: when the branch already points at commit, the pull request is
// re-read until it catches up. A branch at another commit is a conflict.
func (s *proposalSession) caughtUp(h head, pull *ghPull, commit string) (*ghPull, *collab.Failure) {
	if sameCommit(pull.Head.SHA, commit) {
		return pull, nil
	}
	sha, failure := s.headCommit(h)
	if failure != nil && failure.Outcome != collab.OutcomeNotFound {
		return nil, failure
	}
	if failure != nil || !sameCommit(sha, commit) {
		return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved,
			"proposal #%d is at head %s, not %s", pull.Number, pull.Head.SHA, commit)
	}
	pause := s.p.HeadLagPause
	if pause == nil {
		pause = reconcilePause
	}
	for attempt := 1; attempt <= headLagReads; attempt++ {
		if pause(s.ctx, attempt) != nil {
			break
		}
		var read ghPull
		if _, err := s.client.Get(s.ctx, s.repo.path("pulls", strconv.Itoa(pull.Number)), nil, &read); err != nil {
			return nil, fail(err, "read pull request #%d of %s", pull.Number, s.repo.display)
		}
		if pull = &read; sameCommit(pull.Head.SHA, commit) {
			return pull, nil
		}
	}
	return nil, retryable(collab.Fail(collab.OutcomeUnavailable, reasonHeadLagging,
		"branch %s is at %s, and proposal #%d has not caught up with the pushed branch yet: it is still at head %s; retry the upsert",
		h.requested, sha, pull.Number, pull.Head.SHA))
}

// openPullRequest opens a pull request. When GitHub answers that one already exists, or
// the outcome is unknown, it reads the open pull request of the base and head
// back: created is true only when that one carries exactly what was sent.
func (s *proposalSession) openPullRequest(h head, in *collab.ProposalUpsertInput) (*ghPull, bool, *collab.Failure) {
	request := map[string]any{"title": in.Title, "head": h.branch, "base": in.Change.Base, "body": in.Body}
	if !h.sameRepository(s.repo) {
		request["head"] = h.param()
	}
	if in.Draft != nil {
		request["draft"] = *in.Draft
	}
	var pull ghPull
	err := s.client.Write(s.ctx, http.MethodPost, s.repo.path("pulls"), request, &pull)
	if err == nil && pull.Number > 0 {
		return &pull, true, nil
	}
	if err != nil && err.Kind != github.Uncertain && !(err.Status == http.StatusUnprocessableEntity && strings.Contains(err.Message, "already exists")) {
		return nil, false, fail(err, "open a pull request in %s", s.repo.display)
	}
	var found *ghPull
	if !s.reconcile(func() (bool, *collab.Failure) {
		pull, failure := s.openPull(h, in.Change.Base)
		found = pull
		return pull != nil, failure
	}) {
		message := "GitHub acknowledged the pull request without its number"
		if err != nil {
			message = err.Message
		}
		return nil, false, unresolved("run proposals.find for the same change: GitHub keeps one open pull request per base and head, so a repeated upsert never opens a second",
			"opening the pull request %s...%s in %s may have happened (%s), and reading it back did not find it", in.Change.Base, h.requested, s.repo.display, message)
	}
	ours := found.Title == in.Title && deref(found.Body) == in.Body
	return found, ours, nil
}

// edit writes the title, body and draft flag the request names onto an open
// pull request, changing only what differs.
func (s *proposalSession) edit(pull *ghPull, in *collab.ProposalUpsertInput) (*ghPull, *collab.Failure) {
	patch := map[string]any{}
	if pull.Title != in.Title {
		patch["title"] = in.Title
	}
	if deref(pull.Body) != in.Body {
		patch["body"] = in.Body
	}
	path := s.repo.path("pulls", strconv.Itoa(pull.Number))
	// uncertain reads a pull request back after a write whose outcome is
	// unknown, and accepts it when applied reports the write in place.
	uncertain := func(err *github.Error, applied func(*ghPull) bool) *collab.Failure {
		if err.Kind != github.Uncertain {
			return fail(err, "update pull request #%d of %s", pull.Number, s.repo.display)
		}
		var current *ghPull
		if s.reconcile(func() (bool, *collab.Failure) {
			read, failure := s.pull(s.repo.ref(pull.Number))
			current = read
			return failure == nil && applied(read), failure
		}) {
			*pull = *current
			return nil
		}
		return unresolved("read the proposal with proposals.status and compare it with the request; repeating proposals.upsert is safe",
			"the update of pull request #%d may have happened (%s), and reading it back did not show it", pull.Number, err.Message)
	}
	if len(patch) > 0 {
		var updated ghPull
		if err := s.client.Write(s.ctx, http.MethodPatch, path, patch, &updated); err != nil {
			edited := func(current *ghPull) bool { return current.Title == in.Title && deref(current.Body) == in.Body }
			if failure := uncertain(err, edited); failure != nil {
				return nil, failure
			}
		} else {
			*pull = updated
		}
	}
	if in.Draft != nil && pull.Draft != *in.Draft && pull.State == "open" {
		mutation := "mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }"
		if *in.Draft {
			mutation = "mutation($id: ID!) { convertPullRequestToDraft(input: {pullRequestId: $id}) { pullRequest { isDraft } } }"
		}
		if err := s.client.GraphQL(s.ctx, mutation, map[string]any{"id": pull.NodeID}, nil); err != nil {
			toggled := func(current *ghPull) bool { return current.Draft == *in.Draft }
			if failure := uncertain(err, toggled); failure != nil {
				return nil, failure
			}
		}
		fresh, failure := s.pull(s.repo.ref(pull.Number))
		if failure != nil {
			return nil, unresolved("read the proposal with proposals.status before any retry",
				"the draft flag of pull request #%d was changed, and reading it back failed: %s", pull.Number, failure.Error.Message)
		}
		pull = fresh
	}
	return pull, nil
}

var closingReference = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+(?:([A-Za-z0-9-]+/[A-Za-z0-9._-]+))?#([0-9]+)\b`)

// closedIssues lists the issue numbers of this repository a body closes.
func (s *proposalSession) closedIssues(body string) []int {
	var numbers []int
	for _, match := range closingReference.FindAllStringSubmatch(body, -1) {
		if match[1] != "" && !strings.EqualFold(match[1], s.repo.owner+"/"+s.repo.name) {
			continue
		}
		if number, err := strconv.Atoi(match[2]); err == nil && number > 0 && !slices.Contains(numbers, number) {
			numbers = append(numbers, number)
		}
	}
	return numbers
}

// complete applies what the settings add to every written pull request: the
// author's assignment and the inherited labels. Both only add. A failure
// after the pull request itself was written is unresolved with reason
// github.incomplete: repeating the upsert finds the pull request and applies
// the rest, except after a refusal, which every repeat meets again until the
// credential may do it or the setting is off.
func (s *proposalSession) complete(pull *ghPull) (*ghPull, *collab.Failure) {
	partial := func(what, setting string, failure *collab.Failure) *collab.Failure {
		reconcile := "run proposals.find for the same change, then repeat proposals.upsert: it finds the pull request and applies the rest"
		message := "pull request #%d of %s was written, and %s failed: %s"
		if failure.Outcome == collab.OutcomeDenied {
			reconcile = "let the credential's account succeed at " + what + " in " + s.repo.display + ", or turn off settings." + setting +
				", then repeat proposals.upsert: until then every repeat is refused the same way"
			message = "pull request #%d of %s was written, and %s was refused, as it will be on every repeat until the credential may do it: %s"
		}
		result := unresolved(reconcile, message, pull.Number, s.repo.display, what, failure.Error.Message)
		result.Error.Reason = reasonIncomplete
		return result
	}
	path := s.repo.path("issues", strconv.Itoa(pull.Number))
	if s.settings.AssignAuthor {
		login, failure := s.authenticated()
		if failure != nil {
			return nil, partial("reading the account to assign", "assignAuthor", failure)
		}
		if !slices.ContainsFunc(pull.Assignees, func(user ghUser) bool { return strings.EqualFold(user.Login, login) }) {
			var issue ghIssue
			if err := s.client.Write(s.ctx, http.MethodPost, path+"/assignees", map[string]any{"assignees": []string{login}}, &issue); err != nil {
				return nil, partial("assigning "+login, "assignAuthor", fail(err, "assign %s", login))
			}
			if !slices.ContainsFunc(issue.assigneeLogins(), func(name string) bool { return strings.EqualFold(name, login) }) {
				return nil, partial("assigning "+login, "assignAuthor", collab.Fail(collab.OutcomeDenied, reasonForbidden, "GitHub did not assign %s", login))
			}
			pull.Assignees = issue.Assignees
		}
	}
	if len(s.settings.InheritLabelPrefixes) == 0 {
		return pull, nil
	}
	var wanted []string
	for _, number := range s.closedIssues(deref(pull.Body)) {
		var issue ghIssue
		if _, err := s.client.Get(s.ctx, s.repo.path("issues", strconv.Itoa(number)), nil, &issue); err != nil {
			return nil, partial("reading the labels of #"+strconv.Itoa(number), "inheritLabelPrefixes", fail(err, "read #%d", number))
		}
		for _, label := range issue.labelNames() {
			inherited := slices.ContainsFunc(s.settings.InheritLabelPrefixes, func(prefix string) bool { return strings.HasPrefix(label, prefix) })
			present := slices.ContainsFunc(pull.Labels, func(have ghLabel) bool { return strings.EqualFold(have.Name, label) })
			if inherited && !present && !slices.Contains(wanted, label) {
				wanted = append(wanted, label)
			}
		}
	}
	if len(wanted) == 0 {
		return pull, nil
	}
	var labels []ghLabel
	if err := s.client.Write(s.ctx, http.MethodPost, path+"/labels", map[string]any{"labels": wanted}, &labels); err != nil {
		return nil, partial("adding labels "+strings.Join(wanted, ", "), "inheritLabelPrefixes", fail(err, "add labels"))
	}
	for _, label := range wanted {
		if !slices.ContainsFunc(labels, func(have ghLabel) bool { return strings.EqualFold(have.Name, label) }) {
			return nil, partial("adding label "+label, "inheritLabelPrefixes", collab.Fail(collab.OutcomeDenied, reasonForbidden, "GitHub did not add %s", label))
		}
	}
	pull.Labels = labels
	return pull, nil
}

func (s *proposalSession) status(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalRefInput](input)
	if failure != nil {
		return nil, failure
	}
	pull, failure := s.pull(in.Ref)
	if failure != nil {
		return nil, failure
	}
	checks, failure := s.checks(pull.Head.SHA)
	if failure != nil {
		return nil, failure
	}
	reviews, failure := s.reviews(pull.Number)
	if failure != nil {
		return nil, failure
	}
	result := &collab.ProposalStatusResult{Proposal: s.proposal(pull, s.repo.display, s.canonicalHead(pull)), Checks: checks}
	for i := range reviews {
		if review, ok := s.renderReview(&reviews[i]); ok {
			result.Reviews = append(result.Reviews, review)
		}
	}
	if len(result.Reviews) > collab.MaxListMembers {
		result.Reviews = result.Reviews[len(result.Reviews)-collab.MaxListMembers:]
	}
	return result, nil
}

// maxReviewPages bounds the reviews of one pull request this provider reads.
const maxReviewPages = 10

// reviews lists every review of a pull request, oldest first.
func (s *proposalSession) reviews(number int) ([]ghReview, *collab.Failure) {
	var all []ghReview
	for page := 1; ; page++ {
		if page > maxReviewPages {
			return nil, collab.Fail(collab.OutcomeUnavailable, reasonTooManyReviews,
				"pull request #%d has more than %d reviews; this provider reads at most that many", number, maxReviewPages*githubPageSize)
		}
		var reviews []ghReview
		query := url.Values{"per_page": {strconv.Itoa(githubPageSize)}, "page": {strconv.Itoa(page)}}
		header, err := s.client.Get(s.ctx, s.repo.path("pulls", strconv.Itoa(number), "reviews"), query, &reviews)
		if err != nil {
			return nil, fail(err, "list the reviews of pull request #%d", number)
		}
		all = append(all, reviews...)
		if !github.HasNext(header) {
			return all, nil
		}
	}
}

// review renders a submitted review. A pending review (not yet submitted)
// and a dismissed one are not reported.
func (s *proposalSession) renderReview(review *ghReview) (collab.Review, bool) {
	verdicts := map[string]collab.ReviewVerdict{
		"APPROVED":          collab.ReviewVerdictApprove,
		"CHANGES_REQUESTED": collab.ReviewVerdictRequestChanges,
		"COMMENTED":         collab.ReviewVerdictComment,
	}
	verdict, reported := verdicts[review.State]
	if !reported {
		return collab.Review{}, false
	}
	result := collab.Review{
		Ref:         collab.Ref{Source: s.repo.source, ID: "review-" + strconv.FormatInt(review.ID, 10)},
		URL:         displayURL(review.HTMLURL),
		Verdict:     verdict,
		Commit:      commit(review.CommitID),
		SubmittedAt: timestamp(review.SubmittedAt),
	}
	if review.User != nil {
		result.Author = token(review.User.Login)
	}
	return result, true
}

// reviewRequest is what a review's idempotency digest covers.
type reviewRequest struct {
	Verdict collab.ReviewVerdict `json:"verdict"`
	Body    string               `json:"body"`
	Commit  string               `json:"commit"`
}

// publish submits a review whose body records the idempotency key. Before
// submitting it reads every review of the pull request for the key; after a
// submission whose outcome is unknown it reads them again, and answers
// unresolved rather than submitting a second one.
func (s *proposalSession) publish(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalReviewInput](input)
	if failure != nil {
		return nil, failure
	}
	pull, failure := s.pull(in.Ref)
	if failure != nil {
		return nil, failure
	}
	marker := newIdempotency("proposals.review", in.IdempotencyKey, reviewRequest{in.Verdict, in.Body, in.Commit})
	existing, failure := s.reviewWithKey(pull.Number, marker)
	if failure != nil {
		return nil, failure
	}
	if existing != nil {
		_, recorded := splitMarker(existing.Body)
		if recorded.digest != marker.digest {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
				"idempotency key %q already published a different review of #%d", in.IdempotencyKey, pull.Number)
		}
		review, _ := s.renderReview(existing)
		return &collab.ProposalReviewResult{Review: review, Created: false}, nil
	}
	events := map[collab.ReviewVerdict]string{
		collab.ReviewVerdictApprove:        "APPROVE",
		collab.ReviewVerdictRequestChanges: "REQUEST_CHANGES",
		collab.ReviewVerdictComment:        "COMMENT",
	}
	request := map[string]any{"event": events[in.Verdict], "body": withMarker(in.Body, marker)}
	if in.Commit != "" {
		request["commit_id"] = in.Commit
		if sameCommit(pull.Head.SHA, in.Commit) {
			request["commit_id"] = pull.Head.SHA
		}
	}
	var published ghReview
	err := s.client.Write(s.ctx, http.MethodPost, s.repo.path("pulls", strconv.Itoa(pull.Number), "reviews"), request, &published)
	if err == nil && published.ID == 0 {
		err = &github.Error{Kind: github.Uncertain, Status: http.StatusOK, Message: "GitHub acknowledged the review without its identifier"}
	}
	if err != nil {
		if err.Kind != github.Uncertain {
			return nil, fail(err, "publish a review of #%d", pull.Number)
		}
		var found *ghReview
		if !s.reconcile(func() (bool, *collab.Failure) {
			review, failure := s.reviewWithKey(pull.Number, marker)
			found = review
			return review != nil, failure
		}) {
			return nil, unresolved("repeat proposals.review with the same idempotencyKey: the key is recorded in the review body, so the repeat returns the review if it was published",
				"publishing the review of #%d may have happened (%s), and reading the reviews back did not find it", pull.Number, err.Message)
		}
		published = *found
	}
	review, reported := s.renderReview(&published)
	if !reported {
		return nil, unresolved("read the proposal with proposals.status before any retry",
			"GitHub recorded the review of #%d as %s, not as submitted", pull.Number, published.State)
	}
	return &collab.ProposalReviewResult{Review: review, Created: true}, nil
}

// reviewWithKey returns the review of a pull request, submitted by the
// credential's account, whose body carries a marker's key. A review someone
// else submitted never answers for a key, even when it copies the marker.
func (s *proposalSession) reviewWithKey(number int, marker idempotency) (*ghReview, *collab.Failure) {
	login, failure := s.authenticated()
	if failure != nil {
		return nil, failure
	}
	reviews, failure := s.reviews(number)
	if failure != nil {
		return nil, failure
	}
	for i := range reviews {
		if reviews[i].User == nil || !strings.EqualFold(reviews[i].User.Login, login) {
			continue
		}
		if _, recorded := splitMarker(reviews[i].Body); recorded != nil && recorded.key == marker.key {
			return &reviews[i], nil
		}
	}
	return nil, nil
}

// merge merges a pull request whose head is still the expected commit, only
// when the binding allows it. GitHub refuses the merge itself when the head
// moved between the read and the merge.
func (s *proposalSession) merge(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalMergeInput](input)
	if failure != nil {
		return nil, failure
	}
	if !s.settings.AllowMerge {
		return nil, collab.Fail(collab.OutcomeDenied, reasonMergeDisabled,
			"this binding does not allow merging: settings.allowMerge is not true, and nothing was sent")
	}
	pull, failure := s.pull(in.Ref)
	if failure != nil {
		return nil, failure
	}
	merged := func(current *ghPull) (any, *collab.Failure) {
		return &collab.ProposalResult{Proposal: s.proposal(current, s.repo.display, s.canonicalHead(current))}, nil
	}
	if !sameCommit(pull.Head.SHA, in.ExpectedHeadCommit) {
		return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved, "pull request #%d is at head %s, not %s", pull.Number, pull.Head.SHA, in.ExpectedHeadCommit)
	}
	if pull.MergedAt != nil {
		return merged(pull)
	}
	if pull.State != "open" {
		return nil, collab.Fail(collab.OutcomeConflict, reasonNotMergeable, "pull request #%d is closed", pull.Number)
	}
	request := map[string]any{"sha": pull.Head.SHA}
	if in.Method != "" {
		request["merge_method"] = string(in.Method)
	}
	var answer ghMerge
	err := s.client.Write(s.ctx, http.MethodPut, s.repo.path("pulls", strconv.Itoa(pull.Number), "merge"), request, &answer)
	if err != nil && err.Kind == github.Answered {
		switch err.Status {
		case http.StatusMethodNotAllowed:
			return nil, collab.Fail(collab.OutcomeConflict, reasonNotMergeable, "pull request #%d cannot be merged: %s", pull.Number, err.Message)
		case http.StatusConflict:
			return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved, "pull request #%d moved: %s", pull.Number, err.Message)
		}
	}
	if err != nil && err.Kind != github.Uncertain {
		return nil, fail(err, "merge pull request #%d", pull.Number)
	}
	var current *ghPull
	if s.reconcile(func() (bool, *collab.Failure) {
		read, failure := s.pull(in.Ref)
		current = read
		return failure == nil && read.MergedAt != nil, failure
	}) {
		return merged(current)
	}
	if err == nil && answer.Merged {
		return nil, unresolved("read the proposal with proposals.status before any retry",
			"GitHub reported pull request #%d merged, and reading it back does not show it yet", pull.Number)
	}
	message := "GitHub answered without merging"
	if err != nil {
		message = err.Message
	}
	return nil, unresolved("read the proposal with proposals.status before any retry",
		"merging pull request #%d may have happened (%s), and reading it back did not show it merged", pull.Number, message)
}
