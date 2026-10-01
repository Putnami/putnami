package engine

import (
	"fmt"

	"go.putnami.dev/cli/model/extension"
	runner "go.putnami.dev/protocol/runner"
)

// resolvePlacement answers where this run executes. Local placement, or a
// remote request with no extension declaring the reserved provider command,
// takes the ordinary local lifecycle exactly once and returns nil. A remote
// request that finds exactly one provider returns it for the seam between the
// final plan and execution; two providers are an ambiguous configuration and
// an error, never a guess. Nothing here initializes a provider: absence is
// decided before any runtime preparation, credential resolution or capture.
func resolvePlacement(requested string, extensions []*extension.ExtensionDescription) (*extension.ResolvedProvider, error) {
	switch requested {
	case "", "local":
		return nil, nil
	case "remote":
		return extension.ResolveReservedProvider(extensions, runner.ProviderCommandName)
	default:
		return nil, fmt.Errorf("invalid execution placement %q: expected local or remote", requested)
	}
}
