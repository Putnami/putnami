package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	supportproto "go.putnami.dev/protocol/support"
)

// Two gates this REPOSITORY holds itself to, keyed on its own checked-in
// workspace document rather than on a fixture.
//
// # Why they live here
//
// They were part of internal/commands/sdd's spec-validation tests until a
// refactor moved that vertical into the @putnami/sdd extension. They could not
// follow it: the extension takes its workspace from the job wire and must never
// grow a loader, and these two questions are asked
// about a repository on disk, not about a job's project.
//
// They could also not simply be deleted. `specs.missing_support_entry` is a
// WARNING on purpose — an unclassified project is scheduled work in a user's
// workspace, not a broken document — so nothing but a gate keeps Putnami's own
// catalog complete, and a gate that disappears with a refactor is a gate that
// was never load-bearing. So they moved to the package that owns "what the
// checked-in workspace says": the loader.
//
// # What changed in the move, and what did not
//
// The property is identical; the vehicle is not. The old versions asked the
// spec engine for its completeness report and read two of its fields. These ask
// the same question directly, of the workspace document and the support catalog
// — which are the two authorities the answer was ever derived from. The
// predicate is reproduced here rather than imported because the engine that
// held it now ships in a different module; supportKindForProject's convention
// (a `protocol` tag selects the protocol subject kind) is pinned by the first
// test below, exactly as it was.

// TestCheckedInProtocolProjectsCarryProtocolTag pins the authored convention
// that decides which support SUBJECT a project needs.
//
// The classification is keyed on the tag rather than on a repository-specific
// path rule, which is what lets a protocol live anywhere; the cost is that a
// protocol project that lost its tag would silently start requesting a package
// entry, and satisfy it with the wrong one.
func TestCheckedInProtocolProjectsCarryProtocolTag(t *testing.T) {
	ws := loadCheckedInWorkspace(t)

	seen := 0
	for _, project := range ws.Projects {
		if project == nil || !isProtocolPath(project.Path) {
			continue
		}
		seen++
		if !slices.Contains(project.Tags, protocolTag) {
			t.Errorf("protocol project %s (%s) does not carry the %q tag", project.Name, project.Path, protocolTag)
		}
	}
	if seen == 0 {
		t.Fatal("checked-in workspace exposed no protocol projects, so this gate proved nothing")
	}
}

