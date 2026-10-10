package jobs

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/cmderr"
)

// LoadDistributionPolicy reads the distribution section of the repository's CI
// document. It is the ONLY place a publish learns a visibility level or a
// protected channel: the CLI computes neither and carries what the repository
// declared (D5, ADR 0021 §6).
//
// A workspace without a putnami.ci.json declares no policy, which is not a
// failure: publish then carries the neutral chain and protects no channel. An
// invalid document is a failure, because a publish that silently ignored a
// declared level would widen an artifact the repository meant to keep internal.
func LoadDistributionPolicy(wsRoot string) (*ciproto.Distribution, error) {
	if strings.TrimSpace(wsRoot) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(wsRoot, ciproto.Filename))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", ciproto.Filename, err)
	}
	document, err := ciproto.Parse(data)
	if err != nil {
		return nil, cmderr.InvalidConfigf("parse %s: %v", ciproto.Filename, err)
	}
	return document.Distribution, nil
}

// distributionNamespace is THE rule that turns a repository into a provider
// identity: the namespace it declared, else its own workspace name.
func distributionNamespace(policy *ciproto.Distribution, workspaceName string) string {
	if policy != nil && policy.Namespace != "" {
		return policy.Namespace
	}
	return workspaceName
}

// ProtectedChannel reports whether the repository declared this channel
// protected. A protected channel is never advanced by a publish or a branch
// rule; only a user moves it, with the release-set provider's channel command
// (D31).
func ProtectedChannel(policy *ciproto.Distribution, name string) bool {
	if policy == nil {
		return false
	}
	return policy.Channels[name].Protected
}

// RepoVisibility is the level the repository declares for every artifact it
// produces, and the level a silent channel inherits. It defaults to internal:
// the narrowest of the three ordered levels, so a repository that declares
// nothing publishes nothing wider than its own members.
func RepoVisibility(policy *ciproto.Distribution) (distribution.Visibility, error) {
	if policy == nil || strings.TrimSpace(policy.Visibility) == "" {
		return distribution.VisibilityInternal, nil
	}
	return parseVisibilityLevel("distribution.visibility", policy.Visibility)
}

// ChannelVisibility is the level one channel confers, one step of the chain:
// the level declared for it, or the repository level when the channel is
// silent.
func ChannelVisibility(policy *ciproto.Distribution, name string, repo distribution.Visibility) (distribution.Visibility, error) {
	if policy == nil {
		return repo, nil
	}
	declared := strings.TrimSpace(policy.Channels[name].Visibility)
	if declared == "" {
		return repo, nil
	}
	return parseVisibilityLevel("distribution.channels."+name+".visibility", declared)
}

// BuildVisibilityChain resolves the repository's declared inheritance chain
// into the exact document one release carries: repo, registry, version, set,
// and member levels.
//
// The CLI resolves the member SELECTORS and nothing else. The provider owns
// the resolution of the chain itself — which level wins for a given member,
// and the ratchet that never narrows a level already resolved (D5) — because
// only it knows what every previous publication resolved. What the provider
// must never see is a project selector: `tag:public-lib` means nothing outside
// this workspace, so it is turned into member coordinates here
// (protocols/distribution ADR 0001).
//
// set is the `--visibility` value of this publication, the per-publication
// override between the version and the member levels. It is nil in the chain
// when the publication states none.
//
// A member rule that selects nothing is not an error: a repository may declare
// a level for a tag no project carries yet, and refusing the publish for that
// would make an anticipatory declaration a blocker. Rules apply in declaration
// order and a later rule wins over an earlier one for a member both select, so
// a broad rule can be refined by a narrow one below it.
//
// The member link also reads the projects' own putnami.json declarations (see
// memberVisibilities), which is why it is resolved even when the repository
// declares no policy.
func BuildVisibilityChain(
	ws *workspace.Workspace,
	policy *ciproto.Distribution,
	set string,
	members []releaseset.PlannedMember,
) (distribution.VisibilityChain, error) {
	chain := distribution.VisibilityChain{Repo: distribution.VisibilityInternal}
	if trimmed := strings.TrimSpace(set); trimmed != "" {
		level, err := parseVisibilityLevel("--visibility", trimmed)
		if err != nil {
			return chain, err
		}
		chain.Set = &level
	}
	var err error
	if policy != nil {
		if chain.Repo, err = RepoVisibility(policy); err != nil {
			return chain, err
		}
		if chain.Registries, err = registryVisibilities(policy); err != nil {
			return chain, err
		}
		if chain.Versions, err = versionVisibilities(policy); err != nil {
			return chain, err
		}
	}
	if chain.Members, err = memberVisibilities(ws, policy, members); err != nil {
		return chain, err
	}
	return chain, nil
}

