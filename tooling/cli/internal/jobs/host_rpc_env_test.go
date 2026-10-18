package jobs

import (
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/workspace"
	hostrpcproto "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// TestHostRPCChannelComesOnlyFromTheSpawn pins who names a job's host RPC
// channel: only the launch that opened one, through extraEnv. A planned job
// receives none, so its cached result cannot depend on a call its key does not
// cover. Neither an inherited value, which names an outer launch's channel,
// nor a task's own Env reaches the process.
func TestHostRPCChannelComesOnlyFromTheSpawn(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "extension-command-host-rpc", "a-planned-job-and-a-provider-tool-receive-no-channel")
	t.Setenv(hostrpcproto.SocketEnv, "/outer/host.sock")
	t.Setenv(hostrpcproto.TokenEnv, "outer-token")
	launch := []string{hostrpcproto.SocketEnv + "=/launch/host.sock", hostrpcproto.TokenEnv + "=launch-token"}
	taskEnv := map[string]string{hostrpcproto.SocketEnv: "/task/host.sock", hostrpcproto.TokenEnv: "task-token"}
	for _, tc := range []struct {
		name     string
		taskEnv  map[string]string
		extraEnv []string
		want     []string
	}{
		{name: "a planned job inherits no channel"},
		{name: "a task cannot name a channel", taskEnv: taskEnv},
		{name: "a launch's own channel wins over the task's", taskEnv: taskEnv, extraEnv: launch, want: launch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := makeExecutorTestWorkspace(t)
			job := &ScheduledJob{
				Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
				Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
				JobDef: &extension.JobDefinition{
					Name: "build", CommandName: "build", ExtensionName: "@test/ext",
					Command: "true", Env: tc.taskEnv,
				},
			}

			invocation := buildJobInvocation(ws, job, &JobCommandContext{
				WorkspaceRoot: ws.Root,
				Workspace:     JobContextWorkspace{Name: "ws"},
				Extension:     JobContextExtension{Name: "@test/ext"},
				Job:           JobContextJob{Name: "build"},
			}, tc.extraEnv, "")

			var got []string
			for _, entry := range invocation.env {
				if strings.HasPrefix(entry, hostrpcproto.SocketEnv+"=") || strings.HasPrefix(entry, hostrpcproto.TokenEnv+"=") {
					got = append(got, entry)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("the process receives %q, want %q", got, tc.want)
			}
		})
	}
}
