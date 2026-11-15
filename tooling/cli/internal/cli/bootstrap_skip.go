package cli

import (
	"go.putnami.dev/tooling/cli/internal/extension"
)

// readsNoWorkspaceState reports whether the invocation dispatches a
// command-group subcommand that declares workspaceState none: it reads no
// installed workspace state, so the first-use bootstrap (the implicit install
// and the generated-client restore) does not run before it. It resolves the
// subcommand the way dispatch does: a group default stands for a missing
// subcommand word. A collaboration word, a help request, a subcommand two
// extensions claim and every other command answer false, so the bootstrap
// runs as before.
func readsNoWorkspaceState(parsed *ParsedArgs, wsRoot string, extensions []*extension.ExtensionDescription, extensionGroups map[string]bool) bool {
	if parsed == nil || len(parsed.Commands) != 1 || !extensionGroups[parsed.Commands[0]] {
		return false
	}
	group := parsed.Commands[0]
	if isCollaborationCommand(wsRoot, group) {
		return false
	}
	sub, _ := extension.ResolveGroupInvocation(extensions, group, parsed.Subcommand)
	if sub == "" || len(extension.CommandSubcommandOwners(extensions, group, sub)) > 1 {
		return false
	}
	resolved, _ := extension.ResolveSubcommand(extensions, group, sub)
	return resolved != nil && resolved.Subdef.ReadsNoWorkspaceState()
}
