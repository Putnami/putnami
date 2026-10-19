package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/development-verbs"

// extensionRoot is the directory of the extension this command is the
// runtime of, and repositoryRoot the workspace that holds it.
var (
	extensionRoot  = filepath.Join("..", "..")
	repositoryRoot = filepath.Join(extensionRoot, "..", "..")
)

// languageManifests are the manifests of the extensions that run the
// development verbs. Each is a declared test input of this project.
var languageManifests = []string{
	filepath.Join("go", "extension", extension.ManifestFilename),
	filepath.Join("typescript", "extension", extension.ManifestFilename),
	filepath.Join("python", "extension", extension.ManifestFilename),
}

// verbPresentation is what the core shows for one development verb: its
// help description, its one-letter aliases and the commands
// `putnami help <verb>` suggests.
type verbPresentation struct {
	name        string
	description string
	aliases     []string
	related     []string
}

// corePresentation is the development verbs in help order, as the core
// presents them without a vocabulary. A verb that WellKnownVerbs describes
// takes that description; generate and validate take the core catalog's.
var corePresentation = []verbPresentation{
	{name: "build", description: extension.WellKnownVerbs["build"].Description, aliases: []string{"b"}, related: []string{"test", "lint", "serve"}},
	{name: "test", description: extension.WellKnownVerbs["test"].Description, aliases: []string{"t"}, related: []string{"build", "lint"}},
	{name: "lint", description: extension.WellKnownVerbs["lint"].Description, aliases: []string{"l"}, related: []string{"build", "test", "format"}},
	{name: "serve", description: extension.WellKnownVerbs["serve"].Description, aliases: []string{"s"}, related: []string{"build", "run"}},
	{name: "run", description: extension.WellKnownVerbs["run"].Description, related: []string{"build", "serve"}},
	{name: "format", description: extension.WellKnownVerbs["format"].Description, aliases: []string{"f"}, related: []string{"lint"}},
	{name: "package", description: extension.WellKnownVerbs["package"].Description, related: []string{"build", "publish"}},
	{name: "publish", description: extension.WellKnownVerbs["publish"].Description, aliases: []string{"p", "P"}, related: []string{"build", "test", "package"}},
	{name: "deploy", description: extension.WellKnownVerbs["deploy"].Description, aliases: []string{"d", "D"}, related: []string{"build", "package", "publish"}},
	{name: "generate", description: "Run code generators.", related: []string{"build"}},
	{name: "validate", description: "Check the project against its declared specs, contracts and compatibility rules.", related: []string{"lint", "test", "build"}},
}

func readManifest(t *testing.T) *extension.Manifest {
	t.Helper()
	return readManifestAt(t, filepath.Join(extensionRoot, extension.ManifestFilename))
}

// readManifestAt parses the manifest at path strictly, as the package-time
// gate and `putnami extensions validate` do.
func readManifestAt(t *testing.T, path string) *extension.Manifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := extension.ParseManifest(data)
	if manifest == nil || diag.HasErrors(diags) {
		t.Fatalf("%s does not parse strictly: %v", path, diags)
	}
	return manifest
}

// TestTheManifestDeclaresTheElevenVerbsInOrder holds the shipped manifest to
// the shape of a vocabulary-only extension: the eleven development verbs in
// help order, and nothing that runs.
func TestTheManifestDeclaresTheElevenVerbsInOrder(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "eleven-ordered-verbs", "the-manifest-declares-the-eleven-verbs-in-order")
	manifest := readManifest(t)
	if manifest.Name != extensionName {
		t.Fatalf("manifest name %q, runtime answers as %q", manifest.Name, extensionName)
	}
	names := make([]string, 0, len(manifest.Verbs))
	for _, verb := range manifest.Verbs {
		names = append(names, verb.Name)
	}
	want := make([]string, 0, len(corePresentation))
	for _, verb := range corePresentation {
		want = append(want, verb.name)
	}
	if !slices.Equal(names, want) {
		t.Fatalf("verbs %v, want %v", names, want)
	}
	if len(manifest.Commands) != 0 || len(manifest.CommandGroups) != 0 || len(manifest.Tasks) != 0 || len(manifest.Tools) != 0 {
		t.Errorf("the manifest declares something that runs: commands %v, command groups %v, tasks %v, tools %v",
			manifest.Commands, manifest.CommandGroups, manifest.Tasks, manifest.Tools)
	}
	if manifest.AgentContent != nil || manifest.Runtime != nil || manifest.Hooks != nil {
		t.Errorf("the manifest declares agent content %+v, a runtime %+v or hooks %+v", manifest.AgentContent, manifest.Runtime, manifest.Hooks)
	}
	if len(manifest.ExtensionDeps.List) != 0 {
		t.Errorf("the manifest names extension dependencies %v", manifest.ExtensionDeps.List)
	}
}

