package runner

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Invocation providers: the purposes for which the caller let the engine ask
// the workspace's credential provider (protocols/registry credential-provider)
// instead of the host-keyed seam. A provider the request does not name is
// never consulted by the executing engine.
const (
	// InvocationProviderInstall enables the provider's read credential for
	// archive and package downloads.
	InvocationProviderInstall = "install"
	// InvocationProviderPublish enables the provider's publish credential.
	InvocationProviderPublish = "publish"
)

// InvocationProviders lists the known invocation providers in sorted order.
var InvocationProviders = []string{InvocationProviderInstall, InvocationProviderPublish}

// KnownInvocationProvider reports whether name is an invocation provider.
func KnownInvocationProvider(name string) bool {
	return slices.Contains(InvocationProviders, name)
}

// CanonicalProviders returns providers in their request form: sorted and
// unique, and nil when none remains, so a request that enables no provider
// omits the member and keeps its digest. Empty names are dropped.
func CanonicalProviders(providers []string) []string {
	canonical := SortStrings(providers)
	if len(canonical) == 0 {
		return nil
	}
	return canonical
}

// PublicationCommands are the well-known commands whose tasks write outside
// the workspace (the protocols/extension verbs with registry or cloud side
// effects). IsPublicationTask classifies by them; an engine that knows each
// task's declared traits classifies by those instead.
var PublicationCommands = []string{"deploy", "publish"}

// PublicationBlock authorizes a request to plan publication tasks.
type PublicationBlock struct {
	// Barrier lists, sorted and unique, the invocation commands every
	// publication task waits for: each planned task of a barrier command is a
	// transitive dependency of every publication task. None of them is itself
	// a publication command.
	Barrier []string `json:"barrier"`
}

// IsPublicationTask classifies a planned task by its command.
func IsPublicationTask(task PlannedTask) bool {
	return slices.Contains(PublicationCommands, task.Identity.Task.Command)
}

// strictInvocationExtensions checks the optional invocation members, whose
// canonical form omits an empty value: an explicit empty list is a second
// spelling of the same request and is refused.
func strictInvocationExtensions(invocation map[string]json.RawMessage) error {
	if providers, ok := invocation["providers"]; ok {
		if items, err := strictArray(providers, len(InvocationProviders)); err != nil || len(items) == 0 {
			return fmt.Errorf("runner: invocation.providers must be a non-empty array of at most %d providers when present", len(InvocationProviders))
		}
	}
	if publication, ok := invocation["publication"]; ok {
		block, err := strictObject(publication, []string{"barrier"}, nil)
		if err != nil {
			return fmt.Errorf("runner: invocation.publication: %w", err)
		}
		if items, err := strictArray(block["barrier"], MaxCommands); err != nil || len(items) == 0 {
			return fmt.Errorf("runner: invocation.publication.barrier must be a non-empty array")
		}
	}
	return nil
}

func validateProviders(providers []string) error {
	if providers == nil {
		return nil
	}
	if len(providers) == 0 {
		return fmt.Errorf("runner: invocation.providers is absent when empty, never an empty list")
	}
	if err := sortedUnique(providers, len(InvocationProviders)); err != nil {
		return fmt.Errorf("runner: invocation.providers: %w", err)
	}
	for _, provider := range providers {
		if !KnownInvocationProvider(provider) {
			return fmt.Errorf("runner: invocation.providers names unknown provider %q", provider)
		}
	}
	return nil
}

func validatePublicationBlock(invocation InvocationBlock) error {
	if invocation.Publication == nil {
		return nil
	}
	barrier := invocation.Publication.Barrier
	if len(barrier) == 0 {
		return fmt.Errorf("runner: invocation.publication.barrier must name at least one command")
	}
	if err := sortedUnique(barrier, MaxCommands); err != nil {
		return fmt.Errorf("runner: invocation.publication.barrier: %w", err)
	}
	for _, command := range barrier {
		if !slices.Contains(invocation.Commands, command) {
			return fmt.Errorf("runner: invocation.publication.barrier names %q, which invocation.commands does not list", command)
		}
		if slices.Contains(PublicationCommands, command) {
			return fmt.Errorf("runner: invocation.publication.barrier names the publication command %q", command)
		}
	}
	return nil
}

// ValidatePublication checks the plan against the invocation's publication
// block. Without the block, the plan holds no publication task. With it,
// every task of a barrier command is a transitive dependency of every
// publication task, and no barrier task is itself a publication task.
// isPublication decides which tasks publish; ValidateExecutionRequest uses
// IsPublicationTask, and an executing engine passes its trait classifier.
//
// It does not refuse a block over a plan in which isPublication finds no
// task: the command-name classifier cannot see a task that writes to a
// registry under another command, such as an image push under build. The
// executing engine, whose classifier sees declared effects, refuses a block
// over a plan with no publication task.
func ValidatePublication(invocation InvocationBlock, plan PlanBlock, isPublication func(PlannedTask) bool) error {
	var publications []string
	byKey := make(map[string]PlannedTask, len(plan.Tasks))
	for _, task := range plan.Tasks {
		byKey[task.Identity.Key] = task
		if isPublication(task) {
			publications = append(publications, task.Identity.Key)
		}
	}
	if invocation.Publication == nil {
		if len(publications) > 0 {
			return fmt.Errorf("runner: plan task %s publishes, but the request carries no invocation.publication", publications[0])
		}
		return nil
	}
	var barrier []string
	for _, task := range plan.Tasks {
		if !slices.Contains(invocation.Publication.Barrier, task.Identity.Task.Command) {
			continue
		}
		if isPublication(task) {
			return fmt.Errorf("runner: barrier task %s is itself a publication task", task.Identity.Key)
		}
		barrier = append(barrier, task.Identity.Key)
	}
	for _, key := range publications {
		ancestors := transitiveDependencies(byKey, key)
		for _, required := range barrier {
			if !ancestors[required] {
				return fmt.Errorf("runner: publication task %s does not wait for barrier task %s", key, required)
			}
		}
	}
	return nil
}

// transitiveDependencies returns every task key reachable from key through
// dependsOn edges. ValidatePlan has already checked that each edge names a
// planned task.
func transitiveDependencies(byKey map[string]PlannedTask, key string) map[string]bool {
	seen := make(map[string]bool)
	stack := append([]string(nil), byKey[key].DependsOn...)
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[next] {
			continue
		}
		seen[next] = true
		stack = append(stack, byKey[next].DependsOn...)
	}
	return seen
}
