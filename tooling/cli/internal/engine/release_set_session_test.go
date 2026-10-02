package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A session that names the gate beside a channel publish. These tests
// drive Engine.Run over a real fixture workspace: a real extension runtime
// answering the workspace probe, a real coordinator, the real planner, and the
// real release-set scoping. The ONE double is the release-set provider itself,
// which is an external boundary — this test binary answers `resolve` with an
// empty channel, the state a pull-request channel is in on its first push.

// releaseSetFixtureProviderEnv routes a child of this test binary to the
// release-set provider role. The coordinator resolves its provider by running
// os.Executable() with the reserved argv, so the child IS this binary.
const releaseSetFixtureProviderEnv = "PUTNAMI_ENGINE_RELEASE_SET_FIXTURE"

// releaseSetFixtureHeadEnv names a file holding the head the fixture provider
// answers for every channel. Unset, every channel is empty.
const releaseSetFixtureHeadEnv = "PUTNAMI_ENGINE_RELEASE_SET_FIXTURE_HEAD"

// releaseSetFixtureReleasesEnv names the file the fixture provider records a
// release it answered in.
const releaseSetFixtureReleasesEnv = "PUTNAMI_ENGINE_RELEASE_SET_FIXTURE_RELEASES"

// releaseSetFixtureCallsEnv names the file the fixture provider appends one
// line to for every call it receives, before it answers.
const releaseSetFixtureCallsEnv = "PUTNAMI_ENGINE_RELEASE_SET_FIXTURE_CALLS"

// serveReleaseSetFixtureProvider answers one provider call. A `resolve` gets a
// null head for every requested channel, the empty-channel answer that selects
// every member, or the head the releaseSetFixtureHeadEnv file holds. A
// `release` goes to releaseFixtureHead. It validates the request the way a
// real provider must, so a malformed exchange fails here rather than silently
// producing an empty plan.
func serveReleaseSetFixtureProvider(args []string) int {
	if err := recordReleaseSetFixtureCall(args); err != nil {
		fmt.Fprintln(os.Stderr, "release-set fixture calls:", err)
		return 2
	}
	requestPath := ""
	for index, arg := range args {
		if arg == distribution.RequestFileFlag && index+1 < len(args) {
			requestPath = args[index+1]
		}
	}
	if requestPath == "" {
		fmt.Fprintln(os.Stderr, "release-set fixture: no request file")
		return 2
	}
	data, err := os.ReadFile(requestPath) //nolint:gosec // the path is the coordinator's own temp file
	if err != nil {
		fmt.Fprintln(os.Stderr, "release-set fixture:", err)
		return 2
	}
	var head *distribution.ChannelHead
	if path := os.Getenv(releaseSetFixtureHeadEnv); path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // the path is the test's own temp file
		if err == nil {
			err = json.Unmarshal(data, &head)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "release-set fixture head:", err)
			return 2
		}
	}
	var response any
	if slices.Contains(args, distribution.ReleaseCommand) {
		response, err = releaseFixtureHead(data, head)
	} else {
		response, err = resolveFixtureHead(data, head)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release-set fixture:", err)
		return 2
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		fmt.Fprintln(os.Stderr, "release-set fixture:", err)
		return 2
	}
	fmt.Println(string(encoded))
	return 0
}

