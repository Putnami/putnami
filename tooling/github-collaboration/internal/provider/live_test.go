package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	collab "go.putnami.dev/protocol/collaboration"
	"go.putnami.dev/protocol/collaboration/providertest"
	"go.putnami.dev/tooling/github-collaboration/internal/github"
)

// liveRepositoryVariable names the GitHub repository TestLiveGitHub runs
// against, as owner/name. Unset, the test is skipped.
const liveRepositoryVariable = "PUTNAMI_GITHUB_COLLAB_LIVE_REPO"

// liveRepositoryTopic must be a topic of that repository: the test opens
// issues, branches and pull requests there, so it refuses a repository that
// is not explicitly marked as a disposable test repository.
const liveRepositoryTopic = "putnami-collaboration-live-test"

// TestLiveGitHub runs the shared contract scenarios against a real GitHub
// repository with the credential gh or GH_TOKEN provides, then proves that
// GitHub's issue search finds an idempotency key the way a create's lookup
// asks for it. It creates a branch pair and a commit for each proposal
// scenario. When it ends, passed or failed, it closes every issue and pull
// request the run opened and deletes every branch it created. Run it with
// bin/live-test.
func TestLiveGitHub(t *testing.T) {
	repository := os.Getenv(liveRepositoryVariable)
	if repository == "" {
		t.Skip(liveRepositoryVariable + " names no repository; the live GitHub proof runs only on request (bin/live-test)")
	}
	repo, failure := parseRepository(repository, "")
	if failure != nil {
		t.Fatal(failure.Error.Message)
	}
	endpoint, err := github.ResolveEndpoint(github.DefaultHost, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	client := github.New(endpoint, github.NewCredential(endpoint.Host, os.Getenv, github.GHAuthToken), github.Options{})
	ctx := context.Background()

	var about struct {
		DefaultBranch string   `json:"default_branch"`
		Topics        []string `json:"topics"`
	}
	if _, err := client.Get(ctx, repo.path(), nil, &about); err != nil {
		t.Fatalf("read %s: %s", repository, err.Message)
	}
	if !slices.Contains(about.Topics, liveRepositoryTopic) {
		t.Fatalf("%s does not carry the topic %q; the live proof runs only in a repository marked as disposable", repository, liveRepositoryTopic)
	}
	var baseRef ghRef
	if _, err := client.Get(ctx, repo.path(append([]string{"git", "ref", "heads"}, strings.Split(about.DefaultBranch, "/")...)...), nil, &baseRef); err != nil {
		t.Fatalf("read the default branch: %s", err.Message)
	}

	live := &liveCleanup{client: client, repo: repo, tracked: map[int]bool{}}
	live.start(t)
	t.Cleanup(func() { live.run(t) })
	run := strconv.FormatInt(time.Now().Unix(), 36)
	branches := 0
	newTarget := func(contract string) func(t *testing.T) providertest.Target {
		return func(t *testing.T) providertest.Target {
			settings := `{"repository":"` + repository + `"}`
			if contract == collab.ContractTasks {
				settings = `{"repository":"` + repository + `",` + referenceStates + `}`
			}
			target := providertest.Target{
				Handlers:      New().Handlers(),
				WorkspaceRoot: t.TempDir(),
				Settings:      json.RawMessage(settings),
				Base:          about.DefaultBranch,
				Settle:        90 * time.Second,
				Track:         live.track,
			}
			if contract == collab.ContractProposals {
				for i := range target.Heads {
					branches++
					target.Heads[i] = live.branch(t, "putnami-live/"+run+"/"+strconv.Itoa(branches), baseRef.Object.SHA)
				}
			}
			return target
		}
	}
	providertest.RunTasks(t, newTarget(collab.ContractTasks))
	providertest.RunProposals(t, newTarget(collab.ContractProposals))
	t.Run("the issue search finds a key", func(t *testing.T) { liveSearchFindsAKey(t, repository, run, live) })
}

// liveSearchFindsAKey opens a task, then waits until GitHub's issue search
// finds its idempotency key with the query a create sends for a key older
// than the pages it reads (searchKey): the key is a word of an HTML comment
// at the end of the issue body.
func liveSearchFindsAKey(t *testing.T, repository, run string, live *liveCleanup) {
	key := "live-search-" + run
	session := providertest.Open(t, providertest.Target{
		Handlers: New().Handlers(), WorkspaceRoot: t.TempDir(), Settings: json.RawMessage(`{"repository":"` + repository + `"}`),
	})
	response := session.Call("tasks", "create", `{"title":"Search probe `+run+`","body":"Created by the live proof of the key search.","idempotencyKey":"`+key+`"}`)
	var created collab.TaskCreateResult
	if response.Outcome != collab.OutcomeOK || json.Unmarshal(response.Result, &created) != nil {
		t.Fatalf("create the search probe: %s %+v", response.Outcome, response.Error)
	}
	live.track(collab.ContractTasks, created.Task.Ref)

	opened, failure := New().open(context.Background(), repositorySettings{Repository: repository})
	if failure != nil {
		t.Fatal(failure.Error.Message)
	}
	tasks := &taskSession{session: opened}
	login, failure := tasks.authenticated()
	if failure != nil {
		t.Fatal(failure.Error.Message)
	}
	marker := newIdempotency("tasks.create", key, nil)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		found, failure := tasks.searchKey(marker, login)
		if failure == nil && found != nil && strconv.Itoa(found.Number) == created.Task.Ref.ID {
			t.Logf("GitHub's issue search found key %s in #%d", key, found.Number)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GitHub's issue search did not find key %s in #%s within 3 minutes (found another: %v, failure: %+v)",
				key, created.Task.Ref.ID, found != nil, failure)
		}
		time.Sleep(5 * time.Second)
	}
}