// registryVisibilities reads the level declared per ecosystem registry. A
// registry entry that declares only a mirror contributes no level and inherits.
func registryVisibilities(policy *ciproto.Distribution) (map[string]distribution.Visibility, error) {
	var levels map[string]distribution.Visibility
	for _, kind := range slices.Sorted(maps.Keys(policy.Registries)) {
		declared := strings.TrimSpace(policy.Registries[kind].Visibility)
		if declared == "" {
			continue
		}
		level, err := parseVisibilityLevel("distribution.registries."+kind+".visibility", declared)
		if err != nil {
			return nil, err
		}
		if levels == nil {
			levels = make(map[string]distribution.Visibility, len(policy.Registries))
		}
		levels[kind] = level
	}
	return levels, nil
}

// versionVisibilities reads the level a version takes from its shape.
func versionVisibilities(policy *ciproto.Distribution) (distribution.VersionVisibility, error) {
	versions := distribution.VersionVisibility{}
	if policy.Versions == nil {
		return versions, nil
	}
	if declared := strings.TrimSpace(policy.Versions.Stable); declared != "" {
		level, err := parseVisibilityLevel("distribution.versions.stable", declared)
		if err != nil {
			return versions, err
		}
		versions.Stable = level
	}
	if declared := strings.TrimSpace(policy.Versions.Prerelease); declared != "" {
		level, err := parseVisibilityLevel("distribution.versions.prerelease", declared)
		if err != nil {
			return versions, err
		}
		versions.Prerelease = level
	}
	return versions, nil
}

// memberVisibilities resolves the member link of the chain for the members of
// this publication, in canonical (ecosystem, coordinate) order.
//
// Two places state a project's member level. Its putnami.json — its own
// `distribution.visibility`, else the deepest scope's — is the declaration its
// owners keep beside the code. The repository's `distribution.members[]` rules
// are the central one. The putnami.json answer wins, and the rules answer every
// project it leaves silent, so a team makes a package public without editing
// the one CI document every team shares.
//
// When both answer the same project they must agree. Two stated audiences for
// one artifact is an ambiguity publish refuses rather than resolves: picking
// either silently could widen what the other meant to keep narrow.
func memberVisibilities(
	ws *workspace.Workspace,
	policy *ciproto.Distribution,
	members []releaseset.PlannedMember,
) ([]distribution.MemberVisibility, error) {
	if len(members) == 0 {
		return nil, nil
	}
	byProject := make(map[string][]releaseset.PlannedMember, len(members))
	for _, member := range members {
		byProject[member.ProjectID] = append(byProject[member.ProjectID], member)
	}
	ruled, err := ruledProjectVisibilities(ws, policy, byProject)
	if err != nil {
		return nil, err
	}
	declared, err := declaredProjectVisibilities(ws, byProject)
	if err != nil {
		return nil, err
	}
	levels := make(map[string]distribution.Visibility, len(byProject))
	for projectID, rule := range ruled {
		levels[projectID] = rule.level
	}
	for _, projectID := range slices.Sorted(maps.Keys(declared)) {
		declaration := declared[projectID]
		if rule, both := ruled[projectID]; both && rule.level != declaration.level {
			return nil, cmderr.InvalidConfigf(
				"%s is %q but %s selects project %s as %q; declare its level in one place, or make both agree",
				declaration.field, declaration.level, rule.field, projectID, rule.level)
		}
		levels[projectID] = declaration.level
	}
	if len(levels) == 0 {
		return nil, nil
	}
	resolved := make([]distribution.MemberVisibility, 0, len(members))
	for projectID, level := range levels {
		for _, member := range byProject[projectID] {
			resolved = append(resolved, distribution.MemberVisibility{
				Ecosystem: member.Ecosystem, Coordinate: member.Coordinate, Visibility: level,
			})
		}
	}
	slices.SortFunc(resolved, func(a, b distribution.MemberVisibility) int {
		return strings.Compare(releaseset.MemberKey(a.Ecosystem, a.Coordinate), releaseset.MemberKey(b.Ecosystem, b.Coordinate))
	})
	return resolved, nil
}

