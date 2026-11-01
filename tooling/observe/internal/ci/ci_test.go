package ci

import (
	"errors"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe"

const (
	headSHA   = "1111111111111111111111111111111111111111"
	mergeSHA  = "2222222222222222222222222222222222222222"
	baseSHA   = "3333333333333333333333333333333333333333"
	beforeSHA = "4444444444444444444444444444444444444444"
)

func envOf(values map[string]string) Env {
	return func(key string) string { return values[key] }
}

func filesOf(files map[string]string) ReadFile {
	return func(path string) ([]byte, error) {
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, errors.New("no such file")
	}
}

func codes(d Detection) []string {
	if len(d.Diagnostics) == 0 {
		return nil
	}
	out := make([]string, 0, len(d.Diagnostics))
	for _, diagnostic := range d.Diagnostics {
		out = append(out, diagnostic.Code)
	}
	return out
}

func TestAGitHubActionsPullRequest(t *testing.T) {
	spectest.Proves(t, feature, "ci-detection", "a-github-actions-pull-request")
	env := envOf(map[string]string{
		"GITHUB_ACTIONS":       "true",
		"GITHUB_SERVER_URL":    "https://github.com",
		"GITHUB_REPOSITORY":    "acme/shop",
		"GITHUB_REPOSITORY_ID": "123456",
		"GITHUB_SHA":           mergeSHA,
		"GITHUB_REF":           "refs/pull/42/merge",
		"GITHUB_HEAD_REF":      "feature/cart",
		"GITHUB_REF_NAME":      "42/merge",
		"GITHUB_BASE_REF":      "main",
		"GITHUB_RUN_ID":        "987",
		"GITHUB_JOB":           "test",
		"GITHUB_RUN_ATTEMPT":   "2",
		"GITHUB_EVENT_PATH":    "/event.json",
	})
	event := `{"pull_request":{"number":42,"head":{"sha":"` + headSHA + `"},"base":{"sha":"` + baseSHA + `"}}}`
	d := Detect(env, filesOf(map[string]string{"/event.json": event}), Overrides{})

	attempt := 2
	want := Detection{
		Subject: Subject{
			Provider: ProviderGitHub, Host: "github.com", ProjectID: "123456", Repository: "acme/shop",
			SHA: headSHA, Branch: "feature/cart", ChangeRequest: &ChangeRequest{Number: 42},
		},
		Run: Run{
			System: SystemGitHubActions, RunID: "987", JobID: "test", Attempt: &attempt,
			URL: "https://github.com/acme/shop/actions/runs/987",
		},
		Head: "HEAD",
		Base: &Base{Ref: "main", SHA: baseSHA, Source: BaseFromPullRequest},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("detection:\n got %+v\nwant %+v", d, want)
	}
}

func TestAGitHubActionsPushIsMeasuredFromThePreviousCommit(t *testing.T) {
	spectest.Proves(t, feature, "ci-detection", "a-github-actions-pull-request")
	env := envOf(map[string]string{
		"GITHUB_ACTIONS":       "true",
		"GITHUB_REPOSITORY":    "acme/shop",
		"GITHUB_REPOSITORY_ID": "123456",
		"GITHUB_SHA":           headSHA,
		"GITHUB_REF":           "refs/heads/main",
		"GITHUB_REF_NAME":      "main",
		"GITHUB_EVENT_PATH":    "/event.json",
	})
	for name, tc := range map[string]struct {
		before string
		base   *Base
	}{
		"a push":                    {before: beforeSHA, base: &Base{SHA: beforeSHA, Source: BaseFromPush}},
		"a push that made a branch": {before: "0000000000000000000000000000000000000000"},
	} {
		t.Run(name, func(t *testing.T) {
			d := Detect(env, filesOf(map[string]string{"/event.json": `{"before":"` + tc.before + `"}`}), Overrides{})
			if !reflect.DeepEqual(d.Base, tc.base) {
				t.Fatalf("base %+v, want %+v", d.Base, tc.base)
			}
			if d.Subject.Host != "github.com" || d.Subject.ChangeRequest != nil || d.Subject.Branch != "main" {
				t.Fatalf("subject %+v", d.Subject)
			}
			if got := codes(d); (tc.base == nil) != reflect.DeepEqual(got, []string{CodeBaseMissing}) {
				t.Fatalf("codes %v with base %+v", got, tc.base)
			}
		})
	}
}

func TestAnUnreadableGitHubEventIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "ci-detection", "a-github-actions-pull-request")
	env := envOf(map[string]string{
		"GITHUB_ACTIONS":       "true",
		"GITHUB_REPOSITORY_ID": "123456",
		"GITHUB_EVENT_PATH":    "/event.json",
	})
	for name, files := range map[string]map[string]string{
		"missing":   {},
		"malformed": {"/event.json": "{"},
	} {
		t.Run(name, func(t *testing.T) {
			d := Detect(env, filesOf(files), Overrides{})
			if got := codes(d); !reflect.DeepEqual(got, []string{CodeEventUnreadable, CodeBaseMissing}) {
				t.Fatalf("codes %v", got)
			}
		})
	}
}

