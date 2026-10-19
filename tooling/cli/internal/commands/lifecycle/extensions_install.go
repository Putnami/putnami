package lifecycle

import (
	"context"
	"fmt"
	"io"
	"os"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// ExtensionsInstallNamed is `putnami extensions install`. Besides installing,
// it treats naming an extension as the request for that extension's agent
// content: when the named extension ships content, the workspace declares it
// in `extensions`, opts into it in `agentArtifacts`, and the content is
// materialized from the release the install pinned. A second run, or a later
// `putnami install`, changes nothing.
//
// Only the extension on the command line is opted in. An extension installed
// because another one depends on it is not, and an extension without agent
// content leaves putnami.workspace.json as it was. A hosted run writes no
// workspace file, so it installs and stops there.
func ExtensionsInstallNamed(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string) error {
	if err := extensions.ExtensionsInstall(ctx, wsRoot, cfg, args, outputFormat); err != nil {
		return err
	}
	name := extensions.InstallTargetName(args)
	if name == "" || !installWritesWorkspace() {
		return nil
	}
	out := io.Writer(os.Stdout)
	if outputFormat == "jsonl" {
		out = io.Discard
	}
	outcome, err := agentctx.OptIntoExtensionAgentContent(wsRoot, cfg, name)
	if err != nil {
		return fmt.Errorf("opt into the agent content of %s: %w", name, err)
	}
	if !outcome.ShipsContent {
		return nil
	}
	if outcome.OptedIn {
		iox.Fprintf(out, "  Opted into the agent content of %s in %s\n", name, wsproto.WorkspaceConfigFilename)
	}
	if err := agentctx.InstallAgentWorkflows(ctx, wsRoot, wsproto.Load(wsRoot), out); err != nil {
		return protocolcli.WithNext(
			fmt.Errorf("install the agent content of %s: %w", name, err),
			"putnami install",
		)
	}
	return nil
}
