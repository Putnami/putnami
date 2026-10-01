package runnerprovider

import (
	"context"
	"fmt"
	"strings"

	"go.putnami.dev/cli/model/extension"
	internalextension "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// LaunchSpecFor resolves how to start the extension command that serves the
// reserved runner provider. Resolution is by the reserved command only, the
// same rule the cache provider follows: no product name, no version floor.
//
// The provider command gets the runtime preparation a task gets (see
// jobs.PrepareProviderLaunch). A runtime that cannot be prepared is an error,
// so a remote run stops before it submits anything.
func LaunchSpecFor(ctx context.Context, wsRoot string, extensions []*extension.ExtensionDescription, provider *extension.ResolvedProvider) (LaunchSpec, error) {
	if provider == nil {
		return LaunchSpec{}, fmt.Errorf("no runner provider resolved")
	}
	ext := extension.FindExtensionByName(extensions, provider.ExtensionName)
	if ext == nil {
		return LaunchSpec{}, fmt.Errorf("runner provider extension %q was not loaded", provider.ExtensionName)
	}
	job := ext.Jobs[provider.Command]
	if job == nil {
		return LaunchSpec{}, fmt.Errorf("runner provider command %q was not resolved for %s", provider.Command, provider.ExtensionName)
	}
	if strings.TrimSpace(job.Command) == "" {
		return LaunchSpec{}, fmt.Errorf("runner provider command %q has no executable", provider.Command)
	}
	launch, err := jobs.PrepareProviderLaunch(ctx, wsRoot, ext, provider.Command)
	if err != nil {
		return LaunchSpec{}, fmt.Errorf("prepare runner provider %s: %w", provider.ExtensionName, err)
	}
	// The provider runs the extension's code. An extension installed from the
	// artifact store is registry code; any other one is repository code.
	if !internalextension.InArtifactStore(wsRoot, ext) {
		runcredential.MarkRepositoryCodeStarted("runner provider " + ext.Name)
	}
	return LaunchSpec(launch), nil
}
