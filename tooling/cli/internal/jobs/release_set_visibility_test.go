package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/workspace"
	ciproto "go.putnami.dev/protocol/ci"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
)

func visibilityWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	web := &workspace.Project{ID: "/typescript/web", Name: "@putnami/web", Type: "library", Tags: []string{"public-lib"}}
	cli := &workspace.Project{ID: "/tooling/cli", Name: "@putnami/cli", Type: "library", Tags: []string{"go"}}
	return workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{web, cli})
}

func visibilityMembers() []releaseset.PlannedMember {
	return []releaseset.PlannedMember{
		{Ecosystem: "npm", Coordinate: "@putnami/web", ProjectID: "/typescript/web"},
		{Ecosystem: "oci", Coordinate: "putnami/web", ProjectID: "/typescript/web"},
		{Ecosystem: "npm", Coordinate: "@putnami/cli", ProjectID: "/tooling/cli"},
	}
}

// D5: the CLI carries the declared chain and computes no level. Its ONE job is
// to resolve the member selectors — which mean nothing outside this workspace —
// into the exact member coordinates the provider will resolve levels for.
func TestVisibilityChainResolvesSelectorsToCoordinates(t *testing.T) {
	spectest.Proves(t, "cli/channels", "visibility-is-carried-not-computed", "chain-resolves-selectors-to-coordinates")
	policy := &ciproto.Distribution{
		Namespace:  "putnami",
		Visibility: "internal",
		Registries: map[string]ciproto.RegistryPolicy{
			"npm": {Visibility: "private", Mirror: &ciproto.Mirror{To: "https://registry.npmjs.org"}},
			"oci": {Mirror: &ciproto.Mirror{To: "ghcr.io/putnami"}},
		},
		Versions: &ciproto.VersionPolicy{Stable: "public", Prerelease: "internal"},
		Members:  []ciproto.MemberPolicy{{Select: ciproto.Selectors{"tag:public-lib"}, Visibility: "public"}},
	}
	chain, err := BuildVisibilityChain(visibilityWorkspace(t), policy, "private", visibilityMembers())
	if err != nil {
		t.Fatalf("BuildVisibilityChain = %v", err)
	}
	if chain.Repo != distribution.VisibilityInternal {
		t.Fatalf("repo level = %q, want the declared one", chain.Repo)
	}
	if len(chain.Registries) != 1 || chain.Registries["npm"] != distribution.VisibilityPrivate {
		t.Fatalf("registry levels = %+v; want only the registry that states one", chain.Registries)
	}
	if chain.Versions.Stable != distribution.VisibilityPublic || chain.Versions.Prerelease != distribution.VisibilityInternal {
		t.Fatalf("version levels = %+v", chain.Versions)
	}
	if chain.Set == nil || *chain.Set != distribution.VisibilityPrivate {
		t.Fatalf("set level = %+v, want --visibility private", chain.Set)
	}
	// The selector named a PROJECT; the chain names its MEMBERS, both of them,
	// in canonical order, and no member of the project the rule did not select.
	want := []distribution.MemberVisibility{
		{Ecosystem: "npm", Coordinate: "@putnami/web", Visibility: distribution.VisibilityPublic},
		{Ecosystem: "oci", Coordinate: "putnami/web", Visibility: distribution.VisibilityPublic},
	}
	if len(chain.Members) != len(want) {
		t.Fatalf("member levels = %+v, want %+v", chain.Members, want)
	}
	for index, member := range chain.Members {
		if member != want[index] {
			t.Fatalf("member level %d = %+v, want %+v", index, member, want[index])
		}
	}
}

// Every selector kind of the shared vocabulary resolves, and a later rule
// refines an earlier one for a member both select.
func TestVisibilityChainSelectorKindsAndRuleOrder(t *testing.T) {
	ws := visibilityWorkspace(t)
	policy := &ciproto.Distribution{
		Namespace: "putnami",
		Members: []ciproto.MemberPolicy{
			{Select: ciproto.Selectors{"tag:public-lib", "@putnami/cli"}, Visibility: "public"},
			{Select: ciproto.Selectors{"@putnami/cli"}, Visibility: "private"},
		},
	}
	chain, err := BuildVisibilityChain(ws, policy, "", visibilityMembers())
	if err != nil {
		t.Fatalf("BuildVisibilityChain = %v", err)
	}
	levels := make(map[string]distribution.Visibility, len(chain.Members))
	for _, member := range chain.Members {
		levels[member.Coordinate] = member.Visibility
	}
	if levels["@putnami/web"] != distribution.VisibilityPublic {
		t.Fatalf("tag-selected member = %q, want public", levels["@putnami/web"])
	}
	if levels["@putnami/cli"] != distribution.VisibilityPrivate {
		t.Fatalf("twice-selected member = %q, want the later rule's level", levels["@putnami/cli"])
	}
	if chain.Set != nil {
		t.Fatalf("set level = %+v, want none without --visibility", chain.Set)
	}
}

