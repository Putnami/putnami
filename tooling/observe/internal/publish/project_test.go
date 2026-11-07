package publish

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/ci"
)

// answering returns a client that records each request and answers status
// with body.
func answering(status int, body string, seen *[]*http.Request) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		*seen = append(*seen, r)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
	})}
}

func TestTheCodeHostAnswersTheNumericProjectID(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "laptop-repository-key", "a-laptop-run-keys-its-origin-by-the-numeric-project-id")
	var seen []*http.Request
	env := map[string]string{"GITHUB_TOKEN": githubToken}
	id, err := ProjectID(context.Background(), answering(200, `{"id": 123456789}`, &seen), func(k string) string { return env[k] }, ci.ProviderGitHub, "github.com", "acme/widgets")
	if err != nil || id != "123456789" {
		t.Fatalf("id %q, err %v", id, err)
	}
	if got := seen[0].URL.String(); got != "https://api.github.com/repos/acme/widgets" {
		t.Errorf("GET %s", got)
	}
	if got := seen[0].Header.Get("Authorization"); got != "Bearer "+githubToken {
		t.Errorf("authorization %q", got)
	}

	seen = nil
	id, err = ProjectID(context.Background(), answering(200, `{"id": 77}`, &seen), func(string) string { return "" }, ci.ProviderGitLab, "gitlab.com", "group/sub/widgets")
	if err != nil || id != "77" {
		t.Fatalf("id %q, err %v", id, err)
	}
	if got := seen[0].URL.EscapedPath(); got != "/api/v4/projects/group%2Fsub%2Fwidgets" {
		t.Errorf("GET %s", got)
	}
	if got := seen[0].Header.Get("PRIVATE-TOKEN"); got != "" {
		t.Errorf("a token nobody set: %q", got)
	}
}

func TestAPrivateRepositoryWithoutATokenAnswersNoID(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "laptop-repository-key", "an-origin-without-an-id-is-a-diagnostic-naming-the-token")
	var seen []*http.Request
	_, err := ProjectID(context.Background(), answering(404, `{"message":"Not Found"}`, &seen), func(string) string { return "" }, ci.ProviderGitHub, "github.com", "acme/secret")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err %v", err)
	}
	if seen[0].Header.Get("Authorization") != "" {
		t.Errorf("an authorization nobody set")
	}
	if _, err := ProjectID(context.Background(), answering(200, `{"id": "abc"}`, &seen), func(string) string { return "" }, ci.ProviderGitHub, "github.com", "acme/widgets"); err == nil {
		t.Errorf("a non-numeric id was taken")
	}
}
