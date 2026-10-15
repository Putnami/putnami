package remotecache

import (
	"io"
	"slices"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	features "go.putnami.dev/protocol/features"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/cacheprovider"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// initializedSession returns a session that completed initialize against the
// fake provider, which echoes cache.CapabilityRestoreResultOnly when echo is
// set. The session is the only record of the echo.
func initializedSession(t *testing.T, mode cache.Mode, echo bool) *cacheprovider.Session {
	t.Helper()
	server := newFakeCacheServer()
	if echo {
		server.echoResultOnly()
	}
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	go func() {
		serveSharedFakeProvider(reqR, respW, server)
		_ = respW.Close()
		_ = reqR.Close()
	}()
	sess := cacheprovider.Connect(reqW, respR)
	t.Cleanup(func() { _ = sess.Close() })
	if _, err := sess.Initialize(t.Context(), &cache.InitializeParams{
		ProtocolVersion: cache.ProviderProtocolVersion,
		BlobExchangeDir: t.TempDir(),
		Mode:            mode,
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if got := sess.RestoreResultOnly(); got != echo {
		t.Fatalf("session echo = %v, want %v", got, echo)
	}
	return sess
}

func resultOnlyRemote(t *testing.T, mode cache.Mode, echo bool, dependedOn ...string) *RemoteCache {
	t.Helper()
	waited := map[string]bool{}
	for _, key := range dependedOn {
		waited[key] = true
	}
	return &RemoteCache{
		provider:   &providerRemote{mode: mode, ready: true, session: initializedSession(t, mode, echo)},
		dependedOn: waited,
	}
}

func selectedJob(selected ...string) *jobmodel.ScheduledJob {
	job := matrixJob()
	job.Selection = &protocoljob.Selection{ProjectIDs: selected}
	return job
}

func TestTaskFilesNeeded(t *testing.T) {
	leaf := selectedJob("/other")
	cases := []struct {
		name   string
		remote *RemoteCache
		job    *jobmodel.ScheduledJob
		want   bool
	}{
		{"no remote cache", nil, leaf, true},
		{"provider without the echo", resultOnlyRemote(t, cache.ModeMinimal, false), leaf, true},
		{"plan never negotiated", &RemoteCache{provider: &providerRemote{mode: cache.ModeMinimal, ready: true, session: initializedSession(t, cache.ModeMinimal, true)}}, leaf, true},
		{"provider not started", &RemoteCache{provider: &providerRemote{mode: cache.ModeMinimal}, dependedOn: map[string]bool{}}, leaf, true},
		{"provider not ready", &RemoteCache{provider: &providerRemote{mode: cache.ModeMinimal, session: initializedSession(t, cache.ModeMinimal, true)}, dependedOn: map[string]bool{}}, leaf, true},
		{"full mode", resultOnlyRemote(t, cache.ModeFull, true), leaf, true},
		{"minimal mode, nothing waits", resultOnlyRemote(t, cache.ModeMinimal, true), leaf, false},
		{"minimal mode, a job waits", resultOnlyRemote(t, cache.ModeMinimal, true, leaf.Key()), leaf, true},
		{"toplevel mode, project not selected", resultOnlyRemote(t, cache.ModeToplevel, true), leaf, false},
		{"toplevel mode, project selected", resultOnlyRemote(t, cache.ModeToplevel, true), selectedJob("/" + captureTestProject), true},
		{"toplevel mode, no selection", resultOnlyRemote(t, cache.ModeToplevel, true), matrixJob(), true},
		{"toplevel mode, workspace-once job", resultOnlyRemote(t, cache.ModeToplevel, true), func() *jobmodel.ScheduledJob {
			job := selectedJob("/other")
			job.SelectedProjects = []*workspace.Project{{ID: "/other"}}
			return job
		}(), true},
		{"minimal mode, post-run report", resultOnlyRemote(t, cache.ModeMinimal, true), declaredJob("test~verify", "verify",
			&extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"report": fileDeclaration(extension.OutputRootCommandOutput, features.VerificationReportFilename, true),
			}}), true},
		{"minimal mode, policed drift", resultOnlyRemote(t, cache.ModeMinimal, true), func() *jobmodel.ScheduledJob {
			output := dirDeclaration(extension.OutputRootProject, "client", false)
			output.Drift = extension.OutputDriftFail
			return declaredJob("generate~client", "client",
				&extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{"client": output}})
		}(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.remote.TaskFilesNeeded(tc.job); got != tc.want {
				t.Fatalf("TaskFilesNeeded = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeclaresPostRunReport(t *testing.T) {
	commandOutputFile := func(path string) extension.DeclaredOutput {
		return fileDeclaration(extension.OutputRootCommandOutput, path, true)
	}
	runTimeFile := extension.DeclaredOutput{Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput, PathFrom: "report"}
	cases := map[string]struct {
		output extension.DeclaredOutput
		want   bool
	}{
		"verification report":          {commandOutputFile(features.VerificationReportFilename), true},
		"criteria projection":          {commandOutputFile(features.SpecCriteriaProjectionFilename), true},
		"command-output run-time file": {runTimeFile, true},
		"command-output directory":     {dirDeclaration(extension.OutputRootCommandOutput, "bin", false), false},
		"other command-output file":    {commandOutputFile("package.tgz"), false},
		"report name in the project":   {fileDeclaration(extension.OutputRootProject, features.VerificationReportFilename, true), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			job := declaredJob("test~task", "task", &extension.TaskDeclaration{
				Outputs: map[string]extension.DeclaredOutput{"out": tc.output},
			})
			if got := declaresPostRunReport(job); got != tc.want {
				t.Fatalf("declaresPostRunReport = %v, want %v", got, tc.want)
			}
		})
	}
	if declaresPostRunReport(declaredJob("test~task", "task", nil)) {
		t.Fatal("a job without a declaration declares no report")
	}
}

// TestNegotiateNamesAKeyResultOnlyOnlyWhenNoJobSharingItReadsTheFiles pins the
// subset rule: a key two planned jobs restore is result-only only when neither
// reads the files.
func TestNegotiateNamesAKeyResultOnlyOnlyWhenNoJobSharingItReadsTheFiles(t *testing.T) {
	remote := resultOnlyRemote(t, cache.ModeMinimal, true, "/proj:reader")
	leaf := matrixJob()
	waited := declaredJob("reader", "reader", &extension.TaskDeclaration{})
	declared := map[string][]*jobmodel.ScheduledJob{
		"k-leaf":   {leaf},
		"k-shared": {leaf, waited},
		"k-waited": {waited},
	}
	got := remote.resultOnlyKeys([]string{"k-waited", "k-leaf", "k-shared", "k-leaf", "k-legacy"}, declared)
	if want := []string{"k-leaf"}; !slices.Equal(got, want) {
		t.Fatalf("resultOnlyKeys = %v, want %v", got, want)
	}
}
