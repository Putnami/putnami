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
// When a catalog exists, it is the authority: a commit counts as stable only
// when a file it changes belongs to a project the catalog lists as `stable`. A
// preview or experimental project promises no compatibility, and neither does a
// project the catalog does not list or a file no project owns, so a commit that
// touches only those advances a patch at most.
//
// A workspace with no catalog returns a nil test, which reads every commit as
// stable. An unreadable or invalid catalog is an error: reading it as absent
// would raise the bump of every preview project without a word.
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
	stable := make(map[supportSubject]bool, len(catalog.Entries))
	for _, entry := range catalog.Entries {
		if entry.Status == supportproto.StatusStable {
			stable[supportSubject{kind: entry.Kind, id: entry.ID}] = true
		}
	}
	// A path's owners are the same for every commit, and each lookup rebuilds
	// the owner index, so a path is looked up once per test. The test is
	// therefore not safe for concurrent use.
	ownedByStable := make(map[string]bool)
	return func(files []string) bool {
		for _, file := range files {
			promised, seen := ownedByStable[file]
			if !seen {
				for _, owner := range ProjectOwnersForPath(ws, file) {
					kind, id := SupportSubjectOf(owner)
					if stable[supportSubject{kind: kind, id: id}] {
						promised = true
						break
					}
				}
				ownedByStable[file] = promised
			}
			if promised {
				return true
			}
		}
		return false
	}, nil
}
