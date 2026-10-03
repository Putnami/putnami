package jobs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/profiles"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A deployment declaration is a put member a language extension packages: the
// workload's aggregated infra manifest, written by the package step named
// "deployment". These tests pin what the CLI does with it: the step classifies
// the member, a baseline head that carries one is a valid baseline, and the
// member's selection fingerprint moves with every committed file the
// declaration is derived from, a dependency's requirements included.

const (
	deploymentImageCoordinate       = "putnami/service"
	deploymentDeclarationCoordinate = "putnami/service-deployment"
)

// deploymentProfiles is the ecosystem registry these tests resolve against:
// npm and put owned by test extensions, plus the SDK's builtin oci. The CLI
// names no ecosystem itself, so the put owner has to be supplied.
func deploymentProfiles(t *testing.T) *extensionproto.ProfileRegistry {
	t.Helper()
	owner := func(name, id string) extensionproto.NamedManifest {
		return extensionproto.NamedManifest{Name: name, Manifest: &extensionproto.Manifest{
			Name:     name,
			Commands: map[string]extensionproto.CommandDefinition{"package": {}, "publish": {}},
			Ecosystems: []extensionproto.EcosystemProfile{{
				ID:         id,
				Coordinate: extensionproto.PatternRule{Pattern: `^[A-Za-z0-9@._/-]+$`},
				Version:    extensionproto.VersionRule{Pattern: `^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)*$`, Ordering: extensionproto.OrderingSemver},
				Channel:    extensionproto.ChannelNative,
				Registries: json.RawMessage(`{"type":"object"}`),
				Publish:    "publish",
			}},
		}}
	}
	registry, diagnostics := extensionproto.ResolveProfiles([]extensionproto.NamedManifest{
		owner("npm-owner", "npm"),
		owner("put-owner", "put"),
	}, profiles.Builtin())
	if len(diagnostics) != 0 {
		t.Fatalf("test ecosystem profiles are invalid: %v", diagnostics)
	}
	return registry
}

// deploymentWorkload declares a workload's image and its deployment
// declaration. The declaration's publish step is one no step table knows, so
// its kind can only come from its package step.
func deploymentWorkload(t *testing.T) *workspace.Workspace {
	t.Helper()
	service := &workspace.Project{
		ID: "/apps/service", Name: "service", Version: "1.1.0", Type: "application",
		Metadata: releaseSetProjectMembers(t,
			releaseset.MemberDeclaration{
				Ecosystem: "oci", Coordinate: deploymentImageCoordinate, PackageStep: "docker", PublishStep: "docker",
			},
			releaseset.MemberDeclaration{
				Ecosystem: "put", Coordinate: deploymentDeclarationCoordinate,
				PackagePublisher: "@putnami/go", PackageStep: "deployment", PublishStep: "publish-deployment",
			},
		),
	}
	return workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{service})
}

// deploymentMemberFingerprint is the selection fingerprint a member has while
// its inputs do not move: one per member, so moving one member's inputs leaves
// the other member of the same project unselected.
func deploymentMemberFingerprint(ecosystem distribution.Ecosystem, coordinate string) string {
	return testFingerprint(printableMemberKey(releaseset.MemberKey(ecosystem, coordinate)))
}

// prepareDeploymentReleaseSet prepares an attributed, impact-based publication
// of the workload against heads. Every member keeps the fingerprint
// deploymentMemberFingerprint gives it, except the ones moved names.
func prepareDeploymentReleaseSet(
	t *testing.T,
	ws *workspace.Workspace,
	heads map[string]*distribution.ChannelHead,
	moved map[string]string,
) *ReleaseSetRun {
	t.Helper()
	useReleaseSetProvenance(t, nil)
	useReleaseSetProviderFake(t, &fakeReleaseSetProvider{heads: heads})
	options := releaseSetRequest()
	options.Profiles = deploymentProfiles(t)
	options.Policy = attributionPolicy()
	options.Versions = testRunVersions(ws)
	members, err := ReleaseSetMembers(ws.Projects, options.Profiles)
	if err != nil {
		t.Fatalf("release-set members: %v", err)
	}
	fingerprints := make(map[string]string, len(members))
	for key := range members {
		ecosystem, coordinate, _ := strings.Cut(key, "\x00")
		fingerprints[key] = deploymentMemberFingerprint(distribution.Ecosystem(ecosystem), coordinate)
		if override, ok := moved[key]; ok {
			fingerprints[key] = override
		}
	}
	run, err := PrepareReleaseSet(context.Background(), options, ws, ws.Projects, providerDiscovery(), fingerprints)
	if err != nil || run == nil || run.plan == nil {
		t.Fatalf("prepare release set = %+v, %v", run, err)
	}
	return run
}

// assertPlanParses reads the plan the way every extension reads it from its job
// context.
func assertPlanParses(t *testing.T, plan *releaseset.Plan) {
	t.Helper()
	planJSON, err := json.Marshal(releaseset.ClonePlan(plan))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseset.ParseParams(protocoljob.Params{releaseset.ContextParamName: planJSON}); err != nil {
		t.Fatalf("an extension refuses the plan: %v\n%s", err, planJSON)
	}
}

