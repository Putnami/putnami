package provider

import (
	"fmt"
	"net/http"
	"strconv"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/sdk/extension/collabhelper"
	"go.putnami.dev/tooling/gitlab-collaboration/internal/gitlab"
)

// projectMergeMethods are the methods a project's GitLab merge method serves:
// a merge commit, a merge commit after a semi-linear check, or a
// fast-forward.
var projectMergeMethods = map[string]collab.MergeMethod{
	"merge":        collab.MergeMethodMerge,
	"rebase_merge": collab.MergeMethodMerge,
	"ff":           collab.MergeMethodRebase,
}

// merge merges a merge request whose head is still the expected commit, only
// when the binding allows it. The commit goes to GitLab as sha, so GitLab
// refuses the merge itself when the head moved between the read and the
// merge.
func (s *proposalSession) merge(input any) (any, *collab.Failure) {
	in, failure := inputOf[*collab.ProposalMergeInput](input)
	if failure != nil {
		return nil, failure
	}
	if !s.settings.AllowMerge {
		return nil, collab.Fail(collab.OutcomeDenied, reasonMergeDisabled,
			"this binding does not allow merging: settings.allowMerge is not true, and nothing was sent")
	}
	mr, failure := s.mergeRequest(in.Ref)
	if failure != nil {
		return nil, failure
	}
	merged := func(current *glMergeRequest) (any, *collab.Failure) {
		return &collab.ProposalResult{Proposal: s.proposal(current)}, nil
	}
	if !sameCommit(mr.head(), in.ExpectedHeadCommit) {
		return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved, "merge request !%d is at head %s, not %s", mr.IID, mr.head(), in.ExpectedHeadCommit)
	}
	switch mr.State {
	case "merged":
		return merged(mr)
	case "locked":
		return nil, retryable(collab.Fail(collab.OutcomeUnavailable, reasonMerging, "GitLab is merging merge request !%d; read it again", mr.IID))
	case "opened":
	default:
		return nil, collab.Fail(collab.OutcomeConflict, reasonNotMergeable, "merge request !%d is %s", mr.IID, mr.State)
	}
	request := map[string]any{"sha": mr.head()}
	if in.Method != "" {
		var about struct {
			MergeMethod  string `json:"merge_method"`
			SquashOption string `json:"squash_option"`
		}
		if _, err := s.client.Get(s.ctx, s.project.apiPath(), nil, &about); err != nil {
			return nil, fail(err, "read the merge settings of %s", s.project.path)
		}
		switch {
		case in.Method == collab.MergeMethodSquash && about.SquashOption == "never":
			return nil, collab.Fail(collab.OutcomeUnsupported, reasonMergeMethod,
				"%s never squashes merge requests; nothing was sent", s.project.path)
		case in.Method != collab.MergeMethodSquash && about.SquashOption == "always":
			return nil, collab.Fail(collab.OutcomeUnsupported, reasonMergeMethod,
				"%s always squashes merge requests, so a %s merge cannot keep their commits; nothing was sent", s.project.path, in.Method)
		case in.Method != collab.MergeMethodSquash && projectMergeMethods[about.MergeMethod] != in.Method:
			return nil, collab.Fail(collab.OutcomeUnsupported, reasonMergeMethod,
				"%s merges with GitLab's %q method, which does not %s; nothing was sent", s.project.path, about.MergeMethod, describeMethod(in.Method))
		}
		needed := in.Method == collab.MergeMethodSquash
		if mr.squashes() != needed {
			if mr.SquashOnMerge != nil && *mr.SquashOnMerge != mr.Squash {
				return nil, collab.Fail(collab.OutcomeUnsupported, reasonMergeMethod,
					"GitLab reports squash_on_merge %t for merge request !%d while its flag is %t: a rule of %s or of its target branch decides; nothing was sent",
					mr.squashes(), mr.IID, mr.Squash, s.project.path)
			}
			flagged, failure := s.setSquash(mr, needed)
			if failure != nil {
				return nil, failure
			}
			if flagged.squashes() != needed {
				return nil, s.restoreSquash(mr, collab.Fail(collab.OutcomeUnsupported, reasonMergeMethod,
					"merge request !%d was set to squash %t, and GitLab still squashes it %t: a rule of %s or of its target branch decides; nothing was merged",
					mr.IID, needed, flagged.squashes(), s.project.path))
			}
			request["squash"] = needed
			return s.sendRestoring(mr, s.withSettings(request), true)
		}
		request["squash"] = needed
		if needed && !mr.Squash {
			// GitLab's merge call sets the flag when it asks to squash, before
			// it decides whether it can merge.
			return s.sendRestoring(mr, s.withSettings(request), false)
		}
	}
	result, failure, _ := s.send(mr, s.withSettings(request))
	return result, failure
}

// withSettings adds what the binding's settings ask of every merge call.
func (s *proposalSession) withSettings(request map[string]any) map[string]any {
	if s.settings.RemoveSourceBranch {
		request["should_remove_source_branch"] = true
	}
	return request
}

