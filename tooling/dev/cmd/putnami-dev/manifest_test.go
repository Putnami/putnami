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

// verbPresentation is what help shows for one development verb: its
// description, its one-letter aliases and the commands
// `putnami help <verb>` suggests.
type verbPresentation struct {
	name        string
	description string
	aliases     []string
	related     []string
}

// presentation is the development verbs in help order. The core CLI declares
// none of them, so this extension is the only place they are described.
var presentation = []verbPresentation{
	{name: "build", description: "Compile or transpile project sources into runnable or distributable outputs.", aliases: []string{"b"}, related: []string{"test", "lint", "serve"}},
	{name: "test", description: "Run the project's tests, emitting test counts and coverage metrics.", aliases: []string{"t"}, related: []string{"build", "lint"}},
	{name: "lint", description: "Check code for style and correctness findings, emitting per-issue diagnostics.", aliases: []string{"l"}, related: []string{"build", "test", "format"}},
	{name: "serve", description: "Run the project's dev server until interrupted.", aliases: []string{"s"}, related: []string{"build", "run"}},
	{name: "run", description: "Execute the project once and exit.", related: []string{"build", "serve"}},
	{name: "format", description: "Rewrite sources into canonical formatting.", aliases: []string{"f"}, related: []string{"lint"}},
	{name: "package", description: "Assemble distributable artifacts for the project's publish channels.", related: []string{"build", "publish"}},
	{name: "publish", description: "Push packaged artifacts to registries.", aliases: []string{"p", "P"}, related: []string{"build", "test", "package"}},
	{name: "deploy", description: "Converge a cloud environment on the workload's aggregated infra manifest.", aliases: []string{"d", "D"}, related: []string{"build", "package", "publish"}},
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
	want := make([]string, 0, len(presentation))
	for _, verb := range presentation {
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

// TestEachVerbDeclaresItsPresentation pins what help prints for each verb:
// its description, aliases and related commands.
func TestEachVerbDeclaresItsPresentation(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "eleven-ordered-verbs", "each-verb-declares-its-presentation")
	verbs := readManifest(t).Verbs
	if len(verbs) != len(presentation) {
		t.Fatalf("%d verbs, want %d", len(verbs), len(presentation))
	}
	for i, want := range presentation {
		got := verbs[i]
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

// defaultTraits are the traits each verb gives the jobs that declare none.
// The core gives a job no trait, so these are the only defaults a workspace
// has. Generate declares none.
var defaultTraits = map[string]extension.CommandTraits{
	"build":    {Kind: extension.CommandKindFiles},
	"test":     {Kind: extension.CommandKindVerification},
	"lint":     {Kind: extension.CommandKindDiagnostics},
	"serve":    {RequiresRunnable: true, Workload: extension.WorkloadServe},
	"run":      {RequiresRunnable: true, Workload: extension.WorkloadOnce},
	"format":   {RewritesSources: true},
	"package":  {InjectPublishChannels: true},
	"publish":  {InjectPublishChannels: true, SideEffects: extension.SideEffectsRegistry},
	"deploy":   {SideEffects: extension.SideEffectsCloud, ConsumesReleaseSet: true},
	"validate": {Kind: extension.CommandKindCriteria},
}

// TestEachVerbDeclaresItsDefaultTraits pins each verb's default traits whole,
// side effects included, and that a verb without any declares no traits
// object.
func TestEachVerbDeclaresItsDefaultTraits(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, feature, "dev-is-the-sole-declarer", "each-verb-declares-its-default-traits")
	verbs := readManifest(t).Verbs
	if len(verbs) != len(presentation) {
		t.Fatalf("%d verbs, want %d", len(verbs), len(presentation))
	}
	for _, verb := range verbs {
		var got extension.CommandTraits
		if verb.Traits != nil {
			got = *verb.Traits
		}
		if want := defaultTraits[verb.Name]; got != want {
			t.Errorf("%s default traits %+v, want %+v", verb.Name, got, want)
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
