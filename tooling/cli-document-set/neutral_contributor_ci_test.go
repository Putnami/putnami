package documentset

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
)

// The neutral contributor path (ADR 0004).
//
// This repository's workspace installs @putnami/cloud, and a maintainer running
// the gate has credentials in the environment, a warm machine-global store and a
// reachable registry. Every one of those is an invisible input, so "it works for
// me" proves nothing about somebody with no account.
//
// The documented path is therefore not described here and executed somewhere
// else: CONTRIBUTING.md carries ONE fenced block between the
// `neutral-contributor-ci` markers, this file reads that exact block, and runs
// it with every credential cleared and both remote endpoints refusing requests.
// Editing the documentation without changing what works fails. The
// completeness gate over the governance record lives in
// @putnami/repository-checks, which reads it as text.

const (
	contributorGuideRepoPath = "CONTRIBUTING.md"

	neutralRecipeBeginMarker = "<!-- neutral-contributor-ci:begin -->"
	neutralRecipeEndMarker   = "<!-- neutral-contributor-ci:end -->"
	neutralRecipeFence       = "```"
	neutralRecipeCLIPrefix   = "./putnamiw"
)

// neutralRecipeUnsafeShell are the metacharacters that would make a documented
// line mean more than the argument vector this test runs. A recipe that needs
// one of them is a recipe this file cannot honestly claim to have executed.
const neutralRecipeUnsafeShell = "|&;<>$`(){}*?!#\\\"'"

// TestNeutralContributorRecipeRunsWithoutCredentials executes the documented
// contributor path itself. The fixture is a git-backed workspace with an empty
// artifact store and a lock holding no installed artifacts, so `--impacted`
// resolves over real history rather than a stub.
func TestNeutralContributorRecipeRunsWithoutCredentials(t *testing.T) {
	requireShell(t)
	recipe := neutralContributorRecipe(t)

	wsRoot := writeCloudlessReleaseFixture(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	artifactRoot := filepath.Join(t.TempDir(), "artifacts")
	marker := filepath.Join(t.TempDir(), "jobs.log")
	// Clears every cloud, cache and provider credential and redirects HOME, so
	// no ambient developer configuration reaches the run.
	configureCloudlessReleaseEnvironment(t, storeRoot, artifactRoot, marker)

	var networkRequests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		networkRequests.Add(1)
		http.Error(w, "a contributor has no access to this endpoint", http.StatusUnauthorized)
	}))
	t.Cleanup(endpoint.Close)
	// BOTH endpoints refuse. configureCloudlessReleaseEnvironment only clears
	// PUTNAMI_CACHE_URL, which leaves the remote cache unconfigured rather than
	// hostile — a client that started calling it would meet no counter.
	t.Setenv("PUTNAMI_REGISTRY_URL", endpoint.URL)
	t.Setenv("PUTNAMI_CACHE_URL", endpoint.URL)

	initNeutralContributorHistory(t, wsRoot)
	t.Chdir(wsRoot)

	app := &cli.App{}
	for _, command := range recipe {
		output, code := runCloudlessCLI(t, app, context.Background(), command...)
		if code != cli.ExitSuccess {
			t.Fatalf("documented command `%s %s` exit = %d, want %d\n%s",
				neutralRecipeCLIPrefix, strings.Join(command, " "), code, cli.ExitSuccess, output)
		}
	}

	// Non-vacuity, and precision: the one project a contributor touched ran all
	// project-scoped gate tasks, workspace validation ran once, and the untouched
	// project ran none of them. A recipe that
	// selected nothing would exit 0 and prove nothing.
	for _, want := range []string{"go:lint", "go:test", "go:build", "sdd:validate", "sdd:validate-workspace"} {
		if got := countCloudlessMarker(t, marker, want); got != 1 {
			t.Errorf("impacted project ran %s %d times, want exactly 1", want, got)
		}
	}
	for _, unwanted := range []string{"typescript:lint", "typescript:test", "typescript:build"} {
		if got := countCloudlessMarker(t, marker, unwanted); got != 0 {
			t.Errorf("untouched project ran %s %d times, want 0 — --impacted did not discriminate", unwanted, got)
		}
	}
	if got := networkRequests.Load(); got != 0 {
		t.Fatalf("the documented contributor path contacted a credential-bearing endpoint %d times", got)
	}
	assertCloudlessFixtureState(t, wsRoot, artifactRoot)
}

