package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/mcp"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The collaboration contracts on the command line.
//
// `putnami <contract> <operation> --input <json>` is the CLI entry path of the
// operation the MCP server exposes as `<contract>.<operation>`: the same
// request document, the same routing (mcp.CallProviderOperation), the same
// envelope on stdout. A contract word is a command group exactly when the
// workspace document declares options.collaboration, so a workspace that binds
// nothing reserves no command name.

// maxCollaborationInputBytes bounds the request document read from --input,
// --input-file or stdin.
const maxCollaborationInputBytes = collab.MaxDocumentBytes

// addCollaborationGroups adds the contract words to the structured command
// groups when the workspace declares options.collaboration.
func addCollaborationGroups(groups map[string]bool, wsRoot string) map[string]bool {
	if !mcp.CollaborationDeclared(wsRoot) {
		return groups
	}
	if groups == nil {
		groups = map[string]bool{}
	}
	for _, contract := range collab.ContractNames {
		groups[contract] = true
	}
	return groups
}

// commandGroupsFor is the set of structured command groups of one invocation:
// the extensions' groups, plus the contract words when the workspace declares
// options.collaboration. The internal release-set provider mode routes its
// own group only.
func commandGroupsFor(extensions []*extension.ExtensionDescription, wsRoot string, providerMode bool) map[string]bool {
	groups := extension.CommandGroupNames(extensions)
	if providerMode {
		return groups
	}
	return addCollaborationGroups(groups, wsRoot)
}

// printCommandGroupHelp prints the help of a structured command group: a
// collaboration contract's binding and operations, or an extension group's
// subcommands.
func printCommandGroupHelp(ctx context.Context, w io.Writer, wsRoot string, cfg *wsproto.Config, extensions []*extension.ExtensionDescription, group, sub, outputMode string) {
	if isCollaborationCommand(wsRoot, group) {
		providers, unavailable := collaborationProviders(ctx, wsRoot, cfg, extensions)
		printCollaborationContractHelp(w, wsRoot, group, providers, unavailable)
		return
	}
	printExtensionGroupOrSubcommandHelp(extensions, group, sub, outputMode)
}

// isCollaborationCommand reports whether group is a contract word this
// workspace routes.
func isCollaborationCommand(wsRoot, group string) bool {
	return collab.IsContract(group) && mcp.CollaborationDeclared(wsRoot)
}

// collaborationCommandOwners names the extensions that declare a command or a
// command group under a contract word.
func collaborationCommandOwners(extensions []*extension.ExtensionDescription, word string) []string {
	var owners []string
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		if _, group := ext.CommandGroups[word]; group {
			owners = append(owners, ext.Name)
			continue
		}
		if _, command := ext.Commands[word]; command {
			owners = append(owners, ext.Name)
		}
	}
	sort.Strings(owners)
	return owners
}

// runCollaborationCommand answers `putnami <contract> [<operation>]`.
func (a *App) runCollaborationCommand(
	ctx context.Context,
	parsed *ParsedArgs,
	cfg *wsproto.Config,
	wsRoot string,
	extensions []*extension.ExtensionDescription,
	stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	contract := parsed.Commands[0]
	if owners := collaborationCommandOwners(extensions, contract); len(owners) > 0 {
		iox.Fprintf(stderr, "putnami: %s is a collaboration contract of this workspace (options.collaboration) and a command of %s\n",
			contract, strings.Join(owners, ", "))
		iox.Fprintf(stderr, "  Rename the extension command, or remove options.collaboration from putnami.workspace.json.\n")
		return ExitUsage
	}
	providers, unavailable := collaborationProviders(ctx, wsRoot, cfg, extensions)
	if parsed.Subcommand == "" || parsed.Subcommand == "help" || parsed.Global.Help {
		printCollaborationContractHelp(stdout, wsRoot, contract, providers, unavailable)
		return ExitSuccess
	}
	if selection := collaborationSelectionFlags(parsed.Global); selection != "" {
		iox.Fprintf(stderr, "putnami: %s %s takes no %s flag; pass projects, impacted and baseline inside --input\n",
			contract, parsed.Subcommand, selection)
		return ExitUsage
	}
	arguments, err := collaborationInput(parsed.RawJobArgs, stdin)
	if err != nil {
		iox.Fprintf(stderr, "putnami: %s %s: %v\n", contract, parsed.Subcommand, err)
		return ExitUsage
	}
	envelope := mcp.CallProviderOperation(ctx, mcp.ProviderEnvironment{
		WorkspaceRoot: wsRoot,
		Extensions:    providers,
		Unavailable:   unavailable,
		AttachView:    cliWorkspaceView(wsRoot),
	}, contract, parsed.Subcommand, arguments)

	text, err := mcp.EnvelopeText(envelope, !output.StructuredOutput(parsed.Global.Output))
	if err != nil {
		iox.Fprintf(stderr, "putnami: %v\n", err)
		return ExitError
	}
	iox.Fprintln(stdout, text)
	if envelope.Outcome != collab.OutcomeOK {
		return ExitError
	}
	return ExitSuccess
}

// collaborationProviders is the provider selection of the CLI entry path. It
// is the selection the MCP server makes when it starts
// (preparedExtensionSelection over lifecycle.PrepareReadOnly): a registry
// extension serves only from the release the workspace lock pins, and one
// whose exact release was not prepared is unavailable instead of being loaded
// from a stale stable link. Extensions the workspace disables are unavailable
// too, so both entry paths resolve a binding against the same set.
func collaborationProviders(
	ctx context.Context, wsRoot string, cfg *wsproto.Config, discovered []*extension.ExtensionDescription,
) ([]*extension.ExtensionDescription, map[string]bool) {
	roots, unprepared := preparedExtensionSelection(lifecycle.PrepareReadOnly(ctx, wsRoot, cfg))
	unavailable := map[string]bool{}
	if cfg != nil && cfg.Disable != nil {
		for _, name := range cfg.Disable.Extensions {
			unavailable[name] = true
		}
	}
	for _, name := range unprepared {
		unavailable[name] = true
	}
	return extension.SelectPreparedExtensions(discovered, roots), unavailable
}

