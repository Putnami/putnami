package jobs

import (
	"context"
	"fmt"

	runner "go.putnami.dev/protocol/runner"
)

// PlanProcessCapabilityGates materializes the captured runner-owned AFTER
// contract as functional dependencies. Authorization remains a separate check
// of the final DAG, and the scheduler still requires successful gate results.
// Repository-owned environment and serialization edges supply no authority.
//
// A release-set member publication is not gated here (ADR 0023): it flows from
// its own package chain, and the release-set stamp — the finalizer, or the
// same-session barrier — is what waits for every gate command and every
// publication. The barrier itself, a deploy, and any protected job outside a
// release set keep the AFTER gate.
func PlanProcessCapabilityGates(ctx context.Context, planned []*ScheduledJob) error {
	capabilities := processCapabilitiesFromContext(ctx)
	if capabilities == nil || capabilities.cloudToken == "" || capabilities.after == "" {
		return nil
	}
	commands, err := parseCapabilityAfter(capabilities.after)
	if err != nil {
		return err
	}
	leaves := capabilityLeavesByCommand(planned)
	var required []string
	for _, command := range commands {
		required = append(required, leaves[command]...)
	}
	required = dedupeSorted(required)
	// Validate the candidate before changing the caller's graph. A gate that
	// depends on a protected write must remain an error, never a bypass.
	candidate := make([]*ScheduledJob, len(planned))
	for index, job := range planned {
		copy := *job
		copy.DependsOn = append([]string(nil), job.DependsOn...)
		if hasCloudCapabilitySideEffects(job) && !isFinalizerJob(job) && !isReleaseSetMemberPublication(job) {
			copy.DependsOn = dedupeSorted(append(copy.DependsOn, required...))
		}
		candidate[index] = &copy
	}
	if err := validatePlanDAG(candidate); err != nil {
		return fmt.Errorf("runner publication gates are not schedulable: %w", err)
	}
	for index, job := range planned {
		job.DependsOn = candidate[index].DependsOn
	}
	return nil
}

// HasExternalEffects reports whether job writes outside the workspace: its
// command's traits declare registry or cloud side effects, or its manifest
// task declares a registry or cloud effect. It is the classification the
// capability gates above apply.
func HasExternalEffects(job *ScheduledJob) bool {
	return hasCloudCapabilitySideEffects(job)
}

// ValidatePortablePublication is the executing side's publication check of a
// portable request against its re-planned graph. It classifies a task as a
// publication by HasExternalEffects, not by its command name, so a task that
// writes to a registry or a cloud runs only under the request's publication
// block, after every task of its barrier commands. It also refuses a block
// over a plan with no such task, the direction the protocol's command-name
// classifier cannot check: the block is present exactly when the plan
// publishes.
func ValidatePortablePublication(request runner.ExecutionRequest, planned []*ScheduledJob) error {
	publishes := make(map[string]bool, len(planned))
	for _, job := range planned {
		if HasExternalEffects(job) {
			publishes[job.TypedIdentity().Key] = true
		}
	}
	if request.Invocation.Publication != nil && len(publishes) == 0 {
		return fmt.Errorf("runner: invocation.publication is present, but no planned task declares registry or cloud effects")
	}
	return runner.ValidatePublication(request.Invocation, request.Plan, func(task runner.PlannedTask) bool {
		return publishes[task.Identity.Key]
	})
}

// PrepareProcessCapabilityAuthorization keeps graph preparation and independent
// authorization in the shared jobs layer before the engine starts execution.
func PrepareProcessCapabilityAuthorization(ctx context.Context, planned []*ScheduledJob, executesJobs bool) (*ProcessCapabilityAuthorization, error) {
	if err := PlanProcessCapabilityGates(ctx, planned); err != nil {
		return nil, err
	}
	return AuthorizeProcessCapabilities(ctx, planned, executesJobs)
}