// recordReleaseSetFixtureCall appends the call's command, release or resolve,
// to the file releaseSetFixtureCallsEnv names, when it names one.
func recordReleaseSetFixtureCall(args []string) error {
	path := os.Getenv(releaseSetFixtureCallsEnv)
	if path == "" {
		return nil
	}
	call := "resolve"
	if slices.Contains(args, distribution.ReleaseCommand) {
		call = "release"
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the path is the test's own temp file
	if err != nil {
		return err
	}
	if _, err := file.WriteString(call + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// resolveFixtureHead answers every requested channel with head.
func resolveFixtureHead(data []byte, head *distribution.ChannelHead) (*distribution.ResolveResponse, error) {
	var request distribution.ResolveRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return nil, err
	}
	response := &distribution.ResolveResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Heads:           map[string]*distribution.ChannelHead{},
	}
	for _, channel := range request.Channels {
		response.Heads[channel] = head
	}
	return response, nil
}

// releaseFixtureHead answers a release that re-releases head unchanged with
// already-current, the provider's answer to a head confirmation, and records
// the call in the file releaseSetFixtureReleasesEnv names. Any other release
// is refused: the fixture stores nothing.
func releaseFixtureHead(data []byte, head *distribution.ChannelHead) (*distribution.ReleaseResponse, error) {
	var request distribution.ReleaseRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return nil, err
	}
	if head == nil {
		return nil, fmt.Errorf("release without a fixture head")
	}
	ref, diagnostics := distribution.DeriveReleaseSetRef(&request.ReleaseSet)
	if len(diagnostics) != 0 || ref != head.Ref {
		return nil, fmt.Errorf("release of %+v, want the unchanged head %+v", ref, head.Ref)
	}
	response := &distribution.ReleaseResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Outcome:         distribution.ReleaseOutcomeAlreadyCurrent,
		Current:         map[string]*distribution.ChannelHead{},
	}
	for _, channel := range request.Channels {
		response.Current[channel.Name] = &distribution.ChannelHead{Ref: head.Ref, Generation: head.Generation}
	}
	if path := os.Getenv(releaseSetFixtureReleasesEnv); path != "" {
		if err := os.WriteFile(path, []byte(ref.ID+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	return response, nil
}

// releaseSetSessionFixture is one workspace with the three shapes the issue
// distinguishes: a project that owns a release-set member, a library that owns
// none and changed, and a third project that changed nothing.
func releaseSetSessionFixture(t *testing.T) (string, Request) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell provider fixture")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	root := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(wsproto.WorkspaceConfigFilename,
		`{"name":"release-session","includes":["app","lib","other","provider"]}`, 0o644)
	write("app/putnami.json", `{"name":"@fixture/app","version":"1.0.0","extensions":["/provider"]}`, 0o644)
	write("app/marker.txt", "app\n", 0o644)
	write("app/source.txt", "app\n", 0o644)
	write("lib/putnami.json", `{"name":"@fixture/lib","version":"1.0.0","extensions":["/provider"]}`, 0o644)
	write("lib/marker.txt", "lib\n", 0o644)
	write("lib/source.txt", "lib\n", 0o644)
	write("other/putnami.json", `{"name":"@fixture/other","version":"1.0.0","extensions":["/provider"]}`, 0o644)
	write("other/marker.txt", "other\n", 0o644)
	write("other/source.txt", "other\n", 0o644)
	write("provider/putnami.json", `{"name":"@fixture/provider"}`, 0o644)
	write("provider/putnami.extension.json", releaseSetFixtureManifest, 0o644)

	// The member declaration the app's own provider reports about it, exactly
	// as an extension probe does: one member per (ecosystem, coordinate), with
	// the package and publish steps that emit it.
	probe := fmt.Sprintf(`{"version":%d,"extension":"@fixture/provider","projects":[`+
		`{"path":"app","metadata":{"releaseSet":{"ecosystems":[`+
		`{"ecosystem":"fixture","coordinate":"fixture-app","packageStep":"artifact","publishStep":"artifact"}]}}},`+
		`{"path":"lib"},{"path":"other"}]}`, wsproto.ProbeProtocolVersion)
	write("provider/runtime", fmt.Sprintf(`#!/bin/sh
if [ "$1" = "__putnami" ] && [ "$2" = "runtime-info" ]; then
  printf '%%s\n' '{"extension":"@fixture/provider","version":"1.0.0","platform":"%s/%s","cliContract":4,"runtimeProtocol":2,"runtimeABI":1}'
  exit 0
fi
if [ "$1" = "__putnami" ] && [ "$2" = "workspace-probe" ]; then
  printf '%%s\n' '%s'
  exit 0
fi
exit 2
`, runtime.GOOS, runtime.GOARCH, probe), 0o755)

	// initCLISelectionGitRepo commits the whole tree, so the fixture's baseline
	// is a clean checkout and the only diff is what a test then touches.
	initCLISelectionGitRepo(t, root)
	runCLISelectionGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runCLISelectionGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runCLISelectionGit(t, root, "checkout", "-b", "feature")

	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	t.Setenv(releaseSetFixtureProviderEnv, "1")

	return root, Request{
		WorkspaceRoot: root,
		Config:        wsproto.Load(root),
		Commands:      []string{"lint", "test", "build", "validate", "publish"},
		CommandParams: map[string]any{"channel": "pr-0"},
		Global:        GlobalFlags{Impacted: true, Projects: impactedProjectsSentinel, Plan: true, NoCache: true},
		Stdout:        io.Discard,
	}
}