func TestAGitLabMergeRequest(t *testing.T) {
	spectest.Proves(t, feature, "ci-detection", "a-gitlab-merge-request")
	env := envOf(map[string]string{
		"GITLAB_CI":                           "true",
		"CI_SERVER_HOST":                      "gitlab.example.com",
		"CI_PROJECT_ID":                       "77",
		"CI_PROJECT_PATH":                     "group/sub/app",
		"CI_COMMIT_SHA":                       headSHA,
		"CI_COMMIT_REF_NAME":                  "feature/x",
		"CI_MERGE_REQUEST_SOURCE_BRANCH_NAME": "feature/x",
		"CI_MERGE_REQUEST_TARGET_BRANCH_NAME": "main",
		"CI_MERGE_REQUEST_IID":                "9",
		"CI_MERGE_REQUEST_DIFF_BASE_SHA":      baseSHA,
		"CI_PIPELINE_ID":                      "555",
		"CI_PIPELINE_URL":                     "https://gitlab.example.com/group/sub/app/-/pipelines/555",
		"CI_JOB_ID":                           "666",
	})
	d := Detect(env, filesOf(nil), Overrides{})
	want := Detection{
		Subject: Subject{
			Provider: ProviderGitLab, Host: "gitlab.example.com", ProjectID: "77", Repository: "group/sub/app",
			SHA: headSHA, Branch: "feature/x", ChangeRequest: &ChangeRequest{Number: 9},
		},
		Run: Run{
			System: SystemGitLabCI, RunID: "555", JobID: "666",
			URL: "https://gitlab.example.com/group/sub/app/-/pipelines/555",
		},
		Head: "HEAD",
		Base: &Base{SHA: baseSHA, Ref: "main", Source: BaseFromMergeRequest},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("detection:\n got %+v\nwant %+v", d, want)
	}
}

func TestAGitLabBranchPipelineIsMeasuredFromThePreviousCommit(t *testing.T) {
	spectest.Proves(t, feature, "ci-detection", "a-gitlab-merge-request")
	for name, tc := range map[string]struct {
		before string
		base   *Base
	}{
		"a push":                    {before: beforeSHA, base: &Base{SHA: beforeSHA, Source: BaseFromPush}},
		"a push that made a branch": {before: "0000000000000000000000000000000000000000"},
		"no previous commit":        {},
	} {
		t.Run(name, func(t *testing.T) {
			d := Detect(envOf(map[string]string{
				"GITLAB_CI":            "true",
				"CI_SERVER_URL":        "https://gitlab.com",
				"CI_PROJECT_ID":        "77",
				"CI_COMMIT_BRANCH":     "main",
				"CI_COMMIT_BEFORE_SHA": tc.before,
			}), filesOf(nil), Overrides{})
			if !reflect.DeepEqual(d.Base, tc.base) {
				t.Fatalf("base %+v, want %+v", d.Base, tc.base)
			}
			if d.Subject.Host != "gitlab.com" || d.Subject.Branch != "main" {
				t.Fatalf("subject %+v", d.Subject)
			}
		})
	}
}

