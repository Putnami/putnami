package workspaceclient

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gitcandidate"
)

// fixtureMembers is the membership the CLI resolves for a fixture workspace:
// the projects its index lists. The check takes it from the job context and
// never reads the index itself.
func fixtureMembers(t *testing.T, root string) []string {
	t.Helper()
	members, err := indexedProjectPaths(root)
	if err != nil {
		t.Fatalf("read the fixture membership: %v", err)
	}
	return members
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// cutFingerprint digests the candidate cut as the task input `git:**` keys
// it: two equal fingerprints are one cache key.
func cutFingerprint(t *testing.T, root string) string {
	t.Helper()
	tree, err := gitcandidate.Open(root)
	if err != nil || tree == nil {
		t.Fatalf("open the candidate cut: %v, %v", tree, err)
	}
	fingerprint, err := tree.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

// cutStep is one change to a fixture work tree, whether it moves the `git:**`
// key, and the verdict the check reaches after it.
type cutStep struct {
	name    string
	apply   func()
	moves   bool
	verdict string
}

// assertVerdictFollowsTheKey applies each step and checks the two halves of a
// sound cache key: the key moves exactly when the step says it does, and the
// verdict never changes while the key stays put, which is what a replayed
// entry would get wrong.
func assertVerdictFollowsTheKey(t *testing.T, root string, verdict func() string, initial string, steps []cutStep) {
	t.Helper()
	key, got := cutFingerprint(t, root), verdict()
	if got != initial {
		t.Fatalf("initial verdict = %q, want %q", got, initial)
	}
	for _, step := range steps {
		step.apply()
		nextKey, next := cutFingerprint(t, root), verdict()
		if moved := nextKey != key; moved != step.moves {
			t.Errorf("%s: key moved = %v, want %v", step.name, moved, step.moves)
		}
		if nextKey == key && next != got {
			t.Errorf("%s: verdict moved from %q to %q while the key stayed put", step.name, got, next)
		}
		if next != step.verdict {
			t.Errorf("%s: verdict = %q, want %q", step.name, next, step.verdict)
		}
		key, got = nextKey, next
	}
}

// checkVerdict is what a replayed check result carries: each provider with
// its targets, and each finding by code and path.
func checkVerdict(report Report) string {
	providers := make([]string, 0, len(report.Providers))
	for _, item := range report.Providers {
		targets := make([]string, 0, len(item.Targets))
		for _, target := range item.Targets {
			targets = append(targets, string(target.Language)+":"+target.Output)
		}
		providers = append(providers, item.Project+"["+strings.Join(targets, ",")+"]")
	}
	findings := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		findings = append(findings, finding.Code+"@"+finding.Path)
	}
	sort.Strings(findings)
	if len(findings) == 0 {
		findings = []string{"clean"}
	}
	return strings.Join(providers, " ") + " | " + strings.Join(findings, " ")
}

// committedCatalogWorkspace is a Git work tree holding one provider whose
// generated Go client, manifest and contract sidecar agree, one project with
// no contract sidecar, and one consumer. Git ignores the build output, the
// project index and *.local.go files.
func committedCatalogWorkspace(t *testing.T) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	_, expected := analyzerFixture(t)
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".gitignore", ".putnami/\n.gen/\n*.local.go\n")
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json",
		`{"version":4,"projects":[{"path":"apps/web"},{"path":"services/catalog"},{"path":"services/orders"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/putnami.json", `{"name":"catalog","extensions":["@putnami/clientgen"]}`)
	for path, content := range expected {
		writeWorkspaceFile(t, root, path, string(content))
	}
	writeWorkspaceFile(t, root, "services/catalog/schema/openapi.json", string(catalogContractForFixture(t)))
	writeWorkspaceFile(t, root, "services/orders/putnami.json", `{"name":"orders"}`)
	writeWorkspaceFile(t, root, "apps/web/putnami.json", `{"name":"web","dependencies":["/services/catalog"]}`)
	writeWorkspaceFile(t, root, "apps/web/main.go", "package web\n")
	gitIn(t, root, "init", "-q")
	return root, []string{"apps/web", "services/catalog", "services/orders"}
}

// TestTheCheckVerdictFollowsItsGitKey proves the guard's `git:**` key is
// sound: the check reads the candidate cut and the membership the job context
// carries, so a file Git ignores — a source, a generated-marked file in an
// output directory, a built generation contract or OpenAPI document, the
// project index — changes neither the key nor the verdict, and every
// candidate the verdict depends on moves the key.
func TestTheCheckVerdictFollowsItsGitKey(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "the-guard-reads-the-git-candidate-cut-and-keys-on-it")
	root, members := committedCatalogWorkspace(t)
	verdict := func() string { return checkVerdict(InspectCommitted(root, members)) }
	const clean = "services/catalog[go:clients/go] | clean"
	handwritten := `package web