// releaseSetFixtureManifest declares one provider that both verifies and
// publishes: the shape a mixed session has in a real workspace, where the
// extension serving `test` is also the extension serving `publish`.
const releaseSetFixtureManifest = `{
  "name": "@fixture/provider",
  "version": "1.0.0",
  "cliContract": 4,
  "runtime": {"executable": "runtime"},
  "workspace": {"markers": ["marker.txt"], "inputs": ["marker.txt"]},
  "ecosystems": [
    {
      "id": "fixture",
      "coordinate": {"pattern": "^[a-z0-9@._/-]+$"},
      "version": {"pattern": "^[0-9A-Za-z][0-9A-Za-z.+-]*$", "ordering": "semver"},
      "channel": "native",
      "registries": {"type": "object"},
      "publish": "publish"
    }
  ],
  "commands": {
    "lint": {"run": [{"id": "check", "task": "noop"}]},
    "test": {"run": [{"id": "test", "task": "noop"}]},
    "build": {"run": [{"id": "compile", "task": "noop"}]},
    "validate": {"run": [{"id": "rules", "task": "noop"}]},
    "package": {"run": [{"id": "artifact", "task": "noop"}]},
    "publish": {"dependsOn": ["package"], "run": [{"id": "artifact", "task": "noop"}]},
    "cloud-release-set": {"visibility": "internal", "run": [{"id": "provider", "task": "noop"}]}
  },
  "tasks": {
    "noop": {"kind": "command", "command": "/bin/sh", "args": ["-c", "exit 0"], "cwd": "{workspaceRoot}", "cache": false}
  }
}`

// touchFixtureProject makes one project the git-impacted set of the run.
func touchFixtureProject(t *testing.T, root, project string) {
	t.Helper()
	path := filepath.Join(root, project, "source.txt")
	if err := os.WriteFile(path, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func planKeys(t *testing.T, planned []*jobs.ScheduledJob) []string {
	t.Helper()
	keys := make([]string, 0, len(planned))
	for _, job := range planned {
		if job != nil {
			keys = append(keys, job.Key())
		}
	}
	slices.Sort(keys)
	return keys
}

func assertPlanned(t *testing.T, keys []string, want ...string) {
	t.Helper()
	for _, key := range want {
		if !slices.Contains(keys, key) {
			t.Errorf("plan does not contain %s\nplanned: %s", key, strings.Join(keys, " "))
		}
	}
}

func assertNotPlanned(t *testing.T, keys []string, unwanted ...string) {
	t.Helper()
	for _, key := range unwanted {
		if slices.Contains(keys, key) {
			t.Errorf("plan contains %s and must not\nplanned: %s", key, strings.Join(keys, " "))
		}
	}
}

// The regression itself: a library that owns no release-set member had its
// lint, test and validate removed from every session that also published, and
// a changed library's tests therefore never ran on a publishing CI run.
func TestRun_MixedGateAndChannelPublishPlansLibraryTests(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "selection-before-plan", "a-channel-publish-narrows-itself-alone")
	root, req := releaseSetSessionFixture(t)
	touchFixtureProject(t, root, "lib")

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("mixed session = exit %d, err %v", result.ExitCode, err)
	}
	keys := planKeys(t, result.Plan)
	assertPlanned(t, keys,
		"/lib:test~test", "/lib:lint~check", "/lib:build~compile", "/lib:validate~rules",
		"/app:package~artifact", "/app:publish~artifact")
	// The publication stays exactly the coordinator's: the member owner
	// publishes, and the verified library does not — the retained publisher
	// applies to it, but its publication is no part of this release.
	assertNotPlanned(t, keys, "/lib:publish~artifact")
}

// The gate half is the git-impacted set, not the whole workspace: the
// coordinator's `*` exists to key every member, and it must not widen the
// verification either.
func TestRun_MixedGateAndChannelPublishUnderImpactedKeepsTheGitImpactedSet(t *testing.T) {
	root, req := releaseSetSessionFixture(t)
	touchFixtureProject(t, root, "lib")

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("mixed session = exit %d, err %v", result.ExitCode, err)
	}
	keys := planKeys(t, result.Plan)
	assertNotPlanned(t, keys,
		"/other:test~test", "/other:lint~check", "/other:publish~artifact", "/other:package~artifact")
	assertPlanned(t, keys, "/lib:test~test", "/app:publish~artifact")
}

