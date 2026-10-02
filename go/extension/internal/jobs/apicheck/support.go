package apicheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	supportproto "go.putnami.dev/protocol/support"
	wsproto "go.putnami.dev/protocol/workspace"
)

// protocolTag is the project tag that makes a project a protocol subject of
// the support catalog; every other project is a package subject. It is the
// convention the version bump reads (tooling/cli/internal/workspace
// SupportSubjectOf).
const protocolTag = "protocol"

// supportStatus returns the support status the root catalog gives a project.
//
// A project the catalog does not list is stable, and so is every project of a
// workspace with no catalog: without a reviewed status, the reading that
// checks compatibility is the one that cannot surprise a user. An unreadable
// or invalid catalog is an error, because reading it as absent would check
// every preview package as if it were stable, or skip a stable one.
func supportStatus(workspaceRoot, projectDir, name string) (supportproto.Status, error) {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, supportproto.CatalogFilename)) //nolint:gosec // the workspace root plus one protocol filename
	if errors.Is(err, fs.ErrNotExist) {
		return supportproto.StatusStable, nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", supportproto.CatalogFilename, err)
	}
	catalog, findings := supportproto.ParseAndValidateCatalog(data)
	if catalog == nil || diag.HasErrors(findings) {
		return "", fmt.Errorf("%s is invalid: %s", supportproto.CatalogFilename, diag.ErrorText(findings))
	}
	kind := supportproto.SubjectKindPackage
	if slices.Contains(projectTags(workspaceRoot, projectDir), protocolTag) {
		kind = supportproto.SubjectKindProtocol
	}
	for _, entry := range catalog.Entries {
		if entry.Kind == kind && entry.ID == name {
			return entry.Status, nil
		}
	}
	return supportproto.StatusStable, nil
}

// projectTags returns a project's tags as the workspace resolves them: the
// tags its putnami.json declares, else the tags of its scope chain.
func projectTags(workspaceRoot, projectDir string) []string {
	if config := wsproto.LoadProjectConfig(projectDir); config != nil && len(config.Tags) > 0 {
		return config.Tags
	}
	if scope := wsproto.LoadScopeChain(workspaceRoot, projectDir); scope != nil {
		return scope.Tags
	}
	return nil
}

// linePattern returns the tag pattern of the version line a project belongs
// to: the line of its deepest ancestor scope that declares one, below the
// workspace root, else the root line. It is the rule the workspace loader
// applies (tooling/cli/internal/workspace nearestAncestorLine).
func linePattern(workspaceRoot, projectDir string) string {
	dir := filepath.Dir(projectDir)
	for {
		rel, err := filepath.Rel(workspaceRoot, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			break
		}
		if scope := wsproto.ReadScopeConfig(dir); scope.IsLine() {
			return wsproto.LineTagPattern(filepath.ToSlash(rel), scope.Line)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return wsproto.LineTagPattern("", nil)
}
