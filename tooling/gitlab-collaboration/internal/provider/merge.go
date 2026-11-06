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
					"a rule of %s or of its target branch makes GitLab squash merge request !%d %t whatever its flag; nothing was sent",
					s.project.path, mr.IID, mr.squashes())
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
			if s.settings.RemoveSourceBranch {
				request["should_remove_source_branch"] = true
			}
			result, failure := s.send(mr, request)
			if failure != nil && failure.Outcome != collab.OutcomeUnresolved {
				return nil, s.restoreSquash(mr, failure)
			}
			return result, failure
		}
		request["squash"] = needed
	}
	if s.settings.RemoveSourceBranch {
		request["should_remove_source_branch"] = true
	}
	return s.send(mr, request)
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

// send sends the merge call once and answers what GitLab did.
func (s *proposalSession) send(mr *glMergeRequest, request map[string]any) (any, *collab.Failure) {
	merged := func(current *glMergeRequest) (any, *collab.Failure) {
		return &collab.ProposalResult{Proposal: s.proposal(current)}, nil
	}
	var answer glMergeRequest
	err := s.client.Write(s.ctx, http.MethodPut, s.project.apiPath("merge_requests", strconv.Itoa(mr.IID), "merge"), request, &answer)
	if err == nil && answer.State == "merged" {
		return merged(&answer)
	}
	if err == nil && answer.MergeError != "" && answer.MergeError != mr.MergeError {
		return nil, collab.Fail(collab.OutcomeConflict, reasonNotMergeable, "merge request !%d cannot be merged: %s", mr.IID,
			collabhelper.Bounded(answer.MergeError, 400))
	}
	if err != nil && err.Kind == gitlab.Answered {
		switch err.Status {
		case http.StatusMethodNotAllowed, http.StatusNotAcceptable, http.StatusUnprocessableEntity:
			return nil, collab.Fail(collab.OutcomeConflict, reasonNotMergeable, "merge request !%d cannot be merged: %s", mr.IID, err.Message)
		case http.StatusConflict:
			return nil, collab.Fail(collab.OutcomeConflict, reasonHeadMoved, "merge request !%d moved: %s", mr.IID, err.Message)
		}
	}
	if err != nil && err.Kind != gitlab.Uncertain {
		return nil, fail(err, "merge merge request !%d", mr.IID)
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
		"merging merge request !%d may have happened (%s), and reading it back did not show it merged", mr.IID, message)
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
	return nil, unresolved("read the proposal with proposals.status, then repeat proposals.merge",
		"setting the squash flag of merge request !%d may have happened (%s), and reading it back did not show it; nothing was merged", mr.IID, err.Message)
}

func describeMethod(method collab.MergeMethod) string {
	if method == collab.MergeMethodRebase {
		return "fast-forward"
	}
	return "create a merge commit"
}
