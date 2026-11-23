package upload

import (
	"strings"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/publish"
)

// TokenEnv is the Putnami Cloud token a CI job uploads with.
const TokenEnv = "PUTNAMI_CLOUD_TOKEN"

// OrganizationEnv names the Putnami organization a GitHub Actions job, or a
// GitLab CI job of gitlab.com, exchanges its ID token for, when it holds no
// TokenEnv.
const OrganizationEnv = "PUTNAMI_ORGANIZATION"

// ActionEnv is true in a run of Putnami/observe-action, which takes the
// token as its token input and runs no ID token exchange.
const ActionEnv = "PUTNAMI_OBSERVE_ACTION"

// ComponentEnv is true in a job of the GitLab CI/CD catalog component, which
// takes the organization as its organization input and uploads by itself.
const ComponentEnv = "PUTNAMI_OBSERVE_COMPONENT"

// IDTokenEnv is the id_tokens entry a GitLab CI job of gitlab.com declares
// for the ID token exchange.
const IDTokenEnv = "PUTNAMI_ID_TOKEN" //nolint:gosec // G101: the name of a variable, not a credential

// gitlabCom is the GitLab instance whose ID tokens Putnami Cloud verifies.
const gitlabCom = "gitlab.com"

// tokenCreate is the command that creates an organization's ingest token,
// which needs no Putnami workspace.
const tokenCreate = "`putnami cloud token create --organization <id> --name ci`"

// Step is what a run did with its evidence document.
type Step struct {
	// Getenv reads the environment.
	Getenv func(string) string
	// Upload is what the hand-off to the reporter did.
	Upload publish.Upload
	// ReportURL is the report URL the reporter answered, "" for none.
	ReportURL string
}

// Next returns the line that ends the text summary: the report URL an upload
// answered, else the command that keeps the full report. Outside a CI that
// is putnami observe --open; in a CI job whose reporter is Putnami Cloud and
// that holds no token, the token setting and the command that creates the
// organization's ingest token: as the token input in Putnami/observe-action,
// after the ID token exchange in another GitHub Actions job or gitlab.com job
// that names no organization, and as the organization input of the GitLab
// component; else --upload.
func Next(s Step) string {
	switch {
	case s.ReportURL != "":
		return "Full report: " + s.ReportURL
	case s.Upload == publish.Uploaded:
		return "Next: the evidence reporter stored the evidence and answered no report URL"
	case s.Upload == publish.UploadFailed:
		return "Next: the upload failed, as the observe.upload_failed warning says; fix it and run again"
	}
	if _, inCI := evidence.DetectCISystem(s.Getenv); !inCI {
		return "Next: run `putnami observe --open` to sign in and open the full report"
	}
	name, _, err := Reporter(s.Getenv)
	if err != nil || name != DefaultReporter || strings.TrimSpace(s.Getenv(TokenEnv)) != "" {
		return "Next: add --upload to keep the full report"
	}
	if strings.TrimSpace(s.Getenv(ActionEnv)) == "true" {
		return "Next: pass a token from " + tokenCreate + " as this action's token input to keep the full report"
	}
	if system, _ := evidence.DetectCISystem(s.Getenv); system == evidence.CISystemGitHubActions {
		if strings.TrimSpace(s.Getenv(OrganizationEnv)) != "" {
			return "Next: add --upload to keep the full report"
		}
		return "Next: set " + OrganizationEnv + " to your organization id with `permissions: id-token: write`, or " + TokenEnv + " to a token from " + tokenCreate + ", then add --upload to keep the full report"
	}
	if system, _ := evidence.DetectCISystem(s.Getenv); system == evidence.CISystemGitLabCI {
		if line := gitlabNext(s.Getenv); line != "" {
			return line
		}
	}
	return "Next: set " + TokenEnv + " in this CI to a token from " + tokenCreate + ", then add --upload to keep the full report"
}

// gitlabNext returns the last line of a GitLab CI job that holds no token,
// "" for the line of any other CI. A job of gitlab.com exchanges its ID
// token; the component uploads by itself once it holds a credential.
func gitlabNext(getenv func(string) string) string {
	host := strings.ToLower(strings.TrimSpace(getenv("CI_SERVER_HOST")))
	onGitLabCom := host == "" || host == gitlabCom
	component := strings.TrimSpace(getenv(ComponentEnv)) == "true"
	switch {
	case component && onGitLabCom:
		return "Next: set this component's organization input to your organization id, or " + TokenEnv + " in this CI to a token from " + tokenCreate + ", to keep the full report"
	case component:
		return "Next: set " + TokenEnv + " in this CI to a token from " + tokenCreate + " to keep the full report"
	case !onGitLabCom:
		return ""
	case strings.TrimSpace(getenv(OrganizationEnv)) != "":
		return "Next: add --upload to keep the full report"
	}
	return "Next: set " + OrganizationEnv + " to your organization id with an id_tokens entry " + IDTokenEnv + ", or " + TokenEnv + " to a token from " + tokenCreate + ", then add --upload to keep the full report"
}
