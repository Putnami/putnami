package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/typescript/extension/internal/build"
)

// The TypeScript workspace probe.
//
// This is where "what is a TypeScript project?" moves out of the orchestrator.
// Core asks once, with the candidate directories it already knows about; this
// answers with what package.json says about each of them, and nothing else.
//
// Three properties are load-bearing, and each one is a rule core cannot enforce
// from the outside:
//
//   - PURE. The answer is a function of the tree alone. No clock, no absolute
//     path, no environment lookup, no network, no bun invocation. The answer's
//     digest keys core's workspace snapshot, so a byte that varies between two
//     runs over one tree is a cache-correctness bug rather than a cosmetic one.
//
//   - PATHS, NOT NAMES. Dependency edges leave here as repo-relative project
//     PATHS. Resolving `workspace:` specifiers to paths is the part only this
//     side can do — it knows the npm naming rules — and keying the wire on paths
//     is what lets core own canonical identity without learning them.
//
//   - SOLE OWNER. Precedence: `putnami.dependencies` when present, else
//     `workspace:` dependencies; `putnami.*` for type/tags/publish/runsWith;
//     devDependencies for requested extensions. What this file answers IS what
//     the workspace resolves, and its digest keys the cache.

// tsExtensionName is the identity this extension answers under. It keys the
// provider-owned metadata bucket in core's merged view.
const tsExtensionName = "@putnami/typescript"

// workspaceMarkerFile is the manifest whose presence marks a TypeScript
// project. It matches the `workspace.markers` entry in putnami.extension.json;
// the manifest contract test pins the two together.
const workspaceMarkerFile = "package.json"

// workspaceDocumentFile is core's own project document. This probe reads
// publication identity and intent from it (see probeDocument); it owns none of it.
const workspaceDocumentFile = "putnami.json"

// typeScriptWorkspaceRootFiles are the dependency-resolution inputs shared by
// every package in the Bun workspace. They ride the existing per-project
// watchedFiles member so newer providers stay readable by strict v1 cores while
// core still learns no TypeScript-specific filename vocabulary.
var typeScriptWorkspaceRootFiles = []string{"bun.lock", "bun.lockb", workspaceMarkerFile, "putnami.workspace.json"}

