package lifecycle

import (
	"bytes"
	"context"
	"fmt"

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
// Only the extension on the command line is opted in, and only its content is
// materialized: another opted-in extension whose content is not installed on
// this machine neither fails the command nor changes. An extension installed
// because another one depends on it is not opted in, and an extension without
// agent content leaves putnami.workspace.json as it was. A hosted run writes
// no workspace file, so it installs and stops there.
//
// The workspace-file write and each contribution whose files changed are
// reported to env.OnAction, as `putnami install` reports its agent phase; with
// no OnAction the same lines go to env.Out. When the content step fails after
// the opt-in was written, both declarations and the lock pin stay and the error
// names `putnami install` as the next step.
func ExtensionsInstallNamed(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, env LifecycleEnv) error {
	if err := extensions.ExtensionsInstall(ctx, wsRoot, cfg, args, outputFormat); err != nil {
		return err
	}
	name := extensions.InstallTargetName(args)
	if name == "" || !installWritesWorkspace() {
		return nil
	}
	outcome, err := agentctx.OptIntoExtensionAgentContent(wsRoot, cfg, name)
	if err != nil {
		return fmt.Errorf("opt into the agent content of %s: %w", name, err)
	}
	if !outcome.ShipsContent {
		return nil
	}
	out := env.out()
	record := env.OnAction
	if record == nil {
		record = func(action LifecycleAction) { iox.Fprintf(out, "  ✓ %s\n", action.Description) }
	}
	if outcome.OptedIn || outcome.Declared {
		record(LifecycleAction{
			Kind: "workspace-config", Name: wsproto.WorkspaceConfigFilename,
			Description: fmt.Sprintf("Opted into the agent content of %s in %s", name, wsproto.WorkspaceConfigFilename),
		})
	}
	var details bytes.Buffer
	actions, err := agentctx.InstallExtensionAgentContentWithResult(ctx, wsRoot, wsproto.Load(wsRoot), name, &details)
	if err != nil {
		_, _ = out.Write(details.Bytes())
		return protocolcli.WithNext(
			fmt.Errorf("install the agent content of %s: %w", name, err),
			"putnami install",
		)
	}
	for _, action := range actions {
		record(agentWorkflowLifecycleAction(action))
	}
	return nil
}
