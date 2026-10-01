//go:build unix

package jobs

import (
	"errors"
	"io"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// wantRecorded fails t unless this process recorded repository code for
// reason: a credential holder asked for now is refused.
func wantRecorded(t *testing.T, reason string) {
	t.Helper()
	err := runcredential.RequireCustody("cache provider @a/cache")
	var refusal *runcredential.CustodyError
	if !errors.As(err, &refusal) || refusal.Reason != reason {
		t.Errorf("RequireCustody = %v, want a refusal that names %q", err, reason)
	}
}

// Every job of a hosted run other than a credentialed fetch runs repository
// code: its start records it, streamed or interactive.
func TestAHostedJobRecordsRepositoryCode(t *testing.T) {
	hostedJobTest(t)
	t.Cleanup(InstallJobReadCredential(nil))
	ws, job := jobCredentialFixture(t, "build", "build", nil)
	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err != nil || result.Status != "success" {
		t.Fatalf("RunJob = %+v, %v", result, err)
	}
	wantRecorded(t, "job "+job.Key())

	t.Cleanup(runcredential.SetForTest("run-bearer"))
	result, err = RunJobInteractiveWithStreams(t.Context(), ws, job, nil, nil, nil, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil || result.Status != "success" {
		t.Fatalf("RunJobInteractiveWithStreams = %+v, %v", result, err)
	}
	wantRecorded(t, "job "+job.Key())
}

// A hosted fetch that receives the job credential, in the dependency fetch of
// an install, starts as a credential holder and records nothing.
func TestAHostedFetchRecordsNoRepositoryCode(t *testing.T) {
	hostedJobTest(t)
	installCountingJobReadCredential(t, testJobCredential(), nil)
	fetch := extensionproto.WorkspaceFetchCommand
	ws, job := jobCredentialFixture(t, fetch, fetch, nil)
	result, err := RunJob(WithDependencyFetch(t.Context()), ws, job, nil, nil, nil, nil)
	if err != nil || result.Status != "success" {
		t.Fatalf("RunJob = %+v, %v", result, err)
	}
	if err := runcredential.RequireCustody("cache provider @a/cache"); err != nil {
		t.Errorf("after a credentialed fetch: RequireCustody = %v, want nil", err)
	}
}

// A workspace-fetch that a plan selects, such as through the alias
// "lint": "workspace-fetch", is an ordinary job of a hosted run: its start
// records repository code, and the provider is never asked.
func TestAPlannedHostedFetchRecordsRepositoryCode(t *testing.T) {
	hostedJobTest(t)
	calls := installCountingJobReadCredential(t, testJobCredential(), nil)
	fetch := extensionproto.WorkspaceFetchCommand
	ws, job := jobCredentialFixture(t, fetch, fetch, nil)
	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err != nil || result.Status != "success" {
		t.Fatalf("RunJob = %+v, %v", result, err)
	}
	wantRecorded(t, "job "+job.Key())
	if calls.Load() != 0 {
		t.Errorf("the provider was asked %d times, want never", calls.Load())
	}
}

// Once repository code ran, a hosted fetch does not start: the provider is
// never asked, and the error names the fetch and the hook.
func TestAHostedFetchAfterRepositoryCodeIsRefused(t *testing.T) {
	hostedJobTest(t)
	calls := installCountingJobReadCredential(t, testJobCredential(), nil)
	fetch := extensionproto.WorkspaceFetchCommand
	ws, job := jobCredentialFixture(t, fetch, fetch, nil)
	runcredential.MarkRepositoryCodeStarted("hook hooks.commands.install.before")

	result, err := RunJob(WithDependencyFetch(t.Context()), ws, job, nil, nil, nil, nil)
	want := &runcredential.CustodyError{Holder: "job " + job.Key(), Reason: "hook hooks.commands.install.before"}
	if err == nil || err.Error() != want.Error() || result != nil {
		t.Fatalf("RunJob = %+v, %v; want the refusal %q", result, err, want.Error())
	}
	if calls.Load() != 0 {
		t.Errorf("the provider was asked %d times, want never", calls.Load())
	}
}
