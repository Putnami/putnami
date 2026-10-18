package cli

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/iox"
)

var machineLoadShape = regexp.MustCompile(`^(unknown|\d+\.\d\d)\n$`)

// doctor --machine and contributor machine-load run the same code. The live
// load moves between two calls, so this test checks the shape of each output;
// the doctor package tests pin the exact bytes for a fixed sample.
func TestDoctorMachineMatchesContributorMachineLoad(t *testing.T) {
	root := t.TempDir()
	for _, format := range []string{"", "json"} {
		doctorOut, err := iox.CaptureStdout(func() error {
			return cmdDoctor(&CommandEnv{Ctx: context.Background(), WsRoot: root, Args: []string{"--machine"}, OutputFormat: format})
		})
		if err != nil {
			t.Fatalf("doctor --machine (%q): %v", format, err)
		}
		contribOut, err := iox.CaptureStdout(func() error {
			return cmdContributor(&CommandEnv{Ctx: context.Background(), WsRoot: root, Sub: "machine-load", OutputFormat: format})
		})
		if err != nil {
			t.Fatalf("contributor machine-load (%q): %v", format, err)
		}
		if format == "" {
			if !machineLoadShape.MatchString(doctorOut) || !machineLoadShape.MatchString(contribOut) {
				t.Errorf("human output differs in shape: %q vs %q", doctorOut, contribOut)
			}
			continue
		}
		for _, out := range []string{doctorOut, contribOut} {
			if !strings.HasPrefix(out, `{"known":`) || !strings.HasSuffix(out, "}\n") {
				t.Errorf("structured output has the wrong shape: %q", out)
			}
		}
	}
}

func TestDoctorMachineRejectsProfileAndProject(t *testing.T) {
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
