package sourcecli

import (
	"context"
	"fmt"
	"strings"
	"time"

	sourceapiclient "go.putnami.dev/cloud/clients/source-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const (
	// sourcePullRequestPage is the page size of the open pull request read,
	// the largest source-api accepts.
	sourcePullRequestPage = 100
	// sourcePullRequestPages bounds the read: past it the counts are a lower
	// bound and the metrics say so.
	sourcePullRequestPages = 5
	// sourceStaleAfter is how long an open pull request goes without an
	// update before it counts as stale.
	sourceStaleAfter = 14 * 24 * time.Hour
)

// SourceStatusFacts is what `putnami cloud source status` reads: the
// repository view and, when one is connected, its open pull requests.
type SourceStatusFacts struct {
	Repository sourceapiclient.SourceGitHubRepository
	// PullRequests is nil when they were not read: no repository, or a read
	// that failed. The pull requests never change the state.
	PullRequests *SourcePullRequestCounts
}

// SourcePullRequestCounts counts the open pull requests of the repository.
type SourcePullRequestCounts struct {
	Open  int
	Stale int
	// Truncated is true when the read stopped at its page bound, so both
	// counts are lower bounds.
	Truncated bool
}

// sourceStatus backs `putnami cloud source status`: the source node,
// with the connection, its health and its permissions as children.
func sourceStatus(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) error {
	facts, err := readSourceStatus(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	return clicore.WriteStatus(params, ioctx, SourceStatusNodeFrom(facts))
}

// SourceStatusNode is the source line of `putnami cloud status`: whether a
// GitHub repository is connected to the workspace, its health, and its open
// pull requests. A read that fails makes the node unknown.
func SourceStatusNode(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) clicore.StatusNode {
	facts, err := readSourceStatus(params, workspaceRoot, env, ioctx)
	if err != nil {
		return clicore.UnknownStatus("source", "source", err, "putnami cloud source status")
	}
	return SourceStatusNodeFrom(facts)
}

// readSourceStatus reads the repository view, then the open pull requests
// when a repository is connected. The pull request read is best effort.
func readSourceStatus(params map[string]any, workspaceRoot string, env map[string]string, ioctx clicore.IO) (SourceStatusFacts, error) {
	runCtx, stop := sourceCommandContext(ioctx)
	defer stop()
	workspace, err := clicore.ResolveWorkspaceContext(params, workspaceRoot, env, ioctx)
	if err != nil {
		return SourceStatusFacts{}, err
	}
	view, err := sourceCall(runCtx, workspace, workspace.WorkspaceURL(githubSourcePath),
		func(callCtx context.Context, api *sourceapiclient.SourceClient) (*sourceapiclient.SourceGitHubRepository, error) {
			return api.GetV1WorkspacesSourceGithub(callCtx, sourceapiclient.GetV1WorkspacesSourceGithubInput{
				Path: sourceapiclient.GetV1WorkspacesSourceGithubPath{Workspace: workspace.WorkspaceID},
			})
		})
	if err != nil {
		return SourceStatusFacts{}, err
	}
	facts := SourceStatusFacts{Repository: *view}
	if clicore.Deref(view.Connected) {
		facts.PullRequests = readSourcePullRequests(runCtx, workspace, sourceNow(ioctx))
	}
	return facts, nil
}

// readSourcePullRequests counts the open pull requests, at most
// sourcePullRequestPages pages. It answers nil when a page fails.
func readSourcePullRequests(ctx context.Context, workspace *clicore.WorkspaceContext, now time.Time) *SourcePullRequestCounts {
	counts := &SourcePullRequestCounts{}
	limit := int64(sourcePullRequestPage)
	cursor := ""
	for page := 0; page < sourcePullRequestPages; page++ {
		input := sourceapiclient.GetV1WorkspacesSourceGithubPullRequestsInput{
			Path:  sourceapiclient.GetV1WorkspacesSourceGithubPullRequestsPath{Workspace: workspace.WorkspaceID},
			Query: sourceapiclient.GetV1WorkspacesSourceGithubPullRequestsQuery{Limit: &limit},
		}
		if cursor != "" {
			input.Query.Cursor = &cursor
		}
		list, err := sourceCall(ctx, workspace, workspace.WorkspaceURL(githubSourcePath+"/pull-requests"),
			func(callCtx context.Context, api *sourceapiclient.SourceClient) (*sourceapiclient.SourceGitHubPullRequestList, error) {
				return api.GetV1WorkspacesSourceGithubPullRequests(callCtx, input)
			})
		if err != nil {
			return nil
		}
		for _, pull := range clicore.Deref(list.PullRequests) {
			counts.Open++
			updated := clicore.Deref(pull.UpdatedAt)
			if updated.IsZero() {
				updated = clicore.Deref(pull.CreatedAt)
			}
			if !updated.IsZero() && now.Sub(updated) > sourceStaleAfter {
				counts.Stale++
			}
		}
		cursor = strings.TrimSpace(clicore.Deref(list.NextCursor))
		if cursor == "" {
			return counts
		}
	}
	counts.Truncated = true
	return counts
}

// SourceStatusNodeFrom folds the source facts into the source node. A
// workspace with no repository connected is degraded: CI and continuous
// delivery need one. A connected repository whose GitHub App installation
// is suspended or gone is failing. A permission the App lacks is degraded.
// The open pull requests are metrics; they never change the state.
func SourceStatusNodeFrom(facts SourceStatusFacts) clicore.StatusNode {
	view := sourceRepositoryFrom(facts.Repository)
	node := clicore.StatusNode{ID: "source", Title: "source"}
	if !view.Connected || view.Binding == nil {
		connection := clicore.StatusNode{
			ID: "source.connection", Title: "connection", State: clicore.StatusDegraded,
			Detail: "no GitHub repository connected", Fix: "putnami cloud source connect",
		}
		node.Children = []clicore.StatusNode{connection}
		node.State, node.Detail, node.Fix = connection.State, connection.Detail, connection.Fix
		return node
	}
	repository := view.Binding.Owner + "/" + view.Binding.Repo
	connection := clicore.StatusNode{ID: "source.connection", Title: "connection", State: clicore.StatusOK, Detail: repository}
	if view.Binding.InstallationID != 0 {
		connection.Detail += fmt.Sprintf(", installation %d", view.Binding.InstallationID)
	}
	health := sourceHealthNode(view.Health)
	permissions := sourcePermissionsNode(view.Health)
	node.Children = []clicore.StatusNode{connection, health, permissions}
	node.State = clicore.WorstStatus(node.ChildStates()...)

	parts := []string{repository}
	if health.State != clicore.StatusOK {
		parts = append(parts, health.Detail)
	}
	if gaps := len(view.Health.PermissionGaps); gaps > 0 {
		parts = append(parts, fmt.Sprintf("%d permission update(s) required", gaps))
	} else if permissions.State != clicore.StatusOK && health.State == clicore.StatusOK {
		parts = append(parts, permissions.Detail)
	}
	node.Fix = clicore.FirstString(health.Fix, permissions.Fix)
	if pulls := facts.PullRequests; pulls != nil {
		open := fmt.Sprintf("%d open pull request", pulls.Open)
		if pulls.Open != 1 {
			open += "s"
		}
		if pulls.Truncated {
			open = "at least " + open
		}
		if pulls.Stale > 0 {
			open += fmt.Sprintf(" (%d stale)", pulls.Stale)
		}
		parts = append(parts, open)
		node.Metrics = []clicore.StatusMetric{
			{ID: "pull_requests_open", Title: "open pull requests", Value: float64(pulls.Open), Unit: clicore.UnitCount, Kind: clicore.MetricUsage, Estimated: pulls.Truncated},
			{ID: "pull_requests_stale", Title: "stale pull requests (no update in 14 days)", Value: float64(pulls.Stale), Unit: clicore.UnitCount, Kind: clicore.MetricHealth, Estimated: pulls.Truncated},
		}
	}
	node.Detail = strings.Join(parts, ", ")
	return node
}

// sourceHealthNode is the lifecycle of the GitHub App installation and of
// the repository. The permission checks are the permissions child.
func sourceHealthNode(health sourceHealth) clicore.StatusNode {
	node := clicore.StatusNode{ID: "source.health", Title: "health"}
	switch health.Code {
	case "connected", "permission_update_required", "permission_check_failed":
		node.State, node.Detail = clicore.StatusOK, "installation active"
	case "installation_suspended":
		node.State, node.Detail = clicore.StatusFailing, "GitHub App installation suspended"
		node.Fix = "unsuspend the Putnami GitHub App in the GitHub organization or account settings"
	case "installation_uninstalled":
		node.State, node.Detail = clicore.StatusFailing, "GitHub App uninstalled"
		node.Fix = "putnami cloud source connect --replace"
	case "repository_removed":
		node.State, node.Detail = clicore.StatusFailing, "repository removed from the GitHub App installation"
		node.Fix = "putnami cloud source connect --replace"
	case "":
		node.State, node.Detail = clicore.StatusUnknown, "no health reported"
	default:
		node.State, node.Detail = clicore.StatusDegraded, "health "+health.Code
		if health.Healthy {
			node.State = clicore.StatusOK
		}
	}
	return node
}

// sourcePermissionsNode is whether the GitHub App holds every permission it
// needs, and no more.
func sourcePermissionsNode(health sourceHealth) clicore.StatusNode {
	node := clicore.StatusNode{ID: "source.permissions", Title: "permissions"}
	switch {
	case len(health.PermissionGaps) > 0:
		actions := make([]string, 0, len(health.PermissionGaps))
		for _, gap := range health.PermissionGaps {
			actions = append(actions, clicore.FirstString(gap.Action, gap.Permission))
		}
		node.State = clicore.StatusDegraded
		node.Detail = fmt.Sprintf("%d permission update(s) required: %s", len(health.PermissionGaps), strings.Join(actions, "; "))
		node.Fix = "putnami cloud source connect --replace"
	case health.Code == "permission_check_failed":
		node.State, node.Detail = clicore.StatusUnknown, "the GitHub App permissions could not be checked"
	case health.Code == "connected" || health.Code == "permission_update_required":
		node.State, node.Detail = clicore.StatusOK, "every required permission granted"
	default:
		node.State, node.Detail = clicore.StatusUnknown, "not checked while the installation is not active"
	}
	return node
}

// sourceNow is the clock of the status read.
func sourceNow(ioctx clicore.IO) time.Time {
	if ioctx.Now != nil {
		return ioctx.Now()
	}
	return time.Now()
}
