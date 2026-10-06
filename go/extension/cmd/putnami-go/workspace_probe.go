package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/go/extension/internal/gosource"
	"go.putnami.dev/go/extension/internal/releaseplan"
	"go.putnami.dev/go/extension/internal/toolchain"
	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
)

// The Go workspace probe.
//
// This is where "what is a Go project, and what does it depend on?" moves out
// of the orchestrator. Core asks once, with the candidate directories it
// already knows about; this answers with what each directory's go.mod says,
// plus the one fact only a Go-aware reader can derive from the sources — whether
// the module contains a package main, and is therefore an application rather
// than a library.
//
// Four properties are load-bearing, and none of them can be enforced from the
// outside:
//
//   - PURE. The answer is a function of the tree alone. No clock, no absolute
//     path, no environment lookup, no network, no `go` invocation. The answer's
//     digest keys core's workspace snapshot, so a byte that varies between two
//     runs over one tree is a cache-correctness bug — this is the silent-edge-loss shape
//     (source-blind Go keys), not a cosmetic one.
//
//   - PATHS, NOT MODULES. Dependency edges leave here as repo-relative project
//     PATHS. Resolving the module graph — require lines, and the local replaces
//     that carry placeholder versions no proxy has ever seen — is the part only
//     this side can do, and keying the wire on paths is what lets core own
//     canonical identity without learning Go's module rules.
//
//   - THE PROJECT ROOT IS NOT ASSUMED TO BE THE MODULE ROOT. Every path this
//     file computes comes from where the go.mod actually IS, never from the
//     candidate directory standing in for it. Today a Go project's directory
//     holds its own go.mod; the moment one stops doing so, SourceFile and the
//     replace-target index still point at the real file.
//
//   - SOLE OWNER. Edge derivation is what core's `deriveGoDependencies` used to
//     do: requires resolve through a module index, local replaces resolve
//     through a DIRECTORY index over every candidate — not only the Go ones,
//     because core indexes every project by directory and a probe that indexed
//     fewer would silently drop an edge. That parser is gone, so there
//     is no second opinion left to compare against: a dropped edge here is a
//     dropped edge in the build graph. The corpus in
//     workspace_corpus_test.go is now the sole conformance suite.

// goExtensionName is the identity this extension answers under. It keys the
// provider-owned metadata bucket in core's merged view and must match the
// name the manifest declares.
const goExtensionName = "@putnami/go"

// goWorkspaceMarker is the manifest whose presence marks a Go project. It
// matches the `workspace.markers` entry in putnami.extension.json; the manifest
// contract test pins the two together.
const goWorkspaceMarker = "go.mod"

// goWorkspaceRootFiles are provider-owned root invalidation inputs. They ride
// the per-project watchedFiles member. A change to membership or checksum state
// can invalidate any Go module even though neither file belongs to it by path.
//
// putnami.workspace.json is one of them: every image member's coordinate
// carries its `registries.oci.publish` namespace.
var goWorkspaceRootFiles = []string{"go.work", "go.work.sum", goWorkspaceConfigFile}

// goWorkspaceConfigFile is the workspace document. The probe reads its name
// and registries.oci to resolve the image member's logical coordinate.
const goWorkspaceConfigFile = "putnami.workspace.json"

// goProjectConfigFile is the project document. The probe reads publication
// intent, the native registry override, and an explicit OCI package producer.
const goProjectConfigFile = "putnami.json"

// goImageProjectType is the authored project type of a pre-built image: an
// image project uses the shared immutable OCI packager and publisher, including
// when it carries no Go module.
const goImageProjectType = "image"

// goClassificationInput is the provider-declared snapshot witness for project
// type. Classification reads package clauses from Go sources at any depth, so
// the recursive matched set — including creation and deletion — must invalidate
// the recorded probe answer before core adopts it on a later command.
const goClassificationInput = "**/*.go"

// handleWorkspaceProbe answers core's reserved probe invocation. It is called
// before ordinary subcommand dispatch, exactly like the runtime handshake, and
// reports handled=false for every other argv.
func handleWorkspaceProbe(args []string, stdin io.Reader, stdout io.Writer) (bool, error) {
	return wsproto.ServeProbe(args, stdin, stdout, func(request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.ProbeResult{}, fmt.Errorf("resolve workspace root: %w", err)
		}
		return probeGoWorkspace(root, request)
	})
}

