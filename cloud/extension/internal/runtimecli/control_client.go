package runtimecli

import (
	"time"

	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// controlClient resolves control-api's generated Go client bound to ctx's
// control plane, forwarding ctx's workspace bearer per call
// (clicore.WorkspaceContext.CallContext). runtimecli's control-api reads go
// through it in place of hand-written HTTP calls, paired with
// clicore.CallWithSession for the CLI's single 401 re-mint. The deploy calls bind
// their own client per call instead (deploy_control.go), to read the refusals
// of older control planes.
func controlClient(ctx *clicore.WorkspaceContext) (*controlapiclient.ControlClient, error) {
	return clicore.NewServiceClient[controlapiclient.ControlClient](controlapiclient.RegisterControlClient, ctx.ServiceBinding())
}

// deploymentsSummary is what runs where in a workspace, as `cloud env status`
// renders and prints it: one entry per project and environment. The generated
// type sorts its members by name and omits zero values, so this type keeps the
// member order and the always-written members of the structured output.
type deploymentsSummary struct {
	WorkspaceID         string                   `json:"workspace_id"`
	Deployments         []deploymentSummaryEntry `json:"deployments"`
	TriggersUnavailable bool                     `json:"triggers_unavailable,omitempty"`
}

// deploymentSummaryEntry is one project's deployment in one environment: its
// last successful release, then its most recent run.
type deploymentSummaryEntry struct {
	Project        string         `json:"project"`
	Environment    string         `json:"environment,omitempty"`
	Revision       string         `json:"revision,omitempty"`
	URL            string         `json:"url,omitempty"`
	State          string         `json:"state,omitempty"`
	LastReleaseID  string         `json:"last_release_id,omitempty"`
	LastDeployedAt string         `json:"last_deployed_at,omitempty"`
	CommitSHA      string         `json:"commit_sha,omitempty"`
	Ref            string         `json:"ref,omitempty"`
	RepositoryURL  string         `json:"repository_url,omitempty"`
	ConfigVersion  string         `json:"config_version,omitempty"`
	LastRunStatus  string         `json:"last_run_status,omitempty"`
	LastRunReason  string         `json:"last_run_reason,omitempty"`
	Trigger        *deployTrigger `json:"trigger,omitempty"`
}

// followedChannel is one channel an environment follows and the newest move of
// it the control plane received. Receipt is nil when no move arrived yet.
type followedChannel struct {
	Channel string              `json:"channel"`
	Receipt *channelMoveReceipt `json:"receipt,omitempty"`
}

// channelMoveReceipt is the record of one channel move: what the channel
// advanced to and what the control plane did about it.
type channelMoveReceipt struct {
	Namespace    string    `json:"namespace"`
	Channel      string    `json:"channel"`
	Generation   int64     `json:"generation"`
	ReleaseSetID string    `json:"release_set_id"`
	MovedAt      time.Time `json:"moved_at,omitzero"`
	ReceivedAt   time.Time `json:"received_at"`
	Disposition  string    `json:"disposition"`
	Reason       string    `json:"reason,omitempty"`
	ReleaseIDs   []string  `json:"release_ids"`
	Settled      bool      `json:"settled"`
}

// convertFollowedChannel reads the generated client's FollowedChannel into the
// shape status_header.go renders and prints.
func convertFollowedChannel(channel controlapiclient.FollowedChannel) followedChannel {
	out := followedChannel{Channel: clicore.Deref(channel.Channel)}
	if receipt, ok := channel.Receipt.Value(); ok {
		converted := convertChannelMoveReceipt(receipt)
		out.Receipt = &converted
	}
	return out
}

func convertChannelMoveReceipt(receipt controlapiclient.ChannelMoveReceipt) channelMoveReceipt {
	releaseIDs := clicore.Deref(receipt.ReleaseIds)
	if releaseIDs == nil {
		releaseIDs = []string{}
	}
	return channelMoveReceipt{
		Namespace:    clicore.Deref(receipt.Namespace),
		Channel:      clicore.Deref(receipt.Channel),
		Generation:   clicore.Deref(receipt.Generation),
		ReleaseSetID: clicore.Deref(receipt.ReleaseSetId),
		MovedAt:      clicore.Deref(receipt.MovedAt),
		ReceivedAt:   clicore.Deref(receipt.ReceivedAt),
		Disposition:  clicore.Deref(receipt.Disposition),
		Reason:       clicore.Deref(receipt.Reason),
		ReleaseIDs:   releaseIDs,
		Settled:      clicore.Deref(receipt.Settled),
	}
}

// convertDeploymentsSummary reads the generated client's
// DeploymentsSummaryResponse into the shape status.go renders and prints.
func convertDeploymentsSummary(resp *controlapiclient.DeploymentsSummaryResponse) *deploymentsSummary {
	entries := clicore.Deref(resp.Deployments)
	out := &deploymentsSummary{
		WorkspaceID:         clicore.Deref(resp.WorkspaceId),
		TriggersUnavailable: clicore.Deref(resp.TriggersUnavailable),
		Deployments:         make([]deploymentSummaryEntry, 0, len(entries)),
	}
	for _, entry := range entries {
		out.Deployments = append(out.Deployments, convertDeploymentSummaryEntry(entry))
	}
	return out
}

func convertDeploymentSummaryEntry(entry controlapiclient.DeploymentSummaryEntry) deploymentSummaryEntry {
	out := deploymentSummaryEntry{
		Project:        clicore.Deref(entry.Project),
		Environment:    clicore.Deref(entry.Environment),
		Revision:       clicore.Deref(entry.Revision),
		URL:            clicore.Deref(entry.Url),
		State:          clicore.Deref(entry.State),
		LastReleaseID:  clicore.Deref(entry.LastReleaseId),
		LastDeployedAt: clicore.Deref(entry.LastDeployedAt),
		CommitSHA:      clicore.Deref(entry.CommitSha),
		Ref:            clicore.Deref(entry.Ref),
		RepositoryURL:  clicore.Deref(entry.RepositoryUrl),
		ConfigVersion:  clicore.Deref(entry.ConfigVersion),
		LastRunStatus:  clicore.Deref(entry.LastRunStatus),
		LastRunReason:  clicore.Deref(entry.LastRunReason),
	}
	if trigger, ok := entry.Trigger.Value(); ok {
		converted := convertDeployTrigger(trigger)
		out.Trigger = &converted
	}
	return out
}

// convertDeployTrigger reads the trigger of a status row. It leaves out the
// retry members (retry_of, retry_reason, retry_actor, retry_attempt), as the
// status command always has; `deploy status` reads them (deployTriggerFrom).
func convertDeployTrigger(trigger controlapiclient.DeployTrigger) deployTrigger {
	return deployTrigger{
		Kind:           clicore.Deref(trigger.Kind),
		Namespace:      clicore.Deref(trigger.Namespace),
		Channel:        clicore.Deref(trigger.Channel),
		Generation:     clicore.Deref(trigger.Generation),
		Cause:          clicore.Deref(trigger.Cause),
		MovedAt:        clicore.Deref(trigger.MovedAt),
		ReleaseSetID:   clicore.Deref(trigger.ReleaseSetId),
		SourceRevision: clicore.Deref(trigger.SourceRevision),
		SourceTree:     clicore.Deref(trigger.SourceTree),
		FactRevision:   clicore.Deref(trigger.FactRevision),
	}
}
