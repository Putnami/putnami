package deliverycli

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	actionsTestRequestToken = "gha-request-token"
	actionsTestIDToken      = "gha.id.token"
	actionsTestIngestToken  = "ingest-access-token"
	actionsTestAuthURL      = "https://auth.test"
	actionsTestOrganization = "org-1"
)

// actionsFake answers the job's ID token endpoint (actions.test) and the
// auth-server (auth.test), and hands every other request to Delivery's fake.
type actionsFake struct {
	delivery *evidenceFakeServer
	// idTokenRequests holds the bearer and audience of each ID token request.
	idTokenRequests []map[string]string
	// exchanges holds the body of each token request.
	exchanges []map[string]any
	// idTokenStatus answers the ID token request with this status when set.
	idTokenStatus int
	// exchangeStatus and exchangeAnswer answer the token request when set.
	exchangeStatus int
	exchangeAnswer map[string]any
}

func (f *actionsFake) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Host {
	case "actions.test":
		f.idTokenRequests = append(f.idTokenRequests, map[string]string{
			"auth":        req.Header.Get("Authorization"),
			"audience":    req.URL.Query().Get("audience"),
			"api-version": req.URL.Query().Get("api-version"),
		})
		if f.idTokenStatus != 0 {
			return reportJSONResponse(f.idTokenStatus, map[string]any{"message": "Unable to get ACTIONS_ID_TOKEN_REQUEST_URL env variable"}), nil
		}
		return reportJSONResponse(http.StatusOK, map[string]any{"count": 1, "value": actionsTestIDToken}), nil
	case "auth.test":
		if req.URL.Path != "/token" {
			return reportJSONResponse(http.StatusNotFound, map[string]any{"error": "not found"}), nil
		}
		raw, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.exchanges = append(f.exchanges, body)
		if f.exchangeStatus != 0 {
			return reportJSONResponse(f.exchangeStatus, f.exchangeAnswer), nil
		}
		return reportJSONResponse(http.StatusOK, map[string]any{
			"access_token": actionsTestIngestToken, "token_type": "Bearer", "expires_in": 300,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		}), nil
	}
	return f.delivery.RoundTrip(req)
}

// actionsEnv is a GitHub Actions job with permissions id-token: write that
// names its organization and holds no PUTNAMI_CLOUD_TOKEN.
func actionsEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"GITHUB_ACTIONS":         "true",
		"GITHUB_REPOSITORY":      "acme/storefront",
		evidenceOrganizationEnv:  actionsTestOrganization,
		actionsIDTokenURLEnv:     "https://actions.test/_apis/idtoken?api-version=2.0",
		actionsIDTokenTokenEnv:   actionsTestRequestToken,
		"PUTNAMI_AUTH_URL":       actionsTestAuthURL,
		"PUTNAMI_CLOUD_API_URL":  reportTestBaseURL,
		"PUTNAMI_HOME":           t.TempDir(),
		evidenceInvocationDirEnv: t.TempDir(),
	}
}

func runActions(t *testing.T, fake *actionsFake, env map[string]string) (string, error) {
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
	params := clicore.MergeParams(clicore.ParseFlags([]string{"--file", evidenceFixture("turbo-pull-request.json")}))
	err := EvidenceReport(params, env, ioctx, refuseSignIn(t))
	return strings.Join(log, "\n"), err
}

// assertNoToken checks that no message carries a token the job held.
func assertNoToken(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, secret := range []string{actionsTestRequestToken, actionsTestIDToken, actionsTestIngestToken} {
			if strings.Contains(text, secret) {
				t.Errorf("message carries %q: %s", secret, text)
			}
		}
	}
}