// goCandidate is one probed directory that carries a Go module.
type goCandidate struct {
	// path is the repo-relative candidate directory core asked about.
	path string
	// modFile is the repo-relative go.mod the identity was read from. It is
	// derived from the file's real location rather than assumed to be
	// path+"/go.mod", so a future nested module root needs no protocol change.
	modFile string
	// modDir is the repo-relative directory the go.mod lives in.
	modDir string
	// mod is the parsed manifest.
	mod *toolchain.GoModFile
}

// probeGoWorkspace answers one request against the tree rooted at root.
//
// Core spawns the probe with its working directory set to the workspace root
// and every path in the exchange repo-relative, so root is the only absolute
// value in play and it never reaches the answer.
func probeGoWorkspace(root string, request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	result := wsproto.ProbeResult{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: goExtensionName,
	}
	workspace := readGoWorkspaceDocument(filepath.Join(root, goWorkspaceConfigFile))

	// Candidates are walked in sorted order so first-writer-wins on the
	// pathological duplicate-module case does not depend on map iteration.
	ordered := append([]string(nil), request.Paths...)
	sort.Strings(ordered)

	candidates := make([]*goCandidate, 0, len(ordered))
	byModule := make(map[string]*goCandidate, len(ordered))
	// byDir indexes EVERY candidate directory, including those with no go.mod.
	// Core's index is over every project, and a local replace that points at a
	// directory core knows produces an edge there; indexing only Go directories
	// would silently drop it.
	byDir := make(map[string]string, len(ordered))

	for _, candidate := range ordered {
		normalized, ok := wsproto.NormalizeProbePath(candidate)
		if !ok {
			continue
		}
		byDir[normalized] = normalized
	}

	for _, candidate := range ordered {
		normalized, ok := wsproto.NormalizeProbePath(candidate)
		if !ok {
			continue
		}
		found, err := readGoCandidate(root, normalized)
		if err != nil {
			// A malformed go.mod is REPORTED and the directory is skipped.
			// Guessing at a half-parsed manifest would invent identity;
			// dropping it silently would make a project vanish with no
			// diagnostic, which is the silent-project-loss failure this repository already
			// paid for once.
			result.Diagnostics = append(result.Diagnostics,
				diag.Warningf("invalid-manifest", goMarkerPathFor(normalized), "%v", err))
			continue
		}
		if found == nil {
			continue
		}
		candidates = append(candidates, found)
		if found.mod.Module != "" {
			if _, exists := byModule[found.mod.Module]; !exists {
				byModule[found.mod.Module] = found
			}
		}
	}

	for _, candidate := range candidates {
		project := goProbeProjectFor(root, candidate, byModule, byDir, workspace)
		if candidate.modFile == "" {
			project.Type = goImageProjectType
			result.Projects = append(result.Projects, project)
			continue
		}
		classified, err := goProjectType(root, candidate)
		if err != nil {
			// Classification is REPORTED and left empty rather than guessed.
			// Empty is core's "application" default, which is what every Go
			// project got before this probe classified anything — so an
			// unreadable tree degrades to the old behavior instead of
			// silently demoting a workload to a library and taking its
			// serve, run, and infra aggregation with it.
			result.Diagnostics = append(result.Diagnostics,
				diag.Warningf("classification-failed", candidate.path, "%v", err))
		} else {
			project.Type = classified
		}
		result.Projects = append(result.Projects, project)
	}

	wsproto.NormalizeProbeResult(&result)
	return result, nil
}

// readGoCandidate reads one candidate's go.mod or an authored image project's
// Docker publication intent. It returns (nil, nil) for other directories and
// an error when an existing module manifest cannot be read or parsed.
func readGoCandidate(root, candidate string) (*goCandidate, error) {
	rel := goMarkerPathFor(candidate)
	mod, err := toolchain.ReadGoMod(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("go.mod could not be read: %w", err)
	}
	if mod == nil {
		document, found := readGoProjectDocument(filepath.Join(root, filepath.FromSlash(goProjectConfigPathFor(candidate))))
		if found && document.Type == goImageProjectType && document.publishes("docker") {
			return &goCandidate{path: candidate, modDir: candidate, mod: &toolchain.GoModFile{}}, nil
		}
		return nil, nil
	}
	if mod.Module == "" {
		return nil, fmt.Errorf("go.mod declares no module path")
	}
	return &goCandidate{
		path:    candidate,
		modFile: rel,
		modDir:  path.Dir(rel),
		mod:     mod,
	}, nil
}

