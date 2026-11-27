package cli

import (
	"testing"
)

func TestOnlyAJobCommandThatNeitherWatchesNorPublishesGoesThroughTheMachineProcess(t *testing.T) {
	t.Parallel()
	aliases := commandAliases{user: map[string]string{"w": "test --watch", "t": "test"}, serveWorkloads: []string{"dev"}}
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"lint,test,build", "--all"}, true},
		{[]string{"t"}, true},
		{[]string{"test", "--watch"}, false},
		{[]string{"test", "-w"}, false},
		{[]string{"dev"}, false},
		{[]string{"serve", "app"}, false},
		{[]string{"build,publish"}, false},
		{[]string{"deploy"}, false},
		{[]string{"release"}, false},
		{[]string{"config", "show"}, false},
		{[]string{"machine", "status"}, false},
		{[]string{"--version"}, false},
		{[]string{"--help"}, false},
	} {
		parsed := parseArgs(tc.args, aliases, nil)
		if got := servesThroughMachine(parsed, aliases, nil); got != tc.want {
			t.Errorf("servesThroughMachine(%v) = %v, want %v (commands %v, watch %v)", tc.args, got, tc.want, parsed.Commands, parsed.Global.Watch)
		}
	}
}
