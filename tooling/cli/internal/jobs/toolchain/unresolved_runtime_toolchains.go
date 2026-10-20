package toolchain

import (
	"context"
	"sort"

	model "go.putnami.dev/cli/model/extension"
)

// UnresolvedRuntimeToolchainLocks returns the sorted workspace-lock names of
// the required toolchains that a job of command in ext resolves unavailable on
// this host: no pin, no host integrity, or no candidate that runs the pinned
// release. The resolution is the one the job gets
// (ResolveRuntimeToolchainsForCommandsMode), probed with base, and it is made
// on a copy of ext, so ext records none. An error of a strict resolution
// returns as it is.
func UnresolvedRuntimeToolchainLocks(ctx context.Context, workspaceRoot string, ext *model.ExtensionDescription, command string, base []string) ([]string, error) {
	if ext == nil || ext.Runtime == nil {
		return nil, nil
	}
	probe := *ext
	probe.RuntimeToolchains = nil
	active := map[string]bool{command: true}
	if err := ResolveRuntimeToolchainsForCommandsMode(ctx, workspaceRoot, []*model.ExtensionDescription{&probe}, active, base, ResolveStrict); err != nil {
		return nil, err
	}
	runtimeToolchainMu.Lock()
	defer runtimeToolchainMu.Unlock()
	unresolved := map[string]bool{}
	for _, ref := range CommandRuntimeToolchainRefs(&probe, active) {
		requirement, declared := probe.Runtime.Toolchains[ref]
		if !declared || requirement.Optional || requirement.Lock == "" {
			continue
		}
		if resolution, resolved := probe.RuntimeToolchains[ref]; !resolved || !resolution.Available {
			unresolved[requirement.Lock] = true
		}
	}
	locks := make([]string, 0, len(unresolved))
	for lock := range unresolved {
		locks = append(locks, lock)
	}
	sort.Strings(locks)
	return locks, nil
}
