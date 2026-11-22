package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.putnami.dev/protocol/codehost"
	"go.putnami.dev/tooling/observe/internal/ci"
	"go.putnami.dev/tooling/observe/internal/observe"
)

// gitHub publishes on a pull request through the GitHub REST API.
type gitHub struct {
	api
	repository string // owner/name, each segment escaped
	number     int
	headSHA    string
}

// gitHubAPI is the API of github.com; GitHub Enterprise names its own in
// GITHUB_API_URL.
var gitHubAPI = codehost.For(codehost.GitHub).DefaultAPIBase

func newGitHub(client *http.Client, getenv func(string) string, token string, o observe.Observation) (*gitHub, error) {
	base, err := apiBase(firstOf(getenv("GITHUB_API_URL"), gitHubAPI))
	if err != nil {
		return nil, err
	}
	owner, name, ok := strings.Cut(o.Subject.Repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("the repository %q is not owner/name", o.Subject.Repository)
	}
	return &gitHub{
		api: api{client: client, base: base, auth: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Accept", "application/vnd.github+json")
			r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		}},
		repository: url.PathEscape(owner) + "/" + url.PathEscape(name),
		number:     o.Subject.ChangeRequest.Number,
		headSHA:    o.Subject.ChangeRequest.HeadSHA,
	}, nil
}

func (g *gitHub) requestLabel() string { return label(ci.ProviderGitHub, g.number) }

// statusSHA is the pull request's head: the job built a merge commit that is
// no commit of the pull request.
func (g *gitHub) statusSHA() string { return g.headSHA }

func (g *gitHub) setStatus(ctx context.Context, sha string, s status) error {
	body := map[string]string{"state": "success", "context": StatusName, "description": s.Description}
	if s.TargetURL != "" {
		body["target_url"] = s.TargetURL
	}
	return g.do(ctx, http.MethodPost, "/repos/"+g.repository+"/statuses/"+url.PathEscape(sha), nil, body, nil)
}

type gitHubComment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

func (g *gitHub) findNote(ctx context.Context) (string, error) {
	var id string
	err := pages(func(page int) (int, bool, error) {
		var comments []gitHubComment
		query := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if err := g.do(ctx, http.MethodGet, g.commentsPath(), query, nil, &comments); err != nil {
			return 0, false, err
		}
		for _, c := range comments {
			if marked(c.Body) {
				id = strconv.FormatInt(c.ID, 10)
				return len(comments), true, nil
			}
		}
		return len(comments), false, nil
	})
	return id, err
}

func (g *gitHub) createNote(ctx context.Context, body string) (string, error) {
	var c gitHubComment
	err := g.do(ctx, http.MethodPost, g.commentsPath(), nil, map[string]string{"body": body}, &c)
	return c.HTMLURL, err
}

func (g *gitHub) updateNote(ctx context.Context, id, body string) (string, error) {
	var c gitHubComment
	err := g.do(ctx, http.MethodPatch, "/repos/"+g.repository+"/issues/comments/"+url.PathEscape(id), nil, map[string]string{"body": body}, &c)
	return c.HTMLURL, err
}

func (g *gitHub) commentsPath() string {
	return "/repos/" + g.repository + "/issues/" + strconv.Itoa(g.number) + "/comments"
}

// gitLab publishes on a merge request through the GitLab REST API v4.
type gitLab struct {
	api
	project    string // numeric project id
	number     int
	sha        string // the commit the status goes on
	pipeline   uint64 // the pipeline the status joins, 0 for none
	projectURL string // the project's page, "" when unknown
}

func newGitLab(client *http.Client, getenv func(string) string, token string, o observe.Observation) (*gitLab, error) {
	raw := strings.TrimSpace(getenv("CI_API_V4_URL"))
	if raw == "" {
		server := strings.TrimSpace(getenv("CI_SERVER_URL"))
		if server == "" {
			return nil, errors.New("neither CI_API_V4_URL nor CI_SERVER_URL names the GitLab API")
		}
		raw = strings.TrimRight(server, "/") + "/api/v4"
	}
	base, err := apiBase(raw)
	if err != nil {
		return nil, err
	}
	if o.Subject.ProjectID == "" {
		return nil, errors.New("CI_PROJECT_ID names no numeric project")
	}
	g := &gitLab{
		api:     api{client: client, base: base, auth: func(r *http.Request) { r.Header.Set("PRIVATE-TOKEN", token) }},
		project: url.PathEscape(o.Subject.ProjectID),
		number:  o.Subject.ChangeRequest.Number,
		sha:     o.Subject.ChangeRequest.HeadSHA,
	}
	// The status goes on the commit the job's pipeline ran for and joins that
	// pipeline, the one the merge request shows. A merged results pipeline
	// runs for a merge commit: a status on the source branch's head would
	// start another pipeline.
	if commit := strings.ToLower(strings.TrimSpace(getenv("CI_COMMIT_SHA"))); fullSHA(commit) {
		g.sha = commit
		g.pipeline, _ = strconv.ParseUint(o.CI.RunID, 10, 64)
	}
	if page, err := apiBase(getenv("CI_PROJECT_URL")); err == nil {
		g.projectURL = page.String()
	}
	return g, nil
}