// goProbeProjectFor renders one candidate's answer.
func goProbeProjectFor(
	root string,
	c *goCandidate,
	byModule map[string]*goCandidate,
	byDir map[string]string,
	workspace goWorkspaceDocument,
) wsproto.ProbeProject {
	sourceFile := c.modFile
	if sourceFile == "" {
		sourceFile = goProjectConfigPathFor(c.path)
	}
	watched := append([]string{sourceFile, goProjectConfigPathFor(c.path)}, goWorkspaceRootFiles...)
	if c.modFile == "" {
		watched = append(watched, goMarkerPathFor(c.path))
		_, scopeFiles := wsproto.LoadScopeChainWithSources(root, filepath.Join(root, filepath.FromSlash(c.path)))
		watched = append(watched, scopeFiles...)
	}
	dependencies := goDependencyPaths(c, byModule, byDir)
	return wsproto.ProbeProject{
		Path:       c.path,
		SourceName: c.mod.Module,
		SourceFile: sourceFile,
		// A project's own answer changes when its own go.mod changes; declaring
		// it keeps core's watch and snapshot machinery from having to know that
		// go.mod is what this provider reads.
		//
		// Classification also reads the module's .go package clauses. Their
		// recursive matched set is declared once by the adapter as
		// goClassificationInput, where core gives it snapshot-invalidation
		// semantics. Enumerating only today's source files here could not witness
		// creation of the first package main, which is the transition that changes
		// Type and therefore which build pipeline is planned.
		WatchedFiles:      watched,
		Dependencies:      dependencies,
		DependencySources: goDependencySources(root, c, dependencies, byModule),
		Metadata:          goReleaseSetMetadata(root, c, byModule, workspace),
	}
}

// goReleaseSetMetadata records only the internal module paths that the staged
// go.mod will actually require. Replace-only edges deliberately stay in
// Dependencies above: they are required for impact and cache correctness, but
// are stripped from published modules and therefore cannot be release-set
// dependency edges.
func goReleaseSetMetadata(
	root string,
	c *goCandidate,
	byModule map[string]*goCandidate,
	workspace goWorkspaceDocument,
) json.RawMessage {
	document, found := readGoProjectDocument(filepath.Join(root, filepath.FromSlash(goProjectConfigPathFor(c.path))))
	seen := make(map[string]bool, len(c.mod.Requires))
	dependencies := make([]string, 0, len(c.mod.Requires))
	for _, required := range c.mod.Requires {
		target, internal := byModule[required]
		if !internal || target == c || seen[required] {
			continue
		}
		seen[required] = true
		dependencies = append(dependencies, required)
	}
	sort.Strings(dependencies)
	// One declaration per artifact this project is configured to publish. A
	// module that only happens to contain Go sources is not a Go release-set
	// member; likewise a Docker-only service contributes its image but not its
	// module. It matches the jobs the project plans.
	var ecosystems []releaseset.MemberDeclaration
	if found && c.mod.Module != "" && document.publishes("go") {
		ecosystems = append(ecosystems, releaseset.MemberDeclaration{
			Ecosystem:    releaseplan.GoEcosystem,
			Coordinate:   c.mod.Module,
			PackageStep:  "go",
			PublishStep:  "go",
			Dependencies: dependencies,
		})
	}
	if repository := goImageCoordinate(root, document, found, c, workspace); repository != "" {
		packageStep := document.Options.GoPublish.DockerPackageStep
		if packageStep == "" {
			packageStep = "docker"
			if document.Type == goImageProjectType {
				packageStep = "image"
			}
		}
		ecosystems = append(ecosystems, releaseset.MemberDeclaration{
			Ecosystem:        releaseplan.OCIEcosystem,
			Coordinate:       repository,
			PackagePublisher: document.Options.GoPublish.DockerPackagePublisher,
			PackageStep:      packageStep,
			PublishStep:      "docker",
		})
	}
	metadata := map[string]any{
		releaseset.ProjectMetadataKey: releaseset.ProjectMetadata{Ecosystems: ecosystems},
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil
	}
	return encoded
}

