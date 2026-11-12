package planner

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A site project may declare a site-content section that the real
// @putnami/cloud manifest generates in build instead of committing it. These
// tests pin what the planner and the key code do with that manifest: every
// step that reads the section waits for the generate step, and the keys that
// fold the generate step's key, among them the member's selection
// fingerprint, move with the generator's declared inputs and with nothing
// else.

const (
	siteContentProjectPath  = "docs/example.test"
	siteContentGenerateStep = "/" + siteContentProjectPath + ":build~cloud-site-content-generate"
	// siteContentMemberKey is releaseset.MemberKey("put", "cloud/doc-contents-api"):
	// the ecosystem and the coordinate joined by a NUL byte. The planner's tests
	// do not import the SDK.
	siteContentMemberKey = "put\x00cloud/doc-contents-api"
)

// siteContentMembers is the release-set metadata the Cloud workspace probe
// announces for the fixture: one member per section.
const siteContentMembers = `{"releaseSet":{"ecosystems":[
  {"ecosystem":"put","coordinate":"cloud/doc-contents-api","packageStep":"cloud-site-content","publishStep":"cloud-publish-site-content"},
  {"ecosystem":"put","coordinate":"cloud/doc-contents-guide","packageStep":"cloud-site-content","publishStep":"cloud-publish-site-content"}
]}}`

// siteContentGeneratorDocument is the project document of the fixture: one
// committed section, one generated section, and the generator's inputs listed
// for the key of the extension's build command.
const siteContentGeneratorDocument = `{
  "name": "docs/example.test",
  "publish": ["site-content"],
  "options": {
    "@putnami/cloud": {"site-content": {"generated": {"api": {"command": ["go", "run", "../../tools/gen", "{output}"]}}}},
    "@putnami/cloud:build": {"filePatterns": ["../../tools/gen/**"]}
  }
}
`

// siteContentWorkspace writes a Git work tree that ignores .gen, as this
// repository does, with the site project and a generator outside it, and
// returns the workspace, the project and the real Cloud extension. The
// validation step keys the project's Git candidate files, so the workspace is
// a work tree.
func siteContentWorkspace(t *testing.T) (*workspace.Workspace, *workspace.Project, *extension.ExtensionDescription) {
	t.Helper()
	cloud := loadShippedExtension(t, "cloud/extension", "@putnami/cloud")

	isolateTreeKeyedGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	writeTestFile(t, filepath.Join(root, ".gitignore"), ".gen/\n.putnami/\n")
	writeProjectFile(t, root, siteContentProjectPath, "putnami.json", siteContentGeneratorDocument)
	writeProjectFile(t, root, siteContentProjectPath, "guide/index.md", "# Guide\n")
	writeProjectFile(t, root, "tools/gen", "main.go", "package main\n")
	var document wsproto.ProjectConfig
	if err := json.Unmarshal([]byte(siteContentGeneratorDocument), &document); err != nil {
		t.Fatal(err)
	}
	site := &workspace.Project{
		ID: "/" + siteContentProjectPath, Name: siteContentProjectPath, Path: siteContentProjectPath,
		Publish: document.Publish, Config: &document,
		// The Cloud workspace probe announces the members under its own name.
		Metadata: map[string]json.RawMessage{cloud.Name: json.RawMessage(siteContentMembers)},
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{site})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	return ws, site, cloud
}

func planSiteContent(t *testing.T, ws *workspace.Workspace, site *workspace.Project, cloud *extension.ExtensionDescription, command string) map[string]*jobmodel.ScheduledJob {
	t.Helper()
	planned, err := Plan(ws, []string{command}, []*workspace.Project{site}, []*extension.ExtensionDescription{cloud}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan %s: %v", command, err)
	}
	byKey := make(map[string]*jobmodel.ScheduledJob, len(planned))
	for _, job := range planned {
		byKey[job.Key()] = job
	}
	return byKey
}