// TestADeploymentMemberIsClassifiedByItsPackageStep pins the classification: a
// put member packaged by the step named "deployment" is a member of kind
// deployment, whatever its publish step is called, and the released set says
// so.
func TestADeploymentMemberIsClassifiedByItsPackageStep(t *testing.T) {
	ws := deploymentWorkload(t)
	run := prepareDeploymentReleaseSet(t, ws, emptyHead("canary"), nil)

	declaration, ok := run.plan.Member("put", deploymentDeclarationCoordinate)
	if !ok || !declaration.Selected || declaration.Project != "apps/service" || declaration.Kind != distribution.KindDeployment {
		t.Fatalf("planned declaration = %+v, %v; want it selected and attributed to apps/service as a deployment", declaration, ok)
	}
	image, ok := run.plan.Member("oci", deploymentImageCoordinate)
	if !ok || image.Kind != distribution.KindImage {
		t.Fatalf("planned image = %+v, %v; want an image", image, ok)
	}
	assertPlanParses(t, run.plan)

	// One publish job reports both members of the project.
	results := map[string]*JobResult{image.ProjectID + ":publish": {Status: "success", Events: []RawJobEvent{
		publishedMemberEvent(image, digestFor('d')),
		publishedMemberEvent(declaration, digestFor('e')),
	}}}
	final, err := reconcilePublishedReleaseSet(run.plan, results)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var released *distribution.ReleaseSetMember
	for i := range final.Members {
		if final.Members[i].Ecosystem == "put" {
			released = &final.Members[i]
		}
	}
	if released == nil || released.Kind != distribution.KindDeployment || released.ArtifactDigest != digestFor('e') {
		t.Fatalf("released declaration = %+v; want kind deployment with the published digest", released)
	}
}

// TestABaselineHeadCarryingADeploymentMemberIsAValidBaseline pins the reader
// half: a channel head whose set carries a deployment member resolves, and a
// publication that moves only the image carries the declaration over verbatim
// in a plan every extension accepts.
func TestABaselineHeadCarryingADeploymentMemberIsAValidBaseline(t *testing.T) {
	ws := deploymentWorkload(t)
	member := func(ecosystem distribution.Ecosystem, coordinate string, kind distribution.MemberKind, digest string) distribution.ReleaseSetMember {
		return distribution.ReleaseSetMember{
			Ecosystem: ecosystem, Coordinate: coordinate, Version: "1.0.0", ArtifactDigest: digest,
			Dependencies: []distribution.ReleaseSetDependency{}, SourceRevision: testRevision,
			SelectionFingerprint: deploymentMemberFingerprint(ecosystem, coordinate),
			Project:              "apps/service", Kind: kind,
		}
	}
	set := distribution.NormalizeReleaseSet(&distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Members: []distribution.ReleaseSetMember{
			member("oci", deploymentImageCoordinate, distribution.KindImage, digestFor('a')),
			member("put", deploymentDeclarationCoordinate, distribution.KindDeployment, digestFor('b')),
		},
	})
	ref, diagnostics := distribution.DeriveReleaseSetRef(set)
	if len(diagnostics) != 0 {
		t.Fatalf("a set carrying a deployment member does not validate: %v", diagnostics)
	}
	heads := map[string]*distribution.ChannelHead{"canary": {Ref: ref, Generation: 7, ReleaseSet: set}}

	imageKey := releaseset.MemberKey("oci", deploymentImageCoordinate)
	run := prepareDeploymentReleaseSet(t, ws, heads, map[string]string{imageKey: digestFor('9')})

	declaration, ok := run.plan.Member("put", deploymentDeclarationCoordinate)
	if !ok || declaration.Selected || declaration.Kind != distribution.KindDeployment ||
		declaration.ArtifactDigest != digestFor('b') || declaration.Version != "1.0.0" {
		t.Fatalf("carried-over declaration = %+v, %v; want the head's record, unselected, of kind deployment", declaration, ok)
	}
	image, ok := run.plan.Member("oci", deploymentImageCoordinate)
	if !ok || !image.Selected {
		t.Fatalf("image = %+v, %v; want it selected", image, ok)
	}
	assertPlanParses(t, run.plan)

	final, err := reconcilePublishedReleaseSet(run.plan, publishedResults(run.plan, digestFor('e')))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, released := range final.Members {
		if released.Ecosystem == "put" && (released.Kind != distribution.KindDeployment || released.ArtifactDigest != digestFor('b')) {
			t.Fatalf("released declaration = %+v; want the head's record of kind deployment", released)
		}
	}
}

