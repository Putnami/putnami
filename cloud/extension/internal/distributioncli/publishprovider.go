package distributioncli

import (
	"fmt"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	regproto "go.putnami.dev/protocol/registry"
)

// PublishProvider keeps the old direct publish-provider command available for
// compatibility callers. It is intentionally not declared in the Cloud extension
// manifest and must not be used for planner capability detection; contract-v3
// task inputs own that decision. The credential seam publishers actually shell
// is `cloud registry-token --host <host>` (regproto.SeamSubcommand).
//
// A direct invocation prints one self-documenting line naming that seam.
func PublishProvider(_ map[string]any, _ map[string]string, ioctx clicore.IO) error {
	ioctx.Stdout(fmt.Sprintf(
		"@putnami/cloud publish provider (registry-token/v%d): resolve a registry bearer with `putnami %s %s --%s <host>`",
		regproto.ProtocolVersion, regproto.SeamParentCommand, regproto.SeamSubcommand, regproto.SeamHostFlag,
	))
	return nil
}
