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
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// beforeRepositoryCode is the step LifecycleEnv.BeforeRepositoryCode records
// in the tests below.
const beforeRepositoryCode = "before repository code"

// A hosted run fetches the dependencies to completion before any installer
// runs, in the install's scope, and runs BeforeRepositoryCode between the two:
// a hosted build starts its cache provider there. A workspace with nothing to
// fetch installs as it would; a fetch that fails or ends early, or a
// BeforeRepositoryCode that fails, stops the install. A local run runs the
// installers alone, as it always did.
func TestDepsInstallFetchesFirstOnAHostedRun(t *testing.T) {
	spectest.Proves(t, "cli/credential-custody", "fetch-before-repository-code", "fetch-completes-before-install")
	fetch := extensionproto.WorkspaceFetchCommand
	install := "workspace-install"
	runnerErr := errors.New("the run ended early")
	beforeErr := errors.New("the cache provider did not start")
	cases := []struct {
		name      string
		bearer    string
		fetch     WorkspaceJobOutcome
		fetchErr  error
		beforeErr error
		want      []string
		wantErr   string
	}{
		{name: "a local run", want: []string{install}},
		{name: "a hosted run", bearer: "run-bearer", fetch: WorkspaceJobOK, want: []string{fetch, beforeRepositoryCode, install}},
		{name: "no extension fetches", bearer: "run-bearer", fetch: WorkspaceJobMissing, want: []string{fetch, beforeRepositoryCode, install}},
		{name: "the fetch matches no project", bearer: "run-bearer", fetch: WorkspaceJobNoMatches, want: []string{fetch, beforeRepositoryCode, install}},
		{name: "the fetch fails", bearer: "run-bearer", fetch: WorkspaceJobFailed, want: []string{fetch}, wantErr: "deps fetch failed"},
		{name: "the fetch ends early", bearer: "run-bearer", fetch: WorkspaceJobFailed, fetchErr: runnerErr, want: []string{fetch}, wantErr: runnerErr.Error()},
		{name: "the step before repository code fails", bearer: "run-bearer", fetch: WorkspaceJobOK, beforeErr: beforeErr,
			want: []string{fetch, beforeRepositoryCode}, wantErr: beforeErr.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(runcredential.SetForTest(tc.bearer))
			t.Setenv(artifactsEnsuredEnv, "")
			t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
			var ran []string
			var out strings.Builder
			env := LifecycleEnv{Out: &out, RunJob: func(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
				ran = append(ran, req.Job)
				if req.FilterTag != "go" || req.ExcludeTag != "legacy" {
					t.Errorf("%s ran with tags %q/%q, want the install's go/legacy", req.Job, req.FilterTag, req.ExcludeTag)
				}
				if req.Job == fetch {
					return WorkspaceJobResult{Outcome: tc.fetch}, tc.fetchErr
				}
				return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
			}, BeforeRepositoryCode: func(context.Context) error {
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