// A rule that selects nothing is not a failure: a repository may declare a
// level for a tag no project carries yet.
func TestVisibilityChainAcceptsARuleThatSelectsNothing(t *testing.T) {
	policy := &ciproto.Distribution{
		Namespace: "putnami",
		Members:   []ciproto.MemberPolicy{{Select: ciproto.Selectors{"tag:not-carried-here"}, Visibility: "public"}},
	}
	chain, err := BuildVisibilityChain(visibilityWorkspace(t), policy, "", visibilityMembers())
	if err != nil || len(chain.Members) != 0 {
		t.Fatalf("chain = %+v, %v; want no member level and no error", chain.Members, err)
	}
}

// A workspace that declares no policy publishes the neutral chain: everything
// internal, no member override, no set level.
func TestVisibilityChainIsNeutralWithoutAPolicy(t *testing.T) {
	chain, err := BuildVisibilityChain(visibilityWorkspace(t), nil, "", visibilityMembers())
	if err != nil {
		t.Fatalf("BuildVisibilityChain = %v", err)
	}
	if chain.Repo != distribution.VisibilityInternal || chain.Set != nil ||
		len(chain.Registries) != 0 || len(chain.Members) != 0 {
		t.Fatalf("neutral chain = %+v", chain)
	}
}

// A level outside the three ordered ones is a usage error, wherever it was
// spelled: publish never widens an artifact on a value nobody defined.
func TestVisibilityChainRefusesAnUnknownLevel(t *testing.T) {
	if _, err := BuildVisibilityChain(visibilityWorkspace(t), nil, "world", visibilityMembers()); err == nil ||
		!strings.Contains(err.Error(), "--visibility") {
		t.Fatalf("--visibility world = %v, want a usage refusal", err)
	}
	policy := &ciproto.Distribution{Namespace: "putnami", Visibility: "everyone"}
	if _, err := BuildVisibilityChain(visibilityWorkspace(t), policy, "", visibilityMembers()); err == nil {
		t.Fatal("an unknown repository level was accepted")
	}
}

// distributionWorkspace is visibilityWorkspace with the two putnami.json
// declarations: web states its own level, cli inherits one from its scope.
func distributionWorkspace(t *testing.T, web, cli string) *workspace.Workspace {
	t.Helper()
	webProject := &workspace.Project{
		ID: "/typescript/web", Path: "typescript/web", Name: "@putnami/web", Type: "library", Tags: []string{"public-lib"},
		Config: &wsproto.ProjectConfig{Name: "@putnami/web", Distribution: &wsproto.DistributionConfig{Visibility: web}},
	}
	cliProject := &workspace.Project{
		ID: "/tooling/cli", Path: "tooling/cli", Name: "@putnami/cli", Type: "library", Tags: []string{"go"},
		Scope: workspace.ScopeContribution{Distribution: &wsproto.DistributionConfig{Visibility: cli}},
	}
	return workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "putnami"}, []*workspace.Project{webProject, cliProject})
}

func memberLevels(chain distribution.VisibilityChain) map[string]distribution.Visibility {
	levels := make(map[string]distribution.Visibility, len(chain.Members))
	for _, member := range chain.Members {
		levels[string(member.Ecosystem)+":"+member.Coordinate] = member.Visibility
	}
	return levels
}

// A project states its level in its own putnami.json, or inherits its scope's,
// and that level reaches every member it produces — with no putnami.ci.json at
// all, because the declaration no longer needs the shared document.
func TestVisibilityChainReadsProjectAndScopeDeclarations(t *testing.T) {
	spectest.Proves(t, "cli/channels", "project-declares-its-distribution-visibility", "putnami-json-declares-the-member-level")
	chain, err := BuildVisibilityChain(distributionWorkspace(t, "public", "private"), nil, "", visibilityMembers())
	if err != nil {
		t.Fatalf("BuildVisibilityChain = %v", err)
	}
	want := []distribution.MemberVisibility{
		{Ecosystem: "npm", Coordinate: "@putnami/cli", Visibility: distribution.VisibilityPrivate},
		{Ecosystem: "npm", Coordinate: "@putnami/web", Visibility: distribution.VisibilityPublic},
		{Ecosystem: "oci", Coordinate: "putnami/web", Visibility: distribution.VisibilityPublic},
	}
	if len(chain.Members) != len(want) {
		t.Fatalf("member levels = %+v, want %+v", chain.Members, want)
	}
	for index, member := range chain.Members {
		if member != want[index] {
			t.Fatalf("member level %d = %+v, want %+v", index, member, want[index])
		}
	}
	if chain.Repo != distribution.VisibilityInternal {
		t.Fatalf("repo level = %q; a project declaration must not move the repository level", chain.Repo)
	}
}

// A project's own declaration wins over the one its scope gives it.
func TestVisibilityChainProjectDeclarationWinsOverItsScope(t *testing.T) {
	ws := distributionWorkspace(t, "internal", "private")
	web := ws.ProjectByID("/typescript/web")
	web.Scope.Distribution = &wsproto.DistributionConfig{Visibility: "public"}
	chain, err := BuildVisibilityChain(ws, nil, "", visibilityMembers())
	if err != nil {
		t.Fatalf("BuildVisibilityChain = %v", err)
	}
	if level := memberLevels(chain)["npm:@putnami/web"]; level != distribution.VisibilityInternal {
		t.Fatalf("web = %q, want its own internal over its scope's public", level)
	}
}

