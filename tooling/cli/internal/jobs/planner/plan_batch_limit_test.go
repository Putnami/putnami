package planner

import (
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// batchCapOption is the parameter the fixtures' batch policy names, spelled as
// the Go extension names it for go-test.
const batchCapOption = "batch-max-projects"

// TestPlanRefusesAnInvalidBatchProjectCap pins the wiring: a bad value fails
// Plan itself, so every adapter (run, --plan, MCP) reports it before a task
// starts, while a valid or absent value plans normally.
func TestPlanRefusesAnInvalidBatchProjectCap(t *testing.T) {
	t.Parallel()
	plan := func(options extension.ParamMap) error {
		t.Helper()
		ws := makeTestWorkspace()
		ext := makeTestExtension()
		ext.Jobs["test"].Batchable = &extension.TaskBatchPolicy{Tool: "suite", MaxProjectsParam: batchCapOption}
		ws.Config = &wsproto.Config{}
		if options != nil {
			ws.Config.Options = map[string]extension.ParamMap{"test": options}
		}
		_, err := Plan(ws, []string{"test"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
		return err
	}

	for name, options := range map[string]extension.ParamMap{
		"unset": nil,
		"cap 2": {batchCapOption: 2.0},
		"cap 1": {batchCapOption: 1.0},
	} {
		if err := plan(options); err != nil {
			t.Errorf("%s: Plan = %v, want success", name, err)
		}
	}
	for name, options := range map[string]extension.ParamMap{
		"zero":   {batchCapOption: 0.0},
		"string": {batchCapOption: "six"},
		"camel":  {"batchMaxProjects": -1.0},
	} {
		err := plan(options)
		if err == nil || !strings.Contains(err.Error(), "invalid batch size") {
			t.Errorf("%s: Plan = %v, want an invalid batch size error", name, err)
		}
	}
}
