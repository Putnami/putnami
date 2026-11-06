package provider

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/gitlab"
)

// A review is a note on the merge request. GitLab has no review object that
// carries a body and a verdict, so the note holds the escaped body, then a
// hidden verdict and the commit it reviewed, then the idempotency marker:
//
//	<!-- putnami-review verdict=<verdict> commit=<sha> -->

var verdictPattern = regexp.MustCompile(`(?:\r?\n)*<!-- putnami-review verdict=(approve|comment|request_changes)(?: commit=([0-9a-f]{7,64}))? -->\s*$`)

// commitPattern is a full or abbreviated commit id.
var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// withVerdict appends the verdict and commit to a review body.
func withVerdict(body string, verdict collab.ReviewVerdict, commit string) string {
	comment := "<!-- putnami-review verdict=" + string(verdict)
	if commit != "" {
		comment += " commit=" + commit
	}
	comment += " -->"
	if strings.TrimSpace(body) == "" {
		return comment
	}
	return strings.TrimRight(body, "\r\n") + "\n\n" + comment
}

// recordedReview is what a review note records.
type recordedReview struct {
	text    string
	verdict collab.ReviewVerdict
	commit  string
	key     *idempotency
}

// readReview separates a note into the review it records. ok is false for a
// note that records no verdict.
func readReview(body string) (recordedReview, bool) {
	rest, key := splitMarker(body)
	match := verdictPattern.FindStringSubmatchIndex(rest)
	if match == nil {
		return recordedReview{}, false
	}
	review := recordedReview{text: unescapeBody(rest[:match[0]]), verdict: collab.ReviewVerdict(rest[match[2]:match[3]]), key: key}
	if match[4] >= 0 {
		review.commit = rest[match[4]:match[5]]
	}
	return review, true
}

// glNote is a merge request note as the API answers it.
type glNote struct {
	ID        int64   `json:"id"`
	Body      string  `json:"body"`
	Author    *glUser `json:"author"`
	CreatedAt string  `json:"created_at"`
	System    bool    `json:"system"`
	Type      *string `json:"type"`
	Position  *struct {
		HeadSHA      string `json:"head_sha"`
		OldPath      string `json:"old_path"`
		NewPath      string `json:"new_path"`
		PositionType string `json:"position_type"`
		NewLine      *int   `json:"new_line"`
	} `json:"position"`
}

func (n *glNote) author() string {
	if n.Author == nil {
		return ""
	}
	return token(n.Author.Username)
}

// noteRef is the reference of a note: a review note and its body, listed as a
// comment, share it.
func (s *proposalSession) noteRef(id int64) collab.Ref {
	return collab.Ref{Source: s.project.source, ID: "note-" + strconv.FormatInt(id, 10)}
}

func noteURL(mr *glMergeRequest, id int64) string {
	if mr.WebURL == "" {
		return ""
	}
	return collabhelper.DisplayURL(mr.WebURL+"#note_"+strconv.FormatInt(id, 10), collab.MaxURLLength)
}

// maxNotePages bounds the pages of notes one call reads.
const maxNotePages = 20