// goProjectConfigPathFor is the repo-relative putnami.json of a candidate.
func goProjectConfigPathFor(candidate string) string {
	if candidate == "" || candidate == wsproto.ProbeRootPath {
		return goProjectConfigFile
	}
	return path.Join(candidate, goProjectConfigFile)
}

// goProjectDocument is the slice of a project document the probe reads: whether
// the project is an authored image, whether it publishes a container image, and
// its own `registries` override and OCI package route. Nothing else is decoded, so an unrelated key
// cannot change the answer.
type goProjectDocument struct {
	Name       string                     `json:"name"`
	Type       string                     `json:"type"`
	Publish    []string                   `json:"publish"`
	Registries map[string]json.RawMessage `json:"registries"`
	Options    struct {
		GoPublish struct {
			DockerPackagePublisher string `json:"docker-package-publisher"`
			DockerPackageStep      string `json:"docker-package-step"`
		} `json:"@putnami/go:publish"`
		Publish struct {
			Go     bool `json:"go"`
			Docker bool `json:"docker"`
		} `json:"publish"`
	} `json:"options"`
}

// publishes reports the project's publication intent. The top-level declaration
// activates package and publish together; a command-specific publish option is
// still publication intent even when its package configuration is malformed.
// Keeping that member visible lets planning reject the missing package route
// explicitly instead of silently omitting an artifact the user asked to publish.
func (d goProjectDocument) publishes(channel string) bool {
	for _, published := range d.Publish {
		if published == channel {
			return true
		}
	}
	switch channel {
	case "go":
		return d.Options.Publish.Go
	case "docker":
		return d.Options.Publish.Docker
	default:
		return false
	}
}

// goImageCoordinate is the oci repository path this project's image is
// published under, or "" when it publishes no image.
//
// It is the SAME string the shared Docker publisher reports as the member's
// coordinate: the repository path WITHOUT the registry host. Two independent
// facts build it — the image name, derived from the project name exactly as the
// packaging step derives it, and the registry namespace, from
// `registries.oci.publish` — and both are read from the tree.
//
// Authored image projects use the SDK's canonical managed target, including
// its workspace-name namespace when no OCI publish endpoint was authored.
func goImageCoordinate(root string, document goProjectDocument, found bool, c *goCandidate, workspace goWorkspaceDocument) string {
	if !found || !document.publishes("docker") {
		return ""
	}
	registries := workspace.Registries
	if len(document.Registries) > 0 {
		// A project `registries` entry REPLACES the workspace entry for that
		// ecosystem; it does not merge into it.
		if _, overridden := document.Registries["oci"]; overridden {
			registries = document.Registries
		}
	}
	repository := ociRepositoryPrefix(registries)
	name := goImageNameFor(document, c)
	if document.Type == goImageProjectType {
		scopeName := ""
		if scope, _ := wsproto.LoadScopeChainWithSources(root, filepath.Join(root, filepath.FromSlash(c.path))); scope != nil {
			scopeName = scope.ResolveNamePattern(filepath.Base(c.path))
		}
		name = goImageTargetComponent(wsproto.ResolveProjectName(document.Name, c.mod.Module, scopeName, filepath.Base(c.path)))
		endpoint := ociPublishEndpoint(registries)
		host, _, _ := strings.Cut(endpoint, "/")
		if endpoint == "" || (host == "oci.putnami.dev" && repository == "") {
			repository = goImageTargetComponent(workspace.Name)
		}
	}
	if repository == "" {
		return name
	}
	return repository + "/" + name
}

func goImageTargetComponent(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "@")
	return strings.ToLower(strings.ReplaceAll(strings.Trim(value, "/"), "/", "-"))
}