func (g *gitLab) requestLabel() string { return label(ci.ProviderGitLab, g.number) }

func (g *gitLab) statusSHA() string { return g.sha }

// alreadySet is GitLab's answer to a status that already has this state, as
// when a job is retried on the same commit.
const alreadySet = "Cannot transition status"

func (g *gitLab) setStatus(ctx context.Context, sha string, s status) error {
	body := map[string]any{"state": "success", "name": StatusName, "description": s.Description}
	if s.TargetURL != "" {
		body["target_url"] = s.TargetURL
	}
	if g.pipeline > 0 {
		body["pipeline_id"] = g.pipeline
	}
	err := g.do(ctx, http.MethodPost, "/projects/"+g.project+"/statuses/"+url.PathEscape(sha), nil, body, nil)
	var refused *apiError
	if errors.As(err, &refused) && refused.status == http.StatusBadRequest && strings.Contains(refused.message, alreadySet) {
		return nil
	}
	return err
}

type gitLabNote struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	System bool   `json:"system"`
}

func (g *gitLab) findNote(ctx context.Context) (string, error) {
	var id string
	err := pages(func(page int) (int, bool, error) {
		var notes []gitLabNote
		query := url.Values{
			"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)},
			"sort": {"asc"}, "order_by": {"created_at"},
		}
		if err := g.do(ctx, http.MethodGet, g.notesPath(), query, nil, &notes); err != nil {
			return 0, false, err
		}
		for _, n := range notes {
			if !n.System && marked(n.Body) {
				id = strconv.FormatInt(n.ID, 10)
				return len(notes), true, nil
			}
		}
		return len(notes), false, nil
	})
	return id, err
}

func (g *gitLab) createNote(ctx context.Context, body string) (string, error) {
	var n gitLabNote
	if err := g.do(ctx, http.MethodPost, g.notesPath(), nil, map[string]string{"body": body}, &n); err != nil {
		return "", err
	}
	return g.noteURL(n.ID), nil
}

func (g *gitLab) updateNote(ctx context.Context, id, body string) (string, error) {
	var n gitLabNote
	if err := g.do(ctx, http.MethodPut, g.notesPath()+"/"+url.PathEscape(id), nil, map[string]string{"body": body}, &n); err != nil {
		return "", err
	}
	return g.noteURL(n.ID), nil
}

func (g *gitLab) notesPath() string {
	return "/projects/" + g.project + "/merge_requests/" + strconv.Itoa(g.number) + "/notes"
}

func (g *gitLab) noteURL(id int64) string {
	if g.projectURL == "" || id <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/-/merge_requests/%d#note_%d", g.projectURL, g.number, id)
}

// fullSHA reports whether value is a full lowercase SHA-1 or SHA-256 id.
func fullSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	return strings.Trim(value, "0123456789abcdef") == ""
}

// marked reports whether a note body is the Putnami note: it starts with
// Marker after leading white space.
func marked(body string) bool {
	return codehost.IsPutnamiNote(body)
}