// notes reads every note of a merge request, ordered by id.
func (s *proposalSession) notes(iid int) ([]glNote, *collab.Failure) {
	var all []glNote
	for page := 1; ; page++ {
		if page > maxNotePages {
			return nil, collab.Fail(collab.OutcomeUnavailable, reasonTooManyItems,
				"merge request !%d has more than %d notes; this provider reads at most that many", iid, maxNotePages*gitlabPageSize)
		}
		query := url.Values{"sort": {"asc"}, "order_by": {"created_at"}, "per_page": {strconv.Itoa(gitlabPageSize)}, "page": {strconv.Itoa(page)}}
		var notes []glNote
		header, err := s.client.Get(s.ctx, s.project.apiPath("merge_requests", strconv.Itoa(iid), "notes"), query, &notes)
		if err != nil {
			return nil, fail(err, "list the notes of merge request !%d", iid)
		}
		all = append(all, notes...)
		if !gitlab.HasNext(header) {
			break
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return slices.CompactFunc(all, func(a, b glNote) bool { return a.ID == b.ID }), nil
}

// renderReview renders a note that records a review.
func (s *proposalSession) renderReview(mr *glMergeRequest, note *glNote) (collab.Review, bool) {
	if note.System {
		return collab.Review{}, false
	}
	recorded, ok := readReview(note.Body)
	if !ok {
		return collab.Review{}, false
	}
	return collab.Review{Ref: s.noteRef(note.ID), URL: noteURL(mr, note.ID), Verdict: recorded.verdict, Commit: recorded.commit,
		Author: note.author(), SubmittedAt: collabhelper.Timestamp(note.CreatedAt)}, true
}

// glApprovals is a merge request's approval state.
type glApprovals struct {
	ApprovedBy []struct {
		User glUser `json:"user"`
	} `json:"approved_by"`
}

func (s *proposalSession) approvals(iid int) (*glApprovals, *collab.Failure) {
	var approvals glApprovals
	if _, err := s.client.Get(s.ctx, s.project.apiPath("merge_requests", strconv.Itoa(iid), "approvals"), nil, &approvals); err != nil {
		return nil, fail(err, "read the approvals of merge request !%d", iid)
	}
	return &approvals, nil
}

func (a *glApprovals) by(user *glUser) bool {
	return slices.ContainsFunc(a.ApprovedBy, func(approval struct {
		User glUser `json:"user"`
	}) bool {
		return approval.User.ID == user.ID
	})
}

// status reads a merge request, the commit statuses of its head commit, its
// review notes and its current approvals.
func (s *proposalSession) status(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalRefInput](input)
	if failure != nil {
		return nil, failure
	}
	mr, failure := s.mergeRequest(in.Ref)
	if failure != nil {
		return nil, failure
	}
	checks, failure := s.checks(mr.head())
	if failure != nil {
		return nil, failure
	}
	notes, failure := s.notes(mr.IID)
	if failure != nil {
		return nil, failure
	}
	approvals, failure := s.approvals(mr.IID)
	if failure != nil {
		return nil, failure
	}
	result := &collab.ProposalStatusResult{Proposal: s.proposal(mr), Checks: checks}
	for i := range notes {
		if review, ok := s.renderReview(mr, &notes[i]); ok {
			result.Reviews = append(result.Reviews, review)
		}
	}
	for _, approval := range approvals.ApprovedBy {
		if approval.User.ID <= 0 {
			continue
		}
		result.Reviews = append(result.Reviews, collab.Review{
			Ref:     collab.Ref{Source: s.project.source, ID: fmt.Sprintf("approval-%d-%d", mr.IID, approval.User.ID)},
			Verdict: collab.ReviewVerdictApprove, Author: token(approval.User.Username),
		})
	}
	if len(result.Reviews) > collab.MaxListMembers {
		result.Reviews = result.Reviews[len(result.Reviews)-collab.MaxListMembers:]
	}
	return result, nil
}

// maxCheckPages bounds the pages of commit statuses one status read reads.
const maxCheckPages = 10

// checkSeverity orders checks so a truncated list keeps the ones that decide
// the summary: failing first, then pending, passing and skipped.
var checkSeverity = map[collab.CheckState]int{
	collab.CheckStateFailing: 0, collab.CheckStatePending: 1, collab.CheckStatePassing: 2, collab.CheckStateSkipped: 3,
}

// glCommitStatus is one commit status: a pipeline job or an external status.
type glCommitStatus struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	AllowFailure bool   `json:"allow_failure"`
	TargetURL    string `json:"target_url"`
}

// checkState maps a commit status: success passes, failed and canceled fail
// unless the job may fail, skipped and a manual job allowed to fail are
// skipped, and every other status, a blocking manual job included, is not
// finished.
func checkState(status *glCommitStatus) collab.CheckState {
	switch status.Status {
	case "success":
		return collab.CheckStatePassing
	case "failed", "canceled":
		if status.AllowFailure {
			return collab.CheckStateSkipped
		}
		return collab.CheckStateFailing
	case "skipped":
		return collab.CheckStateSkipped
	case "manual":
		if status.AllowFailure {
			return collab.CheckStateSkipped
		}
	}
	return collab.CheckStatePending
}