// collaborationSelectionFlags names a project-selection flag the invocation
// carries, which a collaboration command refuses: the selection a contract
// operation takes is part of its request document, identical on both entry
// paths.
func collaborationSelectionFlags(global GlobalFlags) string {
	switch {
	case global.Projects != "":
		return "--projects"
	case global.All:
		return "--all"
	case global.Impacted:
		return "--impacted"
	case global.Baseline != "":
		return "--baseline"
	case global.FilterTag != "":
		return "--tag"
	case global.ExcludeTag != "":
		return "--exclude-tag"
	case global.Exclude != "":
		return "--exclude"
	}
	return ""
}

// collaborationInput reads the request document from --input <json> or
// --input-file <path|->. No flag is the empty request.
func collaborationInput(args []string, stdin io.Reader) (json.RawMessage, error) {
	var inline, file string
	var inlineSet, fileSet bool
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--input", "--input-file":
		default:
			return nil, fmt.Errorf("unexpected argument %q (usage: --input <json> or --input-file <path|->)", arg)
		}
		if !hasValue {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s needs a value", name)
			}
			i++
			value = args[i]
		}
		if name == "--input" {
			if inlineSet {
				return nil, errors.New("--input is given twice")
			}
			inline, inlineSet = value, true
		} else {
			if fileSet {
				return nil, errors.New("--input-file is given twice")
			}
			file, fileSet = value, true
		}
	}
	switch {
	case inlineSet && fileSet:
		return nil, errors.New("--input and --input-file are exclusive")
	case inlineSet:
		return json.RawMessage(inline), nil
	case fileSet:
		reader := stdin
		if file != "-" {
			opened, err := os.Open(file)
			if err != nil {
				return nil, fmt.Errorf("read --input-file: %w", err)
			}
			defer opened.Close()
			reader = opened
		}
		data, err := io.ReadAll(io.LimitReader(reader, maxCollaborationInputBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read --input-file: %w", err)
		}
		if len(data) > maxCollaborationInputBytes {
			return nil, fmt.Errorf("the request document exceeds %d bytes", maxCollaborationInputBytes)
		}
		return json.RawMessage(data), nil
	}
	return nil, nil
}

// cliWorkspaceView attaches the workspace view to a provider tool that
// declares workspaceSelection, through the resolver every other CLI surface
// uses.
func cliWorkspaceView(wsRoot string) func(context.Context, *extproto.ToolCallRequest, json.RawMessage) error {
	return func(_ context.Context, request *extproto.ToolCallRequest, args json.RawMessage) error {
		ws, err := workspace.Load(wsRoot)
		if err != nil {
			return fmt.Errorf("load workspace: %w", err)
		}
		return mcp.AttachWorkspaceView(ws, request, args, func(ws *workspace.Workspace, selection mcp.ProjectSelection) (*extproto.ToolSelection, error) {
			resolved, err := shared.ResolveProjectSelection(ws, catalogProjectSelection(selection))
			if err != nil {
				return nil, err
			}
			return toolSelectionWire(resolved), nil
		})
	}
}

// printCollaborationContractHelp resolves one contract against the entry
// path's provider selection and describes it.
func printCollaborationContractHelp(w io.Writer, wsRoot, contract string, providers []*extension.ExtensionDescription, unavailable map[string]bool) {
	printCollaborationHelp(w, contract, mcp.CollaborationCapabilities(wsRoot, contract, providers, unavailable))
}

// printCollaborationHelp describes one contract: its binding, its provider and
// every operation with its support.
func printCollaborationHelp(w io.Writer, contract string, capabilities collab.Capabilities) {
	iox.Fprintf(w, "Usage: putnami %s <operation> [--input <json> | --input-file <path|->]\n\n", contract)
	iox.Fprintf(w, "The %s collaboration contract, routed to the provider options.collaboration.%s binds in putnami.workspace.json.\n",
		contract, contract)
	iox.Fprintf(w, "The same operations are MCP tools named %s.<operation>. Every call prints one envelope.\n\n", contract)
	switch {
	case capabilities.Provider != nil && capabilities.Binding != nil:
		iox.Fprintf(w, "Binding: %s, version %d (%s)\n", capabilities.Provider.Name, capabilities.Binding.Version, capabilities.Status)
	case capabilities.Binding != nil:
		iox.Fprintf(w, "Binding: %s, version %d (%s)\n", capabilities.Binding.Provider, capabilities.Binding.Version, capabilities.Status)
	default:
		iox.Fprintf(w, "Binding: none (%s)\n", capabilities.Status)
	}
	for _, issue := range capabilities.Issues {
		iox.Fprintf(w, "  %s: %s\n", issue.Reason, issue.Message)
	}
	iox.Fprintln(w, "\nOperations:")
	iox.Fprintf(w, "  %-12s %-9s %s\n", collab.OperationCapabilities, "read", "the binding and every operation's support")
	for _, op := range capabilities.Operations {
		support := "unsupported"
		if op.Supported {
			support = "supported"
		}
		kind := "optional"
		if op.Required {
			kind = "required"
		}
		iox.Fprintf(w, "  %-12s %-9s %s, %s\n", op.Name, op.Access, kind, support)
	}
}