// TestTheManifestPassesStrictValidationAtContract11 runs the verdict of the
// package-time gate on the source manifest, and holds its claimed contract to
// the one the packager stamps: the command kind contract, which the declared
// kinds, workloads and effects require.
func TestTheManifestPassesStrictValidationAtContract11(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "eleven-ordered-verbs", "the-manifest-passes-strict-validation-at-contract-11")
	manifest := readManifest(t)
	if errs := diag.Errors(extension.FullValidateManifest(manifest)); len(errs) > 0 {
		t.Fatalf("the manifest fails the package-time validation: %v", errs)
	}
	if !extension.DeclaresContractSurface(manifest) {
		t.Fatal("the vocabulary does not count as contract surface")
	}
	if got, want := manifest.CLIContract, extension.RequiredCLIContract(manifest); got != want || got != protocolcli.CommandKindContract {
		t.Errorf("cliContract %d, want %d, the command kind contract the packager stamps (%d)",
			got, protocolcli.CommandKindContract, want)
	}
}

// TestEachVerbKeepsTheCorePresentation pins that loading the vocabulary
// changes nothing help prints: every verb keeps the description, aliases and
// related commands the core shows for it.
func TestEachVerbKeepsTheCorePresentation(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "eleven-ordered-verbs", "each-verb-keeps-the-core-presentation")
	verbs := readManifest(t).Verbs
	if len(verbs) != len(corePresentation) {
		t.Fatalf("%d verbs, want %d", len(verbs), len(corePresentation))
	}
	for i, want := range corePresentation {
		got := verbs[i]
		if want.description == "" {
			t.Fatalf("%s has no core description to compare with", want.name)
		}
		if got.Description != want.description {
			t.Errorf("%s description %q, want %q", want.name, got.Description, want.description)
		}
		if !slices.Equal(got.Aliases, want.aliases) {
			t.Errorf("%s aliases %v, want %v", want.name, got.Aliases, want.aliases)
		}
		if !slices.Equal(got.Related, want.related) {
			t.Errorf("%s related %v, want %v", want.name, got.Related, want.related)
		}
	}
}

// declaredKinds is what each verb declares beyond the core defaults: the kind
// of result the host reads from its jobs, its workload, and its effects.
// Package, publish and generate declare none of them.
var declaredKinds = map[string]extension.CommandTraits{
	"build":    {Kind: extension.CommandKindFiles},
	"test":     {Kind: extension.CommandKindVerification},
	"lint":     {Kind: extension.CommandKindDiagnostics},
	"serve":    {Workload: extension.WorkloadServe},
	"run":      {Workload: extension.WorkloadOnce},
	"format":   {RewritesSources: true},
	"deploy":   {ConsumesReleaseSet: true},
	"validate": {Kind: extension.CommandKindCriteria},
}

// TestEachVerbKeepsTheCoreDefaultsAndDeclaresItsKind pins each verb's default
// traits in two parts. The four core fields equal the ones WellKnownVerbs gives
// the verb, none for generate and validate, so they move no job. The kind,
// workload and effects equal declaredKinds. A verb with neither declares no
// traits object.
func TestEachVerbKeepsTheCoreDefaultsAndDeclaresItsKind(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "core-default-traits", "each-verb-keeps-the-core-defaults-and-declares-its-kind")
	verbs := readManifest(t).Verbs
	if len(verbs) != len(corePresentation) {
		t.Fatalf("%d verbs, want %d", len(verbs), len(corePresentation))
	}
	for _, verb := range verbs {
		var got extension.CommandTraits
		if verb.Traits != nil {
			got = *verb.Traits
		}
		core := extension.CommandTraits{
			Heavy:                 got.Heavy,
			RequiresRunnable:      got.RequiresRunnable,
			InjectPublishChannels: got.InjectPublishChannels,
			SideEffects:           got.SideEffects,
		}
		if want := extension.WellKnownVerbs[verb.Name].Traits; core != want {
			t.Errorf("%s core traits %+v differ from the core defaults %+v", verb.Name, core, want)
		}
		kinds := extension.CommandTraits{
			Kind:               got.Kind,
			Workload:           got.Workload,
			RewritesSources:    got.RewritesSources,
			ConsumesReleaseSet: got.ConsumesReleaseSet,
		}
		if want := declaredKinds[verb.Name]; kinds != want {
			t.Errorf("%s declares kind, workload and effects %+v, want %+v", verb.Name, kinds, want)
		}
		if declares := got != (extension.CommandTraits{}); declares != (verb.Traits != nil) {
			t.Errorf("%s declares an empty traits object", verb.Name)
		}
	}
}

// TestEachLanguageExtensionNamesTheDevExtension pins the dependency that
// brings the vocabulary to every workspace that installs a language
// extension: an empty constraint, under a contract that covers it.
func TestEachLanguageExtensionNamesTheDevExtension(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "language-extensions-depend", "each-language-extension-names-the-dev-extension")
	for _, rel := range languageManifests {
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(repositoryRoot, rel)
			manifest := readManifestAt(t, path)
			constraint, named := manifest.ExtensionDeps.List[extensionName]
			if !named || constraint != "" {
				t.Fatalf("extensionDependencies %v, want %s with an empty constraint", manifest.ExtensionDeps.List, extensionName)
			}
			if manifest.CLIContract < protocolcli.ExtensionDependenciesContract {
				t.Errorf("cliContract %d, want at least %d for the dependency", manifest.CLIContract, protocolcli.ExtensionDependenciesContract)
			}
			if err := extension.CheckDependencyContract(path, manifest); err != nil {
				t.Error(err)
			}
		})
	}
}
