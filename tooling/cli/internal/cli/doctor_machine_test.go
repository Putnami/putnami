package cli

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/iox"
)

var machineLoadShape = regexp.MustCompile(`^(unknown|\d+\.\d\d)\n$`)

// TestDoctorMachinePrintsTheLoadInEachFormat runs doctor --machine through the
// command handler. The live load moves between two runs, so this test checks
// the shape of each output; the doctor package tests pin the exact bytes for a
// fixed sample.
func TestDoctorMachinePrintsTheLoadInEachFormat(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, format := range []string{"", "json"} {
		out, err := iox.CaptureStdout(func() error {
			return cmdDoctor(&CommandEnv{Ctx: context.Background(), WsRoot: root, Args: []string{"--machine"}, OutputFormat: format})
		})
		if err != nil {
			t.Fatalf("doctor --machine (%q): %v", format, err)
		}
		if format == "" {
			if !machineLoadShape.MatchString(out) {
				t.Errorf("human output has the wrong shape: %q", out)
			}
			continue
		}
		if !strings.HasPrefix(out, `{"known":`) || !strings.HasSuffix(out, "}\n") {
			t.Errorf("structured output has the wrong shape: %q", out)
		}
	}
}

func TestDoctorMachineRejectsProfileAndProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cases := []CommandEnv{
		{WsRoot: root, Args: []string{"--machine", "--project", "x"}},
		{WsRoot: root, Args: []string{"--machine", "--project=x"}},
		{WsRoot: root, Args: []string{"--machine"}, Global: GlobalFlags{ProfileFlag: true, EnvProfile: "dev"}},
		{WsRoot: root, Args: []string{"--machine"}, Global: GlobalFlags{Projects: "x"}},
	}
	for i := range cases {
		if err := cmdDoctor(&cases[i]); err == nil {
			t.Errorf("case %d: want a usage error", i)
		}
	}
}