// The members[] rules keep answering every project its putnami.json leaves
// silent, and may restate a putnami.json level as long as they agree with it.
func TestVisibilityChainRulesAnswerSilentProjects(t *testing.T) {
	ws := distributionWorkspace(t, "public", "")
	ws.ProjectByID("/tooling/cli").Scope.Distribution = nil
	policy := &ciproto.Distribution{
		Namespace: "putnami",
		Members: []ciproto.MemberPolicy{
			{Select: ciproto.Selectors{"tag:public-lib"}, Visibility: "public"},
			{Select: ciproto.Selectors{"@putnami/cli"}, Visibility: "private"},
		},
	}
	chain, err := BuildVisibilityChain(ws, policy, "", visibilityMembers())
	if err != nil {
		t.Fatalf("BuildVisibilityChain = %v", err)
	}
	levels := memberLevels(chain)
	if levels["npm:@putnami/web"] != distribution.VisibilityPublic || levels["oci:putnami/web"] != distribution.VisibilityPublic {
		t.Fatalf("web = %+v, want the agreeing declaration and rule's public", levels)
	}
	if levels["npm:@putnami/cli"] != distribution.VisibilityPrivate {
		t.Fatalf("cli = %q, want the rule's level for a project that declares none", levels["npm:@putnami/cli"])
	}
}

// A putnami.json level and a members[] rule that disagree about one project
// are refused, whether the project declared the level or inherited it: either
// silent choice could widen what the other side meant to keep narrow.
func TestVisibilityChainRefusesADeclarationAndARuleThatDisagree(t *testing.T) {
	policy := &ciproto.Distribution{
		Namespace: "putnami",
		Members:   []ciproto.MemberPolicy{{Select: ciproto.Selectors{"tag:public-lib", "tag:go"}, Visibility: "public"}},
	}
	_, err := BuildVisibilityChain(distributionWorkspace(t, "private", "public"), policy, "", visibilityMembers())
	if err == nil || !strings.Contains(err.Error(), "typescript/web/putnami.json distribution.visibility") ||
		!strings.Contains(err.Error(), "distribution.members[0]") {
		t.Fatalf("own declaration vs rule = %v, want a refusal naming both fields", err)
	}
	_, err = BuildVisibilityChain(distributionWorkspace(t, "public", "private"), policy, "", visibilityMembers())
	if err == nil || !strings.Contains(err.Error(), "inherited by /tooling/cli from its scope") {
		t.Fatalf("scope declaration vs rule = %v, want a refusal naming the scope", err)
	}
}

// A putnami.json block that states no level, or an unknown one, fails closed.
func TestVisibilityChainRefusesAnUnknownDeclaredLevel(t *testing.T) {
	for _, level := range []string{"everyone", ""} {
		if _, err := BuildVisibilityChain(distributionWorkspace(t, level, "private"), nil, "", visibilityMembers()); err == nil {
			t.Errorf("project level %q was accepted", level)
		}
		if _, err := BuildVisibilityChain(distributionWorkspace(t, "public", level), nil, "", visibilityMembers()); err == nil {
			t.Errorf("scope level %q was accepted", level)
		}
	}
}

// The policy is read from the repository's own CI document, and its absence is
// an ordinary answer.
func TestLoadDistributionPolicy(t *testing.T) {
	root := t.TempDir()
	policy, err := LoadDistributionPolicy(root)
	if err != nil || policy != nil {
		t.Fatalf("policy without a document = %+v, %v; want none and no error", policy, err)
	}
	document := `{"version":3,"commands":["build"],` +
		`"distribution":{"namespace":"putnami","channels":{"latest":{"visibility":"public","protected":true}}}}`
	if err := os.WriteFile(filepath.Join(root, ciproto.Filename), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, err = LoadDistributionPolicy(root)
	if err != nil || policy == nil || policy.Namespace != "putnami" {
		t.Fatalf("policy = %+v, %v", policy, err)
	}
	if !ProtectedChannel(policy, "latest") || ProtectedChannel(policy, "canary") || ProtectedChannel(nil, "latest") {
		t.Fatal("protected channels are not read from the declared policy")
	}
	repo, err := RepoVisibility(policy)
	if err != nil || repo != distribution.VisibilityInternal {
		t.Fatalf("repo level = %q, %v; want the internal default", repo, err)
	}
	level, err := ChannelVisibility(policy, "latest", repo)
	if err != nil || level != distribution.VisibilityPublic {
		t.Fatalf("latest level = %q, %v", level, err)
	}
	if level, err := ChannelVisibility(policy, "canary", repo); err != nil || level != repo {
		t.Fatalf("silent channel level = %q, %v; want the repository level", level, err)
	}

	if err := os.WriteFile(filepath.Join(root, ciproto.Filename), []byte(`{"version":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDistributionPolicy(root); err == nil {
		t.Fatal("an invalid CI document was accepted")
	}
}
