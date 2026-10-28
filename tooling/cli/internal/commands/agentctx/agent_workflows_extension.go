package agentctx

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// Extension-owned agent content: the one kind of `agentArtifacts` entry.
//
// `extension:<name>` opts the workspace into the agent-content contribution of
// the extension it declares under <name>. The entry names no version and no
// source of its own — the content is the one the resolved extension ships, so
// there is exactly one version to resolve, pin and upgrade — and it is never
// pinned under `agentArtifacts`: the extension's own lock entry is the pin.
// Everything after resolution is the materializer's ordinary path: the same
// plan, the same run-wide collision and overlap checks, the same ownership
// record, the same retirement when the entry is removed.
//
// Declaring the extension is not enough. An extension that carries content
// changes nothing in a workspace until this entry opts in, so installing an
// unrelated extension never activates its workflows.

// extensionAgentContentPrefix marks an `agentArtifacts` entry that opts into
// an extension's agent-content contribution.
const extensionAgentContentPrefix = "extension:"

// parseExtensionAgentContentReference parses an `extension:<name>` entry. The
// bool reports whether declared uses the prefix at all.
func parseExtensionAgentContentReference(declared string) (AgentArtifactRef, bool, error) {
	rest, ok := strings.CutPrefix(declared, extensionAgentContentPrefix)
	if !ok {
		return AgentArtifactRef{}, false, nil
	}
	name := strings.TrimSpace(rest)
	if name == "" {
		return AgentArtifactRef{}, true, fmt.Errorf("agent artifact entry %q names no extension", declared)
	}
	if strings.ContainsAny(name, ": ") {
		return AgentArtifactRef{}, true, fmt.Errorf(
			"agent artifact entry %q must name only the extension: its agent content always has the version the extension resolves to", declared)
	}
	// The name becomes a path component of the ownership record and of the
	// staged build, so it is held to the manifest's own name grammar.
	if !extensionNameFormat.MatchString(name) {
		return AgentArtifactRef{}, true, fmt.Errorf(
			"agent artifact entry %q must name an extension as @scope/name or name (lowercase letters, digits and hyphens)", declared)
	}
	return AgentArtifactRef{Name: name, Extension: name}, true, nil
}

// AgentArtifactDeclarationName names the extension an `extension:<name>`
// opt-in declares. It returns "" for an entry that is not a valid opt-in. It
// installs nothing; `putnami init --force` uses it to merge declarations by
// what they name rather than by how they are spelled.
func AgentArtifactDeclarationName(declared string) string {
	ref, ok, err := parseExtensionAgentContentReference(strings.TrimSpace(declared))
	if !ok || err != nil {
		return ""
	}
	return ref.Name
}

// legacyAgentArtifactDeclarationsError refuses the entries in the forms
// earlier releases installed — a registry artifact (`name` or
// `name:constraint`) or an in-tree project (`/path`) — with nothing written,
// and gives the steps that replace them in this workspace. The entries are
// grouped by the extension whose content supersedes them, and each group gets
// its own steps: its entries are deleted when that extension is already opted
// into, and replaced otherwise; the extension is declared when it is not yet.
// `putnami install` then drops the lock pins the entries left.
func legacyAgentArtifactDeclarationsError(wsRoot string, cfg *wsproto.Config, legacy []string) error {
	quoted := make([]string, 0, len(legacy))
	for _, declared := range legacy {
		quoted = append(quoted, fmt.Sprintf("%q", declared))
	}
	sources := declaredSupersedingSources(wsRoot, cfg)
	var successors []string
	groups := map[string][]string{}
	for _, declared := range legacy {
		successor := supersedingExtension(wsRoot, sources, declared)
		if _, seen := groups[successor]; !seen {
			successors = append(successors, successor)
		}
		groups[successor] = append(groups[successor], fmt.Sprintf("%q", declared))
	}
	declaredNames := extension.DeclaredExtensionNames(wsRoot, cfg)
	steps := make([]string, 0, len(successors))
	for _, successor := range successors {
		var group []string
		if successor == "" {
			group = append(group, "replace each entry with extension:<name> for the extension whose agent content supersedes it",
				"declare that extension in extensions")
		} else {
			optIn := extensionAgentContentPrefix + successor
			if optsInto(cfg, successor) {
				group = append(group, "delete each entry, since "+optIn+" already opts into the content that supersedes it")
			} else {
				group = append(group, "replace each entry with "+optIn)
			}
			if !slices.Contains(declaredNames, successor) {
				group = append(group, "declare "+successor+" in extensions")
			}
		}
		step := strings.Join(group, ", ")
		if len(successors) > 1 {
			step = "for " + strings.Join(groups[successor], ", ") + ": " + step
		}
		steps = append(steps, step)
	}
	return protocolcli.Classify(fmt.Errorf(
		"agentArtifacts declares %s in a form this CLI no longer installs: nothing was changed. "+
			"Agent content is installed only by an extension, through an extension:<name> entry: %s, then run `putnami install`",
		strings.Join(quoted, ", "), strings.Join(steps, "; ")), protocolcli.ErrInvalidConfig)
}

