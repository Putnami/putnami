package upload

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/publish"
)

func envOf(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestALaptopRunNamesOpen(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "next-command", "a-laptop-run-names-open")
	got := Next(Step{Getenv: envOf(nil)})
	if got != "Next: run `putnami observe --open` to sign in and open the full report" {
		t.Errorf("got %q", got)
	}
}

func TestACIJobWithoutATokenNamesTheTokenSetting(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "next-command", "a-ci-job-without-a-token-names-the-token-setting")
	got := Next(Step{Getenv: envOf(map[string]string{"GITLAB_CI": "true"})})
	want := "Next: set PUTNAMI_CLOUD_TOKEN in this CI to a token from `putnami cloud token create --organization <id> --name ci`, then add --upload to keep the full report"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, workspaceOnly := range []string{"--open", "--scopes"} {
		if strings.Contains(got, workspaceOnly) {
			t.Errorf("a CI job is told %q: %q", workspaceOnly, got)
		}
	}
	other := Next(Step{Getenv: envOf(map[string]string{"CI": "true", SelectorEnv: "@acme/reports"})})
	if other != "Next: add --upload to keep the full report" {
		t.Errorf("another reporter: %q", other)
	}
}

func TestAGitHubActionsJobNamesTheIDTokenExchange(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "next-command", "a-github-actions-job-names-the-id-token-exchange")
	got := Next(Step{Getenv: envOf(map[string]string{"GITHUB_ACTIONS": "true"})})
	want := "Next: set PUTNAMI_ORGANIZATION to your organization id with `permissions: id-token: write`, or PUTNAMI_CLOUD_TOKEN to a token from `putnami cloud token create --organization <id> --name ci`, then add --upload to keep the full report"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// With the organization named, the job exchanges its ID token: it only
	// needs --upload.
	named := Next(Step{Getenv: envOf(map[string]string{"GITHUB_ACTIONS": "true", OrganizationEnv: "org-acme"})})
	if named != "Next: add --upload to keep the full report" {
		t.Errorf("an organization named: %q", named)
	}
}

func TestTheActionNamesItsTokenInput(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "next-command", "the-action-names-its-token-input")
	for _, env := range []map[string]string{
		{"GITHUB_ACTIONS": "true", ActionEnv: "true"},
		{"GITHUB_ACTIONS": "true", ActionEnv: "true", OrganizationEnv: "org-acme"},
	} {
		got := Next(Step{Getenv: envOf(env)})
		want := "Next: pass a token from `putnami cloud token create --organization <id> --name ci` as this action's token input to keep the full report"
		if got != want {
			t.Errorf("%v: got %q, want %q", env, got, want)
		}
	}
}

func TestACIJobWithATokenNamesUpload(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "next-command", "a-ci-job-with-a-token-names-upload")
	got := Next(Step{Getenv: envOf(map[string]string{"GITLAB_CI": "true", TokenEnv: "pkt_x"})})
	if got != "Next: add --upload to keep the full report" {
		t.Errorf("got %q", got)
	}
}

func TestAnUploadNamesTheReportURL(t *testing.T) {
	spectest.Proves(t, "tooling/observe", "next-command", "an-upload-names-the-report-url")
	env := envOf(map[string]string{"GITHUB_ACTIONS": "true", TokenEnv: "pkt_x"})
	if got := Next(Step{Getenv: env, Upload: publish.Uploaded, ReportURL: "https://putnami.app/r/1"}); got != "Full report: https://putnami.app/r/1" {
		t.Errorf("got %q", got)
	}
	if got := Next(Step{Getenv: env, Upload: publish.Uploaded}); !strings.Contains(got, "answered no report URL") {
		t.Errorf("got %q", got)
	}
	if got := Next(Step{Getenv: env, Upload: publish.UploadFailed}); !strings.Contains(got, "observe.upload_failed") {
		t.Errorf("got %q", got)
	}
}
