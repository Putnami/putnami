package agentctx

import (
	"fmt"
	"path/filepath"
	"regexp"
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
// pinned under `agentArtifacts`: the extension's own lock entry is the pin, or,
// for an npm extension the root package.json declares, the package manager's
// lock. Everything after resolution is the materializer's ordinary path: the same
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
// files. For an npm extension the tree is the packaged one inside the package
// the package manager installed, with the same three values read from it. A
// local extension that declares its authored source is built with the
// packager's builder on every run and staged in this worktree only.
//
// A contribution that supersedes an artifact this clone still records as
// installed is refused: two owners would claim the same files, and moving one
// to the other is the explicit `putnami migrate agent-content` step.
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
// state is resolved by moving the record to the extension explicitly, never by
// picking one owner silently. A superseded artifact the workspace still
// declares never reaches this check: DeclaredAgentArtifacts refuses the
// declaration first.
func refuseSupersededOwners(wsRoot string, source *extension.AgentContentSource) error {
	superseded := source.Contribution.Supersedes
	if len(superseded) == 0 {
		return nil
	}
	names := make(map[string]bool, len(superseded))
	for _, name := range superseded {
		names[name] = true
	}
	var recorded []string
	for _, name := range sortedNames(names) {
		state, err := agentartifacts.LoadState(wsRoot, name)
		if err != nil {
			return agentArtifactFailure(name, nil, err)
		}
		if state != nil {
			recorded = append(recorded, name)
		}
	}
	if len(recorded) > 0 {
		return protocolcli.WithNext(fmt.Errorf(
			"this clone still records %s as installed, and the agent content of extension %s supersedes it: nothing was changed. "+
				"Its ownership record would claim the files the extension's content claims; move that ownership to the extension",
			strings.Join(recorded, ", "), source.Name), "putnami migrate agent-content "+source.Name+" --apply")
	}
	return nil
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

// AgentContentFollowsPackageManager reports whether any opted-in extension is
// an npm package the root package.json declares. Its content is read from the
// package the package manager installs, so an adopting command plans the agent
// phase after the package manager has run: before it, node_modules can still
// hold the previous release, whose commands the next command no longer loads.
// A declaration that does not resolve reports false here; the agent phase
// reports it.
func AgentContentFollowsPackageManager(wsRoot string, cfg *wsproto.Config) bool {
	refs, err := DeclaredAgentArtifacts(wsRoot, cfg)
	if err != nil {
		return false
	}
	for _, ref := range refs {
		declared, err := extension.FindDeclaredExtension(wsRoot, cfg, ref.Extension)
		if err == nil && declared.Package {
			return true
		}
	}
	return false
}

// pinnedExtensionAgentContent resolves an opted-in contribution for the
// passes that act only on what is already installed — the first-run ensure
// pass and the session-start reconcile. It reports false, with no error, for a
// local extension (those passes never build), for a registry extension the
// lock does not pin (an explicit install pins it first), and for a package the
// package manager has not installed yet (an explicit install runs it).
func pinnedExtensionAgentContent(wsRoot string, cfg *wsproto.Config, lf *lockfile.LockFile, ref AgentArtifactRef) (AgentArtifactResolution, bool, error) {
	declared, err := extension.FindDeclaredExtension(wsRoot, cfg, ref.Extension)
	if err != nil {
		return AgentArtifactResolution{}, false, agentArtifactFailure(ref.Name, nil, err)
	}
	switch {
	case declared.Local:
		return AgentArtifactResolution{}, false, nil
	case declared.Package:
		if !extension.PackageExtensionInstalled(declared) {
			return AgentArtifactResolution{}, false, nil
		}
	default:
		if lf == nil {
			return AgentArtifactResolution{}, false, nil
		}
		if _, pinned := lf.GetExtension(ref.Extension); !pinned {
			return AgentArtifactResolution{}, false, nil
		}
	}
	resolution, err := resolveExtensionAgentContent(wsRoot, cfg, ref)
	if err != nil {
		return AgentArtifactResolution{}, false, agentArtifactFailure(ref.Name, nil, err)
	}
	return resolution, true, nil
}