// TestNeutralContributorRecipeMatchesRepositoryCIJob binds the documented path
// to the CI this repository actually runs: the required job must cover exactly
// the documented gate tasks, and it must take no credential as input. A
// contributor cannot be gated on something they cannot run.
func TestNeutralContributorRecipeMatchesRepositoryCIJob(t *testing.T) {
	t.Parallel()
	repoRoot := repositoryRoot(t)
	documented := neutralContributorGateTasks(t)
	// `validate-workspace` is not named: the @putnami/sdd manifest's `alsoRuns`
	// plans it with `validate`, and that extension owns both commands.
	wantDocumented := []string{"build", "lint", "test", "validate"}
	if strings.Join(documented, ",") != strings.Join(wantDocumented, ",") {
		t.Fatalf("documented gate tasks = %v, want %v", documented, wantDocumented)
	}

	// Maintainer CI is Putnami Cloud's native runner, and the gate it runs is
	// the one the repository's putnami.ci.json declares: a version 3 document
	// that names the ordered commands, the channels a branch publishes, and the
	// environments that follow them, and takes no credential as input. The
	// generated guidance derives its gate line from the same document, so all
	// three declarations of one gate must agree.
	raw, err := os.ReadFile(filepath.Join(repoRoot, ciproto.Filename))
	if err != nil {
		t.Fatalf("%s is absent; the repository authors its gate there: %v", ciproto.Filename, err)
	}
	document, err := ciproto.Parse(raw)
	if err != nil {
		t.Fatalf("%s is not a valid version %d document: %v", ciproto.Filename, ciproto.Version, err)
	}
	authored := make([]string, 0, len(document.Commands))
	for _, command := range document.Commands {
		if !command.Blocking() {
			t.Errorf("%s declares the advisory command %q; a contributor cannot tell it apart from the gate", ciproto.Filename, command.Name)
			continue
		}
		authored = append(authored, command.Name)
	}
	sortedAuthored := append([]string(nil), authored...)
	sort.Strings(sortedAuthored)
	if strings.Join(sortedAuthored, ",") != strings.Join(documented, ",") {
		t.Fatalf("%s commands = %v, documented contributor gate = %v; the two declarations of one gate disagree", ciproto.Filename, sortedAuthored, documented)
	}
	// The generated guidance lists the document's commands in the document's
	// order, so the gate is compared as authored, before any sort.
	gate := strings.Split(agentctx.GateTasks(repoRoot), ",")
	if strings.Join(gate, ",") != strings.Join(authored, ",") {
		t.Fatalf("workspace gate = %v, %s commands in order = %v; the generated guidance must follow the document's order", gate, ciproto.Filename, authored)
	}
	for _, task := range gate {
		if !containsNeutralTask(documented, task) {
			t.Errorf("maintainer CI runs %q, which the documented contributor path never runs", task)
		}
	}
}

// TestNeutralContributorRecipeParserRejectsUnrunnableBlocks proves the recipe
// parser discriminates: a block the test cannot faithfully execute must fail
// rather than be silently reduced to an argument vector.
func TestNeutralContributorRecipeParserRejectsUnrunnableBlocks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, document, wantError string
	}{
		{"missing markers", "no block here\n", "exactly one"},
		{"two blocks", neutralRecipeDocument("./putnamiw install") + neutralRecipeDocument("./putnamiw install"), "exactly one"},
		{"empty block", neutralRecipeDocument(""), "no commands"},
		{"foreign command", neutralRecipeDocument("bun install"), neutralRecipeCLIPrefix},
		{"shell metacharacter", neutralRecipeDocument("./putnamiw build | tee log"), "shell"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			commands, err := parseNeutralContributorRecipe(testCase.document)
			if err == nil {
				t.Fatalf("parser accepted an unrunnable block: %v", commands)
			}
			if !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("error %q does not explain %q", err, testCase.wantError)
			}
		})
	}
	commands, err := parseNeutralContributorRecipe(neutralRecipeDocument("./putnamiw install\n./putnamiw lint,test,build,validate --impacted --enforce-coverage"))
	if err != nil {
		t.Fatalf("parser rejected a valid block: %v", err)
	}
	if len(commands) != 2 || commands[0][0] != "install" || commands[1][0] != "lint,test,build,validate" {
		t.Fatalf("parsed commands = %v, want the two documented invocations", commands)
	}
}

