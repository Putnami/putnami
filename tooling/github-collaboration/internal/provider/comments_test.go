package provider

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/features/spectest"
)

// comments reads every comment of a proposal in pages of size, calling
// between after each page, and fails on a page above size.
func (h *harness) comments(ref collab.Ref, size int, between func(page int)) []collab.ProposalComment {
	h.t.Helper()
	var all []collab.ProposalComment
	cursor := ""
	for page := 1; ; page++ {
		if page > 1000 {
			h.t.Fatalf("the comments of %s did not end after %d pages", ref.ID, page-1)
		}
		request := `{"ref":` + refJSON(ref) + `,"page":{"size":` + strconv.Itoa(size)
		if cursor != "" {
			request += `,"cursor":"` + cursor + `"`
		}
		var result collab.ProposalCommentListResult
		h.ok("proposals", "comments", request+`}}`, &result)
		if len(result.Items) > size {
			h.t.Fatalf("a page of %d carried %d comments", size, len(result.Items))
		}
		all = append(all, result.Items...)
		if cursor = result.Page.Next; cursor == "" {
			return all
		}
		if between != nil {
			between(page)
		}
	}
}

// review adds a review directly to the fake, as another client would.
func (h *harness) review(number int, user, state, body string) *fakeReview {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	review := &fakeReview{id: h.fake.nextID, user: user, body: body, state: state, commit: strings.Repeat("b", 40), submitted: h.fake.tick()}
	h.fake.nextID++
	h.fake.reviews[number] = append(h.fake.reviews[number], review)
	return review
}

// lineComment adds a comment on a file line directly to the fake.
func (h *harness) lineComment(number int, comment fakeComment) *fakeComment {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	added := h.fake.comment(comment)
	h.fake.lineComments[number] = append(h.fake.lineComments[number], added)
	return added
}

// conversationComment adds a conversation comment directly to the fake.
func (h *harness) conversationComment(number int, comment fakeComment) *fakeComment {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	added := h.fake.comment(comment)
	h.fake.conversation[number] = append(h.fake.conversation[number], added)
	return added
}

func refIDs(comments []collab.ProposalComment) []string {
	ids := make([]string, 0, len(comments))
	for _, comment := range comments {
		ids = append(ids, comment.Ref.ID)
	}
	return ids
}

