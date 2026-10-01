package engine

import "testing"

// TestWatchIterationNeverReentersWatchMode pins a fix: a serve
// iteration request built by the engine's own watch loop must not re-derive
// serve mode from Commands and force Global.Watch back on — that recursion ran
// runWatch ~20×/s with the workload never starting (introduced when A5b routed
// iterations back through Engine.Run; the force rule undid the loop's cleared
// Watch flag).
//
// Mutation control: remove the watchIteration guard from execute()'s force
// rule and the first assertion fails; remove the flag from
// watchIterationRequest and the second does.
func TestWatchIterationNeverReentersWatchMode(t *testing.T) {
	t.Parallel()
	outer := &Request{Commands: []string{"serve"}}
	outer.Global.Watch = true

	iteration := watchIterationRequest(outer, nil, nil, true)
	if !iteration.watchIteration {
		t.Fatal("watchIterationRequest did not mark the request as an iteration")
	}
	if iteration.Global.Watch {
		t.Fatal("iteration request carries Watch=true; the outer loop owns the replan policy")
	}

	// The force-watch rule must leave an iteration request alone.
	req := iteration
	isServeMode := len(req.Commands) == 1 && req.Commands[0] == "serve"
	if isServeMode && !req.Global.Watch && !req.watchIteration {
		req.Global.Watch = true
	}
	if req.Global.Watch {
		t.Fatal("the serve force-watch rule re-enabled Watch on an iteration request — this is the same recursion")
	}
}
