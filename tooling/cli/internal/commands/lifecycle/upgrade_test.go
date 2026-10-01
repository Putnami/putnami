package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

type upgradeReleaseSetResolverFunc func(context.Context, *distribution.ResolveRequest) (*distribution.ResolveResponse, error)

func (fn upgradeReleaseSetResolverFunc) Resolve(ctx context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
	return fn(ctx, request)
}

func upgradeReleaseSetResponse(t *testing.T, namespace, npmVersion, goVersion string) *distribution.ResolveResponse {
	t.Helper()
	releaseSet := distribution.NormalizeReleaseSet(&distribution.ReleaseSet{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       namespace,
		Members: []distribution.ReleaseSetMember{
			{
				Ecosystem:            "npm",
				Coordinate:           "@putnami/runtime",
				Version:              npmVersion,
				ArtifactDigest:       "sha256:" + strings.Repeat("a", 64),
				Dependencies:         []distribution.ReleaseSetDependency{},
				SourceRevision:       strings.Repeat("1", 40),
				SelectionFingerprint: "sha256:" + strings.Repeat("c", 64),
			},
			{
				Ecosystem:            "go",
				Coordinate:           "go.putnami.dev/http",
				Version:              goVersion,
				ArtifactDigest:       "sha256:" + strings.Repeat("b", 64),
				Dependencies:         []distribution.ReleaseSetDependency{},
				SourceRevision:       strings.Repeat("1", 40),
				SelectionFingerprint: "sha256:" + strings.Repeat("d", 64),
			},
		},
	})
	ref, diagnostics := distribution.DeriveReleaseSetRef(releaseSet)
	if len(diagnostics) != 0 {
		t.Fatalf("derive release set ref: %v", diagnostics)
	}
	// --release resolves an IMMUTABLE set, which is named by no channel: the
	// answer is a release, not a channel head, and its generation is 0.
	return &distribution.ResolveResponse{
		ProtocolVersion: distribution.ProtocolVersion,
		Release:         &distribution.ChannelHead{Ref: ref, Generation: 0, ReleaseSet: releaseSet},
	}
}

func replaceUpgradeReleaseSetResolver(t *testing.T, resolver upgradeReleaseSetResolver) {
	t.Helper()
	original := newUpgradeReleaseSetResolver
	originalProviderValidation := validateUpgradeReleaseSetProvider
	newUpgradeReleaseSetResolver = func() (upgradeReleaseSetResolver, error) { return resolver, nil }
	validateUpgradeReleaseSetProvider = func(string, *wsproto.Config) error { return nil }
	t.Cleanup(func() {
		newUpgradeReleaseSetResolver = original
		validateUpgradeReleaseSetProvider = originalProviderValidation
	})
}

// --branch derived a channel name from a git branch, which distribution v2
// deletes: a channel is a published intent, not a spelling of a ref. The flag is
// gone from the parser, from the catalog, and from the selector, so a user who
// still types it gets a usage error instead of a silent fallback to stable.
func TestBranchSelectorIsGone(t *testing.T) {
	spectest.Proves(t, "cli/channels", "archives-follow-the-put-projection", "branch-selector-is-gone")

	if reflect.ValueOf(UpgradeFlags{}).FieldByName("Branch").IsValid() {
		t.Error("UpgradeFlags still carries a Branch field")
	}

	upgrade, ok := commandmeta.Lookup("upgrade")
	if !ok {
		t.Fatal("the catalog no longer describes upgrade")
	}
	for _, flag := range upgrade.Flags {
		if flag.Long == "--branch" {
			t.Error("the upgrade catalog row still declares --branch")
		}
	}
	if strings.Contains(upgrade.Usage, "--branch") {
		t.Errorf("the upgrade usage line still shows --branch: %s", upgrade.Usage)
	}
	for _, example := range upgrade.Examples {
		if strings.Contains(example, "--branch") {
			t.Errorf("the upgrade examples still show --branch: %s", example)
		}
	}

	// The three surviving selectors keep their mutual exclusivity, and the error
	// names only them.
	_, err := resolveUpgradeSelector(UpgradeFlags{Channel: "canary", Version: "1.2.3"})
	if err == nil || strings.Contains(err.Error(), "--branch") {
		t.Errorf("selector error = %v, want mutual exclusivity without --branch", err)
	}
}

func TestUpgrade_FromSourceRejectsIncompatibleFlags(t *testing.T) {
	for _, flags := range []UpgradeFlags{
		{FromSource: true, Channel: "canary"},
		{FromSource: true, Version: "1.2.3"},
		{FromSource: true, Release: "rs_" + strings.Repeat("a", 64), Namespace: "putnami"},
		{FromSource: true, Extensions: true},
		{FromSource: true, Deps: true},
	} {
		err := Upgrade(context.Background(), t.TempDir(), &wsproto.Config{}, "1.0.0", t.TempDir(), flags, LifecycleEnv{})
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Errorf("Upgrade(%+v) err = %v, want ErrUsage", flags, err)
		}
	}
}

func writeUpgradeWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sharedtest.WriteJSONConfig(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), map[string]any{
		"name": "upgrade-ws",
	})
	return dir
}

// upgradeTestEnv answers the deps-upgrade pass with a stub instead of the engine:
// these tests exercise `putnami upgrade`'s phase sequencing, not the run.
func upgradeTestEnv(outcome WorkspaceJobOutcome) LifecycleEnv {
	return LifecycleEnv{RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		return WorkspaceJobResult{Outcome: outcome}, nil
	}}
}

