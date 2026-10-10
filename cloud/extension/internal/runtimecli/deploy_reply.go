package runtimecli

import (
	"time"

	"go.putnami.dev/client"
	controlapiclient "go.putnami.dev/cloud/clients/control-api/go"
	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The deploy commands read control-api's generated replies once, into the
// types below, and render and print those. The generated types sort their
// members by name and omit every zero value, so printing them would change the
// structured output. These types keep the member order, the always-written
// members and the null-versus-empty lists the deploy output has always had.

// deployResponse is a deploy submit reply, and the state a --wait poll reads.
type deployResponse struct {
	State                   string           `json:"state,omitempty"`
	ReleaseID               string           `json:"release_id,omitempty"`
	ManifestHash            string           `json:"manifest_hash,omitempty"`
	Projects                []releaseProject `json:"projects,omitempty"`
	Async                   bool             `json:"async,omitempty"`
	Environment             string           `json:"environment,omitempty"`
	CloudWorkloadCount      int              `json:"cloud_workload_count,omitempty"`
	RequestedWorkloadCount  int              `json:"requested_workload_count,omitempty"`
	ServingNewRevisionCount int              `json:"serving_new_revision_count,omitempty"`
	ReusedWorkloadCount     int              `json:"reused_workload_count,omitempty"`
	HeldWorkloadCount       int              `json:"held_workload_count,omitempty"`
	TerminalClassification  string           `json:"terminal_classification,omitempty"`
	ExecutionBackend        string           `json:"execution_backend,omitempty"`
	WorkerService           string           `json:"worker_service,omitempty"`
	WorkerRevision          string           `json:"worker_revision,omitempty"`
	AttemptNumber           int64            `json:"attempt_number,omitempty"`
	AttemptStatus           string           `json:"attempt_status,omitempty"`
	Attempts                []releaseAttempt `json:"attempts,omitempty"`
	Timings                 *releaseTimings  `json:"timings,omitempty"`
}

// releaseResponse is one release as `deploy status` prints it.
type releaseResponse struct {
	ReleaseID               string           `json:"release_id"`
	State                   string           `json:"state"`
	ManifestHash            string           `json:"manifest_hash,omitempty"`
	Projects                []releaseProject `json:"projects"`
	CloudWorkloadCount      int              `json:"cloud_workload_count,omitempty"`
	RequestedWorkloadCount  int              `json:"requested_workload_count,omitempty"`
	ServingNewRevisionCount int              `json:"serving_new_revision_count,omitempty"`
	ReusedWorkloadCount     int              `json:"reused_workload_count,omitempty"`
	HeldWorkloadCount       int              `json:"held_workload_count,omitempty"`
	TerminalClassification  string           `json:"terminal_classification,omitempty"`
	ExecutionBackend        string           `json:"execution_backend,omitempty"`
	WorkerService           string           `json:"worker_service,omitempty"`
	WorkerRevision          string           `json:"worker_revision,omitempty"`
	AttemptNumber           int64            `json:"attempt_number,omitempty"`
	AttemptStatus           string           `json:"attempt_status,omitempty"`
	Attempts                []releaseAttempt `json:"attempts,omitempty"`
	Timings                 *releaseTimings  `json:"timings,omitempty"`
	Async                   bool             `json:"async,omitempty"`
	Environment             string           `json:"environment,omitempty"`
	Trigger                 *deployTrigger   `json:"trigger,omitempty"`
}

// deployStatusResponse is the GET .../deploy/{release} reply: the release and
// each project's migration rollout.
type deployStatusResponse struct {
	releaseResponse
	Migrations []deployMigration `json:"migrations,omitempty"`
}

// deployMigration is one project's migration rollout state within a release.
type deployMigration struct {
	Project string `json:"project"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// releaseProject is one project's outcome inside a release. Ready and
// TrafficPercent stay pointers: an absent observation is not false or 0%.
type releaseProject struct {
	Name                     string           `json:"name"`
	Status                   string           `json:"status"`
	URL                      string           `json:"url,omitempty"`
	Revision                 string           `json:"revision,omitempty"`
	ImageDigest              string           `json:"image_digest,omitempty"`
	CandidateRevision        string           `json:"candidate_revision,omitempty"`
	Ready                    *bool            `json:"ready,omitempty"`
	ServingRevision          string           `json:"serving_revision,omitempty"`
	TrafficPercent           *int             `json:"traffic_percent,omitempty"`
	ConfigFingerprint        string           `json:"config_fingerprint,omitempty"`
	ServingConfigFingerprint string           `json:"serving_config_fingerprint,omitempty"`
	ConfigVersion            string           `json:"config_version,omitempty"`
	Action                   string           `json:"action,omitempty"`
	Detail                   string           `json:"detail,omitempty"`
	HoldReason               string           `json:"hold_reason,omitempty"`
	Timings                  map[string]int64 `json:"timings,omitempty"`
	Provenance               string           `json:"provenance,omitempty"`
	CommitSHA                string           `json:"commit_sha,omitempty"`
	Ref                      string           `json:"ref,omitempty"`
	RepositoryURL            string           `json:"repository_url,omitempty"`
}

// releaseAttempt is one durable worker attempt of a release.
type releaseAttempt struct {
	Number         int64     `json:"number"`
	Backend        string    `json:"backend"`
	WorkerService  string    `json:"worker_service,omitempty"`
	WorkerRevision string    `json:"worker_revision,omitempty"`
	Status         string    `json:"status"`
	ErrorCode      string    `json:"error_code,omitempty"`
	StartedAt      time.Time `json:"started_at,omitzero"`
	FinishedAt     time.Time `json:"finished_at,omitzero"`
}

// releaseTimings are the release timing observations. Nil means the phase was
// not observed, never a measured zero, so each member prints null.
type releaseTimings struct {
	BuildMS              *int64 `json:"build_ms"`
	PublishMS            *int64 `json:"publish_ms"`
	ReleaseSubmitMS      *int64 `json:"release_submit_ms"`
	QueueWaitMS          *int64 `json:"queue_wait_ms"`
	WorkerPreflightMS    *int64 `json:"worker_preflight_ms"`
	ProvisioningTiersMS  *int64 `json:"provisioning_tiers_ms"`
	ReadinessMS          *int64 `json:"readiness_ms"`
	TrafficConvergenceMS *int64 `json:"traffic_convergence_ms"`
	TotalWallClockMS     *int64 `json:"total_wall_clock_ms"`
}

// deployTrigger names the channel move that opened a run nobody submitted by
// hand.
type deployTrigger struct {
	Kind           string    `json:"kind"`
	Namespace      string    `json:"namespace,omitempty"`
	Channel        string    `json:"channel,omitempty"`
	Generation     int64     `json:"generation,omitempty"`
	Cause          string    `json:"cause,omitempty"`
	MovedAt        time.Time `json:"moved_at,omitzero"`
	ReleaseSetID   string    `json:"release_set_id,omitempty"`
	SourceRevision string    `json:"source_revision,omitempty"`
	SourceTree     string    `json:"source_tree,omitempty"`
	FactRevision   int64     `json:"fact_revision,omitempty"`
	RetryOf        string    `json:"retry_of,omitempty"`
	RetryReason    string    `json:"retry_reason,omitempty"`
	RetryActor     string    `json:"retry_actor,omitempty"`
	RetryAttempt   int       `json:"retry_attempt,omitempty"`
}

// deployResponseFrom reads a deploy submit reply. A reply without a body reads
// as an empty response.
func deployResponseFrom(post *controlapiclient.DeployPostResponse) *deployResponse {
	if post == nil {
		return &deployResponse{}
	}
	return &deployResponse{
		State:                   clicore.Deref(post.State),
		ReleaseID:               clicore.Deref(post.ReleaseId),
		ManifestHash:            clicore.Deref(post.ManifestHash),
		Projects:                listFrom(post.Projects, releaseProjectFrom),
		Async:                   clicore.Deref(post.Async),
		Environment:             clicore.Deref(post.Environment),
		CloudWorkloadCount:      intFrom(post.CloudWorkloadCount),
		RequestedWorkloadCount:  intFrom(post.RequestedWorkloadCount),
		ServingNewRevisionCount: intFrom(post.ServingNewRevisionCount),
		ReusedWorkloadCount:     intFrom(post.ReusedWorkloadCount),
		HeldWorkloadCount:       intFrom(post.HeldWorkloadCount),
		TerminalClassification:  clicore.Deref(post.TerminalClassification),
		ExecutionBackend:        clicore.Deref(post.ExecutionBackend),
		WorkerService:           clicore.Deref(post.WorkerService),
		WorkerRevision:          clicore.Deref(post.WorkerRevision),
		AttemptNumber:           clicore.Deref(post.AttemptNumber),
		AttemptStatus:           clicore.Deref(post.AttemptStatus),
		Attempts:                listFrom(post.Attempts, releaseAttemptFrom),
		Timings:                 releaseTimingsFrom(post.Timings),
	}
}

// deployResponseFromStatus reads a release status as the --wait poll reads it:
// the members a submit reply shares with the status.
func deployResponseFromStatus(status *controlapiclient.DeployStatusResponse) *deployResponse {
	release := deployStatusFrom(status).releaseResponse
	return &deployResponse{
		State:                   release.State,
		ReleaseID:               release.ReleaseID,
		ManifestHash:            release.ManifestHash,
		Projects:                release.Projects,
		Async:                   release.Async,
		Environment:             release.Environment,
		CloudWorkloadCount:      release.CloudWorkloadCount,
		RequestedWorkloadCount:  release.RequestedWorkloadCount,
		ServingNewRevisionCount: release.ServingNewRevisionCount,
		ReusedWorkloadCount:     release.ReusedWorkloadCount,
		HeldWorkloadCount:       release.HeldWorkloadCount,
		TerminalClassification:  release.TerminalClassification,
		ExecutionBackend:        release.ExecutionBackend,
		WorkerService:           release.WorkerService,
		WorkerRevision:          release.WorkerRevision,
		AttemptNumber:           release.AttemptNumber,
		AttemptStatus:           release.AttemptStatus,
		Attempts:                release.Attempts,
		Timings:                 release.Timings,
	}
}

// deployStatusFrom reads a release status. A reply without a body reads as an
// empty status.
func deployStatusFrom(status *controlapiclient.DeployStatusResponse) *deployStatusResponse {
	if status == nil {
		return &deployStatusResponse{}
	}
	out := &deployStatusResponse{
		releaseResponse: releaseResponse{
			ReleaseID:               clicore.Deref(status.ReleaseId),
			State:                   clicore.Deref(status.State),
			ManifestHash:            clicore.Deref(status.ManifestHash),
			Projects:                listFrom(status.Projects, releaseProjectFrom),
			CloudWorkloadCount:      intFrom(status.CloudWorkloadCount),
			RequestedWorkloadCount:  intFrom(status.RequestedWorkloadCount),
			ServingNewRevisionCount: intFrom(status.ServingNewRevisionCount),
			ReusedWorkloadCount:     intFrom(status.ReusedWorkloadCount),
			HeldWorkloadCount:       intFrom(status.HeldWorkloadCount),
			TerminalClassification:  clicore.Deref(status.TerminalClassification),
			ExecutionBackend:        clicore.Deref(status.ExecutionBackend),
			WorkerService:           clicore.Deref(status.WorkerService),
			WorkerRevision:          clicore.Deref(status.WorkerRevision),
			AttemptNumber:           clicore.Deref(status.AttemptNumber),
			AttemptStatus:           clicore.Deref(status.AttemptStatus),
			Attempts:                listFrom(status.Attempts, releaseAttemptFrom),
			Timings:                 releaseTimingsFrom(status.Timings),
			Async:                   clicore.Deref(status.Async),
			Environment:             clicore.Deref(status.Environment),
		},
		Migrations: listFrom(status.Migrations, deployMigrationFrom),
	}
	if trigger, ok := status.Trigger.Value(); ok {
		converted := deployTriggerFrom(trigger)
		out.Trigger = &converted
	}
	return out
}

// listFrom converts a list the generated client decoded. An absent list stays
// nil and an empty one stays empty, so the output keeps null and [] apart.
func listFrom[In, Out any](in *[]In, convert func(In) Out) []Out {
	if in == nil || *in == nil {
		return nil
	}
	out := make([]Out, 0, len(*in))
	for _, item := range *in {
		out = append(out, convert(item))
	}
	return out
}

func intFrom(value *int64) int { return int(clicore.Deref(value)) }

// optionalFrom is a present, non-null optional value, or nil.
func optionalFrom[T any](value client.Optional[T]) *T {
	if concrete, ok := value.Value(); ok {
		return &concrete
	}
	return nil
}

func releaseProjectFrom(project controlapiclient.DeployReleaseProjectResponse) releaseProject {
	out := releaseProject{
		Name:                     clicore.Deref(project.Name),
		Status:                   clicore.Deref(project.Status),
		URL:                      clicore.Deref(project.Url),
		Revision:                 clicore.Deref(project.Revision),
		ImageDigest:              clicore.Deref(project.ImageDigest),
		CandidateRevision:        clicore.Deref(project.CandidateRevision),
		Ready:                    optionalFrom(project.Ready),
		ServingRevision:          clicore.Deref(project.ServingRevision),
		ConfigFingerprint:        clicore.Deref(project.ConfigFingerprint),
		ServingConfigFingerprint: clicore.Deref(project.ServingConfigFingerprint),
		ConfigVersion:            clicore.Deref(project.ConfigVersion),
		Action:                   clicore.Deref(project.Action),
		Detail:                   clicore.Deref(project.Detail),
		HoldReason:               clicore.Deref(project.HoldReason),
		Timings:                  clicore.Deref(project.Timings),
		Provenance:               clicore.Deref(project.Provenance),
		CommitSHA:                clicore.Deref(project.CommitSha),
		Ref:                      clicore.Deref(project.Ref),
		RepositoryURL:            clicore.Deref(project.RepositoryUrl),
	}
	if traffic, ok := project.TrafficPercent.Value(); ok {
		percent := int(traffic)
		out.TrafficPercent = &percent
	}
	return out
}

func releaseAttemptFrom(attempt controlapiclient.DeployReleaseAttempt) releaseAttempt {
	return releaseAttempt{
		Number:         clicore.Deref(attempt.Number),
		Backend:        clicore.Deref(attempt.Backend),
		WorkerService:  clicore.Deref(attempt.WorkerService),
		WorkerRevision: clicore.Deref(attempt.WorkerRevision),
		Status:         clicore.Deref(attempt.Status),
		ErrorCode:      clicore.Deref(attempt.ErrorCode),
		StartedAt:      clicore.Deref(attempt.StartedAt),
		FinishedAt:     clicore.Deref(attempt.FinishedAt),
	}
}

func releaseTimingsFrom(value client.Optional[controlapiclient.DeployReleaseTimings]) *releaseTimings {
	timings, ok := value.Value()
	if !ok {
		return nil
	}
	return &releaseTimings{
		BuildMS:              optionalFrom(timings.BuildMs),
		PublishMS:            optionalFrom(timings.PublishMs),
		ReleaseSubmitMS:      optionalFrom(timings.ReleaseSubmitMs),
		QueueWaitMS:          optionalFrom(timings.QueueWaitMs),
		WorkerPreflightMS:    optionalFrom(timings.WorkerPreflightMs),
		ProvisioningTiersMS:  optionalFrom(timings.ProvisioningTiersMs),
		ReadinessMS:          optionalFrom(timings.ReadinessMs),
		TrafficConvergenceMS: optionalFrom(timings.TrafficConvergenceMs),
		TotalWallClockMS:     optionalFrom(timings.TotalWallClockMs),
	}
}

func deployMigrationFrom(migration controlapiclient.DeployMigrationStatus) deployMigration {
	return deployMigration{
		Project: clicore.Deref(migration.Project),
		Status:  clicore.Deref(migration.Status),
		Detail:  clicore.Deref(migration.Detail),
	}
}

// deployTriggerFrom reads every member of a run's trigger, the retry members
// included.
func deployTriggerFrom(trigger controlapiclient.DeployTrigger) deployTrigger {
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
		RetryOf:        clicore.Deref(trigger.RetryOf),
		RetryReason:    clicore.Deref(trigger.RetryReason),
		RetryActor:     clicore.Deref(trigger.RetryActor),
		RetryAttempt:   intFrom(trigger.RetryAttempt),
	}
}
