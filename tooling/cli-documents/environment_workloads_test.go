package documents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
)

// configNamespaceOptionLayers are the manifest option layers, in ascending
// precedence, that declare a project's authored Config namespace. The Cloud
// extension announces a Config member for a project only when one of them
// names a namespace (its workspace probe and its package/publish steps agree
// on the list); a project with a config schema but no declaration has no
// member at all, and its package and publish steps skip.
var configNamespaceOptionLayers = []string{"publish", "@putnami/cloud", "@putnami/cloud:publish", "@putnami/cloud:publish-config"}

// TestDeclaredEnvironmentWorkloadsPublishImageAndConfig holds every workload a
// putnami.ci.json environment follows to the shape an ordinary deployment
// accepts: one published image and one authored Config member per workload,
// both produced by the same run. The provider's publish handoff fails the
// release when a declared workload lacks either member, so an environment must
// not select a project whose manifest cannot yield both.
func TestDeclaredEnvironmentWorkloadsPublishImageAndConfig(t *testing.T) {
	t.Parallel()
	repoRoot := repositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(repoRoot, ciproto.Filename))
	if err != nil {
		t.Fatalf("%s is absent: %v", ciproto.Filename, err)
	}
	document, err := ciproto.Parse(raw)
	if err != nil {
		t.Fatalf("%s is not a valid document: %v", ciproto.Filename, err)
	}
	for name, environment := range document.Envs {
		for _, rule := range environment.Workloads {
			for _, selector := range rule.Select {
				if strings.ContainsAny(selector, "*?[") || strings.HasPrefix(selector, "/") {
					t.Errorf("envs.%s selects %q; Control's ordinary deployment accepts exact project paths only", name, selector)
					continue
				}
				options := manifestOptions(t, filepath.Join(repoRoot, filepath.FromSlash(selector), "putnami.json"))
				if !optionEnabled(options, "publish", "docker") {
					t.Errorf("envs.%s follows %s, which publishes no image (options.publish.docker is not true)", name, selector)
				}
				if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(selector), "schema", "config.json")); err != nil {
					t.Errorf("envs.%s follows %s, which has no schema/config.json to build a Config member from", name, selector)
				}
				if namespace := declaredConfigNamespace(options); namespace == "" {
					t.Errorf("envs.%s follows %s, which declares no Config namespace: the Cloud extension announces no Config member for it, and the publish handoff refuses the release; set options[%q].namespace", name, selector, configNamespaceOptionLayers[len(configNamespaceOptionLayers)-1])
				}
			}
		}
	}
}

func manifestOptions(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var manifest struct {
		Options map[string]map[string]any `json:"options"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return manifest.Options
}

func optionEnabled(options map[string]map[string]any, layer, key string) bool {
	enabled, _ := options[layer][key].(bool)
	return enabled
}

// declaredConfigNamespace resolves the namespace exactly as the Cloud
// extension does: the highest-precedence layer that names one wins.
func declaredConfigNamespace(options map[string]map[string]any) string {
	namespace := ""
	for _, layer := range configNamespaceOptionLayers {
		value, ok := options[layer]["namespace"].(string)
		if ok && strings.TrimSpace(value) != "" {
			namespace = value
		}
	}
	return namespace
}
