package shared

import (
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// BuildExtensionMap builds a name→constraint map from the workspace config.
func BuildExtensionMap(cfg *wsproto.Config) map[string]string {
	extMap := make(map[string]string)
	for name, constraint := range cfg.Extensions.List {
		if constraint != "" {
			extMap[name] = constraint
		} else {
			extMap[name] = "latest"
		}
	}
	return extMap
}

// BuildTemplateMap builds a name→constraint map from the workspace config.
func BuildTemplateMap(cfg *wsproto.Config) map[string]string {
	tplMap := make(map[string]string)
	for _, tpl := range cfg.Templates {
		if idx := strings.IndexByte(tpl, ':'); idx >= 0 {
			tplMap[tpl[:idx]] = tpl[idx+1:]
		} else {
			tplMap[tpl] = "latest"
		}
	}
	return tplMap
}

// IsRegistryArtifactRef reports whether a configured name is a registry-backed
// artifact (installed by name) rather than a workspace-local path, matching the
// "@scope/name" convention used by missingRegistryExtensions. Shared by 2+
// verticals (lifecycle's implicit-ensure pass, agentctx's context generation).
func IsRegistryArtifactRef(name string) bool {
	return strings.HasPrefix(name, "@")
}