// projectVisibility is one project's member level and the field that stated
// it, so a disagreement can name both sides.
type projectVisibility struct {
	level distribution.Visibility
	field string
}

// ruledProjectVisibilities applies the repository's member rules to the
// projects of this publication, in declaration order: a later rule wins over
// an earlier one for a project both select.
func ruledProjectVisibilities(
	ws *workspace.Workspace,
	policy *ciproto.Distribution,
	byProject map[string][]releaseset.PlannedMember,
) (map[string]projectVisibility, error) {
	if policy == nil || len(policy.Members) == 0 {
		return nil, nil
	}
	ruled := make(map[string]projectVisibility, len(byProject))
	for index, rule := range policy.Members {
		field := fmt.Sprintf("distribution.members[%d]", index)
		level, err := parseVisibilityLevel(field+".visibility", rule.Visibility)
		if err != nil {
			return nil, err
		}
		projects, err := resolveMemberSelectors(ws, field+".select", rule.Select)
		if err != nil {
			return nil, err
		}
		for _, projectID := range projects {
			if _, publishes := byProject[projectID]; publishes {
				ruled[projectID] = projectVisibility{level: level, field: field}
			}
		}
	}
	return ruled, nil
}

// declaredProjectVisibilities reads each publishing project's putnami.json
// declaration: its own block, else the one its deepest scope declares. A
// block that states no level or an unknown one is refused, never skipped.
func declaredProjectVisibilities(
	ws *workspace.Workspace,
	byProject map[string][]releaseset.PlannedMember,
) (map[string]projectVisibility, error) {
	if ws == nil {
		return nil, nil
	}
	declared := make(map[string]projectVisibility)
	for _, projectID := range slices.Sorted(maps.Keys(byProject)) {
		project := ws.ProjectByID(projectID)
		if project == nil {
			continue
		}
		block, field := project.Scope.Distribution, "distribution.visibility inherited by "+projectID+" from its scope"
		if project.Config != nil && project.Config.Distribution != nil {
			block, field = project.Config.Distribution, path.Join(filepath.ToSlash(project.Path), wsproto.ConfigFilename)+" distribution.visibility"
		}
		if block == nil {
			continue
		}
		level, err := parseVisibilityLevel(field, block.Visibility)
		if err != nil {
			return nil, err
		}
		declared[projectID] = projectVisibility{level: level, field: field}
	}
	return declared, nil
}

// resolveMemberSelectors turns one rule's selection expressions into workspace
// project ids, using the one selection vocabulary the workspace, the CI
// document and the command line share (wsproto.ParseSelector).
func resolveMemberSelectors(ws *workspace.Workspace, field string, selectors ciproto.Selectors) ([]string, error) {
	if ws == nil {
		return nil, fmt.Errorf("%s cannot be resolved without a workspace", field)
	}
	seen := make(map[string]struct{})
	for _, raw := range selectors {
		selector, err := wsproto.ParseSelector(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		for _, project := range selectProjects(ws, selector) {
			if project != nil {
				seen[project.ID] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// selectProjects resolves one parsed selector. A tag is answered from the
// projects' own tags; every other kind goes through the workspace target
// resolver, which is the same code path `--projects` takes.
func selectProjects(ws *workspace.Workspace, selector wsproto.Selector) []*workspace.Project {
	if selector.Kind == wsproto.SelectorTag {
		var tagged []*workspace.Project
		for _, project := range ws.Projects {
			if project != nil && slices.Contains(project.Tags, selector.Value) {
				tagged = append(tagged, project)
			}
		}
		return tagged
	}
	return workspace.ResolveTarget(ws, selector.Value, ws.Root, ws.ScopeIndex)
}

// parseVisibilityLevel refuses a level outside the three ordered ones. The CI
// document's own validation already rejects it, so this is the second half of a
// fail-closed pair: a policy reaching the coordinator from anywhere else cannot
// widen an artifact by spelling a level nobody defined.
func parseVisibilityLevel(field, value string) (distribution.Visibility, error) {
	level := distribution.Visibility(strings.TrimSpace(value))
	if !level.Valid() {
		return "", cmderr.Usagef("%s is %q; a visibility level is internal, private, or public", field, value)
	}
	return level, nil
}