func TestAnActionsJobExchangesItsIDTokenAndUploads(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "exchanges-a-github-actions-id-token", "an-actions-job-exchanges-its-id-token-and-uploads")
	fake := &actionsFake{delivery: &evidenceFakeServer{}}
	log, err := runActions(t, fake, actionsEnv(t))
	if err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	assertNoToken(t, log)
	if len(fake.idTokenRequests) != 1 {
		t.Fatalf("ID token requests %v", fake.idTokenRequests)
	}
	got := fake.idTokenRequests[0]
	if got["auth"] != "Bearer "+actionsTestRequestToken || got["audience"] != actionsTestAuthURL || got["api-version"] != "2.0" {
		t.Errorf("ID token request %v", got)
	}
	if len(fake.exchanges) != 1 {
		t.Fatalf("exchanges %v", fake.exchanges)
	}
	want := map[string]any{
		"grant_type":         "urn:ietf:params:oauth:grant-type:token-exchange",
		"client_id":          "delivery-ingest",
		"subject_token":      actionsTestIDToken,
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
		if repository["host"] != "github.com" || repository["path"] != "acme/storefront" {
			t.Errorf("%s names the repository %v", r.Path, r.Body["repository"])
		}
	}
}

func TestAnExplicitTokenSkipsTheExchange(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "exchanges-a-github-actions-id-token", "an-explicit-token-skips-the-exchange")
	fake := &actionsFake{delivery: &evidenceFakeServer{}}
	env := actionsEnv(t)
	env[evidenceTokenEnv] = evidenceTestToken
	if _, err := runActions(t, fake, env); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	if len(fake.idTokenRequests) != 0 || len(fake.exchanges) != 0 {
		t.Fatalf("ID token requests %v, exchanges %v", fake.idTokenRequests, fake.exchanges)
	}
	for _, r := range fake.delivery.requests {
		if r.Auth != "Bearer "+evidenceTestToken {
			t.Errorf("%s rides %q", r.Path, r.Auth)
		}
	}

	// Outside GitHub Actions, PUTNAMI_ORGANIZATION alone is no credential.
	fake = &actionsFake{delivery: &evidenceFakeServer{}}
	env = actionsEnv(t)
	delete(env, "GITHUB_ACTIONS")
	_, err := runActions(t, fake, env)
	if err == nil || !strings.Contains(err.Error(), evidenceTokenEnv) || len(fake.idTokenRequests) != 0 || len(fake.delivery.requests) != 0 {
		t.Fatalf("err %v, ID token requests %v, delivery %v", err, fake.idTokenRequests, fake.delivery.paths())
	}
}

func TestAJobWithoutIDTokenPermissionIsToldSo(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "explains-a-refused-exchange", "a-job-without-id-token-permission-is-told-so")
	for _, missing := range []string{actionsIDTokenURLEnv, actionsIDTokenTokenEnv} {
		fake := &actionsFake{delivery: &evidenceFakeServer{}}
		env := actionsEnv(t)
		delete(env, missing)
		_, err := runActions(t, fake, env)
		if err == nil || !strings.Contains(err.Error(), "permissions: id-token: write") {
			t.Fatalf("without %s: %v", missing, err)
		}
		if clicore.ExitCode(err) != clicore.ExitAuth {
			t.Errorf("exit code %d", clicore.ExitCode(err))
		}
		if len(fake.idTokenRequests)+len(fake.exchanges)+len(fake.delivery.requests) != 0 {
			t.Errorf("without %s, requests were sent", missing)
		}
		assertNoToken(t, err.Error())
	}
}

func TestAnUntrustedRepositoryIsRefusedByName(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "explains-a-refused-exchange", "an-untrusted-repository-is-refused-by-name")
	fake := &actionsFake{
		delivery:       &evidenceFakeServer{},
		exchangeStatus: http.StatusBadRequest,
		exchangeAnswer: map[string]any{"error": "invalid_grant", "error_description": "the organization does not trust this repository for observation"},
	}
	_, err := runActions(t, fake, actionsEnv(t))
	if err == nil {
		t.Fatal("an untrusted repository uploaded")
	}
	for _, want := range []string{"the organization does not trust this repository for observation", "acme/storefront", actionsTestOrganization, "observe page", evidenceOrganizationEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q names no %q", err.Error(), want)
		}
	}
	if len(fake.delivery.requests) != 0 {
		t.Errorf("delivery requests %v", fake.delivery.paths())
	}
	assertNoToken(t, err.Error())
}

