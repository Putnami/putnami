package planner

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
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
// else. They also pin what the manifest plans for a project that declares no
// generated section, and that a Go site project plans both extensions' build.

const (
	siteContentProjectPath  = "docs/example.test"
	siteContentGenerateStep = "/" + siteContentProjectPath + ":build~cloud-site-content-generate"
	// siteContentMemberKey is releaseset.MemberKey("put", "cloud/doc-contents-api"):
	// the ecosystem and the coordinate joined by a NUL byte. The planner's tests
	// do not import the SDK.
	siteContentMemberKey = "put\x00cloud/doc-contents-api"
)

// siteContentMember is the release-set member the Cloud workspace probe
// announces for one section of the fixture.
const siteContentMember = `{"ecosystem":"put","coordinate":"cloud/doc-contents-%s","packageStep":"cloud-site-content","publishStep":"cloud-publish-site-content"}`

// siteContentDeclaration is the fixture's site-content.json: one generated
// section, api.
const siteContentDeclaration = `{"generated": {"api": {"command": ["go", "run", "../../tools/gen", "{output}"]}}}
`

// siteContentFixture is one shape of the fixture's site project.
type siteContentFixture struct {
	name string
	// committed adds a committed section, guide.
	committed bool
	// declared writes site-content.json, which declares the generated api
	// section.
	declared bool
	// ignoreGen makes the work tree ignore .gen, as this repository does.
	// Without it the files a generator writes are untracked Git candidates.
	ignoreGen bool
	// goProject makes the site project a Go project that also publishes go.
	goProject bool
}

// siteContentGeneratedFixtures are the shapes that declare a generated
// section: beside a committed section, as the only section, and as the only
// section of a work tree that does not ignore .gen.
var siteContentGeneratedFixtures = []siteContentFixture{
	{name: "beside a committed section", committed: true, declared: true, ignoreGen: true},
	{name: "as the only section", declared: true, ignoreGen: true},
	{name: "in a work tree that does not ignore .gen", declared: true},
}

// siteContentPlan is a planned fixture: the workspace, the site project, the
// real Cloud extension and every extension the run loads.
type siteContentPlan struct {
	ws         *workspace.Workspace
	site       *workspace.Project
	cloud      *extension.ExtensionDescription
	extensions []*extension.ExtensionDescription
}

// siteContentWorkspace writes a Git work tree with the site project of
// fixture and a generator outside it, and returns it with the real Cloud
// extension, and the real Go extension for a Go project. The project lists
// the generator's inputs for the key of the Cloud build command. The
// validation step keys the project's Git candidate files, so the workspace
// is a work tree.
func siteContentWorkspace(t *testing.T, fixture siteContentFixture) siteContentPlan {
	t.Helper()
	cloud := loadShippedExtension(t, "cloud/extension", "@putnami/cloud")
	extensions := []*extension.ExtensionDescription{cloud}

	isolateTreeKeyedGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	ignored := ".putnami/\n"
	if fixture.ignoreGen {
		ignored = ".gen/\n" + ignored
	}
	writeTestFile(t, filepath.Join(root, ".gitignore"), ignored)

	// The project document lists the generator's inputs for the key of the
	// Cloud build command.
	publish, uses := `["site-content"]`, `[]`
	if fixture.goProject {
		goExtension := loadShippedExtension(t, "go/extension", "@putnami/go")
		extensions = append(extensions, goExtension)
		publish, uses = `["go", "site-content"]`, `["`+goExtension.Name+`"]`
		writeProjectFile(t, root, siteContentProjectPath, "go.mod", "module example.test/docs\n\ngo 1.26\n")
		writeProjectFile(t, root, siteContentProjectPath, "main.go", "package main\n\nfunc main() {}\n")
	}
	data := fmt.Sprintf(`{
  "name": %q,
  "publish": %s,
  "extensions": %s,
  "options": {"@putnami/cloud:build": {"filePatterns": ["../../tools/gen/**"]}}
}
`, siteContentProjectPath, publish, uses)
	writeProjectFile(t, root, siteContentProjectPath, "putnami.json", data)
	var document wsproto.ProjectConfig
	if err := json.Unmarshal([]byte(data), &document); err != nil {
		t.Fatal(err)
	}
	var members []string
	if fixture.declared {
		writeProjectFile(t, root, siteContentProjectPath, "site-content.json", siteContentDeclaration)
		members = append(members, fmt.Sprintf(siteContentMember, "api"))
	}
	if fixture.committed {
		writeProjectFile(t, root, siteContentProjectPath, "guide/index.md", "# Guide\n")
		members = append(members, fmt.Sprintf(siteContentMember, "guide"))
	}
	writeProjectFile(t, root, "tools/gen", "main.go", "package main\n")

	site := &workspace.Project{
		ID: "/" + siteContentProjectPath, Name: siteContentProjectPath, Path: siteContentProjectPath,
		Publish: document.Publish, Extensions: document.Extensions, Config: &document,
		// The Cloud workspace probe announces the members under its own name.
		Metadata: map[string]json.RawMessage{cloud.Name: json.RawMessage(`{"releaseSet":{"ecosystems":[` + strings.Join(members, ",") + `]}}`)},
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{site})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	return siteContentPlan{ws: ws, site: site, cloud: cloud, extensions: extensions}
}