import "net/http"

func watch() { _, _ = http.NewRequest("GET", "/catalog/watch", nil) }
`
	marked := "// Code generated by go.putnami.dev/api. DO NOT EDIT.\npackage catalogclient\n"
	builtConfig := `{"targets":["go","ts"],"go":{"output":"clients/go"},"ts":{"output":"clients/ts"}}`
	assertVerdictFollowsTheKey(t, root, verdict, clean, []cutStep{
		{"the ignored project index is garbage", func() {
			writeWorkspaceFile(t, root, ".putnami/workspace-index.json", "not an index")
		}, false, clean},
		{"an ignored source calls the provider by hand", func() {
			writeWorkspaceFile(t, root, "apps/web/catalog.local.go", handwritten)
		}, false, clean},
		{"an ignored generated-marked file sits in the client output", func() {
			writeWorkspaceFile(t, root, "services/catalog/clients/go/extra.local.go", marked)
		}, false, clean},
		{"an ignored built contract adds a TypeScript target", func() {
			writeWorkspaceFile(t, root, "services/catalog/.gen/clientgen/config.json", builtConfig)
		}, false, clean},
		{"an ignored built OpenAPI document describes a project with no sidecar", func() {
			writeWorkspaceFile(t, root, "services/orders/.gen/schema/openapi.json", string(catalogContractForFixture(t)))
		}, false, clean},
		{"the built generation contract is committed", func() {
			gitIn(t, root, "add", "-f", "services/catalog/.gen/clientgen/config.json")
		}, true, "services/catalog[go:clients/go,ts:clients/ts] | clientgen.missing-manifest@services/catalog/clients/ts/client.putnami.json"},
		{"the built generation contract leaves the index", func() {
			gitIn(t, root, "rm", "-q", "--cached", "services/catalog/.gen/clientgen/config.json")
		}, true, clean},
		{"the ignore rule for local sources goes", func() {
			writeWorkspaceFile(t, root, ".gitignore", ".putnami/\n.gen/\n")
		}, true, "services/catalog[go:clients/go] | clientgen.handwritten-first-party-client@apps/web/catalog.local.go " +
			"clientgen.uninventoried-generated-file@services/catalog/clients/go/extra.local.go"},
		{"a tracked generated file is edited by hand", func() {
			writeWorkspaceFile(t, root, "services/catalog/clients/go/client.gen.go", "// hand edit\n")
		}, true, "services/catalog[go:clients/go] | clientgen.forged-generated-hash@services/catalog/clients/go/client.gen.go " +
			"clientgen.handwritten-first-party-client@apps/web/catalog.local.go " +
			"clientgen.uninventoried-generated-file@services/catalog/clients/go/extra.local.go"},
	})
}

// TestTheCheckTakesItsMembershipFromTheJobContext pins that the check never
// falls back to the project index Git ignores: without a membership it
// reports a discovery finding, and a membership path that is not a canonical
// workspace-relative directory, or that repeats, is one too.
func TestTheCheckTakesItsMembershipFromTheJobContext(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "the-guard-reads-the-git-candidate-cut-and-keys-on-it")
	root, members := committedCatalogWorkspace(t)
	if got := checkVerdict(InspectCommitted(root, members)); got != "services/catalog[go:clients/go] | clean" {
		t.Fatalf("verdict with the context membership = %q", got)
	}
	for name, invalid := range map[string][]string{
		"no membership":   nil,
		"an escape":       {"../outside"},
		"a non-canonical": {"services/./catalog"},
		"a repeat":        {"services/catalog", "services/catalog"},
	} {
		report := InspectCommitted(root, invalid)
		if !hasFindingCode(report, "clientgen.discovery") {
			t.Errorf("%s: the check did not report a discovery finding: %+v", name, report.Findings)
		}
		// The finding names the workspace as ".", never the checkout's absolute
		// directory, so a report from one machine reads the same on another.
		for _, finding := range report.Findings {
			if finding.Code == "clientgen.discovery" &&
				(finding.Path != "." || strings.Contains(finding.Message, root)) {
				t.Errorf("%s: discovery finding = %+v, want path \".\" and no absolute directory", name, finding)
			}
		}
	}
	// A project at the workspace root arrives as an empty path and reads as ".".
	if paths, err := memberProjectPaths([]string{"", "services/catalog"}); err != nil || strings.Join(paths, ",") != ".,services/catalog" {
		t.Fatalf("root membership = %v, %v", paths, err)
	}
}

// TestTheCandidateCutListsWhatADirectoryWalkLists pins the cut reader the
// check's discovery and output listing use: a file below a skipped directory
// is not listed, a manifest at the provider's own root is not a target, and
// two manifests of one language are taken in directory-walk order, which
// differs from byte order when a sibling name sorts before "/".
func TestTheCandidateCutListsWhatADirectoryWalkLists(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	for _, rel := range []string{
		"p/client.putnami.json",
		"p/a/client.putnami.json",
		"p/a-b/client.putnami.json",
		"p/node_modules/x/client.putnami.json",
		"p/.cache/client.putnami.json",
		"q/client.putnami.json",
	} {
		writeWorkspaceFile(t, root, rel, "{}")
	}
	gitIn(t, root, "init", "-q")
	tree, err := gitcandidate.Open(root)
	if err != nil || tree == nil {
		t.Fatalf("open the candidate cut: %v, %v", tree, err)
	}
	cut, disk := workspaceFiles{root: root, tree: tree}, workspaceFiles{root: root}
	want := "p/a/client.putnami.json,p/a-b/client.putnami.json"
	for name, files := range map[string]workspaceFiles{"cut": cut, "disk": disk} {
		if got := strings.Join(committedManifestPaths(files, "p"), ","); got != want {
			t.Errorf("%s manifests below p = %s, want %s", name, got, want)
		}
		listed, listErr := files.List("p/a")
		if listErr != nil || strings.Join(listed, ",") != "p/a/client.putnami.json" {
			t.Errorf("%s list p/a = %v, %v", name, listed, listErr)
		}
		single, listErr := files.List("q/client.putnami.json")
		if listErr != nil || strings.Join(single, ",") != "q/client.putnami.json" {
			t.Errorf("%s list of a file = %v, %v", name, single, listErr)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("p/a/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err = gitcandidate.Open(root)
	if err != nil || tree == nil {
		t.Fatalf("reopen the candidate cut: %v, %v", tree, err)
	}
	cut = workspaceFiles{root: root, tree: tree}
	if got := strings.Join(committedManifestPaths(cut, "p"), ","); got != "p/a-b/client.putnami.json" {
		t.Errorf("an ignored manifest is listed: %s", got)
	}
	if cut.isRegularFile("p/a/client.putnami.json") {
		t.Error("an ignored manifest reads as a regular file")
	}
	if _, readErr := cut.Read("p/a/client.putnami.json"); readErr == nil {
		t.Error("an ignored manifest is readable through the cut")
	}
}

func hasFindingCode(report Report, code string) bool {
	for _, finding := range report.Findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