// goImageNameFor derives the image name from the project name the way the
// packaging step does: the leading "@" of a scoped name is dropped and every
// path separator becomes a hyphen, because an image name is one path segment.
//
// The NAME is resolved the way core resolves it, and for a Go project that is
// exactly two rules deep. Core's precedence is: explicit putnami.json name >
// the provider's source identity > the scope namePattern > the directory
// basename. This provider ALWAYS supplies a source identity — the module path —
// so a scope namePattern can never win for a Go project, and the two rules
// below are the whole answer.
func goImageNameFor(document goProjectDocument, c *goCandidate) string {
	name := strings.TrimSpace(document.Name)
	if name == "" {
		name = c.mod.Module
	}
	return strings.ReplaceAll(strings.TrimPrefix(name, "@"), "/", "-")
}

// readGoProjectDocument decodes the project document, or reports absent. An
// unreadable or malformed document yields no declaration rather than an error:
// core validates project documents itself, and a probe that failed here would
// take the whole workspace down over a file it does not own.
func readGoProjectDocument(path string) (goProjectDocument, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return goProjectDocument{}, false
	}
	var document goProjectDocument
	if json.Unmarshal(raw, &document) != nil {
		return goProjectDocument{}, false
	}
	return document, true
}

type goWorkspaceDocument struct {
	Name       string                     `json:"name"`
	Registries map[string]json.RawMessage `json:"registries"`
}

// readGoWorkspaceDocument reads the target namespace and OCI registry settings.
func readGoWorkspaceDocument(path string) goWorkspaceDocument {
	raw, err := os.ReadFile(path)
	if err != nil {
		return goWorkspaceDocument{}
	}
	var document goWorkspaceDocument
	if json.Unmarshal(raw, &document) != nil {
		return goWorkspaceDocument{}
	}
	return document
}

// ociRepositoryPrefix is the namespace part of `registries.oci.publish`: the
// declared endpoint minus its registry HOST, which is where a copy lives rather
// than what a member is. An endpoint that is a bare host yields "".
func ociRepositoryPrefix(registries map[string]json.RawMessage) string {
	_, namespace, _ := strings.Cut(ociPublishEndpoint(registries), "/")
	return namespace
}

func ociPublishEndpoint(registries map[string]json.RawMessage) string {
	entry, ok := registries["oci"]
	if !ok || len(entry) == 0 {
		return ""
	}
	var profile struct {
		Publish string `json:"publish"`
	}
	if json.Unmarshal(entry, &profile) != nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(profile.Publish), "/")
}

// Go project classification.
const (
	goTypeApplication = "application"
	goTypeLibrary     = "library"
)

// goProjectType classifies one candidate as an application or a library.
//
// The rule is the only one Go actually gives us: a module that contains a
// `package main` can produce an executable and is an APPLICATION; a module that
// contains none can only be imported and is a LIBRARY. Nothing is authored,
// nothing is inferred from the directory's name or its position in the tree —
// an authored putnami.json `type` still outranks this, because core applies the
// explicit value before the provider's.
//
// Leaving this empty is not neutral. Core reads an unclassified project as an
// application, so every Go library in the workspace would plan `serve` and
// `run` it has no entrypoint for, join infra aggregation it contributes nothing
// to, and ask for platform compiles it can never produce.
//
// The scan is rooted at the go.mod's own directory rather than at the candidate
// path, for the same reason every other path in this file is: the project root
// is not assumed to be the module root.
func goProjectType(root string, c *goCandidate) (string, error) {
	hasMain, err := gosource.ModuleHasMain(filepath.Join(root, filepath.FromSlash(c.modDir)))
	if err != nil {
		return "", fmt.Errorf("classify %s: %w", c.modDir, err)
	}
	if hasMain {
		return goTypeApplication, nil
	}
	return goTypeLibrary, nil
}