// Both halves are reported, and each keeps its own provenance: the mode and the
// baseline describe how the verification set was chosen, and the release-set
// member names what the channel head decided.
func TestRun_MixedSessionSelectionReportsBothSets(t *testing.T) {
	root, req := releaseSetSessionFixture(t)
	touchFixtureProject(t, root, "lib")

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("mixed session = exit %d, err %v", result.ExitCode, err)
	}
	selection := plannedSelection(t, result.Plan)
	if selection.Mode != protocoljob.SelectionModeImpacted || !selection.Scoped {
		t.Fatalf("selection = %+v, want the caller's impacted projection", selection)
	}
	if selection.Baseline == "" || selection.BaselineSource == "" ||
		selection.BaselineSource == jobs.ReleaseSetHeadBaselineSource {
		t.Fatalf("selection baseline = %q/%q, want the git tier the gate resolved",
			selection.Baseline, selection.BaselineSource)
	}
	if got, want := selection.ProjectIDs, []string{"/app", "/lib"}; !slices.Equal(got, want) {
		t.Fatalf("selection.projects = %v, want the union %v", got, want)
	}
	if got, want := selection.ReleaseSetProjects, []string{"/app"}; !slices.Equal(got, want) {
		t.Fatalf("selection.releaseSetProjects = %v, want the member owner %v", got, want)
	}
}

// A publish-only session is unchanged: the coordinator decides everything, the
// head is the baseline, and both halves of the selection are one list.
func TestRun_PublishOnlyChannelSessionIsUnchanged(t *testing.T) {
	root, req := releaseSetSessionFixture(t)
	touchFixtureProject(t, root, "lib")
	req.Commands = []string{"publish"}

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("publish-only session = exit %d, err %v", result.ExitCode, err)
	}
	keys := planKeys(t, result.Plan)
	assertPlanned(t, keys, "/app:package~artifact", "/app:publish~artifact")
	assertNotPlanned(t, keys, "/lib:test~test", "/lib:publish~artifact", "/lib:package~artifact")

	selection := plannedSelection(t, result.Plan)
	if selection.BaselineSource != jobs.ReleaseSetHeadBaselineSource {
		t.Fatalf("publish-only baselineSource = %q, want the head-measured tier", selection.BaselineSource)
	}
	if !slices.Equal(selection.ProjectIDs, selection.ReleaseSetProjects) {
		t.Fatalf("publish-only selection projects %v, release-set projects %v; want one list",
			selection.ProjectIDs, selection.ReleaseSetProjects)
	}
}

// A gate that selected nothing beside a publish that changes no member plans
// no job at all: the finalizer confirms the head, and the session is not
// widened to the whole workspace to avoid the empty-plan refusal.
func TestRun_EmptyGateAndUnchangedReleaseSetPlansNothing(t *testing.T) {
	root, req := releaseSetSessionFixture(t)
	useUnchangedReleaseSetHead(t, root, req)

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("empty gate with an unchanged release set = exit %d, err %v", result.ExitCode, err)
	}
	if len(result.Plan) != 0 || len(result.Projects) != 0 {
		t.Fatalf("planned %d job(s) over %d project(s), want none: %s",
			len(result.Plan), len(result.Projects), strings.Join(planKeys(t, result.Plan), " "))
	}
}

// Executed, the same session runs no job and still confirms the head: the
// finalizer releases the unchanged set and the provider answers
// already-current.
func TestRun_EmptyGateAndUnchangedReleaseSetConfirmsTheHead(t *testing.T) {
	root, req := releaseSetSessionFixture(t)
	useUnchangedReleaseSetHead(t, root, req)
	releases := filepath.Join(t.TempDir(), "releases")
	t.Setenv(releaseSetFixtureReleasesEnv, releases)
	req.Global.Plan = false

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("empty gate with an unchanged release set = exit %d, err %v", result.ExitCode, err)
	}
	if len(result.Plan) != 0 {
		t.Fatalf("planned %s, want no job", strings.Join(planKeys(t, result.Plan), " "))
	}
	recorded, err := os.ReadFile(releases) //nolint:gosec // the test's own temp file
	if err != nil || len(strings.TrimSpace(string(recorded))) == 0 {
		t.Fatalf("the provider received no release confirming the head: %v", err)
	}
}

// A publish-only session over an unchanged release set plans no job either,
// and confirms the head instead of being refused as a selection that matched
// nothing.
func TestRun_PublishOnlyUnchangedReleaseSetConfirmsTheHead(t *testing.T) {
	root, req := releaseSetSessionFixture(t)
	req.Commands = []string{"publish"}
	useUnchangedReleaseSetHead(t, root, req)
	releases := filepath.Join(t.TempDir(), "releases")
	t.Setenv(releaseSetFixtureReleasesEnv, releases)
	req.Global.Plan = false

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("publish-only session with an unchanged release set = exit %d, err %v", result.ExitCode, err)
	}
	if len(result.Plan) != 0 {
		t.Fatalf("planned %s, want no job", strings.Join(planKeys(t, result.Plan), " "))
	}
	recorded, err := os.ReadFile(releases) //nolint:gosec // the test's own temp file
	if err != nil || len(strings.TrimSpace(string(recorded))) == 0 {
		t.Fatalf("the provider received no release confirming the head: %v", err)
	}
}

