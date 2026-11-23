// The GitLab CI upload of `putnami cloud evidence-report`: a gitlab.com job
// that names the Putnami organization it uploads for, in
// PUTNAMI_ORGANIZATION, declares an ID token in PUTNAMI_ID_TOKEN and holds no
// PUTNAMI_CLOUD_TOKEN exchanges that ID token for a short-lived ingest token,
// so its project stores no Putnami secret.
//
// GitLab mints the ID token before the job starts, from the job's id_tokens
// keyword, with the audience the job names: the auth-server's base URL, the
// audience its GitLab CI verifier expects. The auth-server verifies the token
// against gitlab.com's keys only, so a job of another GitLab instance is told
// to set PUTNAMI_CLOUD_TOKEN instead.
package deliverycli

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const (
	// gitlabIDTokenEnv is the variable the job's id_tokens declares.
	gitlabIDTokenEnv = "PUTNAMI_ID_TOKEN"
	// gitlabHost is the one GitLab instance whose ID tokens the auth-server
	// verifies.
	gitlabHost = "gitlab.com"
	// gitlabIssuer is the iss of the ID tokens gitlab.com mints.
	gitlabIssuer = "https://gitlab.com"
)

// gitlabOrganization returns the organization a GitLab CI job uploads for,
// "" outside such a job or without PUTNAMI_ORGANIZATION.
func gitlabOrganization(env map[string]string) string {
	if strings.TrimSpace(clicore.EnvGet(env, "GITLAB_CI")) != "true" {
		return ""
	}
	return strings.TrimSpace(clicore.EnvGet(env, evidenceOrganizationEnv))
}

// gitlabIngestToken exchanges the job's ID token for the organization's
// ingest token. No error message carries a token.
func gitlabIngestToken(params map[string]any, env map[string]string, ioctx clicore.IO, organization string) (string, error) {
	if host := strings.ToLower(strings.TrimSpace(clicore.EnvGet(env, "CI_SERVER_HOST"))); host != "" && host != gitlabHost {
		return "", clicore.NewError("cloud evidence-report: "+evidenceOrganizationEnv+" is set, but Putnami Cloud exchanges the ID tokens of "+gitlabHost+" jobs only, and this job runs on "+host+": set "+evidenceTokenEnv+" to an organization ingest token (putnami cloud token create --organization "+organization+") instead", clicore.ExitAuth)
	}
	idToken := strings.TrimSpace(clicore.EnvGet(env, gitlabIDTokenEnv))
	if idToken == "" {
		audience := clicore.AuthBaseURL(params, env, "")
		return "", clicore.NewError("cloud evidence-report: "+evidenceOrganizationEnv+" is set, but this GitLab CI job holds no ID token in "+gitlabIDTokenEnv+": declare it in the job's id_tokens with `aud: "+audience+"`, or set "+evidenceTokenEnv, clicore.ExitAuth)
	}
	if fields := strings.Fields(idToken); len(fields) != 1 {
		return "", clicore.NewError("cloud evidence-report: "+gitlabIDTokenEnv+" must contain exactly one ID token", clicore.ExitAuth)
	}
	if err := checkGitLabIDToken(idToken, clicore.AuthBaseURL(params, env, "")); err != nil {
		return "", err
	}
	project := clicore.FirstString(strings.TrimSpace(clicore.EnvGet(env, "CI_PROJECT_PATH")), "this project")
	return exchangeIngestToken(params, env, ioctx, idTokenExchange{
		ci:           "GitLab CI",
		organization: organization,
		idToken:      idToken,
		trustHint:    "Trust " + project + ", or its group,",
	})
}

// checkGitLabIDToken reads the unverified claims of the job's ID token and
// refuses to send one gitlab.com did not mint for the auth-server: the job's
// YAML fixes the audience, so a token meant for another server, or minted by
// a self-managed instance, stays in the job. The auth-server verifies the
// signature. No error message carries the token.
func checkGitLabIDToken(idToken, audience string) error {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return clicore.NewError("cloud evidence-report: "+gitlabIDTokenEnv+" is not a JSON Web Token: declare it in the job's id_tokens", clicore.ExitAuth)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	var claims struct {
		Issuer   string          `json:"iss"`
		Audience json.RawMessage `json:"aud"`
	}
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return clicore.NewError("cloud evidence-report: "+gitlabIDTokenEnv+" holds no readable claims: declare it in the job's id_tokens", clicore.ExitAuth)
	}
	if clicore.TrimURL(claims.Issuer) != gitlabIssuer {
		return clicore.NewError("cloud evidence-report: the ID token in "+gitlabIDTokenEnv+" was issued by "+clicore.FirstString(claims.Issuer, "no issuer")+", and Putnami Cloud exchanges the ID tokens of "+gitlabHost+" jobs only: set "+evidenceTokenEnv+" instead", clicore.ExitAuth)
	}
	var audiences []string
	var single string
	if json.Unmarshal(claims.Audience, &single) == nil {
		audiences = []string{single}
	} else {
		_ = json.Unmarshal(claims.Audience, &audiences)
	}
	for _, aud := range audiences {
		if clicore.TrimURL(aud) == audience {
			return nil
		}
	}
	return clicore.NewError("cloud evidence-report: the ID token in "+gitlabIDTokenEnv+" is for the audience "+clicore.FirstString(strings.Join(audiences, ", "), "none")+", not "+audience+": set `aud: "+audience+"` in the job's id_tokens", clicore.ExitAuth)
}