func TestAFailedExchangeNamesItsCause(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "explains-a-refused-exchange", "a-failed-exchange-names-its-cause")
	cases := map[string]*actionsFake{
		"ID token refused": {idTokenStatus: http.StatusForbidden},
		"organization_id refused": {exchangeStatus: http.StatusBadRequest, exchangeAnswer: map[string]any{
			"error": "invalid_grant", "error_description": "subject_token could not be verified",
		}},
		"exchange unavailable": {exchangeStatus: http.StatusServiceUnavailable, exchangeAnswer: map[string]any{"error": "unavailable"}},
		"no access token":      {exchangeStatus: http.StatusOK, exchangeAnswer: map[string]any{"token_type": "Bearer"}},
	}
	wants := map[string][]string{
		"ID token refused":        {"GitHub Actions answered no ID token", "https://actions.test/_apis/idtoken"},
		"organization_id refused": {"refused the GitHub Actions ID token exchange", "subject_token could not be verified"},
		"exchange unavailable":    {"ID token exchange at " + actionsTestAuthURL + "/token failed"},
		"no access token":         {"answered no access_token"},
	}
	for name, fake := range cases {
		fake.delivery = &evidenceFakeServer{}
		_, err := runActions(t, fake, actionsEnv(t))
		if err == nil {
			t.Fatalf("%s: uploaded", name)
		}
		for _, want := range wants[name] {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %q names no %q", name, err.Error(), want)
			}
		}
		if strings.Contains(err.Error(), "api-version") {
			t.Errorf("%s: %q carries the request URL's query", name, err.Error())
		}
		if len(fake.delivery.requests) != 0 {
			t.Errorf("%s: delivery requests %v", name, fake.delivery.paths())
		}
		assertNoToken(t, err.Error())
	}
}

// The exchange wins over --sign-in, a job without PUTNAMI_ORGANIZATION keeps
// the refusal it had, and the default audience is the production auth-server.
func TestTheExchangeWinsOverSignInAndNeedsAnOrganization(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "exchanges-a-github-actions-id-token", "an-actions-job-exchanges-its-id-token-and-uploads")
	fake := &actionsFake{delivery: &evidenceFakeServer{}}
	env := actionsEnv(t)
	stubReportRetrySleep(t)
	ioctx := clicore.IO{Env: env, Stdout: func(string) {}, Stderr: func(string) {}, Client: &http.Client{Transport: fake}, Now: time.Now}
	params := clicore.MergeParams(clicore.ParseFlags([]string{"--file", evidenceFixture("turbo-pull-request.json"), "--sign-in"}))
	if err := EvidenceReport(params, env, ioctx, refuseSignIn(t)); err != nil {
		t.Fatalf("evidence-report --sign-in: %v", err)
	}
	if len(fake.exchanges) != 1 {
		t.Fatalf("exchanges %v", fake.exchanges)
	}

	fake = &actionsFake{delivery: &evidenceFakeServer{}}
	env = actionsEnv(t)
	delete(env, evidenceOrganizationEnv)
	_, err := runActions(t, fake, env)
	if err == nil || !strings.Contains(err.Error(), evidenceTokenEnv) || len(fake.idTokenRequests) != 0 {
		t.Fatalf("without %s: err %v, ID token requests %v", evidenceOrganizationEnv, err, fake.idTokenRequests)
	}

	fake = &actionsFake{delivery: &evidenceFakeServer{}, idTokenStatus: http.StatusForbidden}
	env = actionsEnv(t)
	delete(env, "PUTNAMI_AUTH_URL")
	if _, err := runActions(t, fake, env); err == nil {
		t.Fatal("a refused ID token request uploaded")
	}
	if len(fake.idTokenRequests) != 1 || fake.idTokenRequests[0]["audience"] != clicore.DefaultAuthURL {
		t.Fatalf("ID token requests %v", fake.idTokenRequests)
	}

	fake = &actionsFake{delivery: &evidenceFakeServer{}}
	env = actionsEnv(t)
	env[actionsIDTokenURLEnv] = "http://actions.test/_apis/idtoken"
	if _, err := runActions(t, fake, env); err == nil || !strings.Contains(err.Error(), "not an https URL") || len(fake.idTokenRequests) != 0 {
		t.Fatalf("an http request URL: err %v, requests %v", err, fake.idTokenRequests)
	}
}