func planSiteContent(t *testing.T, fixture siteContentPlan, command string) map[string]*jobmodel.ScheduledJob {
	t.Helper()
	planned, err := Plan(fixture.ws, []string{command}, []*workspace.Project{fixture.site}, fixture.extensions, nil, nil, nil, nil)
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

// plannedTask returns the job of planned that runs task of the extension
// named extensionName under command, or nil. Its key names the extension
// only when more than one extension contributes to command, so the job is
// found by what it runs.
func plannedTask(planned map[string]*jobmodel.ScheduledJob, extensionName, command, task string) *jobmodel.ScheduledJob {
	for _, job := range planned {
		if job.Extension != nil && job.Extension.Name == extensionName && job.Step != nil && job.Step.Task == task &&
			job.JobDef != nil && job.CommandName() == command {
			return job
		}
	}
	return nil
}

// requireSiteContentReaders fails unless the plan of command holds the
// generate step and the reader of command, and the reader waits for the
// generate step.
func requireSiteContentReaders(t *testing.T, fixture siteContentPlan, command string, planned map[string]*jobmodel.ScheduledJob) {
	t.Helper()
	generate := plannedTask(planned, fixture.cloud.Name, "build", "cloud-site-content-generate")
	if generate == nil {
		t.Fatalf("plan of %s holds no cloud-site-content-generate step: %v", command, slices.Sorted(maps.Keys(planned)))
	}
	readers := map[string]struct{ command, task string }{
		"validate": {"validate", "cloud-validate-site-content"},
		"package":  {"package", "cloud-package-site-content"},
		"publish":  {"package", "cloud-package-site-content"},
	}
	if reader, reads := readers[command]; reads {
		job := plannedTask(planned, fixture.cloud.Name, reader.command, reader.task)
		if job == nil || !slices.Contains(job.DependsOn, generate.Key()) {
			t.Fatalf("%s %s = %+v, want it planned and depending on %s; planned %v",
				reader.command, reader.task, job, generate.Key(), slices.Sorted(maps.Keys(planned)))
		}
	}
}

// Validate, package and publish each plan the generate step, and the step of
// each that reads the generated section waits for it. A project whose only
// section is generated has no section landing page to activate package:
// site-content.json activates it, so its site-content step is planned.
func TestTheStepsThatReadAGeneratedSiteContentSectionWaitForItsGenerateStep(t *testing.T) {
	for _, shape := range siteContentGeneratedFixtures {
		t.Run(shape.name, func(t *testing.T) {
			fixture := siteContentWorkspace(t, shape)
			for _, command := range []string{"validate", "package", "publish"} {
				t.Run(command, func(t *testing.T) {
					requireSiteContentReaders(t, fixture, command, planSiteContent(t, fixture, command))
				})
			}
		})
	}

	cloud := loadShippedExtension(t, "cloud/extension", "@putnami/cloud")

	// The release-set member's package command is package, not build, though
	// build now carries injectPublishChannels: no build step is the package
	// step cloud-site-content.
	if command := extension.MemberPackageCommand([]*extension.ExtensionDescription{cloud}, cloud.Name, cloud.Name, "publish", "cloud-site-content"); command != "package" {
		t.Fatalf("the site-content member's package command = %q, want package", command)
	}
}

// A docs project with committed sections only and no site-content.json plans
// validate and package as before: their build prerequisite plans the generate
// step, which does nothing for it, so the prerequisite is never empty.
func TestAPlainSiteContentProjectPlansValidateAndPackage(t *testing.T) {
	fixture := siteContentWorkspace(t, siteContentFixture{committed: true, ignoreGen: true})
	for _, command := range []string{"validate", "package"} {
		t.Run(command, func(t *testing.T) {
			requireSiteContentReaders(t, fixture, command, planSiteContent(t, fixture, command))
		})
	}
}

// A site project that is also a Go project plans the Go build and the Cloud
// generate step side by side: the Go build-generate cedes .gen/site-content,
// so the two declared outputs do not overlap, with or without a generated
// section.
func TestAGoSiteProjectPlansTheGoBuildBesideTheSiteContentGenerateStep(t *testing.T) {
	for _, shape := range []siteContentFixture{
		{name: "without site-content.json", committed: true, ignoreGen: true, goProject: true},
		{name: "with site-content.json", committed: true, declared: true, ignoreGen: true, goProject: true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			fixture := siteContentWorkspace(t, shape)
			for _, command := range []string{"build", "validate", "package"} {
				t.Run(command, func(t *testing.T) {
					planned := planSiteContent(t, fixture, command)
					requireSiteContentReaders(t, fixture, command, planned)
					if plannedTask(planned, "@putnami/go", "build", "build-generate") == nil {
						t.Fatalf("plan of %s holds no Go build-generate step: %v", command, slices.Sorted(maps.Keys(planned)))
					}
				})
			}
		})
	}
}