// checks reads the commit statuses GitLab keeps for a commit, newest first
// and at most maxCheckPages pages, and keeps the latest of each name, by id:
// GitLab lists the statuses of every pipeline of the commit and hides only
// jobs retried within one. It summarizes them: failing when any fails,
// pending when any is not finished, passing when every one passed or was
// skipped, none when there is none.
func (s *proposalSession) checks(sha string) (collab.Checks, *collab.Failure) {
	summary := collab.Checks{State: collab.ChecksStateNone, Commit: token(sha)}
	if sha == "" {
		summary.Detail = "the merge request has no head commit yet"
		return summary, nil
	}
	latest := map[string]glCommitStatus{}
	truncated := false
	for page := 1; page <= maxCheckPages; page++ {
		query := url.Values{"order_by": {"id"}, "sort": {"desc"}, "per_page": {strconv.Itoa(gitlabPageSize)}, "page": {strconv.Itoa(page)}}
		var statuses []glCommitStatus
		header, err := s.client.Get(s.ctx, s.project.apiPath("repository", "commits", sha, "statuses"), query, &statuses)
		if err != nil {
			return collab.Checks{}, fail(err, "read the commit statuses of %s", sha)
		}
		for _, status := range statuses {
			if kept, seen := latest[status.Name]; !seen || status.ID > kept.ID {
				latest[status.Name] = status
			}
		}
		if !gitlab.HasNext(header) {
			break
		}
		truncated = page == maxCheckPages
	}
	items := make([]collab.Check, 0, len(latest))
	for _, status := range latest {
		items = append(items, collab.Check{Name: collabhelper.OneLine(status.Name, collab.MaxTitleLength, "status"),
			State: checkState(&status), URL: collabhelper.DisplayURL(status.TargetURL, collab.MaxURLLength)})
	}
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
	if truncated {
		summary.Detail = strings.TrimSpace(summary.Detail + fmt.Sprintf(" GitLab reports more than %d statuses for %s; the oldest beyond the newest of them were not read.",
			maxCheckPages*gitlabPageSize, sha))
	}
	summary.Items = items
	return summary, nil
}

// reviewRequest is what a review's idempotency digest covers.
type reviewRequest struct {
	Verdict collab.ReviewVerdict `json:"verdict"`
	Body    string               `json:"body"`
	Commit  string               `json:"commit"`
}

