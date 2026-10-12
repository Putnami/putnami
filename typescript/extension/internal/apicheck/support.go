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
//
// tags are the project's resolved tags as the job context carries them
// (`project.tags`). They decide whether the catalog lists the project as a
// protocol or as a package. A nil list says the context carries none, which is
// not the same as a project with no tags. The tags are then not resolved from
// the manifests: the check reads the catalog both ways, and fails only when the
// two readings disagree on whether the API is checked. A workspace with no
// catalog reads no tag, so a nil list changes nothing there.
func notChecked(workspaceRoot, name string, tags []string) (string, error) {
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
	if tags == nil {
		asPackage := catalogVerdict(catalog, name, supportproto.SubjectKindPackage)
		asProtocol := catalogVerdict(catalog, name, supportproto.SubjectKindProtocol)
		switch {
		case asPackage.checked && asProtocol.checked:
			return "", nil
		case !asPackage.checked && !asProtocol.checked:
			return fmt.Sprintf("%s lists %s as neither a stable package nor a stable protocol, so it promises "+
				"no compatibility: its API is not checked", supportproto.CatalogFilename, name), nil
		}
		return "", fmt.Errorf("the job context carries no resolved tags (`project.tags`), and %s gives %s a "+
			"different status as a protocol than as a package; this orchestrator predates the member",
			supportproto.CatalogFilename, name)
	}
	kind := supportproto.SubjectKindPackage
	if slices.Contains(tags, protocolTag) {
		kind = supportproto.SubjectKindProtocol
	}
	return catalogVerdict(catalog, name, kind).skip, nil
}

// supportVerdict is what the catalog says about one subject: whether its API
// is checked, and otherwise why not.
type supportVerdict struct {
	checked bool
	skip    string
}

// catalogVerdict reads the catalog entry for the subject of the given kind and
// name.
func catalogVerdict(catalog *supportproto.Catalog, name string, kind supportproto.SubjectKind) supportVerdict {
	for _, entry := range catalog.Entries {
		if entry.Kind != kind || entry.ID != name {
			continue
		}
		if entry.Status == supportproto.StatusStable {
			return supportVerdict{checked: true}
		}
		return supportVerdict{skip: fmt.Sprintf("%s is %s in %s, which promises no compatibility: its API is not checked",
			name, entry.Status, supportproto.CatalogFilename)}
	}
	return supportVerdict{skip: fmt.Sprintf("%s does not list the %s %s, so it promises no compatibility: its API is not checked",
		supportproto.CatalogFilename, kind, name)}
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