// optsInto reports whether the workspace already opts into the agent content
// of the extension name.
func optsInto(cfg *wsproto.Config, name string) bool {
	if cfg == nil {
		return false
	}
	for _, declared := range cfg.AgentArtifacts {
		if AgentArtifactDeclarationName(declared) == name {
			return true
		}
	}
	return false
}

// firstPartyAgentContentSuccessors names the extension whose agent content
// supersedes each agent artifact Putnami published separately, for a
// workspace that does not declare that extension yet. A test holds it equal
// to the supersedes list of the contributor extension's manifest.
var firstPartyAgentContentSuccessors = map[string]string{
	"@putnami/agent-workflows":      "@putnami/contributor",
	"@putnami/maintainer-workflows": "@putnami/contributor",
}

// declaredSupersedingSources is the agent content of every declared
// extension whose content supersedes an artifact, in declaration order.
func declaredSupersedingSources(wsRoot string, cfg *wsproto.Config) []*extension.AgentContentSource {
	var sources []*extension.AgentContentSource
	for _, candidate := range extension.DeclaredExtensionNames(wsRoot, cfg) {
		source, err := extension.LocateAgentContent(wsRoot, cfg, candidate)
		if err != nil || len(source.Contribution.Supersedes) == 0 {
			continue
		}
		sources = append(sources, source)
	}
	return sources
}

// supersedingExtension names the extension whose agent content supersedes a
// legacy entry: a declared extension whose content says so, else the
// first-party successor, else "".
func supersedingExtension(wsRoot string, sources []*extension.AgentContentSource, declared string) string {
	for _, source := range sources {
		for _, superseded := range source.Contribution.Supersedes {
			if legacyEntryNames(wsRoot, declared, superseded) {
				return source.Name
			}
		}
	}
	firstParty := make([]string, 0, len(firstPartyAgentContentSuccessors))
	for name := range firstPartyAgentContentSuccessors {
		firstParty = append(firstParty, name)
	}
	sort.Strings(firstParty)
	for _, name := range firstParty {
		if legacyEntryNames(wsRoot, declared, name) {
			return firstPartyAgentContentSuccessors[name]
		}
	}
	return ""
}

// legacyEntryNames reports whether a legacy entry names the artifact: a
// registry entry by what precedes its constraint, an in-tree entry by the name
// its project's putnami.json declares, or, when that project is gone, by its
// last path segment, which is the artifact's unscoped name.
func legacyEntryNames(wsRoot, declared, artifact string) bool {
	declared = strings.TrimSpace(declared)
	if !strings.HasPrefix(declared, "/") {
		name, _, _ := strings.Cut(declared, ":")
		return strings.TrimSpace(name) == artifact
	}
	if name := legacyProjectName(wsRoot, declared); name != "" {
		return name == artifact
	}
	unscoped := artifact
	if strings.HasPrefix(artifact, "@") {
		if _, rest, ok := strings.Cut(artifact, "/"); ok {
			unscoped = rest
		}
	}
	return path.Base(path.Clean(declared)) == unscoped
}

// legacyProjectName is the name the putnami.json of an in-tree entry's project
// declares, or "" when it declares none or is gone. An entry that leaves the
// workspace is not read.
func legacyProjectName(wsRoot, declared string) string {
	if strings.ContainsAny(declared, `\:`) || slices.Contains(strings.Split(declared, "/"), "..") {
		return ""
	}
	clean := strings.TrimPrefix(path.Clean(declared), "/")
	if clean == "" {
		return ""
	}
	project := wsproto.LoadProjectConfig(filepath.Join(wsRoot, filepath.FromSlash(clean)))
	if project == nil {
		return ""
	}
	return strings.TrimSpace(project.Name)
}