// build plans no Cloud job for a project that uses no Cloud feature: a Go
// project, and a project that is neither a Go nor a site project. The Cloud
// build command activates on putnami.json, and each of its steps keeps its
// own condition.
func TestBuildPlansNoCloudJobForAProjectThatUsesNoCloudFeature(t *testing.T) {
	cloud := loadShippedExtension(t, "cloud/extension", "@putnami/cloud")
	goExtension := loadShippedExtension(t, "go/extension", "@putnami/go")
	root := t.TempDir()
	writeProjectFile(t, root, "services/api", "putnami.json", "{\"name\": \"services/api\", \"publish\": [\"go\"]}\n")
	writeProjectFile(t, root, "services/api", "go.mod", "module example.test/api\n\ngo 1.26\n")
	writeProjectFile(t, root, "services/api", "main.go", "package main\n\nfunc main() {}\n")
	writeProjectFile(t, root, "notes", "putnami.json", "{\"name\": \"notes\"}\n")
	goProject := &workspace.Project{
		ID: "/services/api", Name: "services/api", Path: "services/api", Publish: []string{"go"}, Extensions: []string{goExtension.Name},
		Config: &wsproto.ProjectConfig{Name: "services/api", Publish: []string{"go"}, Extensions: []string{goExtension.Name}},
	}
	plain := &workspace.Project{ID: "/notes", Name: "notes", Path: "notes", Config: &wsproto.ProjectConfig{Name: "notes"}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{goProject, plain})
	ws.Graph = workspace.BuildGraph(ws.Projects)

	for _, project := range []*workspace.Project{goProject, plain} {
		t.Run(project.Name, func(t *testing.T) {
			planned, err := Plan(ws, []string{"build"}, []*workspace.Project{project}, []*extension.ExtensionDescription{cloud, goExtension}, nil, nil, nil, nil)
			if err != nil {
				t.Fatalf("plan build: %v", err)
			}
			for _, job := range planned {
				if job.Extension != nil && job.Extension.Name == cloud.Name {
					t.Errorf("build of %s plans the Cloud job %s; the project uses no Cloud feature", project.Name, job.Key())
				}
			}
		})
	}
}