// TestCheckedInWorkspaceClassifiesEveryPublicSubject holds this repository to
// the completeness the SDD spec surface only reports. Promotion criteria live
// in protocols/support ADR 0002.
func TestCheckedInWorkspaceClassifiesEveryPublicSubject(t *testing.T) {
	ws := loadCheckedInWorkspace(t)

	catalog := readCheckedInSupportCatalog(t, ws.Root)
	classified := make(map[supportSubject]bool, len(catalog.Entries))
	for _, entry := range catalog.Entries {
		classified[supportSubject{kind: entry.Kind, id: entry.ID}] = true
	}

	assessed := assessedSupportSubjects(ws, classified)
	if len(assessed) == 0 {
		t.Fatal("the checked-in workspace exposed no publishable project, so this gate proved nothing")
	}
	// Non-vacuity, at the exact spot where this check has silently shrunk
	// before: publication is declared in TWO shapes, and reading only the
	// top-level `publish` array once excused every TypeScript package in the
	// repository while the gate still reported dozens of projects assessed.
	// Both shapes must still be reaching the set.
	var byArray, byOption int
	for _, project := range assessed {
		switch {
		case len(project.Publish) > 0:
			byArray++
		case declaresConsumablePublish(project):
			byOption++
		}
	}
	if byArray == 0 || byOption == 0 {
		t.Fatalf("assessed %d projects but %d declare `publish` and %d declare `options.publish` — "+
			"one of the two publication shapes stopped being detected, so this gate is now blind to a whole family",
			len(assessed), byArray, byOption)
	}

	var unclassified []string
	for _, project := range assessed {
		if !classified[requiredSupportSubject(project)] {
			unclassified = append(unclassified, subjectIDForProject(project))
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) != 0 {
		t.Errorf("public projects carry no reviewed support status: %v\n"+
			"classify each in %s, per protocols/support/doc/adr/0002-promotion-and-demotion-criteria.md",
			unclassified, supportproto.CatalogFilename)
	}
}

// TestCheckedInNPMWorkspaceProjectionMatchesMembership keeps a package from
// becoming an installable-but-unplannable island. package.json workspaces are a
// projection of Putnami membership, not an additional membership authority: a
// package present only in that projection is invisible to the dependency graph
// and therefore cannot join a complete release set even though Bun installs it.
func TestCheckedInNPMWorkspaceProjectionMatchesMembership(t *testing.T) {
	ws := loadCheckedInWorkspace(t)
	data, err := os.ReadFile(filepath.Join(ws.Root, "package.json"))
	if err != nil {
		t.Fatalf("read checked-in package.json: %v", err)
	}
	var rootManifest struct {
		Workspaces []string `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &rootManifest); err != nil {
		t.Fatalf("parse checked-in package.json: %v", err)
	}

	members := make([]string, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(ws.Root, filepath.FromSlash(project.Path), "package.json")); err == nil {
			members = append(members, project.Path)
		}
	}
	sort.Strings(members)
	sort.Strings(rootManifest.Workspaces)
	if !slices.Equal(rootManifest.Workspaces, members) {
		t.Fatalf("package.json workspaces = %v, Putnami npm membership = %v; run `putnami projects sync`",
			rootManifest.Workspaces, members)
	}
}

// TestCheckedInNPMPublishProjectsCarryLocalTypeScriptConfig pins the build
// boundary the npm packager executes. Without a project-local tsconfig.json,
// tsc walks up to the workspace config and rejects the packager's explicit file
// list (TS5112), so a package can pass ordinary source tests yet fail its first
// real release.
func TestCheckedInNPMPublishProjectsCarryLocalTypeScriptConfig(t *testing.T) {
	ws := loadCheckedInWorkspace(t)
	seen := 0
	for _, project := range ws.Projects {
		if project == nil || !projectPublishesNPM(project) {
			continue
		}
		seen++
		for _, filename := range []string{"package.json", "tsconfig.json"} {
			path := filepath.Join(ws.Root, filepath.FromSlash(project.Path), filename)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("npm-published project %s (%s) has no local %s: %v", project.Name, project.Path, filename, err)
			}
		}
	}
	if seen == 0 {
		t.Fatal("checked-in workspace exposed no npm-published project, so this gate proved nothing")
	}
}

// --- the two authorities -----------------------------------------------------

// checkedInRoot is this module's repository root: internal/workspace is three
// levels below tooling/cli, which is two below the workspace root.
func checkedInRoot() string {
	return filepath.Clean(filepath.Join("..", "..", "..", ".."))
}

func loadCheckedInWorkspace(t *testing.T) *Workspace {
	t.Helper()
	root := checkedInRoot()
	if _, err := os.Stat(filepath.Join(root, "putnami.workspace.json")); err != nil {
		if os.IsNotExist(err) {
			t.Skip("checked-in workspace is unavailable")
		}
		t.Fatal(err)
	}
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("load checked-in workspace: %v", err)
	}
	return ws
}

func readCheckedInSupportCatalog(t *testing.T, root string) *supportproto.Catalog {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, supportproto.CatalogFilename))
	if err != nil {
		t.Fatalf("read %s: %v — this repository's support catalog is the authority this gate reads",
			supportproto.CatalogFilename, err)
	}
	catalog, findings := supportproto.ParseAndValidateCatalog(data)
	if catalog == nil || diag.HasErrors(findings) {
		t.Fatalf("%s is invalid, so completeness cannot be assessed: %+v", supportproto.CatalogFilename, findings)
	}
	return catalog
}

// --- the predicate, reproduced --------------------------------------------

const protocolTag = "protocol"

// protocolPathPrefix is used ONLY to find the projects the tag convention
// applies to, never to classify one. Classification reads the tag.
const protocolPathPrefix = "protocols/"

func isProtocolPath(path string) bool {
	return len(path) > len(protocolPathPrefix) && path[:len(protocolPathPrefix)] == protocolPathPrefix
}

// supportSubject mirrors the support protocol's identity boundary: kind and id
// are both required. A package entry and a protocol entry with the same id
// classify different subjects and must never satisfy one another.
type supportSubject struct {
	kind supportproto.SubjectKind
	id   string
}

func subjectIDForProject(project *Project) string {
	if project.Name != "" {
		return project.Name
	}
	return project.ID
}

func requiredSupportSubject(project *Project) supportSubject {
	kind := supportproto.SubjectKindPackage
	if slices.Contains(project.Tags, protocolTag) {
		kind = supportproto.SubjectKindProtocol
	}
	return supportSubject{kind: kind, id: subjectIDForProject(project)}
}

// consumableSupportPublishChannels are the `options.publish` channels that
// produce an artifact someone outside this repository installs and depends on.
// The top-level `publish` array is only one of the two shapes a project can
// declare publication in: TypeScript packages publish through
// `options.publish.npm`, so keying completeness on the array alone would
// silently excuse the whole TypeScript framework family.
var consumableSupportPublishChannels = []string{"npm", "archives"}

func declaresConsumablePublish(project *Project) bool {
	if len(project.Publish) > 0 {
		return true
	}
	if project.Config == nil {
		return false
	}
	publish := project.Config.Options["publish"]
	for _, channel := range consumableSupportPublishChannels {
		// An explicit `false` is a decision not to publish and is honored — the
		// Go and Python sample applications set `archives: false` beside
		// `docker: true`.
		if enabled, isBool := publish[channel].(bool); isBool && enabled {
			return true
		}
	}
	return false
}

func projectPublishesNPM(project *Project) bool {
	if project == nil || project.Config == nil {
		return false
	}
	publish := project.Config.Options["publish"]
	enabled, isBool := publish["npm"].(bool)
	return isBool && enabled
}

// assessedSupportSubjects is the set completeness checks: everything that
// declares publication, plus every project the reviewed catalog ALREADY
// classifies. The catalog is the authority on what Putnami calls a public
// package, so a classified subject must stay assessable even when its own
// putnami.json declares no channel — putnami-extension-sdk is `stable` in the
// catalog and declares none.
func assessedSupportSubjects(ws *Workspace, classified map[supportSubject]bool) []*Project {
	assessed := make([]*Project, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		if declaresConsumablePublish(project) || classified[requiredSupportSubject(project)] {
			assessed = append(assessed, project)
		}
	}
	sort.Slice(assessed, func(i, j int) bool { return assessed[i].Path < assessed[j].Path })
	return assessed
}
