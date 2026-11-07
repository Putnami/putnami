package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const openReportURL = "https://putnami.app/acme/shop/changes/1"

// answeringReporter makes the fixture's CLI answer a protocol CLI result that
// names the report URL, and records its arguments.
func answeringReporter(t *testing.T, env map[string]string, record string) {
	t.Helper()
	cli := filepath.Join(t.TempDir(), "putnami")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + record + "/args'\n" +
		"echo '{\"protocolVersion\":2,\"status\":\"success\",\"data\":{\"reportUrl\":\"" + openReportURL + "\"}}'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env["PUTNAMI_CLI_EXECUTABLE"] = cli
}

// laptop drops the CI variables of the fixture: a person runs it.
func laptop(env map[string]string) map[string]string {
	return map[string]string{"PUTNAMI_CLI_EXECUTABLE": env["PUTNAMI_CLI_EXECUTABLE"]}
}

// recordBrowser replaces the browser and returns the URLs it was asked to
// open.
func recordBrowser(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	previous := openBrowser
	openBrowser = func(url string) error {
		opened = append(opened, url)
		return nil
	}
	t.Cleanup(func() { openBrowser = previous })
	return &opened
}

func reporterArgs(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(record, "args"))
	if err != nil {
		t.Fatalf("the reporter did not run: %v", err)
	}
	return strings.Fields(string(data))
}

func TestOpenPassesSignInAndPrintsTheReportURL(t *testing.T) {
	spectest.Proves(t, "tooling/observe-upload", "open-signs-in-and-uploads", "open-passes-sign-in-and-prints-the-report-url")
	repo, ciEnv, record := uploadFixture(t, "0")
	answeringReporter(t, ciEnv, record)
	env := laptop(ciEnv)
	opened := recordBrowser(t)
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--no-fetch", "--open"}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if args := reporterArgs(t, record); !slices.Contains(args, "--sign-in") || args[1] != "evidence-report" {
		t.Fatalf("reporter args %v", args)
	}
	if !strings.Contains(stderr.String(), openReportURL) {
		t.Errorf("stderr names no report URL: %s", stderr.String())
	}
	if !strings.HasSuffix(stdout.String(), "Full report: "+openReportURL+"\n") {
		t.Errorf("the summary does not end with the report URL:\n%s", stdout.String())
	}
	if !slices.Equal(*opened, []string{openReportURL}) {
		t.Errorf("opened %v", *opened)
	}
}

func TestOpenOpensTheBrowserOnALaptopOnly(t *testing.T) {
	spectest.Proves(t, "tooling/observe-upload", "open-signs-in-and-uploads", "open-opens-the-browser-on-a-laptop-only")
	repo, env, record := uploadFixture(t, "0")
	answeringReporter(t, env, record)
	opened := recordBrowser(t)
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--no-fetch", "--open"}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if args := reporterArgs(t, record); !slices.Contains(args, "--sign-in") {
		t.Fatalf("reporter args %v", args)
	}
	if len(*opened) != 0 {
		t.Errorf("a CI job opened %v", *opened)
	}
	if !strings.HasSuffix(stdout.String(), "Full report: "+openReportURL+"\n") {
		t.Errorf("the summary does not end with the report URL:\n%s", stdout.String())
	}
}

func TestARunWithoutUploadOrOpenRunsNoReporter(t *testing.T) {
	spectest.Proves(t, "tooling/observe-upload", "nothing-leaves-without-upload-or-open", "a-run-without-upload-or-open-runs-no-reporter")
	repo, ciEnv, record := uploadFixture(t, "0")
	env := laptop(ciEnv)
	opened := recordBrowser(t)
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--no-fetch"}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(record, "args")); !os.IsNotExist(err) {
		t.Fatalf("a reporter ran: %v", err)
	}
	if len(*opened) != 0 {
		t.Errorf("opened %v", *opened)
	}
	if !strings.HasSuffix(stdout.String(), "Next: run `putnami observe --open` to sign in and open the full report\n") {
		t.Errorf("the summary does not end with --open:\n%s", stdout.String())
	}
}
