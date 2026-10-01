package shared

import (
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
)

// EnsureProjectExtension reads the project's config and adds the extension
// name to the "extensions" array if not already present.
func EnsureProjectExtension(projectDir, extName string) error {
	cfgPath := wsproto.ResolveFile(projectDir, wsproto.ConfigFilename)
	raw, err := jsonutil.ReadFile(cfgPath)
	if err != nil {
		return nil // no config file — nothing to patch
	}

	// Check if extension is already listed
	if exts, ok := raw.Get("extensions"); ok {
		if arr, ok := exts.([]any); ok {
			for _, e := range arr {
				if s, ok := e.(string); ok && s == extName {
					return nil // already present
				}
			}
		}
	}

	// Add extension and rewrite
	existing, _ := raw.Get("extensions")
	arr, _ := existing.([]any)
	raw.Set("extensions", append(arr, extName))

	return jsonutil.WriteFile(cfgPath, raw)
}
