package provider

import (
	"net/http"
	"strconv"

	collab "go.putnami.dev/protocol/collaboration"
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
	switch in.Method {
	case collab.MergeMethodSquash:
		request["squash"] = true
	case collab.MergeMethodMerge, collab.MergeMethodRebase:
		var about struct {
			MergeMethod string `json:"merge_method"`
		}
		if _, err := s.client.Get(s.ctx, s.project.apiPath(), nil, &about); err != nil {
			return nil, fail(err, "read the merge method of %s", s.project.path)
		}
		if projectMergeMethods[about.MergeMethod] != in.Method {
			return nil, collab.Fail(collab.OutcomeUnsupported, reasonMergeMethod,
				"%s merges with GitLab's %q method, which does not %s; nothing was sent", s.project.path, about.MergeMethod, describeMethod(in.Method))
		}
		request["squash"] = false
	}
	if s.settings.RemoveSourceBranch {
		request["should_remove_source_branch"] = true
	}
	var answer glMergeRequest
	err := s.client.Write(s.ctx, http.MethodPut, s.project.apiPath("merge_requests", strconv.Itoa(mr.IID), "merge"), request, &answer)
	if err == nil && answer.State == "merged" {
		return merged(&answer)
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

func describeMethod(method collab.MergeMethod) string {
	if method == collab.MergeMethodRebase {
		return "fast-forward"
	}
	return "create a merge commit"
}
