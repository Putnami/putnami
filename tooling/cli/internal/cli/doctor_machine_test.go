package cli

import "testing"

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
