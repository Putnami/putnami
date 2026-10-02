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

// notChecked returns why the support catalog exempts a project from the check,
// or "" when the project's API is checked.
//
// When the workspace has a root catalog, it is the authority: only a project
// it lists as stable promises compatibility, so only such a project is
// checked. A project it lists as preview or experimental, or does not list at
// all, promises nothing. A workspace with no catalog has made no statement, so
// every project is checked: the reading that cannot surprise a user. An
// unreadable or invalid catalog is an error, because reading it as absent
// would check every project it exempts.
//
// The version bump reads the catalog by the same rule
// (tooling/cli/internal/workspace SupportSubjectOf).
func notChecked(workspaceRoot, projectDir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, supportproto.CatalogFilename)) //nolint:gosec // the workspace root plus one protocol filename
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
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
		if entry.Kind != kind || entry.ID != name {
			continue
		}
		if entry.Status == supportproto.StatusStable {
			return "", nil
		}
		return fmt.Sprintf("%s is %s in %s, which promises no compatibility: its API is not checked",
			name, entry.Status, supportproto.CatalogFilename), nil
	}
	return fmt.Sprintf("%s does not list the %s %s, so it promises no compatibility: its API is not checked",
		supportproto.CatalogFilename, kind, name), nil
}

// projectTags returns a project's tags as the workspace loader resolves them
// (tooling/cli-model/workspace probe_view.go): the tags its putnami.json
// declares, else the tags of its scope chain. An authored empty list is a
// declaration, "this project has no tags", and blocks the scope's tags as it
// does for the loader. The loader also reads tags a language provider reports
// for a project that declares none; a task cannot see those, so such a project
// takes its scope's tags here.
func projectTags(workspaceRoot, projectDir string) []string {
	if config, _ := wsproto.LoadProjectConfigWithDiagnostics(projectDir); config != nil && config.Tags != nil {
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
