// Typed task identity.
//
// The identity every consumer joins on — diagnostics, events, session records,
// machine documents, cache-key task naming — is ONE typed value computed from
// the plan node, in ONE place. Before this, the v2 machine surfaces
// derived it in internal/machine and everything else spelled ad hoc strings
// (Project.ID + ":" + PlanName); a second derivation is how two surfaces start
// disagreeing about who a task is.
//
// The representation IS protocols/cli's TaskIdentity — the wire's own type —
// so the plan cannot drift from what consumers read (the B0a schema and the
// plan node share one struct). The string Key() stays the hot-path spelling
// and is pinned byte-identical to TypedIdentity().Key by
// TestTypedIdentity_KeyIsTheDerivedView; it is a DERIVED VIEW, not an
// independent string.
//
// Identities are stamped once at the end of Plan (AttachIdentities), after the
// fixpoint and dependency resolution, and are immutable from then on. A job
// assembled outside Plan (tests, the reducer's key-only fallback) derives the
// same value lazily through TypedIdentity — the function is pure in the node's
// fields, so stamped and derived values cannot differ.
package jobs

import (
	"strings"

	"go.putnami.dev/cli/model/extension"
	protocolcli "go.putnami.dev/protocol/cli"
)

// unknownIdentityMember is what a required identity member carries when the
// node genuinely does not know it. Every member is required and non-empty by
// contract (protocols/cli result_v2_rules.go), so the choice is between a
// stated placeholder and an invalid document.
const unknownIdentityMember = "unknown"

// TypedIdentity returns the job's immutable typed identity: the stamped value
// when Plan attached one, the pure derivation otherwise.
func (s *ScheduledJob) TypedIdentity() protocolcli.TaskIdentity {
	if s == nil {
		return TaskIdentityOfKey("")
	}
	if s.Identity != nil {
		return *s.Identity
	}
	return deriveIdentity(s)
}

// AttachIdentities stamps every planned job's typed identity exactly once, at
// the end of Plan. After this point TypedIdentity is a field read.
func AttachIdentities(planned []*ScheduledJob) {
	for _, job := range planned {
		identity := deriveIdentity(job)
		job.Identity = &identity
	}
}

// deriveIdentity is the single projection of a plan node onto the typed
// identity. Key is always the DERIVED value (project.id + ":" + task.name) —
// exactly ScheduledJob.Key(), byte for byte, which is what keeps a v2
// consumer's join string the v1 plan key.
func deriveIdentity(job *ScheduledJob) protocolcli.TaskIdentity {
	id := protocolcli.TaskIdentity{
		Scope: protocolcli.TaskScopeProject,
		Task: protocolcli.TaskRef{
			Name:    orUnknownIdentity(job.PlanName()),
			Command: orUnknownIdentity(job.CommandName()),
			Step:    job.StepID(),
			Kind:    orUnknownIdentity(identityTaskKind(job)),
		},
		Provider: protocolcli.ProviderIdentity{Extension: unknownIdentityMember},
	}
	if job.Project != nil {
		id.Project = protocolcli.ProjectIdentity{
			ID:   orUnknownIdentity(job.Project.ID),
			Name: orUnknownIdentity(job.Project.Name),
		}
	} else {
		id.Project = protocolcli.ProjectIdentity{ID: unknownIdentityMember, Name: unknownIdentityMember}
	}
	if job.Extension != nil && job.Extension.Name != "" {
		id.Provider.Extension = job.Extension.Name
		id.Provider.Version = job.Extension.Version
	}
	// SelectedProjects is set exactly for a job that runs once for the whole
	// workspace (see ScheduledJob), so the scope is read off the plan rather
	// than off a synthetic project name a reader has to recognize.
	if len(job.SelectedProjects) > 0 {
		id.Scope = protocolcli.TaskScopeWorkspace
	}
	id.Key = id.DerivedKey()
	return id
}

// TaskIdentityOfKey is the fallback for a result the plan does not name: the
// run map's key is all the identity that exists. Keys are built as
// projectID + ":" + planName, so splitting at the first colon recovers both
// halves; a key with no colon leaves the task name carrying the whole string.
func TaskIdentityOfKey(key string) protocolcli.TaskIdentity {
	projectID, taskName, found := cutIdentityKey(key)
	if !found {
		projectID, taskName = unknownIdentityMember, key
	}
	id := protocolcli.TaskIdentity{
		Scope:   protocolcli.TaskScopeProject,
		Project: protocolcli.ProjectIdentity{ID: orUnknownIdentity(projectID), Name: orUnknownIdentity(projectID)},
		Task: protocolcli.TaskRef{
			Name:    orUnknownIdentity(taskName),
			Command: orUnknownIdentity(identityCommandOf(taskName)),
			Kind:    orUnknownIdentity(taskName),
		},
		Provider: protocolcli.ProviderIdentity{Extension: unknownIdentityMember},
	}
	id.Key = id.DerivedKey()
	return id
}

// identityTaskKind is the step's declared task when the job is an expanded
// pipeline step, and the job's own name otherwise — the same rule
// applyTaskIdentity uses for TaskResult.TaskKind.
func identityTaskKind(job *ScheduledJob) string {
	if job.Step != nil && job.Step.Task != "" {
		return job.Step.Task
	}
	if job.JobDef != nil {
		return job.JobDef.Name
	}
	return ""
}

// JobCommandAndStep splits a job name into its command and pipeline step id.
// "build~generate" → ("build", "generate"); "build" → ("build", "").
//
// It lives beside the identity derivation because that is what it is: the plan
// name is one half of a node's identity, and CommandName()/StepID() are the
// typed views of it. Its previous home (infra_closure.go) went with the
// requirements-closure helpers an earlier cleanup deleted.
func JobCommandAndStep(name string) (command, step string) {
	if i := strings.Index(name, extension.StepSeparator); i >= 0 {
		return name[:i], name[i+len(extension.StepSeparator):]
	}
	return name, ""
}

// identityCommandOf recovers the root command from a plan name of the form
// "command~step". Used only on the key fallback, where no plan node exists to
// answer the question properly.
func identityCommandOf(planName string) string {
	for i := 0; i < len(planName); i++ {
		if planName[i] == '~' {
			return planName[:i]
		}
	}
	return planName
}

func cutIdentityKey(key string) (string, string, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i], key[i+1:], true
		}
	}
	return key, "", false
}

func orUnknownIdentity(value string) string {
	if value == "" {
		return unknownIdentityMember
	}
	return value
}
