package build

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	httproutes "go.putnami.dev/protocol/http-routes"
)

const (
	httpRoutesFragmentDir = "http-routes.d"
	httpRoutesAssetPath   = "schema/http-routes.json"
)

type httpRoutesFragment struct {
	Routes []httproutes.Route `json:"routes"`
}

// ResetHTTPRoutes removes only the generated route fragments and their prior
// aggregate so a removed framework hook cannot leave stale public routes.
func ResetHTTPRoutes(projectPath string) error {
	genDir := filepath.Join(projectPath, ".gen")
	if err := os.RemoveAll(filepath.Join(genDir, httpRoutesFragmentDir)); err != nil {
		return fmt.Errorf("clear HTTP route fragments: %w", err)
	}
	if err := os.Remove(filepath.Join(genDir, httpRoutesAssetPath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear HTTP route inventory: %w", err)
	}
	return nil
}

// AggregateHTTPRoutes merges every framework hook fragment into one canonical
// putnami.http-routes.v1 artifact. Fragment order cannot affect its bytes.
func AggregateHTTPRoutes(projectPath string) (string, error) {
	fragmentDir := filepath.Join(projectPath, ".gen", httpRoutesFragmentDir)
	entries, err := os.ReadDir(fragmentDir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read HTTP route fragments: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var routes []httproutes.Route
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(fragmentDir, entry.Name())
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", fmt.Errorf("read HTTP route fragment %s: %w", entry.Name(), readErr)
		}
		var fragment httpRoutesFragment
		if unmarshalErr := json.Unmarshal(body, &fragment); unmarshalErr != nil {
			return "", fmt.Errorf("parse HTTP route fragment %s: %w", entry.Name(), unmarshalErr)
		}
		routes = append(routes, fragment.Routes...)
	}

	manifest, diags := httproutes.Canonicalize(routes)
	if diag.HasErrors(diags) {
		return "", httpRoutesDiagnosticError(diags)
	}
	body, diags := httproutes.CanonicalJSON(manifest)
	if diag.HasErrors(diags) {
		return "", httpRoutesDiagnosticError(diags)
	}
	out := filepath.Join(projectPath, ".gen", httpRoutesAssetPath)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", fmt.Errorf("prepare HTTP route inventory: %w", err)
	}
	if err := os.WriteFile(out, body, 0o644); err != nil {
		return "", fmt.Errorf("write HTTP route inventory: %w", err)
	}
	return out, nil
}

func httpRoutesDiagnosticError(diags []diag.Diagnostic) error {
	parts := make([]string, 0, len(diags))
	for _, d := range diags {
		if d.Field == "" {
			parts = append(parts, fmt.Sprintf("[%s] %s", d.Code, d.Message))
		} else {
			parts = append(parts, fmt.Sprintf("[%s] %s: %s", d.Code, d.Field, d.Message))
		}
	}
	return fmt.Errorf("HTTP route inventory validation failed: %s", strings.Join(parts, "; "))
}