// sendRestoring sends a merge call that changes the merge request's squash
// flag: the provider set it just before (providerWrote), or GitLab's merge
// call sets it itself once it accepted the head. An answer that merged
// nothing puts the flag back first, unless only GitLab could have written it
// and it refused before doing so; an unresolved one names the flag's earlier
// value.
func (s *proposalSession) sendRestoring(mr *glMergeRequest, request map[string]any, providerWrote bool) (any, *collab.Failure) {
	result, failure, gitlabWrote := s.send(mr, request)
	switch {
	case failure == nil:
		return result, nil
	case !providerWrote && !gitlabWrote && failure.Outcome != collab.OutcomeUnresolved:
		return nil, failure
	case failure.Outcome == collab.OutcomeUnresolved:
		failure.Error.Message += fmt.Sprintf("; the squash flag of merge request !%d was %t before this merge", mr.IID, mr.Squash)
		failure.Error.Reconcile += fmt.Sprintf(", and if it did not merge, set its squash flag back to %t", mr.Squash)
		return nil, failure
	}
	return nil, s.restoreSquash(mr, failure)
}

// restoreSquash puts back the squash flag a merge set before an answer that
// merged nothing, so the answer changed nothing; a flag that cannot be put
// back makes the answer unresolved.
func (s *proposalSession) restoreSquash(mr *glMergeRequest, answer *collab.Failure) *collab.Failure {
	if _, failure := s.setSquash(mr, mr.Squash); failure != nil {
		incomplete := unresolved(fmt.Sprintf("read the proposal with proposals.status, then set the squash flag of merge request !%d back to %t in GitLab", mr.IID, mr.Squash),
			"%s; the squash flag of merge request !%d was changed for the merge, and putting it back to %t failed: %s",
			answer.Error.Message, mr.IID, mr.Squash, failure.Error.Message)
		incomplete.Error.Reason = reasonIncomplete
		return incomplete
	}
	answer.Error.Message += "; the squash flag was put back as it was"
	return answer
}

// send sends the merge call once and answers what GitLab did. reached is
// true when GitLab got past its checks of the credential and the head and
// stored the call's squash, which it does before it decides whether it can
// merge: an answer of 405, 406 or 422, or one that did not merge.
func (s *proposalSession) send(mr *glMergeRequest, request map[string]any) (_ any, _ *collab.Failure, reached bool) {
	merged := func(current *glMergeRequest) (any, *collab.Failure, bool) {
		return &collab.ProposalResult{Proposal: s.proposal(current)}, nil, true
	}
	var answer glMergeRequest
	err := s.client.Write(s.ctx, http.MethodPut, s.project.apiPath("merge_requests", strconv.Itoa(mr.IID), "merge"), request, &answer)
	if err == nil && answer.State == "merged" {
		return merged(&answer)
	}
	if err == nil && answer.MergeError != "" && answer.MergeError != mr.MergeError {
		return nil, collab.Fail(collab.OutcomeConflict, reasonNotMergeable, "merge request !%d cannot be merged: %s", mr.IID,
			collabhelper.Bounded(answer.MergeError, 400)), true
	}
	if err != nil && err.Kind == gitlab.Answered {
		switch err.Status {
		case http.StatusMethodNotAllowed, http.StatusNotAcceptable, http.StatusUnprocessableEntity:
			return nil, collab.Fail(collab.OutcomeConflict, reasonNotMergeable, "merge request !%d cannot be merged: %s", mr.IID, err.Message), true
		case http.StatusConflict:
			return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved, "merge request !%d moved: %s", mr.IID, err.Message), false
		}
	}
	if err != nil && err.Kind != gitlab.Uncertain {
		return nil, fail(err, "merge merge request !%d", mr.IID), false
	}
	var current *glMergeRequest
	if s.reconcile(func() (bool, *collab.Failure) {
		read, failure := s.readMergeRequest(mr.IID)
		current = read
		return failure == nil && read.State == "merged", failure
	}) {
		return merged(current)
	}
	message := "GitLab answered without merging"
	if err != nil {
		message = err.Message
	}
	return nil, unresolved("read the proposal with proposals.status before any retry",
		"merging merge request !%d may have happened (%s), and reading it back did not show it merged", mr.IID, message), true
}

// setSquash sets a merge request's squash flag, which GitLab decides a
// merge by and a merge call never clears, and answers the merge request as
// GitLab then reports it.
func (s *proposalSession) setSquash(mr *glMergeRequest, squash bool) (*glMergeRequest, *collab.Failure) {
	var updated glMergeRequest
	err := s.client.Write(s.ctx, http.MethodPut, s.project.apiPath("merge_requests", strconv.Itoa(mr.IID)), map[string]any{"squash": squash}, &updated)
	if err == nil {
		return &updated, nil
	}
	if err.Kind != gitlab.Uncertain {
		return nil, fail(err, "set the squash flag of merge request !%d", mr.IID)
	}
	var current *glMergeRequest
	if s.reconcile(func() (bool, *collab.Failure) {
		read, failure := s.readMergeRequest(mr.IID)
		current = read
		return failure == nil && read.Squash == squash, failure
	}) {
		return current, nil
	}
	return nil, unresolved(fmt.Sprintf("read the proposal with proposals.status, set its squash flag back to %t if it changed, then repeat proposals.merge", mr.Squash),
		"setting the squash flag of merge request !%d may have happened (%s), and reading it back did not show it; nothing was merged", mr.IID, err.Message)
}

func describeMethod(method collab.MergeMethod) string {
	if method == collab.MergeMethodRebase {
		return "fast-forward"
	}
	return "create a merge commit"
}