// neutralContributorRecipe reads the documented block and returns one argument
// vector per documented command, with the `./putnamiw` word removed.
func neutralContributorRecipe(t *testing.T) [][]string {
	t.Helper()
	document := readCloudlessFile(t, filepath.Join(repositoryRoot(t), contributorGuideRepoPath))
	commands, err := parseNeutralContributorRecipe(document)
	if err != nil {
		t.Fatalf("read the documented contributor path from %s: %v", contributorGuideRepoPath, err)
	}
	return commands
}

// neutralContributorGateTasks returns the task names the documented gate runs,
// read from the comma-joined command word of the last documented invocation.
func neutralContributorGateTasks(t *testing.T) []string {
	t.Helper()
	commands := neutralContributorRecipe(t)
	last := commands[len(commands)-1]
	tasks := strings.Split(last[0], ",")
	sort.Strings(tasks)
	return tasks
}

// parseNeutralContributorRecipe extracts the documented commands. Every failure
// mode is an error rather than a silent reduction: the whole point of the block
// is that what is documented is what runs.
func parseNeutralContributorRecipe(document string) ([][]string, error) {
	if strings.Count(document, neutralRecipeBeginMarker) != 1 || strings.Count(document, neutralRecipeEndMarker) != 1 {
		return nil, fmt.Errorf("the contributor guide must carry exactly one %s / %s pair",
			neutralRecipeBeginMarker, neutralRecipeEndMarker)
	}
	start := strings.Index(document, neutralRecipeBeginMarker) + len(neutralRecipeBeginMarker)
	end := strings.Index(document, neutralRecipeEndMarker)
	if end < start {
		return nil, fmt.Errorf("the %s marker precedes %s", neutralRecipeEndMarker, neutralRecipeBeginMarker)
	}

	var commands [][]string
	inFence := false
	for _, raw := range strings.Split(document[start:end], "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, neutralRecipeFence) {
			inFence = !inFence
			continue
		}
		if !inFence || line == "" {
			continue
		}
		if strings.ContainsAny(line, neutralRecipeUnsafeShell) {
			return nil, fmt.Errorf("documented line %q uses shell syntax this gate cannot execute faithfully", line)
		}
		fields := strings.Fields(line)
		if fields[0] != neutralRecipeCLIPrefix {
			return nil, fmt.Errorf("documented line %q must start with %s so the gate runs the same engine a contributor does",
				line, neutralRecipeCLIPrefix)
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("documented line %q names no command", line)
		}
		commands = append(commands, fields[1:])
	}
	if len(commands) == 0 {
		return nil, fmt.Errorf("the documented block contains no commands")
	}
	return commands, nil
}

// neutralRecipeDocument builds a synthetic contributor guide around body, for
// the parser's own discrimination tests.
func neutralRecipeDocument(body string) string {
	return neutralRecipeBeginMarker + "\n" + neutralRecipeFence + "bash\n" + body + "\n" +
		neutralRecipeFence + "\n" + neutralRecipeEndMarker + "\n"
}

// initNeutralContributorHistory turns the cloudless fixture into the git state a
// contributor is actually in: a trunk, a branch, and one edited file. Generated
// and installed state is ignored so the only change `--impacted` sees is the
// contributor's own.
func initNeutralContributorHistory(t *testing.T, root string) {
	t.Helper()
	writeCloudlessFile(t, root, ".gitignore", ".putnami/\n", 0o644)
	neutralGit(t, root, "init", "-q", "-b", "main")
	neutralGit(t, root, "add", "-A")
	neutralGit(t, root, "commit", "-q", "-m", "contributor clone")
	neutralGit(t, root, "checkout", "-q", "-b", "contributor-change")
	writeCloudlessFile(t, root, "apps/go/main.go", "package main\n\n// A contributor's edit.\nfunc main() {}\n", 0o644)
}

func TestInitNeutralContributorHistoryIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		root := t.TempDir()
		writeCloudlessFile(t, root, "apps/go/main.go", "package main\n\nfunc main() {}\n", 0o644)
		initNeutralContributorHistory(t, root)
		return root
	})
}

func neutralGit(t *testing.T, root string, args ...string) {
	t.Helper()
	identity := []string{
		"-c", "user.name=Neutral Contributor",
		"-c", "user.email=contributor@example.invalid",
		"-c", "commit.gpgsign=false",
	}
	cmd := gittest.Command(root, append(identity, args...)...)
	// The identity comes from the -c options above, so the environment sets
	// none of its own.
	cmd.Env = gittest.Environ(os.Environ())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func containsNeutralTask(tasks []string, want string) bool {
	for _, task := range tasks {
		if task == want {
			return true
		}
	}
	return false
}