// liveCleanup closes what a live run opened. It walks the numbers above the
// newest one GitHub had given when the run started, so an item no scenario
// reported — one a failed scenario opened before it could, or a duplicate —
// is found too, and it closes only what is the run's own (ours).
type liveCleanup struct {
	client *github.Client
	repo   repository
	// login is the credential's account; only what it opened is closed.
	login string
	// before is the newest number GitHub had given when the run started.
	before int

	mu       sync.Mutex
	branches []string
	// tracked holds the numbers of the items the scenarios reported.
	tracked map[int]bool
}

// track records an item a scenario reported.
func (l *liveCleanup) track(_ string, ref collab.Ref) {
	if number, ours := l.repo.number(ref); ours {
		l.mu.Lock()
		l.tracked[number] = true
		l.mu.Unlock()
	}
}

// start records the account and the newest number GitHub has given: the
// last one before notGivenRun numbers in a row answer 404.
func (l *liveCleanup) start(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var user ghUser
	if _, err := l.client.Get(ctx, "/user", nil, &user); err != nil {
		t.Fatalf("read the authenticated account: %s", err.Message)
	}
	if user.Login == "" {
		t.Fatal("GitHub named no authenticated account")
	}
	l.login = user.Login
	var newest []ghIssue
	query := url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"desc"}, "per_page": {"1"}}
	if _, err := l.client.Get(ctx, l.repo.path("issues"), query, &newest); err != nil {
		t.Fatalf("read the newest issue: %s", err.Message)
	}
	if len(newest) > 0 {
		l.before = newest[0].Number
	}
	misses := 0
	for number := l.before + 1; misses < notGivenRun; number++ {
		_, answer, err := readNumber(ctx, l.client, l.repo, number)
		if err != nil {
			t.Fatalf("read #%d: %s", number, err.Message)
		}
		if answer == numberAbsent {
			misses++
			continue
		}
		misses = 0
		l.before = number
	}
}

