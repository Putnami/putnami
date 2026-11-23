package workspace

import (
	"slices"
	"strings"
)

// RetiredExtension is a first-party extension Putnami retired, and the
// extension that replaces it.
type RetiredExtension struct {
	// Name is the retired extension, as a workspace declares it.
	Name string
	// Successor is the extension that replaces it.
	Successor string
}

// retiredExtensions is the registry of retired extensions. A checkout cut
// before an extension retired still binds it, and the provider behind it may
// refuse every call: each surface that reads the workspace names the
// successor instead of a generic failure.
var retiredExtensions = []RetiredExtension{
	{Name: "@putnami/intelligence", Successor: "@putnami/cloud"},
}

// RetiredExtensions answers the registry of retired extensions, in a copy.
func RetiredExtensions() []RetiredExtension {
	return slices.Clone(retiredExtensions)
}

// Notice is the line install, doctor and the agent session hooks print for a
// checkout that binds r.
func (r RetiredExtension) Notice() string {
	return "This checkout binds " + r.Name + ", which is retired; rebase on the default branch or bind " + r.Successor + "."
}

// collaborationNamespace is the options namespace that binds each
// collaboration contract to its provider.
const collaborationNamespace = "collaboration"

// agentContentPrefix marks an agentArtifacts entry that opts into the agent
// content of an extension.
const agentContentPrefix = "extension:"

// RetiredBindings answers each retired extension the workspace binds, once,
// in registry order: one it lists in extensions, whose agent content it opts
// into with extension:<name>, or that it names as the provider of a contract
// under options.collaboration. A nil config binds none.
func (c *Config) RetiredBindings() []RetiredExtension {
	if c == nil {
		return nil
	}
	bound := map[string]bool{}
	for _, entry := range c.Extensions.Names() {
		bound[extensionEntryName(entry)] = true
	}
	for _, entry := range c.AgentArtifacts {
		if name, ok := strings.CutPrefix(strings.TrimSpace(entry), agentContentPrefix); ok {
			bound[strings.TrimSpace(name)] = true
		}
	}
	for _, binding := range c.Options[collaborationNamespace] {
		if member, ok := binding.(map[string]any); ok {
			if provider, ok := member["provider"].(string); ok {
				bound[strings.TrimSpace(provider)] = true
			}
		}
	}
	var retired []RetiredExtension
	for _, extension := range retiredExtensions {
		if bound[extension.Name] {
			retired = append(retired, extension)
		}
	}
	return retired
}

// extensionEntryName is the extension an extensions entry names, without a
// version written after its name, as in @scope/name@1.2.3.
func extensionEntryName(entry string) string {
	entry = strings.TrimSpace(entry)
	if at := strings.LastIndex(entry, "@"); at > 0 {
		return entry[:at]
	}
	return entry
}