// extensionNameFormat is the extension manifest's `name` grammar
// (protocols/extension/schemas/extension.json).
var extensionNameFormat = regexp.MustCompile(`^(@[a-z0-9-]+/[a-z0-9-]+|[a-z0-9][a-z0-9-]*)$`)

// resolveExtensionAgentContent locates the opted-in extension's contribution
// and returns the exact tree to materialize with the pin that binds it.
//
// For an installed extension the tree is the packaged one inside the pinned
// release: the version is the extension's pin, the manifest digest is the one
// the extension manifest declares, and the identity digest recorded in the
// ownership state is the extension manifest's own digest — the value the lock
// binds — so a record names exactly which extension release installed its
// files. A local extension that declares its authored source is built with the
// packager's builder on every run and staged in this worktree only.
//
// A contribution that supersedes an artifact this clone still records as
// installed is refused: two owners would claim the same files.
func resolveExtensionAgentContent(wsRoot string, cfg *wsproto.Config, ref AgentArtifactRef) (AgentArtifactResolution, error) {
	source, err := extension.LocateAgentContent(wsRoot, cfg, ref.Extension)
	if err != nil {
		return AgentArtifactResolution{}, err
	}
	if err := refuseSupersededOwners(wsRoot, source); err != nil {
		return AgentArtifactResolution{}, err
	}
	return resolveAgentContentSource(wsRoot, source)
}

// resolveAgentContentSource returns the exact tree of a located contribution
// with the pin that binds it: the packaged tree of an installed release, or
// the build of a local extension's authored source.
func resolveAgentContentSource(wsRoot string, source *extension.AgentContentSource) (AgentArtifactResolution, error) {
	contribution := source.Contribution
	if source.Local && contribution.Source != "" {
		return buildLocalExtensionAgentContent(wsRoot, source)
	}
	if strings.TrimSpace(source.Version) == "" {
		return AgentArtifactResolution{}, fmt.Errorf(
			"extension %s ships built agent content but declares no version to bind it to", source.Name)
	}
	return AgentArtifactResolution{
		Entry: lockfile.AgentArtifactLockEntry{
			Version:      source.Version,
			Integrity:    source.ManifestHash,
			ManifestHash: contribution.ManifestSHA256,
		},
		Dir: filepath.Join(source.Root, filepath.FromSlash(contribution.Path)),
	}, nil
}

// refuseSupersededOwners keeps one owner per set of workflows. An extension's
// content that supersedes an artifact (agentContent.supersedes) cannot be
// installed while this clone still records that artifact as installed: its
// ownership record would claim the files the extension's record claims. The
// user resolves it by deleting that record, which the refusal names, never
// Putnami by picking one owner silently; the next run keeps every file whose
// bytes match the content and reports every other one as a collision, and
// `putnami install` drops the artifact's lock pin. A superseded artifact the
// workspace still declares never reaches this check: DeclaredAgentArtifacts
// refuses the declaration first.
func refuseSupersededOwners(wsRoot string, source *extension.AgentContentSource) error {
	superseded := source.Contribution.Supersedes
	if len(superseded) == 0 {
		return nil
	}
	names := make(map[string]bool, len(superseded))
	for _, name := range superseded {
		names[name] = true
	}
	var recorded, records []string
	for _, name := range sortedNames(names) {
		state, err := agentartifacts.LoadState(wsRoot, name)
		if err != nil {
			return agentArtifactFailure(name, nil, err)
		}
		if state != nil {
			recorded = append(recorded, name)
			records = append(records, agentArtifactStateRelPath(wsRoot, name))
		}
	}
	if len(recorded) == 0 {
		return nil
	}
	return fmt.Errorf(
		"this clone still records %s as installed, and the agent content of extension %s supersedes it: nothing was changed. "+
			"Its ownership record would claim the files the extension's content claims; delete %s, then rerun",
		strings.Join(recorded, ", "), source.Name, strings.Join(records, ", "))
}

