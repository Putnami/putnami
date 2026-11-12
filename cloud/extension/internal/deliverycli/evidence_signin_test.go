package deliverycli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	personTestToken = "person-session-token"
	testReportURL   = "https://putnami.app/o/acme/changes/pr-7"
)

// personEnv is a laptop with no token, whose Putnami home holds a session
// when signedIn.
func personEnv(t *testing.T, signedIn bool) map[string]string {
	t.Helper()
	env := map[string]string{"PUTNAMI_CLOUD_API_URL": reportTestBaseURL, "PUTNAMI_HOME": t.TempDir()}
	if signedIn {
		storePersonSession(t, env)
	}
	return env
}

func storePersonSession(t *testing.T, env map[string]string) {
	t.Helper()
	auth := &clicore.StoredToken{AccessToken: personTestToken, TokenType: "Bearer", ExpiresAt: "2030-01-01T00:00:00Z"}
	if err := clicore.WriteAuth(auth, env); err != nil {
		t.Fatal(err)
	}
}

// runSignIn runs evidence-report with signIn and returns its stdout, stderr
// and error.
func runSignIn(t *testing.T, fake *evidenceFakeServer, env map[string]string, signIn func() error, args ...string) (string, string, error) {
	t.Helper()
	stubReportRetrySleep(t)
	env[evidenceInvocationDirEnv] = t.TempDir()
	var out, log []string
	ioctx := clicore.IO{
		Env:    env,
		Stdout: func(line string) { out = append(out, line) },
		Stderr: func(line string) { log = append(log, line) },
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC) },
	}
	err := EvidenceReport(clicore.MergeParams(clicore.ParseFlags(args)), env, ioctx, signIn)
	return strings.Join(out, "\n"), strings.Join(log, "\n"), err
}

func refuseSignIn(t *testing.T) func() error {
	return func() error {
		t.Error("the device sign-in ran")
		return nil
	}
}

// assertPersonUpload checks every request rides the person's bearer and names
// the repository of the turbo fixture, never a workspace.
func assertPersonUpload(t *testing.T, fake *evidenceFakeServer) {
	t.Helper()
	if len(fake.requests) < 2 {
		t.Fatalf("requests %v", fake.paths())
	}
	for _, r := range fake.requests {
		if r.Auth != "Bearer "+personTestToken {
			t.Errorf("%s %s rides %q", r.Method, r.Path, r.Auth)
		}
		if _, ok := r.Body["workspace"]; ok {
			t.Errorf("%s names a workspace: %v", r.Path, r.Body["workspace"])
		}
		repository, _ := r.Body["repository"].(map[string]any)
		if repository["host"] != "github.com" || repository["path"] != "acme/storefront" {
			t.Errorf("%s names the repository %v", r.Path, r.Body["repository"])
		}
		if r.Body["provider"] != "github" || r.Body["repo"] == nil || r.Body["repo"] == "" {
			t.Errorf("%s names provider %v, repo %v", r.Path, r.Body["provider"], r.Body["repo"])
		}
	}
}

func TestSignInUsesTheStoredSession(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "signs-a-person-in", "sign-in-uses-the-stored-session")
	spectest.Proves(t, evidenceFeature, "signs-a-person-in", "a-person-upload-names-the-repository-and-no-workspace")
	fake := &evidenceFakeServer{}
	if _, _, err := runSignIn(t, fake, personEnv(t, true), refuseSignIn(t), "--file", evidenceFixture("turbo-pull-request.json"), "--sign-in"); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	assertPersonUpload(t, fake)
}

func TestSignInRunsTheDeviceSignInWithoutASession(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "signs-a-person-in", "sign-in-runs-the-device-sign-in-without-a-session")
	fake := &evidenceFakeServer{}
	env := personEnv(t, false)
	calls := 0
	signIn := func() error {
		calls++
		storePersonSession(t, env)
		return nil
	}
	if _, _, err := runSignIn(t, fake, env, signIn, "--file", evidenceFixture("turbo-pull-request.json"), "--sign-in"); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the device sign-in ran %d times", calls)
	}
	assertPersonUpload(t, fake)

	// Without --sign-in, no token is a refusal that names both ways.
	fake = &evidenceFakeServer{}
	_, _, err := runSignIn(t, fake, personEnv(t, false), refuseSignIn(t), "--file", evidenceFixture("turbo-pull-request.json"))
	if err == nil || !strings.Contains(err.Error(), "--sign-in") || len(fake.requests) != 0 {
		t.Fatalf("err %v, requests %v", err, fake.paths())
	}
}

// An organization's ingest token names no workspace: Delivery finds the
// observed workspace from the repository every request names.
func TestATokenUploadNamesTheRepository(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "signs-a-person-in", "a-token-upload-names-the-repository")
	fake := &evidenceFakeServer{}
	env := personEnv(t, true)
	env[evidenceTokenEnv] = evidenceTestToken
	if _, _, err := runSignIn(t, fake, env, refuseSignIn(t), "--file", evidenceFixture("turbo-pull-request.json"), "--sign-in"); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if len(fake.requests) < 2 {
		t.Fatalf("requests %v", fake.paths())
	}
	for _, r := range fake.requests {
		if r.Auth != "Bearer "+evidenceTestToken {
			t.Errorf("%s rides %q", r.Path, r.Auth)
		}
		repository, _ := r.Body["repository"].(map[string]any)
		if repository["host"] != "github.com" || repository["path"] != "acme/storefront" {
			t.Errorf("%s names the repository %v", r.Path, r.Body["repository"])
		}
		if r.Body["provider"] != "github" || r.Body["repo"] == nil || r.Body["repo"] == "" {
			t.Errorf("%s names provider %v, repo %v", r.Path, r.Body["provider"], r.Body["repo"])
		}
	}
}

func TestTheResultCarriesTheReportURL(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "answers-the-report-url", "the-result-carries-the-report-url")
	fake := &evidenceFakeServer{reportURL: testReportURL}
	stdout, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json"), "--output=json")
	if err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	var result struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("result %s: %v", stdout, err)
	}
	if result.Data["reportUrl"] != testReportURL {
		t.Fatalf("data %v", result.Data)
	}
	text, err := runEvidenceReport(t, &evidenceFakeServer{reportURL: testReportURL}, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json"))
	if err != nil || !strings.Contains(text, "the report is at "+testReportURL) {
		t.Fatalf("summary %q, err %v", text, err)
	}
	stdout, err = runEvidenceReport(t, &evidenceFakeServer{reportURL: "http://putnami.app/r"}, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json"), "--output=json")
	if err != nil || strings.Contains(stdout, "reportUrl") {
		t.Fatalf("an http report URL reached the result: %s (%v)", stdout, err)
	}
}