// The key of the validation step that reads a generated section folds the
// generate step's key: it moves with a listed generator input and not with
// the generated output, which the generate step can rewrite between the run's
// key pass and the validation step's start. That holds in a work tree that
// does not ignore .gen too, where the generated files are Git candidates.
func TestTheGeneratedSiteContentValidationKeyFoldsTheGenerateKey(t *testing.T) {
	for _, shape := range siteContentGeneratedFixtures {
		t.Run(shape.name, func(t *testing.T) {
			fixture := siteContentWorkspace(t, shape)
			planned := slices.Collect(maps.Values(planSiteContent(t, fixture, "validate")))
			validate := fixture.site.ID + ":validate~cloud-site-content"
			key := func() string {
				t.Helper()
				got := siteContentKeys(t, fixture.ws, planned)[validate]
				if got == "" {
					t.Fatalf("no key for %s", validate)
				}
				return got
			}

			base := key()
			writeProjectFile(t, fixture.ws.Root, siteContentProjectPath, ".gen/site-content/api/index.md", "# API\n")
			if got := key(); got != base {
				t.Errorf("the generated output moved the validation key; the generate step can rewrite it during the run: %s, want %s", got, base)
			}
			writeProjectFile(t, fixture.ws.Root, "tools/gen", "main.go", "package main\n\nfunc main() {}\n")
			if got := key(); got == base {
				t.Error("a change to a listed generator input left the validation key unmoved; it does not fold the generate key")
			}
		})
	}
}

// The selection fingerprint of a generated section's member is the key of its
// package step, which waits for the generate step: it moves when a file the
// project lists as a generator input or the generator command moves, and not
// when an unlisted file or the generated output itself moves. It holds for a
// project whose only section is generated, and in a work tree that does not
// ignore .gen.
func TestTheGeneratedSiteContentSelectionFingerprintFollowsTheGeneratorInputs(t *testing.T) {
	for _, shape := range siteContentGeneratedFixtures {
		t.Run(shape.name, func(t *testing.T) {
			assertSiteContentFingerprintFollowsTheGeneratorInputs(t, shape)
		})
	}
}

func assertSiteContentFingerprintFollowsTheGeneratorInputs(t *testing.T, shape siteContentFixture) {
	t.Helper()
	fixture := siteContentWorkspace(t, shape)
	ws := fixture.ws
	planned := slices.Collect(maps.Values(planSiteContent(t, fixture, "package")))
	memberKey := siteContentMemberKey
	members := map[string]*workspace.Project{memberKey: fixture.site}
	registry := putEcosystemProfiles(t)
	fingerprint := func() string {
		t.Helper()
		// A fresh manager each time: the memoized file hashes would otherwise
		// hide the content change the key is supposed to see.
		cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
		cache.SetInputResolver(noListedFiles{})
		got, err := cachekey.SelectionFingerprints(ws, planned, nil, cache, registry, fixture.extensions, members)
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

	writeProjectFile(t, ws.Root, siteContentProjectPath, "site-content.json",
		`{"generated": {"api": {"command": ["go", "run", "../../tools/gen", "--all", "{output}"]}}}`)
	if got := fingerprint(); got == input {
		t.Error("a change to the generator command left the fingerprint unmoved")
	}
}