// goDependencyPaths resolves one module's require and replace directives onto
// the repo-relative paths of the candidates that answer for them.
//
// Two independent signals resolve an edge, because a local go.mod replace
// carries a placeholder version and the require line alone is not enough:
//
//   - a require's module path → the candidate whose go.mod declares it;
//   - a replace target → for a local (`./`, `../`) target, the candidate
//     directory it resolves to; for a module-path target, the module index.
//
// A module no candidate answers for contributes nothing: it is an external
// dependency, and inventing a path for it would make core resolve an edge to a
// directory that is not a project. A self-edge is dropped for the same reason
// core drops one.
func goDependencyPaths(c *goCandidate, byModule map[string]*goCandidate, byDir map[string]string) []string {
	seen := make(map[string]bool, len(c.mod.Requires)+len(c.mod.Replaces))
	var paths []string
	add := func(target string) {
		if target == "" || target == c.path || seen[target] {
			return
		}
		seen[target] = true
		paths = append(paths, target)
	}

	for _, required := range c.mod.Requires {
		if target, ok := byModule[required]; ok {
			add(target.path)
		}
	}
	for _, replace := range c.mod.Replaces {
		if !replace.NewLocal {
			if target, ok := byModule[replace.NewPath]; ok {
				add(target.path)
			}
			continue
		}
		// A local replace is relative to the go.mod's own directory, which is
		// why the resolution starts at modDir rather than at the project path.
		resolved, ok := wsproto.NormalizeProbePath(path.Join(c.modDir, filepath.ToSlash(replace.NewPath)))
		if !ok {
			// The target escapes the workspace root — it is not a workspace
			// project, and a path outside the root cannot travel on the wire.
			continue
		}
		if target, ok := byDir[resolved]; ok {
			add(target)
		}
	}

	sort.Strings(paths)
	return paths
}

// goDependencySources attributes every reported edge this provider answers
// with, and the attribution is the module graph's answer to one question: does
// a source file of this module import that module?
//
// An edge a source file imports is `go-module` — the build really reads it. An
// edge only go.mod states is `declared`: the requirement is available to the
// build and nothing uses it, which is the same standing a putnami.json entry
// has. The edge SET is unchanged; a require stays an edge until someone removes
// it from go.mod, and the consumers that act on real imports alone — the
// visibility check, the declared-edge check — read the attribution rather than
// re-deriving it.
//
// One scan serves every edge of the module, and it is skipped entirely for a
// module with no workspace edge to attribute.
func goDependencySources(root string, c *goCandidate, dependencies []string,
	byModule map[string]*goCandidate) map[string]wsproto.DependencySource {
	if len(dependencies) == 0 {
		return nil
	}
	moduleByPath := make(map[string]string, len(byModule))
	// providers is every module an import may resolve to: the workspace's and
	// every module this go.mod requires, so a module nested under a wanted
	// path answers for its own packages wherever it lives.
	providers := append(make([]string, 0, len(byModule)+len(c.mod.Requires)), c.mod.Requires...)
	for module, candidate := range byModule {
		moduleByPath[candidate.path] = module
		providers = append(providers, module)
	}
	direct := directRequires(c)
	wanted := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		if module, ok := moduleByPath[dependency]; ok && direct[module] {
			wanted = append(wanted, module)
		}
	}
	scan := scanModuleImports(goModuleDirOf(root, c), wanted, providers)

	sources := make(map[string]wsproto.DependencySource, len(dependencies))
	for _, dependency := range dependencies {
		module, known := moduleByPath[dependency]
		// Two edges are attributed to the module graph without a scan. A
		// dependency path no candidate module answers to is one this provider
		// resolved onto a directory it does not know as a module. An
		// `// indirect` requirement is the module graph's own need rather than
		// this module's declaration, and `go mod tidy` puts it back.
		if !known || !direct[module] || scan.importsModule(module) {
			sources[dependency] = wsproto.DependencySourceGoModule
			continue
		}
		sources[dependency] = wsproto.DependencySourceDeclared
	}
	return sources
}

// directRequires is the set of modules this go.mod requires on its own behalf:
// every require the file does not annotate `// indirect`.
func directRequires(c *goCandidate) map[string]bool {
	direct := make(map[string]bool, len(c.mod.Requires))
	for _, required := range c.mod.Requires {
		if !c.mod.Indirect[required] {
			direct[required] = true
		}
	}
	return direct
}

// goModuleDirOf is the absolute directory of a candidate's go.mod.
func goModuleDirOf(root string, c *goCandidate) string {
	if c.modDir == "" || c.modDir == wsproto.ProbeRootPath {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(c.modDir))
}

// goMarkerPathFor is the repo-relative go.mod path for a candidate directory.
func goMarkerPathFor(candidate string) string {
	if candidate == "" || candidate == wsproto.ProbeRootPath {
		return goWorkspaceMarker
	}
	return path.Join(candidate, goWorkspaceMarker)
}
