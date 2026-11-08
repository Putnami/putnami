package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/cli"
)

func TestRunAnswersTheRuntimeHandshake(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, strings.NewReader(""), &stdout, &stderr, nil); code != 0 || !strings.Contains(stdout.String(), "@putnami/java") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunHandsTheTasksToTheJobDispatcher(t *testing.T) {
	var got []string
	code := run([]string{"test"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, func(jobs map[string]cli.JobFunc) {
		for name := range jobs {
			got = append(got, name)
		}
	})
	slices.Sort(got)
	want := []string{"build", "lint", "package-docker", "publish-docker", "run", "serve", "test"}
	if code != 0 || !slices.Equal(got, want) {
		t.Fatalf("exit %d, jobs %v", code, got)
	}
}

func TestRunReportsAFailedControlCallOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"__putnami", "workspace-probe"}, strings.NewReader("not json"), &stdout, &stderr, nil)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "workspace-probe") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRunAnswersTheToolchainResolveCall(t *testing.T) {
	var stdout, stderr bytes.Buffer
	request := `{"version":1,"extension":"@putnami/java","identities":["java"],"platform":"linux/amd64"}`
	if code := run([]string{"__putnami", "toolchain-resolve"}, strings.NewReader(request), &stdout, &stderr, nil); code != 0 || !strings.Contains(stdout.String(), "@putnami/java") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