// The deps-upgrade params are part of the job cache key, so their Go types and
// membership must survive the trip to the engine adapter untouched.
func TestUpgradeDeps_ForwardsParamsWithTheirTypes(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)

	var got WorkspaceJobRequest
	env := LifecycleEnv{RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
		got = req
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
	if _, err := upgradeDeps(context.Background(), dir, cfg,
		upgradeSelector{Display: "stable", RegistryTarget: "latest", DepsTarget: "1.2.3"}, true, false, env); err != nil {
		t.Fatalf("upgradeDeps: %v", err)
	}

	if got.Job != "deps-upgrade" {
		t.Errorf("job = %q, want deps-upgrade", got.Job)
	}
	want := map[string]any{
		"putnami-selector": "stable",
		"putnami-channel":  "latest",
		"putnami-version":  "1.2.3",
		"dry-run":          true,
	}
	if len(got.Params) != len(want) {
		t.Fatalf("params = %#v, want %#v", got.Params, want)
	}
	for k, v := range want {
		gotV, ok := got.Params[k]
		if !ok {
			t.Fatalf("params missing %q: %#v", k, got.Params)
		}
		if gotV != v {
			t.Errorf("params[%q] = %#v (%T), want %#v (%T)", k, gotV, gotV, v, v)
		}
	}
}