// branch creates a branch one commit ahead of base, so a pull request can
// open on it.
func (l *liveCleanup) branch(t *testing.T, name, base string) string {
	t.Helper()
	ctx := context.Background()
	if err := l.client.Write(ctx, http.MethodPost, l.repo.path("git", "refs"), map[string]any{"ref": "refs/heads/" + name, "sha": base}, nil); err != nil {
		t.Fatalf("create branch %s: %s", name, err.Message)
	}
	l.mu.Lock()
	l.branches = append(l.branches, name)
	l.mu.Unlock()
	content := base64.StdEncoding.EncodeToString([]byte("Created by the GitHub collaboration provider's live proof.\n"))
	file := strings.ReplaceAll(name, "/", "-") + ".md"
	if err := l.client.Write(ctx, http.MethodPut, l.repo.path("contents", "putnami-live", file), map[string]any{
		"message": "test: live proof branch " + name, "content": content, "branch": name}, nil); err != nil {
		t.Fatalf("commit on branch %s: %s", name, err.Message)
	}
	return name
}

// run closes every open issue and pull request of the run that the account
// opened since start, and deletes every branch the run created. An open item
// of the account that is not the run's own is left open and named: another
// run's, or a person's working with the same credential.
func (l *liveCleanup) run(t *testing.T) {
	ctx := context.Background()
	var open []ghIssue
	misses := 0
	for number := l.before + 1; misses < notGivenRun; number++ {
		issue, answer, err := readNumber(ctx, l.client, l.repo, number)
		if err != nil {
			t.Errorf("read #%d, so it and every later number may still be open: %s", number, err.Message)
			break
		}
		if answer == numberAbsent {
			misses++
			continue
		}
		misses = 0
		if issue != nil && issue.State == "open" && issue.User != nil && strings.EqualFold(issue.User.Login, l.login) {
			open = append(open, *issue)
		}
	}
	// Every scenario labels what it creates with a label of its own
	// ("pt-<token>"): an issue carrying the label of a reported issue is the
	// run's too, even when no scenario could report it.
	labels := map[string]bool{}
	for _, issue := range open {
		if l.tracked[issue.Number] {
			for _, name := range issue.labelNames() {
				if strings.HasPrefix(name, "pt-") {
					labels[name] = true
				}
			}
		}
	}
	for i := range open {
		issue := &open[i]
		if !l.ours(ctx, issue, labels) {
			t.Logf("left #%d open: the account opened it during the run, and it is not the run's own", issue.Number)
			continue
		}
		path, patch := l.repo.path("issues", strconv.Itoa(issue.Number)), map[string]any{"state": "closed", "state_reason": "not_planned"}
		if issue.isPullRequest() {
			path, patch = l.repo.path("pulls", strconv.Itoa(issue.Number)), map[string]any{"state": "closed"}
		}
		if err := l.client.Write(ctx, http.MethodPatch, path, patch, nil); err != nil {
			t.Errorf("close #%d: %s", issue.Number, err.Message)
		}
	}
	for _, name := range l.branches {
		segments := append([]string{"git", "refs", "heads"}, strings.Split(name, "/")...)
		if err := l.client.Write(ctx, http.MethodDelete, l.repo.path(segments...), nil, nil); err != nil {
			t.Errorf("delete branch %s: %s", name, err.Message)
		}
	}
}

// ours reports whether an open item is the run's own: a scenario reported
// it, it is a pull request whose head is a branch of the run, or it is an
// issue carrying a label of an issue a scenario reported.
func (l *liveCleanup) ours(ctx context.Context, issue *ghIssue, labels map[string]bool) bool {
	if l.tracked[issue.Number] {
		return true
	}
	if issue.isPullRequest() {
		var pull ghPull
		if _, err := l.client.Get(ctx, l.repo.path("pulls", strconv.Itoa(issue.Number)), nil, &pull); err != nil {
			return false
		}
		return slices.Contains(l.branches, pull.Head.Ref) && pull.Head.Repo != nil &&
			strings.EqualFold(pull.Head.Repo.FullName, l.repo.owner+"/"+l.repo.name)
	}
	return slices.ContainsFunc(issue.labelNames(), func(name string) bool { return labels[name] })
}
