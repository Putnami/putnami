package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/extensions"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
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
//
// A structured outputFormat (--output=json|jsonl) writes one "extensions
// install" result, one record per extension outcome, once the agent content
// step succeeds too, so a failure leaves only the failure result on stdout.
func ExtensionsInstallNamed(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, outputFormat string, env LifecycleEnv) error {
	return extensions.WithActionRecords(outputFormat, "extensions install", func(records *extensions.ActionRecords) error {
		if err := extensions.ExtensionsInstallWithOptions(ctx, wsRoot, cfg, args, extensions.InstallOptions{
			OutputFormat: outputFormat, Out: os.Stdout, Records: records,
		}); err != nil {
			return err
		}
		return installNamedAgentContent(ctx, wsRoot, cfg, args, env)
	})
}

// installNamedAgentContent opts the extension named in args into its agent
// content and materializes that content (ExtensionsInstallNamed).
func installNamedAgentContent(ctx context.Context, wsRoot string, cfg *wsproto.Config, args []string, env LifecycleEnv) error {
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

// ExtensionsInstallChannel reads --channel from the arguments of `putnami
// extensions install` and returns them without the flag, the named registry
// extension pinned to that channel: `--channel canary @acme/x` installs
// `@acme/x@canary`, as init installs its extensions on the channel it chose.
// The channel is validated as init validates it, and stable reads latest.
//
// Only the flag chooses: PUTNAMI_CHANNEL and the install record keep the
// meaning they have for init, which resolves a whole workspace, and do not
// change what installing one extension resolves. A --channel with no registry
// extension named, or beside a name that carries its own version or channel,
// is a usage error rather than an ignored flag.
func ExtensionsInstallChannel(args []string) ([]string, error) {
	var channel string
	set := false
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if value, ok := strings.CutPrefix(args[i], initChannelFlag+"="); ok {
			channel, set = value, true
			continue
		}
		if args[i] != initChannelFlag {
			rest = append(rest, args[i])
			continue
		}
		set = true
		if i+1 < len(args) {
			channel = args[i+1]
			i++
		}
	}
	if !set {
		return rest, nil
	}
	name, err := channelName(channel, initChannelFlag)
	if err != nil {
		return nil, err
	}
	at := installNameIndex(rest)
	if at < 0 || extensionRefIsLocal(rest[at]) {
		return nil, cmderr.Usagef("%s needs the registry extension to install, for example "+
			"`putnami extensions install @putnami/observe %s canary`", initChannelFlag, initChannelFlag)
	}
	target := rest[at]
	if strings.Contains(strings.TrimPrefix(target, "@"), "@") {
		return nil, cmderr.Usagef("%s names its own version or channel: drop %s, or the @ suffix", target, initChannelFlag)
	}
	if name != latestChannel {
		rest[at] = shared.ChannelArtifact(target, name)
	}
	return rest, nil
}

// installNameIndex is the index of the extension the arguments of `putnami
// extensions install` name: the first argument that is no flag and no value
// of --platform or --dest, or -1 when they name none.
func installNameIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--platform" || args[i] == "--dest":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return i
		}
	}
	return -1
}

// extensionRefIsLocal reports whether an install argument names a local
// extension directory, which no registry channel serves.
func extensionRefIsLocal(ref string) bool {
	return strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") ||
		filepath.IsAbs(filepath.FromSlash(ref))
}
