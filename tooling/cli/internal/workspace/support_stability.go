package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
	supportproto "go.putnami.dev/protocol/support"
	"go.putnami.dev/tooling/cli/internal/git"
)

// supportProtocolTag is the project tag that classifies a project as a protocol
// subject of the support catalog; every other project is a package subject. A
// checked-in conformance test pins the convention.
const supportProtocolTag = "protocol"

// supportSubject is a catalog identity: kind and id are both required, so a
// package entry and a protocol entry with the same id never classify one
// another's project.
type supportSubject struct {
	kind supportproto.SubjectKind
	id   string
}

// SupportSubjectOf is the catalog subject a project is classified under: its
// name, or its id when it has none, as a protocol when it carries the
// `protocol` tag and as a package otherwise.
func SupportSubjectOf(project *Project) (supportproto.SubjectKind, string) {
	kind := supportproto.SubjectKindPackage
	if slices.Contains(project.Tags, supportProtocolTag) {
		kind = supportproto.SubjectKindProtocol
	}
	if project.Name != "" {
		return kind, project.Name
	}
	return kind, project.ID
}

// StableChangeTest is the test the version bump asks of each commit: does it
// touch a project the root support catalog lists as stable?
//
// A commit reads as stable unless every file it changes belongs to a project
// the catalog lists as preview or experimental. A file no project owns, and a
// project the catalog does not list, count as stable: without a reviewed
// status, the larger bump is the one that cannot surprise a user.
//
// A workspace with no catalog returns a nil test, which reads every commit as
// stable. An unreadable or invalid catalog is an error, because reading it as
// absent would raise the bump of every preview package without a word.
func StableChangeTest(ws *Workspace) (git.StableTest, error) {
	if ws == nil {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(ws.Root, supportproto.CatalogFilename)) //nolint:gosec // the workspace root plus one protocol filename
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", supportproto.CatalogFilename, err)
	}
	catalog, findings := supportproto.ParseAndValidateCatalog(data)
	if catalog == nil || diag.HasErrors(findings) {
		return nil, fmt.Errorf("%s is invalid: %s", supportproto.CatalogFilename, diag.ErrorText(findings))
	}
	unpromised := make(map[supportSubject]bool, len(catalog.Entries))
	for _, entry := range catalog.Entries {
		if entry.Status == supportproto.StatusPreview || entry.Status == supportproto.StatusExperimental {
			unpromised[supportSubject{kind: entry.Kind, id: entry.ID}] = true
		}
	}
	return func(files []string) bool {
		if len(files) == 0 {
			return true
		}
		for _, file := range files {
			owners := ProjectOwnersForPath(ws, file)
			if len(owners) == 0 {
				return true
			}
			for _, owner := range owners {
				kind, id := SupportSubjectOf(owner)
				if !unpromised[supportSubject{kind: kind, id: id}] {
					return true
				}
			}
		}
		return false
	}, nil
}