// siteContentKeys computes the key of every cacheable job of planned with a
// fresh cache manager, so no memoized file hash hides a content change.
func siteContentKeys(t *testing.T, ws *workspace.Workspace, planned []*jobmodel.ScheduledJob) map[string]string {
	t.Helper()
	cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	cache.SetInputResolver(noListedFiles{})
	keys, err := cachekey.PrecomputeKeys(ws, planned, nil, nil, cache, cachekey.CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	if keys[siteContentGenerateStep] == "" {
		t.Fatalf("no key for %s in %v", siteContentGenerateStep, slices.Sorted(maps.Keys(keys)))
	}
	return keys
}

// putEcosystemProfiles is the ecosystem registry the selection fingerprint
// resolves against: the site-content members' put ecosystem, published by the
// publish command. The CLI names no ecosystem itself, so the put owner has to
// be supplied.
func putEcosystemProfiles(t *testing.T) *extensionproto.ProfileRegistry {
	t.Helper()
	registry, diagnostics := extensionproto.ResolveProfiles([]extensionproto.NamedManifest{{
		Name: "put-owner",
		Manifest: &extensionproto.Manifest{
			Name:     "put-owner",
			Commands: map[string]extensionproto.CommandDefinition{"package": {}, "publish": {}},
			Ecosystems: []extensionproto.EcosystemProfile{{
				ID:         "put",
				Coordinate: extensionproto.PatternRule{Pattern: `^[A-Za-z0-9@._/-]+$`},
				Version:    extensionproto.VersionRule{Pattern: `^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)*$`, Ordering: extensionproto.OrderingSemver},
				Channel:    extensionproto.ChannelNative,
				Registries: json.RawMessage(`{"type":"object"}`),
				Publish:    "publish",
			}},
		},
	}}, nil)
	if len(diagnostics) != 0 {
		t.Fatalf("test ecosystem profiles are invalid: %v", diagnostics)
	}
	return registry
}

// Validate, package and publish each plan the generate step, and the step of
// each that reads the generated section waits for it.
func TestTheStepsThatReadAGeneratedSiteContentSectionWaitForItsGenerateStep(t *testing.T) {
	ws, site, cloud := siteContentWorkspace(t)
	for command, readers := range map[string][]string{
		"validate": {"validate~cloud-site-content"},
		"package":  {"package~cloud-site-content"},
		"publish":  {"package~cloud-site-content"},
	} {
		t.Run(command, func(t *testing.T) {
			planned := planSiteContent(t, ws, site, cloud, command)
			generate := planned[siteContentGenerateStep]
			if generate == nil || generate.Step == nil || generate.Step.Task != "cloud-site-content-generate" {
				t.Fatalf("plan of %s holds no %s: %v", command, siteContentGenerateStep, slices.Sorted(maps.Keys(planned)))
			}
			for _, reader := range readers {
				job := planned[site.ID+":"+reader]
				if job == nil || !slices.Contains(job.DependsOn, siteContentGenerateStep) {
					t.Fatalf("%s:%s = %+v, want it to depend on %s", site.ID, reader, job, siteContentGenerateStep)
				}
			}
		})
	}

	// The release-set member's package command is package, not build, though
	// build now carries injectPublishChannels: no build step is the package
	// step cloud-site-content.
	if command := extension.MemberPackageCommand([]*extension.ExtensionDescription{cloud}, cloud.Name, cloud.Name, "publish", "cloud-site-content"); command != "package" {
		t.Fatalf("the site-content member's package command = %q, want package", command)
	}
}

// The key of the validation step that reads a generated section folds the
// generate step's key: it moves with a listed generator input and not with
// the generated output, which the generate step can rewrite between the run's
// key pass and the validation step's start.
func TestTheGeneratedSiteContentValidationKeyFoldsTheGenerateKey(t *testing.T) {
	ws, site, cloud := siteContentWorkspace(t)
	planned := slices.Collect(maps.Values(planSiteContent(t, ws, site, cloud, "validate")))
	validate := site.ID + ":validate~cloud-site-content"
	key := func() string {
		t.Helper()
		got := siteContentKeys(t, ws, planned)[validate]
		if got == "" {
			t.Fatalf("no key for %s", validate)
		}
		return got
	}

	base := key()
	writeProjectFile(t, ws.Root, siteContentProjectPath, ".gen/site-content/api/index.md", "# API\n")
	if got := key(); got != base {
		t.Errorf("the generated output moved the validation key; the generate step can rewrite it during the run: %s, want %s", got, base)
	}
	writeProjectFile(t, ws.Root, "tools/gen", "main.go", "package main\n\nfunc main() {}\n")
	if got := key(); got == base {
		t.Error("a change to a listed generator input left the validation key unmoved; it does not fold the generate key")
	}
}

// The selection fingerprint of a generated section's member is the key of its
// package step, which waits for the generate step: it moves when a file the
// project lists as a generator input or the generator command moves, and not
// when an unlisted file or the generated output itself moves.
func TestTheGeneratedSiteContentSelectionFingerprintFollowsTheGeneratorInputs(t *testing.T) {
	ws, site, cloud := siteContentWorkspace(t)
	planned := slices.Collect(maps.Values(planSiteContent(t, ws, site, cloud, "package")))
	memberKey := siteContentMemberKey
	members := map[string]*workspace.Project{memberKey: site}
	registry := putEcosystemProfiles(t)
	fingerprint := func() string {
		t.Helper()
		// A fresh manager each time: the memoized file hashes would otherwise
		// hide the content change the key is supposed to see.
		cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
		cache.SetInputResolver(noListedFiles{})
		got, err := cachekey.SelectionFingerprints(ws, planned, nil, cache, registry,
			[]*extension.ExtensionDescription{cloud}, members)
		if err != nil {
			t.Fatalf("SelectionFingerprints: %v", err)
		}
		if got[memberKey] == "" {
			t.Fatalf("no fingerprint for the generated member in %v", got)
		}
		return got[memberKey]
	}

	base := fingerprint()
	if again := fingerprint(); again != base {
		t.Fatalf("the fingerprint is not deterministic: %s then %s", base, again)
	}

	writeProjectFile(t, ws.Root, "tools", "notes.txt", "not a generator input\n")
	if got := fingerprint(); got != base {
		t.Errorf("a file the project does not list as a generator input moved the fingerprint: %s, want %s", got, base)
	}
	writeProjectFile(t, ws.Root, siteContentProjectPath, ".gen/site-content/api/index.md", "# API\n")
	if got := fingerprint(); got != base {
		t.Errorf("the generated output moved the fingerprint; it is a function of the generate key: %s, want %s", got, base)
	}

	writeProjectFile(t, ws.Root, "tools/gen", "main.go", "package main\n\nfunc main() {}\n")
	input := fingerprint()
	if input == base {
		t.Fatal("a change to a listed generator input left the fingerprint unmoved; the member would keep the old section")
	}

	writeProjectFile(t, ws.Root, siteContentProjectPath, "putnami.json",
		`{"name":"docs/example.test","publish":["site-content"],"options":{"@putnami/cloud":{"site-content":{"generated":{"api":{"command":["go","run","../../tools/gen","--all","{output}"]}}}},"@putnami/cloud:build":{"filePatterns":["../../tools/gen/**"]}}}`)
	if got := fingerprint(); got == input {
		t.Error("a change to the generator command left the fingerprint unmoved")
	}
}