// probeManifest is the subset of package.json this probe reads. It is
// deliberately NOT strict: a package.json carries dozens of members this
// extension has no opinion about, and rejecting an unknown one would make an
// ordinary manifest undiscoverable.
type probeManifest struct {
	Name                 string            `json:"name"`
	Main                 string            `json:"main"`
	Bin                  json.RawMessage   `json:"bin"`
	Exports              json.RawMessage   `json:"exports"`
	Workspaces           json.RawMessage   `json:"workspaces"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	Putnami              *probePutnami     `json:"putnami"`
}

// probePutnami is the `putnami` section a package.json may carry.
type probePutnami struct {
	Type         string   `json:"type"`
	Tags         []string `json:"tags"`
	Publish      []string `json:"publish"`
	Dependencies []string `json:"dependencies"`
	RunsWith     []string `json:"runsWith"`
}

// handleWorkspaceProbe answers core's reserved probe invocation. It is called
// before ordinary subcommand dispatch, exactly like the runtime handshake, and
// reports handled=false for every other argv.
func handleWorkspaceProbe(args []string) (bool, error) {
	return wsproto.ServeProbe(args, os.Stdin, os.Stdout, func(request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
		root, err := os.Getwd()
		if err != nil {
			return wsproto.ProbeResult{}, fmt.Errorf("resolve workspace root: %w", err)
		}
		return probeTypeScriptWorkspace(root, request)
	})
}

// probeTypeScriptWorkspace answers one request against the tree rooted at root.
//
// Core spawns the probe with its working directory set to the workspace root
// and every path in the exchange repo-relative, so root is the only absolute
// value in play and it never reaches the answer.
func probeTypeScriptWorkspace(root string, request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	result := wsproto.ProbeResult{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: tsExtensionName,
	}

	manifests := make(map[string]*probeManifest, len(request.Paths))
	documents := make(map[string]*probeDocument, len(request.Paths))
	byName := make(map[string]string, len(request.Paths))
	workspaceRepository := ociRepositoryPrefix(readWorkspaceRegistries(root))

	// Candidates are normalized and de-duplicated before they are read; sorting
	// follows normalization; an unrepresentable path is skipped and request
	// validation reports it.
	orderedPaths := make([]string, 0, len(request.Paths))
	seen := make(map[string]bool, len(request.Paths))
	for _, candidate := range request.Paths {
		normalized, ok := wsproto.NormalizeProbePath(candidate)
		if !ok || seen[normalized] {
			continue
		}
		seen[normalized] = true
		orderedPaths = append(orderedPaths, normalized)
	}
	sort.Strings(orderedPaths)

	for _, candidate := range orderedPaths {
		manifest, err := readProbeManifest(root, candidate)
		if err != nil {
			// A malformed manifest is reported and the directory is skipped.
			// Guessing at a half-parsed package.json would invent identity;
			// dropping it silently would make a project vanish with no
			// diagnostic, which is the silent-project-loss failure this repository already
			// paid for once.
			result.Diagnostics = append(result.Diagnostics, probeWarning(candidate, err))
			continue
		}
		if manifest == nil {
			continue
		}
		manifests[candidate] = manifest
		documents[candidate] = readProbeDocument(root, candidate)
		if manifest.Name != "" {
			// First writer wins on the pathological duplicate-name case, and
			// candidates are walked in sorted order, so the winner does not
			// depend on map iteration.
			if _, exists := byName[manifest.Name]; !exists {
				byName[manifest.Name] = candidate
			}
		}
	}

	for _, candidate := range orderedPaths {
		manifest, ok := manifests[candidate]
		if !ok {
			continue
		}
		result.Projects = append(result.Projects, probeProjectFor(root, candidate, manifest, documents[candidate], byName, workspaceRepository))
	}

	wsproto.NormalizeProbeResult(&result)
	return result, nil
}

// readProbeManifest reads one candidate's package.json. It returns (nil, nil)
// when the directory carries no manifest — the ordinary "not a TypeScript
// project" answer — and an error only when a manifest exists and cannot be read.
//
// candidate must already be NORMALIZED (see probeTypeScriptWorkspace): the root
// guard below compares it against ProbeRootPath by value, so a raw "./" would
// read the root manifest and then walk straight past the guard.
func readProbeManifest(root, candidate string) (*probeManifest, error) {
	rel := workspaceMarkerFile
	if candidate != "" && candidate != wsproto.ProbeRootPath {
		rel = path.Join(candidate, workspaceMarkerFile)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var manifest probeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("package.json is not valid JSON: %w", err)
	}
	if len(manifest.Workspaces) > 0 && candidate == wsproto.ProbeRootPath {
		// A root manifest that declares `workspaces` is the workspace's own
		// manifest, not a package member. Claiming it as a project would give
		// the workspace root an npm identity it does not have.
		return nil, nil
	}
	return &manifest, nil
}

// probeProjectFor renders one candidate's answer.
func probeProjectFor(root, candidate string, manifest *probeManifest, document *probeDocument,
	byName map[string]string, workspaceRepository string) wsproto.ProbeProject {
	project := wsproto.ProbeProject{
		Path:       candidate,
		SourceName: manifest.Name,
		SourceFile: markerPathFor(candidate),
		// Every project's own answer changes when its own manifest changes;
		// declaring it keeps core's watch and snapshot machinery from having
		// to know that package.json is what this provider reads.
		//
		// putnami.json is declared for the same reason: an oci member below is a
		// function of its options.publish.docker, so a change to that file has to
		// re-probe this project or the release set would keep planning against
		// the previous answer.
		WatchedFiles: append([]string{markerPathFor(candidate), documentPathFor(candidate)}, typeScriptWorkspaceRootFiles...),
	}

	var declaredDeps []string
	if manifest.Putnami != nil {
		project.Type = manifest.Putnami.Type
		project.Tags = manifest.Putnami.Tags
		project.Publish = manifest.Putnami.Publish
		project.RunsWith = manifest.Putnami.RunsWith
		declaredDeps = manifest.Putnami.Dependencies
	}
	// The package's own `workspace:` dependencies are resolved WHATEVER the
	// precedence decides, because they are the candidates for an import: the
	// `putnami` block overrides which edges are reported, never what the
	// package really reads. Splitting the two here is what lets a consumer act
	// on imports alone without re-reading this manifest — and it changes no
	// edge, because the reported set stays exactly the one precedence picked.
	linked := workspaceProtocolDeps(manifest.Dependencies)
	linkedDeps := resolveDependencyPaths(candidate, linked, byName)
	if len(declaredDeps) == 0 {
		project.Dependencies = linkedDeps
	} else {
		project.Dependencies = resolveDependencyPaths(candidate, declaredDeps, byName)
	}
	importedDeps := resolveDependencyPaths(candidate, importedWorkspacePackages(root, candidate, linked, generatedPackageImports(manifest), byName), byName)
	project.DependencySources = tsDependencySources(project.Dependencies, importedDeps)
	project.Extensions = workspaceProtocolDeps(manifest.DevDependencies)
	project.Metadata = probeMetadataFor(candidate, manifest, document, byName, workspaceRepository)
	return project
}

func markerPathFor(candidate string) string {
	return candidatePathFor(candidate, workspaceMarkerFile)
}

func documentPathFor(candidate string) string {
	return candidatePathFor(candidate, workspaceDocumentFile)
}

func candidatePathFor(candidate, name string) string {
	if candidate == "" || candidate == wsproto.ProbeRootPath {
		return name
	}
	return path.Join(candidate, name)
}

// probeDocument is the subset of a project's putnami.json this probe reads.
//
// Core owns that file. The publication intent, name and registry namespace read
// here determine the exact native coordinate this provider declares.
type probeDocument struct {
	Name       string                     `json:"name"`
	Publish    []string                   `json:"publish"`
	Registries map[string]json.RawMessage `json:"registries"`
	Options    struct {
		Publish struct {
			NPM    bool `json:"npm"`
			Docker bool `json:"docker"`
		} `json:"publish"`
	} `json:"options"`
}

// readProbeDocument reads one candidate's putnami.json. An absent or malformed
// document yields nil: core validates that file and reports its defects, and a
// provider that failed the whole probe over it would make a project vanish for
// a reason core already has a better diagnostic for.
func readProbeDocument(root, candidate string) *probeDocument {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(documentPathFor(candidate))))
	if err != nil {
		return nil
	}
	var document probeDocument
	if json.Unmarshal(data, &document) != nil {
		return nil
	}
	return &document
}

// publishesDocker reports whether the project declares an image publication.
func (d *probeDocument) publishesDocker() bool {
	return d != nil && d.publishes("docker")
}

// publishes reports the project's publication intent. A top-level channel
// activates package and publish together; a command-specific publish option is
// still intent even when its package configuration is malformed. The latter is
// kept visible so planning can report the missing package route instead of
// silently losing a requested release-set member.
func (d *probeDocument) publishes(channel string) bool {
	if d == nil {
		return false
	}
	for _, published := range d.Publish {
		if published == channel {
			return true
		}
	}
	switch channel {
	case "npm":
		return d.Options.Publish.NPM
	case "docker":
		return d.Options.Publish.Docker
	default:
		return false
	}
}

// projectName is the identity the image repository is derived from: the
// explicit putnami.json name when there is one, otherwise the package name,
// which is exactly the precedence core applies when it names the project.
func (d *probeDocument) projectName(manifest *probeManifest) string {
	if d != nil && d.Name != "" {
		return d.Name
	}
	return manifest.Name
}

// ociRepositoryName flattens a project name into the image repository the
// packager writes: the scope marker is dropped and every separator becomes a
// dash, so one project yields one repository component under whatever registry
// namespace publish is pointed at.
func ociRepositoryName(projectName string) string {
	name := strings.ReplaceAll(strings.TrimPrefix(projectName, "@"), "/", "-")
	return strings.Trim(name, "-")
}

func readWorkspaceRegistries(root string) map[string]json.RawMessage {
	data, err := os.ReadFile(filepath.Join(root, "putnami.workspace.json"))
	if err != nil {
		return nil
	}
	var document probeDocument
	if json.Unmarshal(data, &document) != nil {
		return nil
	}
	return document.Registries
}

func ociRepositoryPrefix(registries map[string]json.RawMessage) string {
	var profile struct {
		Publish string `json:"publish"`
	}
	if json.Unmarshal(registries["oci"], &profile) != nil {
		return ""
	}
	endpoint := strings.Trim(strings.TrimSpace(profile.Publish), "/")
	_, namespace, _ := strings.Cut(endpoint, "/")
	return namespace
}

// workspaceProtocolDeps returns the dependency NAMES declared with the
// `workspace:` protocol, sorted. Map iteration order is random and this list
// reaches a digest, so sorting is a correctness requirement.
func workspaceProtocolDeps(deps map[string]string) []string {
	var names []string
	for name, spec := range deps {
		if strings.HasPrefix(spec, "workspace:") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// resolveDependencyPaths maps declared dependency NAMES onto the repo-relative
// paths of the candidates that answer to them.
//
// A name no candidate answers to contributes nothing: it is an external package
// (or a project outside the probed set), and inventing a path for it would make
// core resolve an edge to a directory that is not a project. A self-edge is
// dropped for the same reason core drops one.
func resolveDependencyPaths(candidate string, names []string, byName map[string]string) []string {
	var paths []string
	for _, name := range names {
		target, ok := byName[name]
		if !ok || target == candidate {
			continue
		}
		paths = append(paths, target)
	}
	sort.Strings(paths)
	return paths
}

// importedWorkspacePackages narrows the package's `workspace:` dependencies to
// the ones its sources import: its own committed sources, which the scan reads,
// and the generated sources a build writes into .gen, whose imports generated
// names.
//
// A name no candidate answers to is dropped before the scan: it is an external
// package, and this provider reports no edge for it either.
func importedWorkspacePackages(root, candidate string, linked, generated []string, byName map[string]string) []string {
	var wanted, scanned []string
	for _, name := range linked {
		if target, ok := byName[name]; ok && target != candidate {
			wanted = append(wanted, name)
			if !slices.Contains(generated, name) {
				scanned = append(scanned, name)
			}
		}
	}
	scan := scanPackageImports(packageDirOf(root, candidate), scanned)
	imported := make([]string, 0, len(wanted))
	for _, name := range wanted {
		if slices.Contains(generated, name) || scan.importsPackage(name) {
			imported = append(imported, name)
		}
	}
	return imported
}

// generatedPackageImports names the packages the project's generated sources
// import. The scan cannot see them: it skips .gen so that a cold clone and a
// warm checkout agree about one commit. The answer is derived from the
// manifest instead, from the rule the build follows.
//
// A project that lists build.BundledServeHookPackage among its dependencies or
// devDependencies runs that package's preBuild hook, and the build writes a
// serve entry that imports build.BundledServeImports. The rule over-approximates
// in the safe direction: a project whose hook reports no capability manifest,
// or that has no entry file, gets no serve entry, and keeps an edge the
// manifest already states.
func generatedPackageImports(manifest *probeManifest) []string {
	hook := build.BundledServeHookPackage
	if _, ok := manifest.Dependencies[hook]; ok {
		return build.BundledServeImports()
	}
	if _, ok := manifest.DevDependencies[hook]; ok {
		return build.BundledServeImports()
	}
	return nil
}

// packageDirOf is the absolute directory of a probed candidate.
func packageDirOf(root, candidate string) string {
	if candidate == "" || candidate == wsproto.ProbeRootPath {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(candidate))
}

// tsDependencySources attributes each reported edge: package-json for the ones
// the package's own dependencies link with a `workspace:` specifier AND its
// sources import, declared for the rest — a `putnami` block states an edge
// without the package importing anything, and so does a `workspace:` entry no
// source file reads.
//
// An import the reported set does not carry is attributed to nothing: the
// protocol refuses provenance for an edge the answer does not report, and this
// answer's edges are the ones precedence picked.
func tsDependencySources(reported, imported []string) map[string]wsproto.DependencySource {
	if len(reported) == 0 {
		return nil
	}
	isImport := make(map[string]bool, len(imported))
	for _, path := range imported {
		isImport[path] = true
	}
	sources := make(map[string]wsproto.DependencySource, len(reported))
	for _, path := range reported {
		if isImport[path] {
			sources[path] = wsproto.DependencySourcePackageJSON
			continue
		}
		sources[path] = wsproto.DependencySourceDeclared
	}
	return sources
}

// probeMetadataFor carries the TypeScript-shaped project fields core currently
// keeps on its own Project struct. They land under
// `project.metadata["@putnami/typescript"]`, namespaced so no other provider
// can collide with them, which is what lets job context v2 drop its
// language-specific project members.
//
// Absent members are omitted rather than encoded as null: an omitted member and
// a null one would digest differently while meaning the same thing.
func probeMetadataFor(candidate string, manifest *probeManifest, document *probeDocument, byName map[string]string, workspaceRepository string) json.RawMessage {
	metadata := make(map[string]any, 4)
	if manifest.Main != "" {
		metadata["main"] = manifest.Main
	}
	if len(manifest.Bin) > 0 {
		metadata["bin"] = manifest.Bin
	}
	if len(manifest.Exports) > 0 {
		metadata["exports"] = manifest.Exports
	}
	// One declaration per artifact this project is configured to publish: an npm
	// member for an npm publication, and an oci member for a Docker publication.
	// Merely having a package name does not make it a release-set member: the
	// planned publish jobs must be able to emit every selected member.
	//
	// The coordinate is the artifact's NAME in its ecosystem, not the project
	// path: a release-set member is keyed by (ecosystem, coordinate). npm
	// resolves the package name; the oci repository is the flattened project
	// name the packager writes into the local image candidate. A project without
	// either name declares no member rather than an unnamed one.
	//
	// `uses: ["oci"]` in this extension's manifest is what makes the second
	// declaration legitimate: the SDK owns the oci profile, because both
	// language extensions publish images through the same shared publisher.
	declaration := releaseset.ProjectMetadata{}
	publishesNPM := document.publishes("npm")
	if manifest.Putnami != nil && slices.Contains(manifest.Putnami.Publish, "npm") {
		publishesNPM = true
	}
	if publishesNPM && manifest.Name != "" {
		declaration.Ecosystems = append(declaration.Ecosystems, releaseset.MemberDeclaration{
			Ecosystem:    "npm",
			Coordinate:   manifest.Name,
			PackageStep:  "npm",
			PublishStep:  "npm",
			Dependencies: npmReleaseSetDependencies(candidate, manifest, byName),
		})
	}
	publishesDocker := document.publishesDocker()
	if manifest.Putnami != nil && slices.Contains(manifest.Putnami.Publish, "docker") {
		publishesDocker = true
	}
	if publishesDocker {
		if repository := ociRepositoryName(document.projectName(manifest)); repository != "" {
			namespace := workspaceRepository
			if document != nil {
				if _, overridden := document.Registries["oci"]; overridden {
					namespace = ociRepositoryPrefix(document.Registries)
				}
			}
			if namespace != "" {
				repository = namespace + "/" + repository
			}
			declaration.Ecosystems = append(declaration.Ecosystems, releaseset.MemberDeclaration{
				Ecosystem:   "oci",
				Coordinate:  repository,
				PackageStep: "docker",
				PublishStep: "docker",
			})
		}
	}
	metadata[releaseset.ProjectMetadataKey] = declaration
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil
	}
	return encoded
}

// npmReleaseSetDependencies is the exact internal dependency surface retained
// in a published package.json. It is deliberately independent of the wider
// build graph above: authored putnami.dependencies may tune scheduling, while
// peer and optional dependencies still need exact release-set versions even
// when they do not create build-order edges.
func npmReleaseSetDependencies(candidate string, manifest *probeManifest, byName map[string]string) []string {
	seen := make(map[string]bool)
	var dependencies []string
	add := func(values map[string]string) {
		for name := range values {
			target, internal := byName[name]
			if !internal || target == candidate || seen[name] {
				continue
			}
			seen[name] = true
			dependencies = append(dependencies, name)
		}
	}
	add(manifest.Dependencies)
	add(manifest.PeerDependencies)
	add(manifest.OptionalDependencies)
	sort.Strings(dependencies)
	return dependencies
}

func probeWarning(candidate string, err error) diag.Diagnostic {
	return diag.Warningf("invalid-manifest", markerPathFor(candidate), "%v", err)
}