// agentArtifactStateRelPath names an ownership record by its workspace path,
// with / separators, so a refusal tells the user which file to delete.
func agentArtifactStateRelPath(wsRoot, name string) string {
	record := agentartifacts.StatePath(wsRoot, name)
	if rel, err := filepath.Rel(wsRoot, record); err == nil {
		return filepath.ToSlash(rel)
	}
	return record
}

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// buildLocalExtensionAgentContent builds a local extension's authored
// contribution. The version is derived from the emitted files
// (localContentVersion), so an unchanged source rebuilds into the same staged
// tree and the ownership record moves exactly when the content does.
func buildLocalExtensionAgentContent(wsRoot string, source *extension.AgentContentSource) (AgentArtifactResolution, error) {
	contribution := source.Contribution
	probe, err := sdkagentartifact.BuildExtensionContent(source.Root, contribution.Source, source.Name, localContentProbeVersion)
	if err != nil {
		return AgentArtifactResolution{}, fmt.Errorf("build the agent content of %s: %w", source.Name, err)
	}
	version := localContentVersion(probe.Files)
	result, err := sdkagentartifact.BuildExtensionContent(source.Root, contribution.Source, source.Name, version)
	if err != nil {
		return AgentArtifactResolution{}, fmt.Errorf("build the agent content of %s: %w", source.Name, err)
	}
	entry := lockfile.AgentArtifactLockEntry{
		Version:      version,
		Integrity:    result.ArchiveSHA256,
		ManifestHash: result.ManifestSHA256,
	}
	dir := layout.ArtifactDir(wsRoot, layout.AgentArtifacts, source.Name, version)
	if _, err := agentartifacts.LoadArtifact(dir, source.Name, entry); err == nil {
		return AgentArtifactResolution{Entry: entry, Dir: dir}, nil
	}
	if err := stageLocalContent(dir, result); err != nil {
		return AgentArtifactResolution{}, fmt.Errorf("stage the agent content of %s: %w", source.Name, err)
	}
	return AgentArtifactResolution{Entry: entry, Dir: dir}, nil
}

// prepareExtensionAgentContent carries one opted-in contribution to the last
// point before a write. It is never pinned under agentArtifacts.
func prepareExtensionAgentContent(wsRoot string, cfg *wsproto.Config, ref AgentArtifactRef) (preparedAgentArtifact, error) {
	resolution, err := resolveExtensionAgentContent(wsRoot, cfg, ref)
	if err != nil {
		return preparedAgentArtifact{}, agentArtifactFailure(ref.Name, nil, err)
	}
	artifact, plan, err := planResolvedAgentArtifact(wsRoot, ref.Name, resolution)
	if err != nil {
		return preparedAgentArtifact{}, agentArtifactFailure(ref.Name, nil, err)
	}
	return preparedAgentArtifact{ref: ref, artifact: artifact, plan: plan}, nil
}

// localExtensionAgentContent reports whether an opted-in extension is a local
// development source, the only kind a pass that never installs may build.
func localExtensionAgentContent(wsRoot string, cfg *wsproto.Config, ref AgentArtifactRef) (bool, error) {
	declared, err := extension.FindDeclaredExtension(wsRoot, cfg, ref.Extension)
	if err != nil {
		return false, agentArtifactFailure(ref.Name, nil, err)
	}
	return declared.Local, nil
}

// pinnedExtensionAgentContent resolves an opted-in contribution for the
// pass that acts only on what is already installed — the first-run ensure
// pass. It reports false, with no error, for a local extension (that pass
// never builds), for a registry extension the lock does not pin (an explicit install pins it first).
func pinnedExtensionAgentContent(wsRoot string, cfg *wsproto.Config, lf *lockfile.LockFile, ref AgentArtifactRef) (AgentArtifactResolution, bool, error) {
	declared, err := extension.FindDeclaredExtension(wsRoot, cfg, ref.Extension)
	if err != nil {
		return AgentArtifactResolution{}, false, agentArtifactFailure(ref.Name, nil, err)
	}
	if declared.Local || lf == nil {
		return AgentArtifactResolution{}, false, nil
	}
	if _, pinned := lf.GetExtension(ref.Extension); !pinned {
		return AgentArtifactResolution{}, false, nil
	}
	resolution, err := resolveExtensionAgentContent(wsRoot, cfg, ref)
	if err != nil {
		return AgentArtifactResolution{}, false, agentArtifactFailure(ref.Name, nil, err)
	}
	return resolution, true, nil
}