// A channel is a native registry tag on each ecosystem, so upgrade must not
// consult the release-set provider for one: doing that needed a namespace, and
// upgrade filled it with the workspace name — which only the publisher's own
// workspace could ever get right.
// One ecosystem that cannot resolve the selector must not let the other rewrite
// its dependency metadata, so the run stops at the first failure unless the
// operator asked for the opposite.
// Each ecosystem resolves its own registry from the workspace `registries`
// entry, so the dependency job receives it verbatim. A workspace that declares
// none sends no key at all rather than an empty map, which would look like a
// deliberate "no registries" answer to an extension.
func TestUpgradeDeps_ForwardsTheWorkspaceRegistries(t *testing.T) {
	spectest.Proves(t, "cli/channels", "archives-follow-the-put-projection", "archive-download-sends-the-credential")

	// Written from the typed config rather than an untyped literal: the shape
	// under test is `registries`, whose entries are raw by contract.
	dir := t.TempDir()
	declared, err := json.Marshal(&wsproto.Config{
		Name: "upgrade-ws",
		Registries: map[string]json.RawMessage{
			"put": json.RawMessage(`{"registry":"https://put.example"}`),
			"npm": json.RawMessage(`{"publish":"https://npm.example"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), declared, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := wsproto.Load(dir)

	var got WorkspaceJobRequest
	env := LifecycleEnv{RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
		got = req
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
	selector := upgradeSelector{Display: "canary", RegistryTarget: "canary", DepsTarget: "canary"}
	if _, err := upgradeDeps(context.Background(), dir, cfg, selector, true, false, env); err != nil {
		t.Fatalf("upgradeDeps: %v", err)
	}

	registries, ok := got.Params["registries"].(map[string]json.RawMessage)
	if !ok {
		t.Fatalf("params[registries] = %#v, want the workspace registries map", got.Params["registries"])
	}
	if len(registries) != 2 || !strings.Contains(string(registries["npm"]), "https://npm.example") {
		t.Errorf("registries = %v, want the declared npm and put entries", registries)
	}

	if url := extension.ResolvePutRegistryURL(registries); url != "https://put.example" {
		t.Errorf("put registry = %q, want the declared entry", url)
	}

	bare := writeUpgradeWorkspace(t)
	if _, err := upgradeDeps(context.Background(), bare, wsproto.Load(bare), selector, true, false, env); err != nil {
		t.Fatalf("upgradeDeps without registries: %v", err)
	}
	if _, present := got.Params["registries"]; present {
		t.Errorf("params carry a registries key for a workspace that declares none: %#v", got.Params)
	}
}

func TestUpgradeDeps_ForwardsContinueOnErrorToTheRun(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	selector := upgradeSelector{Display: "canary", RegistryTarget: "canary", DepsTarget: "canary"}

	for _, continueOnError := range []bool{false, true} {
		var got WorkspaceJobRequest
		env := LifecycleEnv{RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
			got = req
			return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
		}}
		if _, err := upgradeDeps(context.Background(), dir, cfg, selector, true, continueOnError, env); err != nil {
			t.Fatalf("upgradeDeps: %v", err)
		}
		if got.ContinueOnError != continueOnError {
			t.Errorf("ContinueOnError = %v, want %v", got.ContinueOnError, continueOnError)
		}
	}
}

func TestUpgrade_ChannelResolvesNativelyWithoutAReleaseSet(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	factoryCalled := false
	providerChecked := false
	originalFactory := newUpgradeReleaseSetResolver
	originalProviderValidation := validateUpgradeReleaseSetProvider
	newUpgradeReleaseSetResolver = func() (upgradeReleaseSetResolver, error) {
		factoryCalled = true
		return nil, errors.New("channels must not resolve a release set")
	}
	validateUpgradeReleaseSetProvider = func(string, *wsproto.Config) error {
		providerChecked = true
		return errors.New("channels must not require Cloud")
	}
	t.Cleanup(func() {
		newUpgradeReleaseSetResolver = originalFactory
		validateUpgradeReleaseSetProvider = originalProviderValidation
	})

	var gotJob WorkspaceJobRequest
	env := LifecycleEnv{RunJob: func(_ context.Context, request WorkspaceJobRequest) (WorkspaceJobResult, error) {
		gotJob = request
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
	if err := Upgrade(context.Background(), dir, cfg, "1.0.0", t.TempDir(), UpgradeFlags{
		Deps: true, Channel: "canary", DryRun: true,
	}, env); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if factoryCalled || providerChecked {
		t.Fatalf("explicit channel used the release-set provider (factory=%v, provider=%v)", factoryCalled, providerChecked)
	}
	if _, exists := gotJob.Params["releaseSet"]; exists {
		t.Fatalf("channel upgrade received a releaseSet param: %#v", gotJob.Params)
	}
	if gotJob.Params["putnami-channel"] != "canary" || gotJob.Params["putnami-version"] != "canary" {
		t.Errorf("channel params = %#v, want the native channel name", gotJob.Params)
	}
}

func TestUpgrade_ReleaseUsesExplicitImmutableSelector(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	resolved := upgradeReleaseSetResponse(t, "putnami", "2.0.1", "v2.0.0")
	var gotResolve distribution.ResolveRequest
	replaceUpgradeReleaseSetResolver(t, upgradeReleaseSetResolverFunc(func(_ context.Context, request *distribution.ResolveRequest) (*distribution.ResolveResponse, error) {
		gotResolve = *request
		return resolved, nil
	}))
	var gotJob WorkspaceJobRequest
	env := LifecycleEnv{RunJob: func(_ context.Context, request WorkspaceJobRequest) (WorkspaceJobResult, error) {
		gotJob = request
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}

	if err := Upgrade(context.Background(), dir, cfg, "1.0.0", t.TempDir(), UpgradeFlags{
		Release: resolved.Release.Ref.ID, Namespace: "putnami", DryRun: true,
	}, env); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if gotResolve.ReleaseID != resolved.Release.Ref.ID || len(gotResolve.Channels) != 0 {
		t.Errorf("resolve request = %+v, want immutable releaseId only", gotResolve)
	}
	// The namespace is the one the operator named, never the workspace name.
	if gotResolve.Namespace != "putnami" {
		t.Errorf("resolve namespace = %q, want the --namespace value", gotResolve.Namespace)
	}
	if gotResolve.Namespace == cfg.Name {
		t.Errorf("resolve namespace fell back to the workspace name %q", cfg.Name)
	}
	if gotJob.Job != "deps-upgrade" {
		t.Errorf("job = %q, want deps-upgrade", gotJob.Job)
	}
	// The parameter handed to deps-upgrade is the CHANNEL HEAD shape, which is
	// what both language extensions parse: {ref, generation, releaseSet}.
	carried, ok := gotJob.Params["releaseSet"].(distribution.ChannelHead)
	if !ok || carried.Ref != resolved.Release.Ref || carried.ReleaseSet == nil {
		t.Errorf("releaseSet param = %#v, want exact ref %+v", gotJob.Params["releaseSet"], resolved.Release.Ref)
	}
}

func TestUpgrade_VersionPreservesUniformLegacyPath(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	factoryCalled := false
	original := newUpgradeReleaseSetResolver
	newUpgradeReleaseSetResolver = func() (upgradeReleaseSetResolver, error) {
		factoryCalled = true
		return nil, errors.New("must not resolve")
	}
	t.Cleanup(func() { newUpgradeReleaseSetResolver = original })
	var gotJob WorkspaceJobRequest
	env := LifecycleEnv{RunJob: func(_ context.Context, request WorkspaceJobRequest) (WorkspaceJobResult, error) {
		gotJob = request
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
	if err := Upgrade(context.Background(), dir, cfg, "1.0.0", t.TempDir(), UpgradeFlags{
		Deps: true, Version: "1.7.3", DryRun: true,
	}, env); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if factoryCalled {
		t.Fatal("legacy --version unexpectedly created a release-set resolver")
	}
	if _, exists := gotJob.Params["releaseSet"]; exists {
		t.Fatalf("legacy --version received releaseSet: %#v", gotJob.Params)
	}
	if gotJob.Params["putnami-version"] != "1.7.3" || gotJob.Params["putnami-channel"] != "1.7.3" {
		t.Errorf("legacy params = %#v, want uniform 1.7.3", gotJob.Params)
	}
}

func TestUpgrade_ImplicitStablePreservesCloudlessLegacyPath(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	providerChecked := false
	originalProviderValidation := validateUpgradeReleaseSetProvider
	validateUpgradeReleaseSetProvider = func(string, *wsproto.Config) error {
		providerChecked = true
		return errors.New("no cloud provider")
	}
	t.Cleanup(func() { validateUpgradeReleaseSetProvider = originalProviderValidation })
	var gotJob WorkspaceJobRequest
	env := LifecycleEnv{RunJob: func(_ context.Context, request WorkspaceJobRequest) (WorkspaceJobResult, error) {
		gotJob = request
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}
	if err := Upgrade(context.Background(), dir, cfg, "1.0.0", t.TempDir(), UpgradeFlags{
		Deps: true, DryRun: true,
	}, env); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if providerChecked {
		t.Fatal("implicit stable compatibility path unexpectedly required a provider")
	}
	if _, exists := gotJob.Params["releaseSet"]; exists {
		t.Fatalf("implicit stable compatibility path received releaseSet: %#v", gotJob.Params)
	}
	if gotJob.Params["putnami-channel"] != "latest" || gotJob.Params["putnami-version"] != "latest" {
		t.Errorf("implicit stable params = %#v, want legacy latest", gotJob.Params)
	}
}

// An immutable release id is the only selector a native tag cannot express, so
// it is the only one that still needs the release-set provider — and it fails
// closed when that provider is absent.
func TestUpgrade_ReleaseFailsClosedWithoutProvider(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	originalProviderValidation := validateUpgradeReleaseSetProvider
	originalFactory := newUpgradeReleaseSetResolver
	validateUpgradeReleaseSetProvider = func(string, *wsproto.Config) error {
		return fmt.Errorf("%w: test provider missing", releaseset.ErrProviderAbsent)
	}
	factoryCalled := false
	newUpgradeReleaseSetResolver = func() (upgradeReleaseSetResolver, error) {
		factoryCalled = true
		return nil, nil
	}
	t.Cleanup(func() {
		validateUpgradeReleaseSetProvider = originalProviderValidation
		newUpgradeReleaseSetResolver = originalFactory
	})
	err := Upgrade(context.Background(), dir, cfg, "1.0.0", t.TempDir(), UpgradeFlags{
		Release: "rs_" + strings.Repeat("a", 64), Namespace: "putnami", DryRun: true,
	}, upgradeTestEnv(WorkspaceJobOK))
	if !errors.Is(err, releaseset.ErrProviderAbsent) {
		t.Fatalf("Upgrade error = %v, want ErrProviderAbsent", err)
	}
	if factoryCalled {
		t.Fatal("resolver factory ran without a validated reserved provider")
	}
}

// --release is scoped by the namespace that published the id. The workspace
// name is not that namespace, so it is asked for rather than guessed.
func TestResolveUpgradeSelector_ReleaseRequiresNamespace(t *testing.T) {
	_, err := resolveUpgradeSelector(UpgradeFlags{Release: "rs_" + strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "--namespace") {
		t.Fatalf("err = %v, want a --namespace requirement", err)
	}
}

func TestResolveUpgradeSelector_NamespaceOnlyAppliesToRelease(t *testing.T) {
	for _, flags := range []UpgradeFlags{
		{Namespace: "putnami", Channel: "canary"},
		{Namespace: "putnami", Version: "1.2.3"},
		{Namespace: "putnami"},
	} {
		_, err := resolveUpgradeSelector(flags)
		if err == nil || !strings.Contains(err.Error(), "only with --release") {
			t.Errorf("resolveUpgradeSelector(%+v) error = %v, want --namespace rejection", flags, err)
		}
	}
}

// The selector table is the contract every phase reads: which registry target
// the CLI/extension registries get, which target the dependency job gets, and
// whether the mode needs a release set at all.
func TestResolveUpgradeSelector_Table(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		flags          UpgradeFlags
		mode           string
		display        string
		registryTarget string
		depsTarget     string
		namespace      string
		releaseSetMode bool
	}{
		{
			name: "implicit stable", flags: UpgradeFlags{},
			mode: "channel", display: "stable", registryTarget: "latest", depsTarget: "latest",
		},
		{
			name: "explicit stable", flags: UpgradeFlags{Channel: "stable"},
			mode: "channel", display: "stable", registryTarget: "latest", depsTarget: "latest",
		},
		{
			name: "canary", flags: UpgradeFlags{Channel: "canary"},
			mode: "channel", display: "canary", registryTarget: "canary", depsTarget: "canary",
		},
		{
			name: "version", flags: UpgradeFlags{Version: "1.2.3"},
			mode: "version", display: "1.2.3", registryTarget: "1.2.3", depsTarget: "1.2.3",
		},
		{
			name:  "release",
			flags: UpgradeFlags{Release: "rs_" + strings.Repeat("a", 64), Namespace: "putnami"},
			mode:  "release", display: "rs_" + strings.Repeat("a", 64),
			depsTarget: "rs_" + strings.Repeat("a", 64), namespace: "putnami", releaseSetMode: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			selector, err := resolveUpgradeSelector(testCase.flags)
			if err != nil {
				t.Fatalf("resolveUpgradeSelector: %v", err)
			}
			if selector.Mode != testCase.mode || selector.Display != testCase.display {
				t.Errorf("mode/display = %q/%q, want %q/%q", selector.Mode, selector.Display, testCase.mode, testCase.display)
			}
			if selector.RegistryTarget != testCase.registryTarget || selector.DepsTarget != testCase.depsTarget {
				t.Errorf("targets = %q/%q, want %q/%q", selector.RegistryTarget, selector.DepsTarget, testCase.registryTarget, testCase.depsTarget)
			}
			if selector.Namespace != testCase.namespace {
				t.Errorf("namespace = %q, want %q", selector.Namespace, testCase.namespace)
			}
			if selector.ReleaseSetMode != testCase.releaseSetMode {
				t.Errorf("releaseSetMode = %v, want %v", selector.ReleaseSetMode, testCase.releaseSetMode)
			}
		})
	}
}

func TestUpgrade_ChannelCLIOnlyDoesNotResolveReleaseSet(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	providerChecked := false
	originalProviderValidation := validateUpgradeReleaseSetProvider
	originalUpdate := versionUpdateWithOptions
	validateUpgradeReleaseSetProvider = func(string, *wsproto.Config) error {
		providerChecked = true
		return errors.New("must not require Cloud")
	}
	var gotOptions versioncmd.VersionUpdateOptions
	versionUpdateWithOptions = func(_ context.Context, _ string, _ string, options versioncmd.VersionUpdateOptions) (versioncmd.VersionUpdateResult, error) {
		gotOptions = options
		return versioncmd.VersionUpdateResult{}, nil
	}
	t.Cleanup(func() {
		validateUpgradeReleaseSetProvider = originalProviderValidation
		versionUpdateWithOptions = originalUpdate
	})
	if err := Upgrade(context.Background(), dir, cfg, "1.0.0", t.TempDir(), UpgradeFlags{
		CLI: true, Channel: "canary", DryRun: true,
	}, LifecycleEnv{}); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if providerChecked {
		t.Fatal("CLI-only channel upgrade unexpectedly required a release-set provider")
	}
	if gotOptions.Channel != "canary" {
		t.Errorf("CLI registry channel = %q, want canary", gotOptions.Channel)
	}
}

func TestResolveUpgradeSelector_SelectorsAreMutuallyExclusive(t *testing.T) {
	selectors := []UpgradeFlags{
		{Release: "rs_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Channel: "canary"},
		{Release: "rs_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Version: "1.2.3"},
		{Channel: "canary", Version: "1.2.3"},
	}
	for _, flags := range selectors {
		if _, err := resolveUpgradeSelector(flags); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("resolveUpgradeSelector(%+v) error = %v, want mutual exclusivity", flags, err)
		}
	}
}

func TestUpgradeDeps_NoExtensionProvidesJob(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)

	out, err := captureStdout(t, func() error {
		_, err := upgradeDeps(context.Background(), dir, cfg, upgradeSelector{Display: "stable", RegistryTarget: "latest", DepsTarget: "latest"}, false, false, upgradeTestEnv(WorkspaceJobMissing))
		return err
	})
	if err != nil {
		t.Fatalf("upgradeDeps: %v", err)
	}
	if !strings.Contains(out, `No extension provides the "deps-upgrade" job. Skipping.`) {
		t.Errorf("output = %q, want skip message", out)
	}
}

func TestUpgrade_DepsPhaseOnly(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)

	out, err := captureStdout(t, func() error {
		return Upgrade(context.Background(), dir, cfg, "1.0.0", t.TempDir(), UpgradeFlags{Deps: true}, upgradeTestEnv(WorkspaceJobMissing))
	})
	// No deps-upgrade job → upgradeDeps returns nil → no overall error.
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if !strings.Contains(out, "Dependencies") {
		t.Errorf("output = %q, want Dependencies header", out)
	}
	// Only the deps phase should run.
	if strings.Contains(out, "\n  CLI") {
		t.Errorf("output = %q, should not run CLI phase", out)
	}
	if strings.Contains(out, "Extensions") {
		t.Errorf("output = %q, should not run extensions phase", out)
	}
}

func TestUpgrade_CLIPhaseFailurePropagates(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)

	// Force VersionUpdate to fail-closed: resolver advertises a version with no
	// integrity header.
	t.Setenv("PUTNAMI_UNSAFE_UPDATE", "")
	t.Setenv("PUTNAMI_REGISTRY_URL", "http://127.0.0.1:1")
	// Dead registry refuses instantly; skip the retry backoff so the test
	// doesn't pay the full (1s+2s) budget waiting on a host that won't recover.
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")

	_, err := captureStdout(t, func() error {
		return Upgrade(context.Background(), dir, cfg, "1.0.0", t.TempDir(), UpgradeFlags{CLI: true}, LifecycleEnv{})
	})
	if err == nil || !strings.Contains(err.Error(), "some upgrade phases failed") {
		t.Fatalf("err = %v, want aggregated phase failure", err)
	}
}

// The restart carries the providers this process enabled as PUTNAMI_PROVIDERS
// in its environment, never as --providers: an older restart target refuses
// the flag and ignores the variable.
func TestUpgrade_RestartsForRemainingPhasesAfterCLIUpdate(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	originalUpdate := versionUpdateWithOptions
	originalRestart := restartUpgradeProcess
	originalRefresh := refreshCompletionsAfterCLIUpdateFn
	t.Cleanup(func() {
		versionUpdateWithOptions = originalUpdate
		restartUpgradeProcess = originalRestart
		refreshCompletionsAfterCLIUpdateFn = originalRefresh
	})
	versionUpdateWithOptions = func(context.Context, string, string, versioncmd.VersionUpdateOptions) (versioncmd.VersionUpdateResult, error) {
		return versioncmd.VersionUpdateResult{Updated: true, Version: "0.1.0-new", BinaryPath: "/verified/putnami"}, nil
	}
	refreshCompletionsAfterCLIUpdateFn = func(context.Context, string) error { return nil }
	var gotPath string
	var gotArgs, gotEnv []string
	restartUpgradeProcess = func(path string, args []string, env ...string) error {
		gotPath = path
		gotArgs = append([]string(nil), args...)
		gotEnv = append([]string(nil), env...)
		return nil
	}

	out, err := captureStdout(t, func() error {
		return Upgrade(context.Background(), dir, cfg, "0.1.0-old", t.TempDir(), UpgradeFlags{Providers: []string{"install", "publish"}}, upgradeTestEnv(WorkspaceJobMissing))
	})
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if gotPath != "/verified/putnami" {
		t.Fatalf("restart path = %q, want verified replacement", gotPath)
	}
	wantArgs := []string{"upgrade", "--extensions", "--deps"}
	if strings.Join(gotArgs, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("restart args = %q, want %q", gotArgs, wantArgs)
	}
	if wantEnv := []string{"PUTNAMI_PROVIDERS=install,publish"}; strings.Join(gotEnv, "\x00") != strings.Join(wantEnv, "\x00") {
		t.Errorf("restart env = %q, want %q", gotEnv, wantEnv)
	}
	for _, phase := range []string{"\n  Extensions", "\n  Templates", "\n  Dependencies"} {
		if strings.Contains(out, phase) {
			t.Errorf("output = %q, should hand off before %s", out, strings.TrimSpace(phase))
		}
	}
}

// The restart hands the ORIGINAL selector to the replacement CLI. A channel
// carries only its name now: there is no resolved snapshot to preserve across
// the exec, because each ecosystem resolves the channel itself.
func TestUpgrade_RestartCarriesTheChannelSelectorWithoutAReleaseSet(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	factoryCalled := false
	originalFactory := newUpgradeReleaseSetResolver
	newUpgradeReleaseSetResolver = func() (upgradeReleaseSetResolver, error) {
		factoryCalled = true
		return nil, errors.New("channels must not resolve a release set")
	}
	originalUpdate := versionUpdateWithOptions
	originalRestart := restartUpgradeProcess
	originalRefresh := refreshCompletionsAfterCLIUpdateFn
	t.Cleanup(func() {
		newUpgradeReleaseSetResolver = originalFactory
		versionUpdateWithOptions = originalUpdate
		restartUpgradeProcess = originalRestart
		refreshCompletionsAfterCLIUpdateFn = originalRefresh
	})
	versionUpdateWithOptions = func(context.Context, string, string, versioncmd.VersionUpdateOptions) (versioncmd.VersionUpdateResult, error) {
		return versioncmd.VersionUpdateResult{Updated: true, Version: "0.1.0-new", BinaryPath: "/verified/putnami"}, nil
	}
	refreshCompletionsAfterCLIUpdateFn = func(context.Context, string) error { return nil }

	var gotArgs []string
	restartUpgradeProcess = func(_ string, args []string, _ ...string) error {
		gotArgs = append([]string(nil), args...)
		return nil
	}

	if err := Upgrade(context.Background(), dir, cfg, "0.1.0-old", t.TempDir(), UpgradeFlags{
		Channel: "canary",
	}, upgradeTestEnv(WorkspaceJobMissing)); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if factoryCalled {
		t.Fatal("channel restart resolved a release set")
	}
	wantArgs := []string{"upgrade", "--extensions", "--deps", "--channel", "canary"}
	if strings.Join(gotArgs, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("restart args = %q, want %q", gotArgs, wantArgs)
	}
}

// The replacement CLI must be able to resolve the same immutable snapshot, so
// the namespace travels with the id.
func TestUpgradeRemainingArgs_ReleaseCarriesItsNamespace(t *testing.T) {
	args := upgradeRemainingArgs(false, UpgradeFlags{Deps: true}, upgradeSelector{
		Mode: "release", Display: "rs_" + strings.Repeat("a", 64), Namespace: "putnami",
	})
	want := []string{"upgrade", "--deps", "--release", "rs_" + strings.Repeat("a", 64), "--namespace", "putnami"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args = %q, want %q", args, want)
	}
}

func TestUpgrade_CLIPassOnlyDoesNotRestart(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	lf := lockfile.NewLockFile()
	lf.SetCLI(lockfile.LockEntry{Version: "0.1.0-old"})
	if err := lockfile.WriteLockFile(dir, lf); err != nil {
		t.Fatal(err)
	}
	originalUpdate := versionUpdateWithOptions
	originalRestart := restartUpgradeProcess
	originalRefresh := refreshCompletionsAfterCLIUpdateFn
	t.Cleanup(func() {
		versionUpdateWithOptions = originalUpdate
		restartUpgradeProcess = originalRestart
		refreshCompletionsAfterCLIUpdateFn = originalRefresh
	})
	versionUpdateWithOptions = func(context.Context, string, string, versioncmd.VersionUpdateOptions) (versioncmd.VersionUpdateResult, error) {
		return versioncmd.VersionUpdateResult{Updated: true, Version: "0.1.0-new", BinaryPath: "/verified/putnami"}, nil
	}
	refreshCompletionsAfterCLIUpdateFn = func(context.Context, string) error { return nil }
	restarted := false
	restartUpgradeProcess = func(string, []string, ...string) error {
		restarted = true
		return nil
	}

	var err error
	stderr := captureStderr(t, func() {
		_, err = captureStdout(t, func() error {
			return Upgrade(context.Background(), dir, cfg, "0.1.0-old", t.TempDir(), UpgradeFlags{CLI: true}, LifecycleEnv{})
		})
	})
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if restarted {
		t.Fatal("CLI-only upgrade restarted despite having no remaining phases")
	}
	if !strings.Contains(stderr, "despite the active CLI link") || !strings.Contains(stderr, "putnami pin 0.1.0-new") {
		t.Errorf("stderr = %q, want CLI-only pin warning", stderr)
	}
}

func TestUpgrade_ActivationFailureStopsRemainingPhases(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	cfg := wsproto.Load(dir)
	originalUpdate := versionUpdateWithOptions
	t.Cleanup(func() { versionUpdateWithOptions = originalUpdate })
	versionUpdateWithOptions = func(context.Context, string, string, versioncmd.VersionUpdateOptions) (versioncmd.VersionUpdateResult, error) {
		return versioncmd.VersionUpdateResult{}, &versioncmd.CLIActivationError{
			BinaryPath: "/verified/putnami",
			LinkPath:   "/verified/putnami-link",
			Cause:      errors.New("test failure"),
		}
	}

	var out string
	var err error
	stderr := captureStderr(t, func() {
		out, err = captureStdout(t, func() error {
			return Upgrade(context.Background(), dir, cfg, "0.1.0-old", t.TempDir(), UpgradeFlags{}, upgradeTestEnv(WorkspaceJobMissing))
		})
	})
	if err == nil || !strings.Contains(err.Error(), "CLI update did not activate") {
		t.Fatalf("err = %v, want activation failure that stops the upgrade", err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want activation error to render once through the command handler", stderr)
	}
	if next := protocolcli.SuggestedNext(err); !strings.Contains(next, "PUTNAMI_NO_RELAUNCH=1") || !strings.Contains(next, "/verified/putnami") {
		t.Errorf("next = %q, want direct verified-binary recovery", next)
	}
	for _, phase := range []string{"\n  Extensions", "\n  Templates", "\n  Dependencies"} {
		if strings.Contains(out, phase) {
			t.Errorf("output = %q, should not run %s after activation failure", out, strings.TrimSpace(phase))
		}
	}
}

func TestRestartRemainingUpgradePhases_ReportsRecoveryCommand(t *testing.T) {
	originalRestart := restartUpgradeProcess
	t.Cleanup(func() { restartUpgradeProcess = originalRestart })
	restartUpgradeProcess = func(string, []string, ...string) error { return errors.New("exec format error") }

	var restarting bool
	var err error
	_, captureErr := captureStdout(t, func() error {
		restarting, err = restartRemainingUpgradePhases(versioncmd.VersionUpdateResult{
			Updated:    true,
			Version:    "0.1.0-new",
			BinaryPath: "/verified/putnami",
		}, true, UpgradeFlags{Providers: []string{"install"}}, upgradeSelector{Mode: "channel", Display: "stable"})
		return nil
	})
	if captureErr != nil {
		t.Fatalf("capture restart output: %v", captureErr)
	}
	if !restarting {
		t.Fatal("restartRemainingUpgradePhases did not report a failed handoff")
	}
	if err == nil || !strings.Contains(err.Error(), "could not restart with the updated CLI") {
		t.Fatalf("err = %v, want contextual restart failure", err)
	}
	if next := protocolcli.SuggestedNext(err); !strings.HasPrefix(next, "PUTNAMI_NO_RELAUNCH=1 PUTNAMI_PROVIDERS=install /verified/putnami upgrade") {
		t.Errorf("next = %q, want direct recovery command", next)
	}
}

func TestWorkspaceCLIPinUpgradeWarning(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	lf := lockfile.NewLockFile()
	lf.SetCLI(lockfile.LockEntry{Version: "0.1.0-old"})
	if err := lockfile.WriteLockFile(dir, lf); err != nil {
		t.Fatal(err)
	}

	got := workspaceCLIPinUpgradeWarning(dir, "0.1.0-new", true)
	for _, want := range []string{"0.1.0-old", "0.1.0-new", "putnami pin 0.1.0-new"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning = %q, want %q", got, want)
		}
	}
	if got := workspaceCLIPinUpgradeWarning(dir, "0.1.0-old", true); got != "" {
		t.Errorf("matching pin warning = %q, want empty", got)
	}
	if got := workspaceCLIPinUpgradeWarning(dir, "v0.1.0-old", false); got != "" {
		t.Errorf("v-prefixed matching pin warning = %q, want empty", got)
	}
	if got := workspaceCLIPinUpgradeWarning(dir, "0.1.0-new", false); !strings.Contains(got, "despite the active CLI link") {
		t.Errorf("CLI-only warning = %q, want active-link explanation", got)
	}
}

func TestUpgradeRemainingArgs_PreservesSelector(t *testing.T) {
	tests := []struct {
		name     string
		flags    UpgradeFlags
		selector upgradeSelector
		want     []string
	}{
		{
			name:     "exact version",
			flags:    UpgradeFlags{Extensions: true},
			selector: upgradeSelector{Mode: "version", Display: "0.1.0-abc"},
			want:     []string{"upgrade", "--extensions", "--version", "0.1.0-abc"},
		},
		{
			name:     "release",
			flags:    UpgradeFlags{Deps: true},
			selector: upgradeSelector{Mode: "release", Display: "rs_abc", Namespace: "putnami"},
			want:     []string{"upgrade", "--deps", "--release", "rs_abc", "--namespace", "putnami"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upgradeRemainingArgs(false, tt.flags, tt.selector)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("upgradeRemainingArgs = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRefreshCompletionsAfterCLIUpdate_Skipped(t *testing.T) {
	// No SHELL set and an unknown shell means completions are skipped rather
	// than failing.
	hometest.Temp(t)
	t.Setenv("SHELL", "/bin/unknownshell")

	out, err := captureStdout(t, func() error {
		return refreshCompletionsAfterCLIUpdate(context.Background(), t.TempDir())
	})
	if err != nil {
		t.Fatalf("refreshCompletionsAfterCLIUpdate: %v", err)
	}
	if !strings.Contains(out, "Shell completions skipped") {
		t.Errorf("output = %q, want skipped message", out)
	}
}

// A source workspace records no version, so the pin-drift wording would print
// "remains pinned to " with nothing after it. The warning must say the true
// thing instead: upgrading the installed CLI does not change which engine this
// workspace's commands run, because that engine is built from the tree.
func TestWorkspaceCLIPinUpgradeWarningReportsASourceWorkspace(t *testing.T) {
	dir := writeUpgradeWorkspace(t)
	lf := lockfile.NewLockFile()
	lf.SetCLI(lockfile.LockEntry{Source: lockfile.SourceWorkspace})
	if err := lockfile.WriteLockFile(dir, lf); err != nil {
		t.Fatal(err)
	}

	for _, remaining := range []bool{true, false} {
		got := workspaceCLIPinUpgradeWarning(dir, "0.1.0-new", remaining)
		for _, want := range []string{"builds its own CLI from source", lockfile.SourceWorkspace, "./putnamiw", "0.1.0-new"} {
			if !strings.Contains(got, want) {
				t.Errorf("hasRemainingPhases=%v: warning = %q, want %q", remaining, got, want)
			}
		}
		if strings.Contains(got, "remains pinned to ,") || strings.Contains(got, "putnami pin \n") {
			t.Errorf("hasRemainingPhases=%v: warning leaked an empty version: %q", remaining, got)
		}
	}
}

// `putnami upgrade` registers the putnami MCP server like init and install do
// (ADR 0040), keeps a registration someone diverged on purpose, and its dry run
// writes nothing.
func TestUpgrade_RegistersTheMCPServerAndKeepsADivergedEntry(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "implicit-mcp-registration", "a-diverged-entry-is-kept-byte-for-byte")
	hometest.Temp(t)
	upgrade := func(dir string, dryRun bool) string {
		t.Helper()
		out, err := captureStdout(t, func() error {
			return Upgrade(context.Background(), dir, wsproto.Load(dir), "1.0.0", t.TempDir(),
				UpgradeFlags{Extensions: true, DryRun: dryRun}, upgradeTestEnv(WorkspaceJobMissing))
		})
		if err != nil {
			t.Fatalf("Upgrade: %v", err)
		}
		return out
	}
	const registration = ".mcp.json"

	dry := writeUpgradeWorkspace(t)
	if out := upgrade(dry, true); !strings.Contains(out, "Would register the putnami MCP server") {
		t.Fatalf("dry run did not describe the registration:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dry, registration)); !os.IsNotExist(err) {
		t.Fatalf("a dry-run upgrade wrote .mcp.json: stat err %v", err)
	}

	fresh := writeUpgradeWorkspace(t)
	if out := upgrade(fresh, false); !strings.Contains(out, "putnami MCP server registered in .mcp.json") {
		t.Fatalf("upgrade did not report the registration:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(fresh, registration))
	if err != nil || !strings.Contains(string(data), `"putnami"`) {
		t.Fatalf("upgrade registered no putnami server: %s (%v)", data, err)
	}

	diverged := writeUpgradeWorkspace(t)
	custom := `{"mcpServers":{"putnami":{"command":"./putnamiw","args":["mcp"]}}}` + "\n"
	if err := os.WriteFile(filepath.Join(diverged, registration), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := upgrade(diverged, false); strings.Contains(out, "registered in .mcp.json") {
		t.Fatalf("upgrade reported a registration over a diverged entry:\n%s", out)
	}
	if got, err := os.ReadFile(filepath.Join(diverged, registration)); err != nil || string(got) != custom {
		t.Fatalf("upgrade rewrote a diverged entry:\n%s\n(%v)", got, err)
	}
}

// A hosted run refuses `putnami upgrade` before any phase, with a usage error
// that names the run credential: no CLI download, no lifecycle job, no restart
// of the installed CLI after the deps-upgrade hooks. Without the run
// credential the same flags reach their phases.
func TestUpgrade_AHostedRunIsRefusedBeforeAnyPhase(t *testing.T) {
	origUpdate := versionUpdateWithOptions
	t.Cleanup(func() { versionUpdateWithOptions = origUpdate })
	var updates, jobs int
	versionUpdateWithOptions = func(context.Context, string, string, versioncmd.VersionUpdateOptions) (versioncmd.VersionUpdateResult, error) {
		updates++
		return versioncmd.VersionUpdateResult{}, nil
	}
	env := LifecycleEnv{Out: &strings.Builder{}, RunJob: func(context.Context, WorkspaceJobRequest) (WorkspaceJobResult, error) {
		jobs++
		return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
	}}

	restore := runcredential.SetForTest("run-bearer")
	for _, flags := range []UpgradeFlags{{}, {Deps: true}, {Extensions: true}, {CLI: true}, {FromSource: true}} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, wsproto.WorkspaceConfigFilename), []byte(`{"name":"upgrade-ws"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		err := Upgrade(context.Background(), dir, wsproto.Load(dir), "1.0.0", t.TempDir(), flags, env)
		if !errors.Is(err, ErrHostedUpgrade) || !errors.Is(err, cmderr.ErrUsage) || !strings.Contains(err.Error(), runcredential.Flag) {
			t.Errorf("hosted Upgrade(%+v) = %v, want ErrHostedUpgrade, a usage error naming %s", flags, err, runcredential.Flag)
		}
	}
	restore()
	if updates != 0 || jobs != 0 {
		t.Fatalf("a hosted upgrade ran %d CLI updates and %d lifecycle jobs, want none", updates, jobs)
	}

	err := Upgrade(context.Background(), t.TempDir(), &wsproto.Config{}, "1.0.0", t.TempDir(), UpgradeFlags{CLI: true}, env)
	if errors.Is(err, ErrHostedUpgrade) || updates != 1 {
		t.Errorf("Upgrade without the run credential = %v after %d CLI updates, want the CLI phase to run", err, updates)
	}
}
