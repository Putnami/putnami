package datacli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MigrationNamespaceFromOptions is shared by the workspace probe and the
// migration publisher, including publish tasks whose command is "publish"
// rather than "publish-migration". Only the project declares this namespace.
func MigrationNamespaceFromOptions(options map[string]map[string]any) (string, bool, error) {
	namespace, declared := "", false
	for _, layer := range []string{"publish", "@putnami/cloud", "@putnami/cloud:publish", "@putnami/cloud:publish-migration"} {
		value, found := options[layer]["namespace"]
		if !found {
			continue
		}
		candidate, ok := value.(string)
		if !ok || candidate != strings.TrimSpace(candidate) || !migrationAddressPartPattern.MatchString(candidate) {
			return "", false, fmt.Errorf("options.%s.namespace must be one canonical native path segment", layer)
		}
		namespace, declared = candidate, true
	}
	return namespace, declared, nil
}

func declaredMigrationNamespace(appDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(appDir, "putnami.json"))
	if err != nil {
		return "", fmt.Errorf("read migration project manifest: %w", err)
	}
	var manifest struct {
		Options map[string]map[string]any `json:"options"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "", fmt.Errorf("parse migration project manifest: %w", err)
	}
	namespace, _, err := MigrationNamespaceFromOptions(manifest.Options)
	return namespace, err
}