// TestCommentsListReviewBodiesLineCommentsAndConversationOnce: whatever the
// page size, comments lists the bodies of the submitted reviews, then the line
// comments, then the conversation comments, each once and oldest first, across
// GitHub's pages of 100. A comment added during a traversal moves no other
// one across a page.
func TestCommentsListReviewBodiesLineCommentsAndConversationOnce(t *testing.T) {
	spectest.Proves(t, feature, "proposal-comments", "comments-list-review-bodies-line-comments-and-conversation-once")
	h := newHarness(t)
	proposal := h.openProposal("Commented")
	n := number(t, proposal.Ref)
	sha := strings.Repeat("b", 40)
	if listed := h.comments(proposal.Ref, 20, nil); len(listed) != 0 {
		t.Fatalf("a proposal nobody wrote on lists %+v", listed)
	}

	var published collab.ProposalReviewResult
	h.ok("proposals", "review", `{"ref":`+refJSON(proposal.Ref)+`,"verdict":"comment","body":"Split this function.","idempotencyKey":"c1"}`, &published)
	reviewID, _ := strconv.ParseInt(strings.TrimPrefix(published.Review.Ref.ID, "review-"), 10, 64)
	h.review(n, "octocat", "PENDING", "A thought not submitted yet.")
	h.review(n, "hubot", "APPROVED", "")
	dismissed := h.review(n, "hubot", "DISMISSED", "Superseded.")
	anchored := h.lineComment(n, fakeComment{user: "octocat", body: "Rename this.", path: "src/a.go", line: 12, commit: sha, review: reviewID})
	reply := h.lineComment(n, fakeComment{user: "hubot", body: "Agreed.", path: "src/a.go", line: 12, commit: sha, review: dismissed.id})
	const conversed = 105
	conversation := make([]string, 0, conversed)
	for i := range conversed {
		comment := fakeComment{user: "octocat", body: "Note " + strconv.Itoa(i) + "."}
		if i == 0 {
			comment.url = "https://octocat:secret@github.com/acme/app/pull/1#issuecomment-1"
		}
		conversation = append(conversation, "comment-"+strconv.FormatInt(h.conversationComment(n, comment).id, 10))
	}
	want := append([]string{published.Review.Ref.ID, "review-" + strconv.FormatInt(dismissed.id, 10),
		"line-" + strconv.FormatInt(anchored.id, 10), "line-" + strconv.FormatInt(reply.id, 10)}, conversation...)

	for _, size := range []int{1, 3, 100} {
		listed := h.comments(proposal.Ref, size, nil)
		if got := refIDs(listed); !slices.Equal(got, want) {
			t.Fatalf("pages of %d list %v, want %v", size, got, want)
		}
		first, line, talk := listed[0], listed[2], listed[4]
		if first.Body != "Split this function." || first.Review == nil || *first.Review != published.Review.Ref || first.Ref != published.Review.Ref ||
			first.Commit != sha || first.Author != "putnami-bot" || first.SubmittedAt == "" || first.URL == "" || first.Path != "" {
			t.Fatalf("a review body: %+v", first)
		}
		if line.Body != "Rename this." || line.Review == nil || *line.Review != published.Review.Ref || line.Path != "src/a.go" ||
			line.Line == nil || *line.Line != 12 || line.Commit != sha || line.Author != "octocat" || line.URL == "" {
			t.Fatalf("a line comment: %+v", line)
		}
		if listed[3].Review == nil || listed[3].Review.ID != "review-"+strconv.FormatInt(dismissed.id, 10) {
			t.Fatalf("a reply names the review it was written in: %+v", listed[3])
		}
		if talk.Body != "Note 0." || talk.Review != nil || talk.Path != "" || talk.Line != nil || talk.Commit != "" || talk.URL != "" ||
			talk.Author != "octocat" || listed[5].URL == "" {
			t.Fatalf("a conversation comment, whose URL carries credentials: %+v, then %+v", talk, listed[5])
		}
	}

	// A comment added during a traversal lists no other twice and skips none
	// that was there throughout.
	listed := h.comments(proposal.Ref, 2, func(page int) {
		switch page {
		case 1:
			h.lineComment(n, fakeComment{user: "hubot", body: "Late.", path: "src/b.go", line: 1, commit: sha, review: reviewID})
		case 3:
			h.conversationComment(n, fakeComment{user: "hubot", body: "Later."})
		}
	})
	got := refIDs(listed)
	if len(got) != len(want)+2 || !slices.Equal(got[:4], want[:4]) || !slices.Equal(got[5:5+len(conversation)], conversation) {
		t.Fatalf("a traversal during writes lists %v", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("%s is listed twice: %v", id, got)
		}
		seen[id] = true
	}

	for _, cursor := range []string{"p1.n1", "x.p1.n1", "r.", "r.p1.n1", "r.n99999999999999999999", "c.forged"} {
		failure := h.fails("proposals", "comments", `{"ref":`+refJSON(proposal.Ref)+`,"page":{"cursor":"`+cursor+`"}}`, collab.OutcomeInvalid)
		if failure.Reason != collab.ReasonRequestInvalid {
			t.Fatalf("cursor %q: %+v", cursor, failure)
		}
	}
	h.fails("proposals", "comments", `{"ref":{"source":"github:acme/other","id":"`+strconv.Itoa(n)+`"}}`, collab.OutcomeNotFound)
	h.fails("proposals", "comments", `{"ref":{"source":"github:acme/app","id":"999"}}`, collab.OutcomeNotFound)
}

// TestAReviewSubmittedAfterANewerOneIsListedOnce: GitHub lists reviews in
// submission order, and a review started as a pending draft keeps the lower
// identifier it got then. Submitted after a newer review, its body is still
// listed once, in identifier order, whatever the page size.
func TestAReviewSubmittedAfterANewerOneIsListedOnce(t *testing.T) {
	spectest.Proves(t, feature, "proposal-comments", "comments-list-review-bodies-line-comments-and-conversation-once")
	h := newHarness(t)
	proposal := h.openProposal("Drafted review")
	n := number(t, proposal.Ref)
	drafted := h.review(n, "octocat", "PENDING", "Started first, submitted last.")
	newer := h.review(n, "hubot", "COMMENTED", "Started last, submitted first.")
	thanks := h.conversationComment(n, fakeComment{user: "octocat", body: "Thanks."})
	h.fake.mu.Lock()
	drafted.state, drafted.submitted = "COMMENTED", h.fake.tick()
	h.fake.reviews[n] = []*fakeReview{newer, drafted}
	h.fake.mu.Unlock()

	want := []string{"review-" + strconv.FormatInt(drafted.id, 10), "review-" + strconv.FormatInt(newer.id, 10),
		"comment-" + strconv.FormatInt(thanks.id, 10)}
	for _, size := range []int{1, 2, 20} {
		listed := h.comments(proposal.Ref, size, nil)
		if got := refIDs(listed); !slices.Equal(got, want) {
			t.Fatalf("pages of %d list %v, want %v", size, got, want)
		}
		if listed[0].Body != "Started first, submitted last." || listed[1].Body != "Started last, submitted first." {
			t.Fatalf("pages of %d list the bodies %q and %q", size, listed[0].Body, listed[1].Body)
		}
	}
}

