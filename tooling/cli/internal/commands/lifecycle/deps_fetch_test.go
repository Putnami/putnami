package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// beforeRepositoryCode is the step LifecycleEnv.BeforeRepositoryCode records
// in the tests below.
const beforeRepositoryCode = "before repository code"

// The two fetch steps of a hosted install, as recordedJob names them.
var (
	storeFetch = extensionproto.WorkspaceFetchCommand + " (dependency fetch)"
	pathFetch  = extensionproto.WorkspaceFetchCommand + " (path-extension fetch)"
)

// recordedJob names the job req runs, with the hosted fetch step that ctx
// marks, if any.
func recordedJob(ctx context.Context, req WorkspaceJobRequest) string {
	if step := jobs.HostedFetchStep(ctx); step != "" {
		return req.Job + " (" + step + ")"
	}
	return req.Job
}

// A hosted run fetches the dependencies to completion before any installer
// runs, in the install's scope: the store extensions' fetch, then
// BeforeRepositoryCode, where a hosted build starts its cache provider, then
// the path extensions' fetch, which is repository code. A workspace with
// nothing to fetch installs as it would; a fetch that fails or ends early, or
// a BeforeRepositoryCode that fails, stops the install. A local run runs the
// installers alone, as it always did.
func TestDepsInstallFetchesFirstOnAHostedRun(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "fetch-completes-before-install")
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "path-extension-fetch-runs-after-custody")
	install := "workspace-install"
	runnerErr := errors.New("the run ended early")
	beforeErr := errors.New("the cache provider did not start")
	cases := []struct {
		name      string
		bearer    string
		fetch     WorkspaceJobOutcome
		pathFetch WorkspaceJobOutcome
		fetchErr  error
		beforeErr error
		want      []string
		wantErr   string
	}{
		{name: "a local run", want: []string{install}},
		{name: "a hosted run", bearer: "run-bearer", fetch: WorkspaceJobOK, want: []string{storeFetch, beforeRepositoryCode, pathFetch, install}},
		{name: "no extension fetches", bearer: "run-bearer", fetch: WorkspaceJobMissing, pathFetch: WorkspaceJobMissing,
			want: []string{storeFetch, beforeRepositoryCode, pathFetch, install}},
		{name: "the fetch matches no project", bearer: "run-bearer", fetch: WorkspaceJobNoMatches, pathFetch: WorkspaceJobNoMatches,
			want: []string{storeFetch, beforeRepositoryCode, pathFetch, install}},
		{name: "the fetch fails", bearer: "run-bearer", fetch: WorkspaceJobFailed, want: []string{storeFetch}, wantErr: "deps fetch failed"},
		{name: "the fetch ends early", bearer: "run-bearer", fetch: WorkspaceJobFailed, fetchErr: runnerErr, want: []string{storeFetch}, wantErr: runnerErr.Error()},
		{name: "the step before repository code fails", bearer: "run-bearer", fetch: WorkspaceJobOK, beforeErr: beforeErr,
			want: []string{storeFetch, beforeRepositoryCode}, wantErr: beforeErr.Error()},
		{name: "the path extensions' fetch fails", bearer: "run-bearer", fetch: WorkspaceJobOK, pathFetch: WorkspaceJobFailed,
			want: []string{storeFetch, beforeRepositoryCode, pathFetch}, wantErr: "deps fetch failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(runcredential.SetForTest(tc.bearer))
			t.Setenv(artifactsEnsuredEnv, "")
			t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
			var ran []string
			var out strings.Builder
			env := LifecycleEnv{Out: &out, RunJob: func(ctx context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
				job := recordedJob(ctx, req)
				ran = append(ran, job)
				if req.FilterTag != "go" || req.ExcludeTag != "legacy" {
					t.Errorf("%s ran with tags %q/%q, want the install's go/legacy", req.Job, req.FilterTag, req.ExcludeTag)
				}
				switch job {
				case storeFetch:
					return WorkspaceJobResult{Outcome: tc.fetch}, tc.fetchErr
				case pathFetch:
					return WorkspaceJobResult{Outcome: tc.pathFetch}, nil
				}
				return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
			}, BeforeRepositoryCode: func(ctx context.Context) error {
				if step := jobs.HostedFetchStep(ctx); step != "" {
					t.Errorf("BeforeRepositoryCode runs in the %s", step)
				}
				ran = append(ran, beforeRepositoryCode)
				return tc.beforeErr
			}}

			err := DepsInstall(context.Background(), t.TempDir(), &wsproto.Config{}, "go", "legacy", env)
			if !reflect.DeepEqual(ran, tc.want) {
				t.Errorf("ran %q, want %q", ran, tc.want)
			}
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Errorf("DepsInstall = %v, want %q", err, tc.wantErr)
			}
			if out.Len() != 0 {
				t.Errorf("the install printed %q, want nothing", out.String())
			}
		})
	}
}
