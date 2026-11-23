package deliverycli

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

// gitlabTestIDToken is an ID token gitlab.com minted for the test
// auth-server; its signature is the auth-server's to verify.
var gitlabTestIDToken = gitlabIDToken(map[string]any{"iss": "https://gitlab.com", "aud": actionsTestAuthURL, "project_path": "shop/backoffice"})

// gitlabIDToken is an unsigned JSON Web Token with the claims.
func gitlabIDToken(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"
}

// gitlabComDocument is the merge request fixture of a project on gitlab.com,
// the instance the job runs on.
func gitlabComDocument(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(evidenceFixture("symfony-merge-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["subject"].(map[string]any)["host"] = gitlabHost
	return writeEvidenceDoc(t, doc)
}

// gitlabEnv is a gitlab.com CI job whose id_tokens declares PUTNAMI_ID_TOKEN,
// that names its organization and holds no PUTNAMI_CLOUD_TOKEN.
func gitlabEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"GITLAB_CI":              "true",
		"CI_SERVER_HOST":         "gitlab.com",
		"CI_PROJECT_PATH":        "shop/backoffice",
		evidenceOrganizationEnv:  actionsTestOrganization,
		gitlabIDTokenEnv:         gitlabTestIDToken,
		"PUTNAMI_AUTH_URL":       actionsTestAuthURL,
		"PUTNAMI_CLOUD_API_URL":  reportTestBaseURL,
		"PUTNAMI_HOME":           t.TempDir(),
		evidenceInvocationDirEnv: t.TempDir(),
	}
}

func runGitLab(t *testing.T, fake *actionsFake, env map[string]string, extra ...string) (string, error) {
	t.Helper()
	stubReportRetrySleep(t)
	var log []string
	ioctx := clicore.IO{
		Env:    env,
		Stdout: func(string) {},
		Stderr: func(line string) { log = append(log, line) },
		Client: &http.Client{Transport: fake},
		Now:    func() time.Time { return time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC) },
	}
	params := clicore.MergeParams(clicore.ParseFlags(append([]string{"--file", gitlabComDocument(t)}, extra...)))
	err := EvidenceReport(params, env, ioctx, refuseSignIn(t))
	return strings.Join(log, "\n"), err
}

// assertNoGitLabToken checks that no message carries a token the job held.
func assertNoGitLabToken(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, secret := range []string{gitlabTestIDToken, actionsTestIngestToken} {
			if strings.Contains(text, secret) {
				t.Errorf("message carries %q: %s", secret, text)
			}
		}
	}
}

func TestAGitLabJobExchangesItsIDTokenAndUploads(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "exchanges-a-gitlab-ci-id-token", "a-gitlab-job-exchanges-its-id-token-and-uploads")
	for _, signIn := range []bool{false, true} {
		fake := &actionsFake{delivery: &evidenceFakeServer{}}
		var extra []string
		if signIn {
			extra = []string{"--sign-in"}
		}
		log, err := runGitLab(t, fake, gitlabEnv(t), extra...)
		if err != nil {
			t.Fatalf("evidence-report (sign-in %v): %v", signIn, err)
		}
		assertNoGitLabToken(t, log)
		if len(fake.idTokenRequests) != 0 {
			t.Errorf("a GitLab job requested an ID token: %v", fake.idTokenRequests)
		}
		if len(fake.exchanges) != 1 {
			t.Fatalf("exchanges %v", fake.exchanges)
		}
		want := map[string]any{
			"grant_type":         "urn:ietf:params:oauth:grant-type:token-exchange",
			"client_id":          "delivery-ingest",
			"subject_token":      gitlabTestIDToken,
			"subject_token_type": "urn:ietf:params:oauth:token-type:id_token",
			"organization_id":    actionsTestOrganization,
		}
		for key, value := range want {
			if fake.exchanges[0][key] != value {
				t.Errorf("exchange %s = %v, want %v", key, fake.exchanges[0][key], value)
			}
		}
		requests := fake.delivery.requests
		if len(requests) < 2 {
			t.Fatalf("delivery requests %v", fake.delivery.paths())
		}
		for _, r := range requests {
			if r.Auth != "Bearer "+actionsTestIngestToken {
				t.Errorf("%s rides %q", r.Path, r.Auth)
			}
			repository, _ := r.Body["repository"].(map[string]any)
			if repository["host"] != gitlabHost || repository["path"] != "shop/backoffice" {
				t.Errorf("%s names the repository %v", r.Path, r.Body["repository"])
			}
		}
	}

	// A job whose CI_SERVER_HOST is unset exchanges too: the auth-server
	// decides.
	fake := &actionsFake{delivery: &evidenceFakeServer{}}
	env := gitlabEnv(t)
	delete(env, "CI_SERVER_HOST")
	if _, err := runGitLab(t, fake, env); err != nil || len(fake.exchanges) != 1 {
		t.Fatalf("without CI_SERVER_HOST: err %v, exchanges %v", err, fake.exchanges)
	}
}

