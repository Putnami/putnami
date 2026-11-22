package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// jenkins drops the GitLab variables of the fixture and keeps a Jenkins job
// of a github.com clone that holds a read token.
func jenkins(t *testing.T, repo string, fixture map[string]string) map[string]string {
	t.Helper()
	git(t, repo, "remote", "add", "origin", "git@github.com:acme/shop.git")
	return map[string]string{
		"JENKINS_URL":            "https://jenkins.example.com/",
		"GITHUB_TOKEN":           "ghs_fixturetoken",
		"PUTNAMI_CLI_EXECUTABLE": fixture["PUTNAMI_CLI_EXECUTABLE"],
	}
}

func TestAJenkinsUploadKeysItsOriginWithTheToken(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "ci-repository-key", "another-ci-keys-its-origin-by-the-numeric-project-id")
	repo, fixture, record := uploadFixture(t, "0")
	env := jenkins(t, repo, fixture)
	var asked []string
	previous := publishClient
	publishClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.Method+" "+r.URL.String()+" "+r.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id": 777}`)), Request: r}, nil
	})}
	t.Cleanup(func() { publishClient = previous })
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--no-fetch", "--base", "main", "--upload"}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if len(asked) != 1 || asked[0] != "GET https://api.github.com/repos/acme/shop Bearer ghs_fixturetoken" {
		t.Fatalf("asked %v", asked)
	}
	data, err := os.ReadFile(filepath.Join(record, "document"))
	if err != nil {
		t.Fatalf("the reporter got no document: %v", err)
	}
	var doc struct {
		Subject struct {
			Provider  string `json:"provider"`
			Host      string `json:"host"`
			ProjectID string `json:"projectId"`
		} `json:"subject"`
		CI struct {
			System string `json:"system"`
		} `json:"ci"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Subject.Provider != "github" || doc.Subject.Host != "github.com" || doc.Subject.ProjectID != "777" || doc.CI.System != "jenkins" {
		t.Fatalf("document subject %+v, ci %+v", doc.Subject, doc.CI)
	}
}

func TestAJenkinsRunWithoutUploadAsksTheCodeHostNothing(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "ci-repository-key", "another-ci-without-upload-asks-the-code-host-nothing")
	repo, fixture, record := uploadFixture(t, "0")
	env := jenkins(t, repo, fixture)
	previous := publishClient
	publishClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		t.Errorf("a run without --upload called %s", r.URL)
		return nil, errors.New("no request leaves the machine")
	})}
	t.Cleanup(func() { publishClient = previous })
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--no-fetch", "--base", "main"}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(record, "args")); !os.IsNotExist(err) {
		t.Fatalf("a reporter ran: %v", err)
	}
}