// A gate that did select a project keeps verifying exactly that project when
// the release set is unchanged.
func TestRun_ImpactedGateAndUnchangedReleaseSetVerifiesTheGateSelection(t *testing.T) {
	root, req := releaseSetSessionFixture(t)
	useUnchangedReleaseSetHead(t, root, req)
	touchFixtureProject(t, root, "lib")

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("impacted gate with an unchanged release set = exit %d, err %v", result.ExitCode, err)
	}
	if got, want := projectIDs(result.Projects), []string{"/lib"}; !slices.Equal(got, want) {
		t.Fatalf("planned projects = %v, want the gate's selection %v", got, want)
	}
	keys := planKeys(t, result.Plan)
	assertPlanned(t, keys, "/lib:test~test", "/lib:lint~check", "/lib:build~compile", "/lib:validate~rules")
	assertNotPlanned(t, keys, "/app:publish~artifact", "/app:package~artifact", "/other:test~test")
}

// useUnchangedReleaseSetHead makes the fixture provider answer a head that
// records every member at its current selection fingerprint, so the release
// set changes no member. The fingerprints are the coordinator's own: the head
// is read from a plan prepared exactly as a session prepares it, over an empty
// channel.
func useUnchangedReleaseSetHead(t *testing.T, root string, req Request) {
	t.Helper()
	ctx := context.Background()
	probe := req
	probe.Commands = []string{"publish"}
	ws, discovered, code := loadWorkspaceAndExtensions(&probe)
	if code != ExitSuccess {
		t.Fatalf("load fixture workspace = exit %d", code)
	}
	if code := synchronizeWorkspaceProbe(ctx, &probe, ws, discovered); code != ExitSuccess {
		t.Fatalf("probe fixture workspace = exit %d", code)
	}
	options, verification, _, code := resolveReleaseSetSelections(&probe, ws, discovered.Extensions...)
	if code != ExitSuccess {
		t.Fatalf("resolve release-set selections = exit %d", code)
	}
	selected, code := selectProjects(&probe, ws, discovered.Extensions...)
	if code != ExitSuccess {
		t.Fatalf("select fixture projects = exit %d", code)
	}
	extensions := probe.planExtensions(discovered)
	keying, code := buildPlan(&probe, ws, selected, extensions, discovered)
	if code != ExitSuccess {
		t.Fatalf("build keying plan = exit %d", code)
	}
	preparation, err := jobs.PrepareReleaseSetRun(ctx, options, ws, selected, verification, extensions, discovered,
		keying, probe.CommandParams, jobs.NewRunCacheManager(root, ""), probe.selection, false)
	if err != nil || preparation.Run == nil || preparation.Run.Plan() == nil {
		t.Fatalf("prepare fixture release set = %+v, %v", preparation, err)
	}
	plan := preparation.Run.Plan()
	set := distribution.ReleaseSet{ProtocolVersion: distribution.ProtocolVersion, Namespace: plan.Namespace}
	for _, member := range plan.Members {
		set.Members = append(set.Members, distribution.ReleaseSetMember{
			Ecosystem: member.Ecosystem, Coordinate: member.Coordinate, Version: member.Version,
			ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
			Dependencies:   append([]distribution.ReleaseSetDependency{}, member.Dependencies...),
			SourceRevision: member.SourceRevision, SelectionFingerprint: member.SelectionFingerprint,
			Project: member.Project, Kind: member.Kind,
		})
	}
	head := &distribution.ChannelHead{Generation: 1, ReleaseSet: distribution.NormalizeReleaseSet(&set)}
	ref, diagnostics := distribution.DeriveReleaseSetRef(head.ReleaseSet)
	if len(diagnostics) != 0 {
		t.Fatalf("fixture head is invalid: %v", diagnostics)
	}
	head.Ref = ref
	encoded, err := json.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "head.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(releaseSetFixtureHeadEnv, path)
	workspace.InvalidateLoadCache(root)
}

// plannedSelection reads the resolved selection the run stamped on every node,
// which is the same block a task receives on the wire and the session records.
func plannedSelection(t *testing.T, planned []*jobs.ScheduledJob) *protocoljob.Selection {
	t.Helper()
	if len(planned) == 0 {
		t.Fatal("the run planned nothing")
	}
	selection := planned[0].Selection
	if selection == nil {
		t.Fatal("the run stamped no selection on its plan")
	}
	for _, job := range planned {
		if job.Selection != selection {
			t.Fatalf("%s carries a different selection block", job.Key())
		}
	}
	return selection
}