// TestTheDeploymentSelectionFingerprintFollowsTheDeclarationInputs plans the
// shipped Go extension's package command for a workload that depends on a
// library, and keys the put member its deployment step packages.
//
// The fingerprint is that step's task key, so it must move with every
// committed file the declaration is derived from: a library's
// infra/requirements.json, which no per-project key of the workload covers,
// and the workload's own infra/runtime.json and infra/overrides.json. A file
// the declaration does not read leaves it alone. Without the deployment
// channel the step is not planned and the member cannot be keyed, which is
// the loud failure a publisher that forgot to enable it gets.
func TestTheDeploymentSelectionFingerprintFollowsTheDeclarationInputs(t *testing.T) {
	repoRoot := findJobsRepoRoot(t)
	goExtension := extension.LoadExtensionFromDir(filepath.Join(repoRoot, "go", "extension"), "/go/extension")
	if goExtension == nil {
		t.Fatal("go/extension/putnami.extension.json did not load")
	}
	// The implementation digest of a local extension is not under test here.
	goExtension.LocalSource = false

	root := t.TempDir()
	writeProjectFile(t, root, "lib", "go.mod", "module example.com/lib\n")
	writeProjectFile(t, root, "lib", "infra/requirements.json", `{"protocolVersion":2}`)
	writeProjectFile(t, root, "app", "go.mod", "module example.com/app\n")
	lib := &workspace.Project{
		ID: "/lib", Name: "lib", Path: "lib", Version: "1.0.0", Type: "library",
		Extensions: []string{goExtension.Name},
	}
	app := &workspace.Project{
		ID: "/app", Name: "app", Path: "app", Version: "1.0.0", Type: "application",
		Dependencies: []string{"lib"}, Extensions: []string{goExtension.Name},
		Metadata: releaseSetProjectMembers(t, releaseset.MemberDeclaration{
			Ecosystem: "put", Coordinate: "putnami/app-deployment",
			PackagePublisher: goExtension.Name, PackageStep: "deployment", PublishStep: "publish-deployment",
		}),
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{lib, app})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	memberKey := releaseset.MemberKey("put", "putnami/app-deployment")
	members := map[string]*workspace.Project{memberKey: app}

	plan := func(params extension.ParamMap) []*ScheduledJob {
		t.Helper()
		planned, err := Plan(ws, []string{"package"}, []*workspace.Project{app},
			[]*extension.ExtensionDescription{goExtension}, params, nil, nil)
		if err != nil {
			t.Fatalf("plan package: %v", err)
		}
		return planned
	}
	params := extension.ParamMap{"deployment": true}
	planned := plan(params)
	var step *ScheduledJob
	for _, job := range planned {
		if job.Project != nil && job.Project.ID == app.ID && job.StepID() == "deployment" {
			step = job
		}
	}
	if step == nil || step.Step == nil || step.Step.Task != "package-deployment" {
		t.Fatalf("package plan with the deployment channel has no deployment step for the workload: %+v", step)
	}

	fingerprint := func() string {
		t.Helper()
		// A fresh manager each time: the memoized file hashes would otherwise
		// hide the content change the key is supposed to see.
		cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
		got, err := SelectionFingerprints(ws, planned, params, cache, deploymentProfiles(t), members)
		if err != nil {
			t.Fatalf("SelectionFingerprints: %v", err)
		}
		if got[memberKey] == "" {
			t.Fatalf("no fingerprint for the deployment member in %v", got)
		}
		return got[memberKey]
	}

	base := fingerprint()
	if again := fingerprint(); again != base {
		t.Fatalf("the deployment fingerprint is not deterministic: %s then %s", base, again)
	}

	writeProjectFile(t, root, "lib", "notes.txt", "not an input\n")
	if got := fingerprint(); got != base {
		t.Errorf("a file the declaration does not read moved its fingerprint: %s, want %s", got, base)
	}

	writeProjectFile(t, root, "lib", "infra/requirements.json",
		`{"protocolVersion":2,"databases":[{"name":"reports","engine":"postgres"}]}`)
	library := fingerprint()
	if library == base {
		t.Error("a requirements-only change in a dependency left the deployment fingerprint unmoved; " +
			"the workload would keep a declaration without the library's database")
	}

	writeProjectFile(t, root, "app", "infra/runtime.json", `{"scaling":{"max":2}}`)
	runtime := fingerprint()
	if runtime == library {
		t.Error("the workload's infra/runtime.json did not move the deployment fingerprint")
	}
	writeProjectFile(t, root, "app", "infra/overrides.json", `{"protocolVersion":2}`)
	if got := fingerprint(); got == runtime {
		t.Error("the workload's infra/overrides.json did not move the deployment fingerprint")
	}

	// Without the channel nothing is planned, and the member says so.
	unplanned := plan(nil)
	for _, job := range unplanned {
		if job.Project != nil && job.Project.ID == app.ID && job.StepID() == "deployment" {
			t.Fatalf("the deployment step was planned without the deployment channel: %s", job.Key())
		}
	}
	cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	_, err := SelectionFingerprints(ws, unplanned, nil, cache, deploymentProfiles(t), members)
	if err == nil || !strings.Contains(err.Error(), `package step "deployment"`) {
		t.Fatalf("keying the member without its package step = %v, want a refusal naming the step", err)
	}
}
