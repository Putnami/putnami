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
	got := Next(Step{Getenv: envOf(map[string]string{"GITHUB_ACTIONS": "true"})})
	for _, want := range []string{"PUTNAMI_CLOUD_TOKEN", "`putnami cloud token create", "--upload"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q names no %q", got, want)
		}
	}
	if strings.Contains(got, "--open") {
		t.Errorf("a CI job is told to open a browser: %q", got)
	}
	other := Next(Step{Getenv: envOf(map[string]string{"CI": "true", SelectorEnv: "@acme/reports"})})
	if other != "Next: add --upload to keep the full report" {
		t.Errorf("another reporter: %q", other)
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