func TestAnExplicitTokenSkipsTheGitLabExchange(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "exchanges-a-gitlab-ci-id-token", "an-explicit-token-skips-the-gitlab-exchange")
	fake := &actionsFake{delivery: &evidenceFakeServer{}}
	env := gitlabEnv(t)
	env[evidenceTokenEnv] = evidenceTestToken
	if _, err := runGitLab(t, fake, env); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if len(fake.exchanges) != 0 {
		t.Fatalf("exchanges %v", fake.exchanges)
	}
	for _, r := range fake.delivery.requests {
		if r.Auth != "Bearer "+evidenceTestToken {
			t.Errorf("%s rides %q", r.Path, r.Auth)
		}
	}

	// Outside GitLab CI, PUTNAMI_ORGANIZATION and PUTNAMI_ID_TOKEN are no
	// credential, and without PUTNAMI_ORGANIZATION the job keeps the refusal
	// it had.
	for _, unset := range []string{"GITLAB_CI", evidenceOrganizationEnv} {
		fake = &actionsFake{delivery: &evidenceFakeServer{}}
		env = gitlabEnv(t)
		delete(env, unset)
		_, err := runGitLab(t, fake, env)
		if err == nil || !strings.Contains(err.Error(), "needs a credential") || len(fake.exchanges) != 0 || len(fake.delivery.requests) != 0 {
			t.Fatalf("without %s: err %v, exchanges %v, delivery %v", unset, err, fake.exchanges, fake.delivery.paths())
		}
		assertNoGitLabToken(t, err.Error())
	}
}

func TestAGitLabJobWithoutAnIDTokenIsToldSo(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "explains-a-refused-gitlab-exchange", "a-gitlab-job-without-an-id-token-is-told-so")
	for name, value := range map[string]string{"unset": "", "blank": "  ", "two tokens": "a.b.c d.e.f"} {
		fake := &actionsFake{delivery: &evidenceFakeServer{}}
		env := gitlabEnv(t)
		if value == "" {
			delete(env, gitlabIDTokenEnv)
		} else {
			env[gitlabIDTokenEnv] = value
		}
		_, err := runGitLab(t, fake, env)
		if err == nil || !strings.Contains(err.Error(), gitlabIDTokenEnv) {
			t.Fatalf("%s: %v", name, err)
		}
		if value != "a.b.c d.e.f" {
			for _, want := range []string{"id_tokens", "aud: " + actionsTestAuthURL, evidenceTokenEnv} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: %q names no %q", name, err.Error(), want)
				}
			}
		} else if strings.Contains(err.Error(), "a.b.c") {
			t.Errorf("%s: %q carries the token", name, err.Error())
		}
		if clicore.ExitCode(err) != clicore.ExitAuth {
			t.Errorf("%s: exit code %d", name, clicore.ExitCode(err))
		}
		if len(fake.exchanges)+len(fake.delivery.requests) != 0 {
			t.Errorf("%s: requests were sent", name)
		}
	}
}

func TestASelfManagedGitLabJobIsToldToUseAToken(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "explains-a-refused-gitlab-exchange", "a-self-managed-gitlab-job-is-told-to-use-a-token")
	fake := &actionsFake{delivery: &evidenceFakeServer{}}
	env := gitlabEnv(t)
	env["CI_SERVER_HOST"] = "gitlab.example.com"
	_, err := runGitLab(t, fake, env)
	if err == nil {
		t.Fatal("a self-managed GitLab job uploaded")
	}
	for _, want := range []string{"gitlab.com", "gitlab.example.com", evidenceTokenEnv, "putnami cloud token create --organization " + actionsTestOrganization} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q names no %q", err.Error(), want)
		}
	}
	if clicore.ExitCode(err) != clicore.ExitAuth {
		t.Errorf("exit code %d", clicore.ExitCode(err))
	}
	if len(fake.exchanges)+len(fake.delivery.requests) != 0 {
		t.Errorf("requests were sent")
	}
	assertNoGitLabToken(t, err.Error())
}

