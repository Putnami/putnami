package agentctx

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"go.putnami.dev/tooling/cli/internal/iox"
)

// Putnami no longer serves an MCP server, so no workspace needs the .mcp.json
// entry that earlier releases wrote for it. An explicit `putnami install` and
// `putnami upgrade` remove that entry when it is exactly the one Putnami wrote,
// and leave every other byte of the file to its owner.
//
// This removal is deleted in the minor release that follows the one that ships
// it: a workspace that has run install or upgrade since has nothing left to
// remove.

// mcpConfigPath is the workspace-relative MCP registration file that Claude
// Code discovers at the project root.
const mcpConfigPath = ".mcp.json"

// MCPRemoval says what RemoveMCPRegistration did to .mcp.json.
type MCPRemoval int

const (
	// MCPRemovalNone: nothing was removed. The file is absent, holds no
	// putnami entry, or holds one that was kept (see RemoveMCPRegistration).
	MCPRemovalNone MCPRemoval = iota
	// MCPRemovalEntry: the putnami entry was removed and the rest of the file
	// kept.
	MCPRemovalEntry
	// MCPRemovalFile: the file held nothing but the putnami entry and was
	// deleted.
	MCPRemovalFile
)

// Description is the install summary line of a removal, empty for none.
func (r MCPRemoval) Description() string {
	switch r {
	case MCPRemovalEntry:
		return "Putnami MCP server entry removed from " + mcpConfigPath
	case MCPRemovalFile:
		return mcpConfigPath + " removed (it held only the Putnami MCP server entry)"
	default:
		return ""
	}
}

// writtenMCPServerEntry is the entry earlier releases wrote under
// mcpServers.putnami for a launcher command: `putnami`, or `./putnamiw` in a
// source workspace. The environment variable is the one the server's launcher
// read; it is spelled out here because nothing else reads it any more.
func writtenMCPServerEntry(command string) map[string]any {
	return map[string]any{
		"command": command,
		"args":    []any{"mcp"},
		"env": map[string]any{
			"PUTNAMI_AGENT_WORKSPACE": "${CLAUDE_PROJECT_DIR:-.}",
		},
	}
}

// writtenMCPServerEntries are the canonical JSON forms of the entry under
// both launcher commands: a workspace may have switched between them since
// the write.
func writtenMCPServerEntries() []string {
	var forms []string
	for _, command := range []string{"putnami", "./putnamiw"} {
		encoded, err := canonicalJSON(writtenMCPServerEntry(command))
		if err != nil {
			// The entry is a literal of JSON-representable types.
			panic("agentctx: written MCP entry is not encodable: " + err.Error())
		}
		forms = append(forms, encoded)
	}
	return forms
}

// canonicalJSON renders a value the one way both sides of an entry comparison
// agree on. A parsed entry and a constructed one hold different Go types for
// the same document; json.Marshal erases that difference and sorts object
// keys, so the comparison is by value and order-independent.
func canonicalJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// RemoveMCPRegistration removes the .mcp.json entry Putnami wrote for its MCP
// server.
//
//   - The putnami entry is removed when it equals, by canonical JSON, the entry
//     written under either launcher command. The file is deleted when nothing
//     but an empty mcpServers object remains, and rewritten without the entry
//     otherwise.
//   - A putnami entry that differs was written on purpose, by a person or by
//     another tool: it stays byte for byte, with one warning on warn that names
//     the file and says to remove the entry by hand.
//   - A file that cannot be read or parsed stays untouched, with a warning. An
//     absent file, or one with no putnami entry, is silent.
//
// A nil warn discards the warnings.
func RemoveMCPRegistration(wsRoot string, warn io.Writer) MCPRemoval {
	path := filepath.Join(wsRoot, mcpConfigPath)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return MCPRemovalNone
	}
	if err != nil {
		warnMCPRegistration(warn, "cannot read "+mcpConfigPath+": "+err.Error()+"; it is left untouched. Remove its putnami MCP server entry by hand if it has one.")
		return MCPRemovalNone
	}

	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		warnMCPRegistration(warn, "cannot parse "+mcpConfigPath+": "+err.Error()+"; it is left untouched. Remove its putnami MCP server entry by hand if it has one.")
		return MCPRemovalNone
	}
	servers, _ := root["mcpServers"].(map[string]any)
	existing, present := servers["putnami"]
	if !present {
		return MCPRemovalNone
	}

	encoded, err := canonicalJSON(existing)
	if err != nil {
		warnMCPRegistration(warn, "cannot read the putnami entry of "+mcpConfigPath+": "+err.Error()+"; it is left untouched. Remove the entry by hand.")
		return MCPRemovalNone
	}
	written := false
	for _, form := range writtenMCPServerEntries() {
		if encoded == form {
			written = true
		}
	}
	if !written {
		warnMCPRegistration(warn, "the putnami entry of "+mcpConfigPath+" differs from the one Putnami wrote, so it is kept. Putnami serves no MCP server any more: remove the entry by hand.")
		return MCPRemovalNone
	}

	delete(servers, "putnami")
	if len(servers) == 0 && len(root) == 1 {
		if err := os.Remove(path); err != nil {
			warnMCPRegistration(warn, "cannot remove "+mcpConfigPath+": "+err.Error()+". Remove it by hand.")
			return MCPRemovalNone
		}
		return MCPRemovalFile
	}
	if err := writeMCPConfig(path, root); err != nil {
		warnMCPRegistration(warn, "cannot rewrite "+mcpConfigPath+": "+err.Error()+". Remove the putnami entry by hand.")
		return MCPRemovalNone
	}
	return MCPRemovalEntry
}

func warnMCPRegistration(warn io.Writer, message string) {
	if warn != nil {
		iox.Fprintf(warn, "putnami: warning: %s\n", message)
	}
}

// writeMCPConfig writes root as indented JSON with a trailing newline, the
// form earlier releases wrote the file in.
func writeMCPConfig(path string, root map[string]any) error {
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}