// TestAReviewBodyIsListedWithoutItsIdempotencyMarker: GitHub keeps the
// marker that records a review's idempotency key in the review body, and
// comments lists that body without it. A review whose body is only the marker
// is no comment.
func TestAReviewBodyIsListedWithoutItsIdempotencyMarker(t *testing.T) {
	spectest.Proves(t, feature, "proposal-comments", "a-review-body-is-listed-without-its-idempotency-marker")
	h := newHarness(t)
	proposal := h.openProposal("Reviewed")
	ref := refJSON(proposal.Ref)
	var review collab.ProposalReviewResult
	h.ok("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"Looks fine.","idempotencyKey":"marked"}`, &review)
	h.ok("proposals", "review", `{"ref":`+ref+`,"verdict":"comment","body":"","idempotencyKey":"only-the-marker"}`, &review)
	h.fake.mu.Lock()
	stored := h.fake.reviews[number(t, proposal.Ref)]
	marked := len(stored) == 2 && strings.Contains(stored[0].body, "putnami-idempotency") && strings.Contains(stored[1].body, "putnami-idempotency")
	h.fake.mu.Unlock()
	if !marked {
		t.Fatal("GitHub does not hold the markers this test strips")
	}
	listed := h.comments(proposal.Ref, 20, nil)
	if len(listed) != 1 || listed[0].Body != "Looks fine." {
		t.Fatalf("the review bodies list %+v", listed)
	}
}

// TestAnOutdatedLineCommentKeepsItsPathWithoutALine: a comment whose line left
// the diff, which GitHub answers with a null line, keeps its file without a
// line. A path the contract cannot carry is dropped with its line, since a
// line never comes without its file.
func TestAnOutdatedLineCommentKeepsItsPathWithoutALine(t *testing.T) {
	spectest.Proves(t, feature, "proposal-comments", "an-outdated-line-comment-keeps-its-path-without-a-line")
	h := newHarness(t)
	proposal := h.openProposal("Rewritten")
	n := number(t, proposal.Ref)
	review := h.review(n, "octocat", "COMMENTED", "")
	h.lineComment(n, fakeComment{user: "octocat", body: "This moved.", path: "src/old.go", commit: strings.Repeat("b", 40), review: review.id})
	h.lineComment(n, fakeComment{user: "octocat", body: "Odd name.", path: "src/a\nb.go", line: 3, review: review.id})
	listed := h.comments(proposal.Ref, 20, nil)
	if len(listed) != 2 {
		t.Fatalf("comments %+v", listed)
	}
	outdated, unnamed := listed[0], listed[1]
	if outdated.Path != "src/old.go" || outdated.Line != nil || outdated.Review == nil || outdated.Review.ID != "review-"+strconv.FormatInt(review.id, 10) {
		t.Fatalf("an outdated line comment: %+v", outdated)
	}
	if unnamed.Path != "" || unnamed.Line != nil || unnamed.Body != "Odd name." {
		t.Fatalf("a comment on a path the contract cannot carry: %+v", unnamed)
	}
}

// TestOnlyALineOfTheNewFileIsReported: GitHub answers a comment on a whole
// file with line 1, and a comment on a deleted line with that line's number
// in the old file. Neither is a line of the file at the comment's commit, so
// each keeps its path, commit and review without a line, while a comment on
// a line of the new file keeps it.
func TestOnlyALineOfTheNewFileIsReported(t *testing.T) {
	spectest.Proves(t, feature, "proposal-comments", "an-outdated-line-comment-keeps-its-path-without-a-line")
	sha := strings.Repeat("b", 40)
	for name, tc := range map[string]struct {
		comment fakeComment
		line    int
	}{
		"a comment on the whole file": {fakeComment{path: "src/a.go", line: 1, subjectType: "file"}, 0},
		"a comment on a deleted line": {fakeComment{path: "src/a.go", line: 935, side: "LEFT"}, 0},
		"a comment on a new line":     {fakeComment{path: "src/a.go", line: 935, side: "RIGHT"}, 935},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			proposal := h.openProposal("Commented file")
			n := number(t, proposal.Ref)
			review := h.review(n, "octocat", "COMMENTED", "")
			tc.comment.user, tc.comment.body, tc.comment.commit, tc.comment.review = "octocat", "See here.", sha, review.id
			h.lineComment(n, tc.comment)
			listed := h.comments(proposal.Ref, 20, nil)
			if len(listed) != 1 {
				t.Fatalf("comments %+v", listed)
			}
			comment := listed[0]
			if comment.Path != "src/a.go" || comment.Commit != sha || comment.Review == nil || comment.Review.ID != "review-"+strconv.FormatInt(review.id, 10) {
				t.Fatalf("the comment lost its path, commit or review: %+v", comment)
			}
			if got := comment.Line; (tc.line == 0) != (got == nil) || got != nil && *got != tc.line {
				t.Fatalf("line %v, want %d (0 for none)", got, tc.line)
			}
		})
	}
}