func firstOf(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ProjectID asks the code host API for the numeric id of the repository at
// path on host: GitHub GET /repos/{owner}/{repo}, GitLab GET /projects/{path}.
// It sends GITHUB_TOKEN or PUTNAMI_GITLAB_TOKEN when one is set, so a private
// repository answers too. GITHUB_API_URL and CI_API_V4_URL name another API.
func ProjectID(ctx context.Context, client *http.Client, getenv func(string) string, provider, host, path string) (string, error) {
	var raw, resource, token string
	var auth func(*http.Request, string)
	switch provider {
	case ci.ProviderGitHub:
		owner, name, ok := strings.Cut(path, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return "", fmt.Errorf("the repository %q is not owner/name", path)
		}
		raw = firstOf(getenv("GITHUB_API_URL"), gitHubAPI)
		resource = "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
		token = strings.TrimSpace(getenv("GITHUB_TOKEN"))
		auth = func(r *http.Request, token string) {
			r.Header.Set("Accept", "application/vnd.github+json")
			r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
		}
	case ci.ProviderGitLab:
		raw = firstOf(getenv("CI_API_V4_URL"), "https://"+host+"/api/v4")
		resource = "/projects/" + url.PathEscape(path)
		token = strings.TrimSpace(getenv("PUTNAMI_GITLAB_TOKEN"))
		auth = func(r *http.Request, token string) {
			if token != "" {
				r.Header.Set("PRIVATE-TOKEN", token)
			}
		}
	default:
		return "", fmt.Errorf("the code host %q has no API observe reads", provider)
	}
	base, err := apiBase(raw)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var answer struct {
		ID json.Number `json:"id"`
	}
	a := api{client: apiClient(client), base: base, auth: func(r *http.Request) { auth(r, token) }}
	if err := a.do(ctx, http.MethodGet, resource, nil, nil, &answer); err != nil {
		return "", err
	}
	id := answer.ID.String()
	if id == "" || strings.Trim(id, "0123456789") != "" || id[0] == '0' {
		return "", fmt.Errorf("the API answered the id %q, not a positive integer", id)
	}
	return id, nil
}

// PullRequestQuery names the pull request of a run from a fork: the head
// owner, branch and commit the run built, in the job's repository.
type PullRequestQuery struct {
	// Repository is the job's repository, owner/name.
	Repository string
	HeadOwner  string
	HeadBranch string
	HeadSHA    string
	// Getenv reads GITHUB_API_URL and GITHUB_TOKEN.
	Getenv func(string) string
	// Client sends the request; nil uses a client with a per-request
	// timeout. Redirects are never followed.
	Client *http.Client
}

// queryHost is the GitHub API of the query's repository, with GITHUB_TOKEN.
func queryHost(q PullRequestQuery) (*gitHub, error) {
	token := strings.TrimSpace(q.Getenv(GitHubTokenEnv))
	if token == "" {
		return nil, fmt.Errorf("%s is unset", GitHubTokenEnv)
	}
	return newGitHub(apiClient(q.Client), q.Getenv, token, observe.Observation{
		Subject: ci.Subject{Repository: q.Repository, ChangeRequest: &ci.ChangeRequest{}},
	})
}

// FindPullRequest asks the GitHub API for the open pull requests of the
// job's repository whose head is the query's owner and branch, and returns
// the number of the one whose head commit is the query's, 0 when none is.
// GitHub lists no pull request in the workflow_run payload of a fork's run,
// so the job asks with its own GITHUB_TOKEN.
func FindPullRequest(ctx context.Context, q PullRequestQuery) (int, error) {
	if q.HeadOwner == "" || q.HeadBranch == "" || q.HeadSHA == "" {
		return 0, errors.New("the workflow run names no head owner, branch and commit")
	}
	g, err := queryHost(q)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var number int
	err = pages(func(page int) (int, bool, error) {
		var pulls []struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		query := url.Values{
			"state":    {"open"},
			"head":     {q.HeadOwner + ":" + q.HeadBranch},
			"per_page": {strconv.Itoa(perPage)},
			"page":     {strconv.Itoa(page)},
		}
		if err := g.do(ctx, http.MethodGet, "/repos/"+g.repository+"/pulls", query, nil, &pulls); err != nil {
			return 0, false, err
		}
		for _, p := range pulls {
			if p.Number > 0 && strings.EqualFold(p.Head.SHA, q.HeadSHA) {
				number = p.Number
				return len(pulls), true, nil
			}
		}
		return len(pulls), false, nil
	})
	return number, err
}

// CommitParents returns the parents of the commit sha of the query's
// repository, as the GitHub API answers them, lowercase.
func CommitParents(ctx context.Context, q PullRequestQuery, sha string) ([]string, error) {
	g, err := queryHost(q)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var commit struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := g.do(ctx, http.MethodGet, "/repos/"+g.repository+"/commits/"+url.PathEscape(sha), nil, nil, &commit); err != nil {
		return nil, err
	}
	parents := make([]string, 0, len(commit.Parents))
	for _, p := range commit.Parents {
		parents = append(parents, strings.ToLower(p.SHA))
	}
	return parents, nil
}

// MergeBase returns the merge base of base and head in the query's
// repository, as the GitHub API compares them, lowercase.
func MergeBase(ctx context.Context, q PullRequestQuery, base, head string) (string, error) {
	g, err := queryHost(q)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var compare struct {
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	query := url.Values{"per_page": {"1"}}
	if err := g.do(ctx, http.MethodGet, "/repos/"+g.repository+"/compare/"+url.PathEscape(base+"..."+head), query, nil, &compare); err != nil {
		return "", err
	}
	return strings.ToLower(compare.MergeBaseCommit.SHA), nil
}

// MergeCommit returns the test merge commit GitHub keeps for the open pull
// request number of the query's repository, lowercase, "" when it has none.
func MergeCommit(ctx context.Context, q PullRequestQuery, number int) (string, error) {
	g, err := queryHost(q)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	var pull struct {
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	if err := g.do(ctx, http.MethodGet, "/repos/"+g.repository+"/pulls/"+strconv.Itoa(number), nil, nil, &pull); err != nil {
		return "", err
	}
	return strings.ToLower(pull.MergeCommitSHA), nil
}
