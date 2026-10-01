package cli

import (
	"reflect"
	"testing"

	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

func TestWherePlacementNeverBecomesACommandParameter(t *testing.T) {
	t.Parallel()
	base := ParseArgs([]string{"test", "--projects", "app", "--coverage", "true"}, nil, nil)
	for _, where := range []string{"local", "remote"} {
		parsed := ParseArgs([]string{"test", "--projects", "app", "--coverage", "true", "--where=" + where}, nil, nil)
		if !reflect.DeepEqual(parsed.RawJobArgs, base.RawJobArgs) || !reflect.DeepEqual(parsed.JobFlags, base.JobFlags) {
			t.Errorf("placement %s changed task parameters: args=%v flags=%v", where, parsed.RawJobArgs, parsed.JobFlags)
		}
	}
}

func TestWhereFlagRejectsInvalidPlacement(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--where"}, {"--where", ""}, {"--where", "Remote"}, {"--where", "auto"}} {
		if _, _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%q) accepted an invalid placement", args)
		}
	}
}

func TestWhereFlagCatalog(t *testing.T) {
	t.Parallel()
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.Long == "--where" {
			if flag.Type != commandmeta.FlagValue || !reflect.DeepEqual(flag.Values, []string{"local", "remote"}) {
				t.Errorf("placement flag catalog = %+v", flag)
			}
			return
		}
	}
	t.Fatal("--where missing from shared parser/help/completion catalog")
}