func TestAnUntrustedGitLabProjectIsRefusedByName(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "explains-a-refused-gitlab-exchange", "an-untrusted-gitlab-project-is-refused-by-name")
	// The exchange words an untrusted GitLab project as a repository.
	for _, reason := range []string{
		"the organization does not trust this repository for observation",
	} {
		fake := &actionsFake{
			delivery:       &evidenceFakeServer{},
			exchangeStatus: http.StatusBadRequest,
			exchangeAnswer: map[string]any{"error": "invalid_grant", "error_description": reason},
		}
		_, err := runGitLab(t, fake, gitlabEnv(t))
		if err == nil {
			t.Fatal("an untrusted project uploaded")
		}
		for _, want := range []string{reason, "GitLab CI", "Trust shop/backoffice, or its group,", actionsTestOrganization, "observe page", evidenceOrganizationEnv} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q names no %q", err.Error(), want)
			}
		}
		if len(fake.delivery.requests) != 0 {
			t.Errorf("delivery requests %v", fake.delivery.paths())
		}
		assertNoGitLabToken(t, err.Error())
	}

	// Any other refusal, even one that distrusts something else, names the
	// exchange and its reason, and no trust to add.
	for _, reason := range []string{"subject_token could not be verified", "the organization does not trust this issuer"} {
		fake := &actionsFake{
			delivery:       &evidenceFakeServer{},
			exchangeStatus: http.StatusBadRequest,
			exchangeAnswer: map[string]any{"error": "invalid_grant", "error_description": reason},
		}
		_, err := runGitLab(t, fake, gitlabEnv(t))
		if err == nil || !strings.Contains(err.Error(), "refused the GitLab CI ID token exchange at "+actionsTestAuthURL+"/token: "+reason) || strings.Contains(err.Error(), "observe page") {
			t.Fatalf("%s: err %v", reason, err)
		}
		assertNoGitLabToken(t, err.Error())
	}
}

func TestAGitLabIDTokenForAnotherIssuerOrAudienceIsNotSent(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "explains-a-refused-gitlab-exchange", "a-gitlab-id-token-for-another-issuer-or-audience-is-not-sent")
	cases := map[string]struct {
		token string
		wants []string
	}{
		"self-managed issuer": {gitlabIDToken(map[string]any{"iss": "https://gitlab.example.com", "aud": actionsTestAuthURL}), []string{"https://gitlab.example.com", evidenceTokenEnv}},
		"another audience":    {gitlabIDToken(map[string]any{"iss": "https://gitlab.com", "aud": clicore.DefaultAuthURL}), []string{clicore.DefaultAuthURL, "`aud: " + actionsTestAuthURL + "`"}},
		// The exchange compares the audience with its base URL exactly, so a
		// trailing slash is refused here, not by the auth-server.
		"trailing slash":           {gitlabIDToken(map[string]any{"iss": "https://gitlab.com", "aud": actionsTestAuthURL + "/"}), []string{actionsTestAuthURL + "/", "`aud: " + actionsTestAuthURL + "`"}},
		"audience list without it": {gitlabIDToken(map[string]any{"iss": "https://gitlab.com", "aud": []string{"other", actionsTestAuthURL + "/"}}), []string{"`aud: " + actionsTestAuthURL + "`"}},
		"no audience":              {gitlabIDToken(map[string]any{"iss": "https://gitlab.com"}), []string{"audience none", "`aud: " + actionsTestAuthURL + "`"}},
		"not a JWT":                {"opaque-value", []string{"not a JSON Web Token", "id_tokens"}},
		"unreadable claims":        {"a.%%%.c", []string{"no readable claims"}},
	}
	for name, c := range cases {
		fake := &actionsFake{delivery: &evidenceFakeServer{}}
		env := gitlabEnv(t)
		env[gitlabIDTokenEnv] = c.token
		_, err := runGitLab(t, fake, env)
		if err == nil {
			t.Fatalf("%s: uploaded", name)
		}
		for _, want := range c.wants {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %q names no %q", name, err.Error(), want)
			}
		}
		if strings.Contains(err.Error(), c.token) {
			t.Errorf("%s: %q carries the token", name, err.Error())
		}
		if clicore.ExitCode(err) != clicore.ExitAuth {
			t.Errorf("%s: exit code %d", name, clicore.ExitCode(err))
		}
		if len(fake.exchanges)+len(fake.delivery.requests) != 0 {
			t.Errorf("%s: requests were sent", name)
		}
	}

	// An audience list that holds the auth-server's base URL itself is sent.
	fake := &actionsFake{delivery: &evidenceFakeServer{}}
	env := gitlabEnv(t)
	env[gitlabIDTokenEnv] = gitlabIDToken(map[string]any{"iss": "https://gitlab.com", "aud": []string{"other", actionsTestAuthURL}})
	if _, err := runGitLab(t, fake, env); err != nil || len(fake.exchanges) != 1 {
		t.Fatalf("an audience list: err %v, exchanges %v", err, fake.exchanges)
	}
}
