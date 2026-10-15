package jobs

import (
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// RequirePlannedRuntimeToolchains resolves, before any planned job runs or is
// served from the cache, each toolchain a planned job requires that runtime
// synchronization left unresolved (toolchain.ResolveDeferred). A job of another
// command is checked before a job of toolchain.ToolchainProvisioningCommand, so
// an alias both need resolves strictly. The error is the one strict resolution
// returns.
func RequirePlannedRuntimeToolchains(ws *workspace.Workspace, planned []*ScheduledJob) error {
	for _, provisioning := range []bool{false, true} {
		for _, job := range planned {
			if job == nil || toolchain.IsToolchainProvisioningCommand(job.CommandName()) != provisioning {
				continue
			}
			if err := toolchain.EnsureJobRuntimeToolchains(ws, job); err != nil {
				return runtimeFailure(extensionproto.FailureRuntimePrepareFailed,
					"resolve extension runtime toolchains: %v", err)
			}
		}
	}
	return nil
}