// review publishes a review as a note whose body records the verdict and the
// idempotency key. Before it, approve approves the merge request at the
// reviewed head and request_changes withdraws the account's approval, each
// only when the approval is not already in that state, so a repeat
// converges. A note whose post is lost is read back, and answered unresolved
// rather than posted twice.
func (s *proposalSession) review(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalReviewInput](input)
	if failure != nil {
		return nil, failure
	}
	if in.Commit != "" && !commitPattern.MatchString(in.Commit) {
		return nil, collab.Fail(collab.OutcomeInvalid, reasonCommit, "commit %q is not a commit id", in.Commit)
	}
	mr, failure := s.mergeRequest(in.Ref)
	if failure != nil {
		return nil, failure
	}
	marker := newIdempotency("proposals.review", in.IdempotencyKey, reviewRequest{in.Verdict, in.Body, in.Commit})
	existing, failure := s.reviewWithKey(mr, marker)
	if failure != nil {
		return nil, failure
	}
	if existing != nil {
		recorded, _ := readReview(existing.Body)
		if recorded.key.digest != marker.digest {
			return nil, collab.Fail(collab.OutcomeConflict, collab.ReasonIdempotencyMismatch,
				"idempotency key %q already published a different review of !%d", in.IdempotencyKey, mr.IID)
		}
		review, _ := s.renderReview(mr, existing)
		return &collab.ProposalReviewResult{Review: review, Created: false}, nil
	}
	commit := strings.ToLower(in.Commit)
	if commit == "" || sameCommit(mr.head(), commit) {
		commit = mr.head()
	}
	approval := ""
	switch in.Verdict {
	case collab.ReviewVerdictApprove:
		if commit != mr.head() {
			return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved,
				"merge request !%d is at head %s, not %s: GitLab approves only the current head", mr.IID, mr.head(), in.Commit)
		}
		if approval, failure = s.setApproval(mr, true); failure != nil {
			return nil, failure
		}
	case collab.ReviewVerdictRequestChanges:
		if approval, failure = s.setApproval(mr, false); failure != nil {
			return nil, failure
		}
	}
	body := withMarker(withVerdict(escapeBody(in.Body), in.Verdict, token(commit)), marker)
	var note glNote
	err := s.client.Write(s.ctx, http.MethodPost, s.project.apiPath("merge_requests", strconv.Itoa(mr.IID), "notes"), map[string]any{"body": body}, &note)
	if err == nil && note.ID == 0 {
		err = &gitlab.Error{Kind: gitlab.Uncertain, Status: http.StatusCreated, Message: "GitLab acknowledged the note without its id"}
	}
	if err != nil && err.Kind != gitlab.Uncertain {
		failure := fail(err, "publish a review of !%d", mr.IID)
		if approval == "" {
			return nil, failure
		}
		incomplete := unresolved("repeat proposals.review with the same idempotencyKey: the approval is in place, so the repeat posts the note alone",
			"%s merge request !%d, and posting the review note failed: %s", approval, mr.IID, failure.Error.Message)
		incomplete.Error.Reason = reasonIncomplete
		return nil, incomplete
	}
	if err != nil {
		var found *glNote
		if !s.reconcile(func() (bool, *collab.Failure) {
			note, failure := s.reviewWithKey(mr, marker)
			found = note
			return note != nil, failure
		}) {
			return nil, unresolved("repeat proposals.review with the same idempotencyKey: the key is recorded in the note, so the repeat returns the review if it was published",
				"publishing the review of !%d may have happened (%s), and reading the notes back did not find it", mr.IID, err.Message)
		}
		note = *found
	}
	review, _ := s.renderReview(mr, &note)
	return &collab.ProposalReviewResult{Review: review, Created: true}, nil
}

// setApproval approves a merge request at its head, or withdraws the
// account's approval, unless the approval is already in that state. It
// answers what it did, empty when nothing was needed.
func (s *proposalSession) setApproval(mr *glMergeRequest, approve bool) (string, *collab.Failure) {
	me, failure := s.authenticated()
	if failure != nil {
		return "", failure
	}
	current, failure := s.approvals(mr.IID)
	if failure != nil {
		return "", failure
	}
	if current.by(me) == approve {
		return "", nil
	}
	action, done := "unapprove", "withdrew the approval of"
	var request any
	if approve {
		action, request, done = "approve", map[string]any{"sha": mr.head()}, "approved"
	}
	err := s.client.Write(s.ctx, http.MethodPost, s.project.apiPath("merge_requests", strconv.Itoa(mr.IID), action), request, nil)
	if err == nil {
		return done, nil
	}
	if err.Kind == gitlab.Answered && err.Status == http.StatusConflict {
		return "", collab.Fail(collab.OutcomeConflict, reasonHeadMoved, "merge request !%d moved: %s", mr.IID, err.Message)
	}
	if err.Kind != gitlab.Uncertain {
		return "", fail(err, "%s merge request !%d", action, mr.IID)
	}
	if s.reconcile(func() (bool, *collab.Failure) {
		read, failure := s.approvals(mr.IID)
		return failure == nil && read.by(me) == approve, failure
	}) {
		return done, nil
	}
	return "", unresolved("read the proposal with proposals.status, then repeat proposals.review with the same idempotencyKey",
		"the %s of merge request !%d may have happened (%s), and reading its approvals back did not show it", action, mr.IID, err.Message)
}