func TestAGenericCITakesItsBaseAndHeadFromFlags(t *testing.T) {
	spectest.Proves(t, feature, "ci-detection", "a-generic-ci-takes-its-base-and-head-from-flags")
	d := Detect(envOf(nil), filesOf(nil), Overrides{Base: "origin/main", Head: "feature"})
	want := Detection{
		Subject: Subject{Provider: ProviderGeneric},
		Run:     Run{System: SystemGeneric},
		Head:    "feature",
		Base:    &Base{Ref: "origin/main", Source: BaseFromFlag},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("detection:\n got %+v\nwant %+v", d, want)
	}

	d = Detect(envOf(nil), filesOf(nil), Overrides{Base: "ABCDEF0123456789ABCDEF0123456789ABCDEF01"})
	if !reflect.DeepEqual(d.Base, &Base{SHA: "abcdef0123456789abcdef0123456789abcdef01", Source: BaseFromFlag}) {
		t.Fatalf("a commit id base reads %+v", d.Base)
	}

	d = Detect(envOf(nil), filesOf(nil), Overrides{})
	if d.Base != nil || d.Head != "HEAD" {
		t.Fatalf("no flags gives base %+v and head %q", d.Base, d.Head)
	}
	if got := codes(d); !reflect.DeepEqual(got, []string{CodeBaseMissing}) {
		t.Fatalf("codes %v, want the missing base", got)
	}
}

func TestFlagsOverrideWhatTheCISays(t *testing.T) {
	spectest.Proves(t, feature, "ci-detection", "a-generic-ci-takes-its-base-and-head-from-flags")
	d := Detect(envOf(map[string]string{
		"GITLAB_CI":                      "true",
		"CI_PROJECT_ID":                  "77",
		"CI_COMMIT_SHA":                  headSHA,
		"CI_MERGE_REQUEST_DIFF_BASE_SHA": baseSHA,
	}), filesOf(nil), Overrides{Base: "release", Head: "HEAD~1"})
	if !reflect.DeepEqual(d.Base, &Base{Ref: "release", Source: BaseFromFlag}) {
		t.Fatalf("base %+v", d.Base)
	}
	if d.Head != "HEAD~1" || d.Subject.SHA != "" {
		t.Fatalf("head %q and sha %q: the flag's head is resolved by Git, not taken from the CI", d.Head, d.Subject.SHA)
	}
}

func TestTheRepositoryKeyIsTheNumericProjectID(t *testing.T) {
	spectest.Proves(t, feature, "ci-detection", "the-repository-key-is-the-numeric-project-id")
	for name, tc := range map[string]struct {
		env   map[string]string
		id    string
		codes []string
	}{
		"github": {
			env: map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/renamed", "GITHUB_REPOSITORY_ID": "123456", "GITHUB_BASE_REF": "main"},
			id:  "123456",
		},
		"github without an id": {
			env:   map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_REPOSITORY": "acme/shop", "GITHUB_BASE_REF": "main"},
			codes: []string{CodeProjectIDMissing},
		},
		"gitlab": {
			env: map[string]string{"GITLAB_CI": "true", "CI_PROJECT_PATH": "moved/app", "CI_PROJECT_ID": "77", "CI_MERGE_REQUEST_DIFF_BASE_SHA": baseSHA},
			id:  "77",
		},
		"gitlab with a path for an id": {
			env:   map[string]string{"GITLAB_CI": "true", "CI_PROJECT_ID": "group/app", "CI_MERGE_REQUEST_DIFF_BASE_SHA": baseSHA},
			codes: []string{CodeProjectIDMissing},
		},
		"gitlab with a leading zero": {
			env:   map[string]string{"GITLAB_CI": "true", "CI_PROJECT_ID": "077", "CI_MERGE_REQUEST_DIFF_BASE_SHA": baseSHA},
			codes: []string{CodeProjectIDMissing},
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := Detect(envOf(tc.env), filesOf(nil), Overrides{})
			if d.Subject.ProjectID != tc.id {
				t.Fatalf("project id %q, want %q", d.Subject.ProjectID, tc.id)
			}
			if got := codes(d); !reflect.DeepEqual(got, tc.codes) {
				t.Fatalf("codes %v, want %v", got, tc.codes)
			}
		})
	}
}