// reviewWithKey returns the note of a merge request, written by the
// credential's account, that records a marker's key. A note someone else
// wrote never answers for a key, even when it copies the marker.
func (s *proposalSession) reviewWithKey(mr *glMergeRequest, marker idempotency) (*glNote, *collab.Failure) {
	me, failure := s.authenticated()
	if failure != nil {
		return nil, failure
	}
	notes, failure := s.notes(mr.IID)
	if failure != nil {
		return nil, failure
	}
	for i := range notes {
		if notes[i].System || notes[i].Author == nil || notes[i].Author.ID != me.ID {
			continue
		}
		if recorded, ok := readReview(notes[i].Body); ok && recorded.key != nil && recorded.key.key == marker.key {
			return &notes[i], nil
		}
	}
	return nil, nil
}

var noteCursorPattern = regexp.MustCompile(`^n([0-9]{1,19})$`)

// comments lists the notes people wrote on a merge request, by ascending note
// id: the cursor n<id> is the last note listed, so every note present
// throughout a traversal is listed once. System notes are not comments.
func (s *proposalSession) comments(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalCommentsInput](input)
	if failure != nil {
		return nil, failure
	}
	mr, failure := s.mergeRequest(in.Ref)
	if failure != nil {
		return nil, failure
	}
	size, after := collab.DefaultPageSize, int64(0)
	if in.Page != nil {
		if in.Page.Size > 0 {
			size = in.Page.Size
		}
		if in.Page.Cursor != "" {
			match := noteCursorPattern.FindStringSubmatch(in.Page.Cursor)
			var err error
			if match != nil {
				after, err = strconv.ParseInt(match[1], 10, 64)
			}
			if match == nil || err != nil {
				return nil, collab.Fail(collab.OutcomeInvalid, collab.ReasonRequestInvalid, "cursor %q was not issued by this provider", in.Page.Cursor)
			}
		}
	}
	notes, failure := s.notes(mr.IID)
	if failure != nil {
		return nil, failure
	}
	result := &collab.ProposalCommentListResult{Items: []collab.ProposalComment{}}
	for i := range notes {
		if notes[i].ID <= after {
			continue
		}
		comment, kept := s.comment(mr, &notes[i])
		if !kept {
			continue
		}
		if len(result.Items) == size {
			result.Page.Next = "n" + strconv.FormatInt(after, 10)
			break
		}
		result.Items = append(result.Items, comment)
		after = notes[i].ID
	}
	return result, nil
}

// comment renders a note people wrote. A review note lists its body without
// its markers and names itself as its review. A diff note keeps its path, and
// its line only when it is anchored to a line of the new file. A system note
// and an empty one are no comment.
func (s *proposalSession) comment(mr *glMergeRequest, note *glNote) (collab.ProposalComment, bool) {
	if note.System {
		return collab.ProposalComment{}, false
	}
	result := collab.ProposalComment{Ref: s.noteRef(note.ID), URL: noteURL(mr, note.ID), Author: note.author(),
		SubmittedAt: collabhelper.Timestamp(note.CreatedAt)}
	text := note.Body
	if recorded, ok := readReview(note.Body); ok {
		text = recorded.text
		review := s.noteRef(note.ID)
		result.Review = &review
		result.Commit = recorded.commit
	}
	result.Body = collabhelper.BoundedBody(text, collab.MaxBodyBytes)
	if strings.TrimSpace(result.Body) == "" {
		return collab.ProposalComment{}, false
	}
	if position := note.Position; position != nil && note.Type != nil && *note.Type == "DiffNote" {
		path := position.NewPath
		if path == "" {
			path = position.OldPath
		}
		if result.Path = filePath(path); result.Path != "" {
			result.Commit = token(strings.ToLower(position.HeadSHA))
			if position.PositionType != "file" && position.NewLine != nil && *position.NewLine > 0 {
				line := *position.NewLine
				result.Line = &line
			}
		}
	}
	return result, true
}

// filePath keeps a repository-relative path the contract accepts.
func filePath(path string) string {
	if strings.TrimSpace(path) == "" || utf8.RuneCountInString(path) > collab.MaxLocatorLength || strings.ContainsAny(path, "\x00\r\n") {
		return ""
	}
	return path
}
