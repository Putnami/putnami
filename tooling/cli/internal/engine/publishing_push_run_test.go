package engine

import (
	"context"
	"strings"
	"testing"
)

// A push run on main executes the gate in one session with `publish --channel`
// and every project selected. Release-set scoping must not re-plan that whole
// session on the projects that own a release-set member: a library that owns
// no member would then never run its tests on main, and a failure that a pull
// request run reports would merge unseen.
//
// The replay is that session over a workspace with three shapes: `app` owns a
// member, `lib` and `other` own none. When the scoping re-plans the session,
// the plan holds `/app:test~test` and neither library's.
func TestAPublishingPushRunPlansEveryProjectsTests(t *testing.T) {
	_, req := releaseSetSessionFixture(t)
	req.CommandParams["channel"] = "canary"
	req.Global.Impacted = false
	req.Global.All = true
	req.Global.Projects = "*"

	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil {
		t.Fatalf("push-run session: %v", err)
	}
	keys := planKeys(t, result.Plan)
	var tests []string
	for _, key := range keys {
		if strings.HasSuffix(key, ":test~test") {
			tests = append(tests, key)
		}
	}
	if len(tests) != 3 {
		t.Errorf("test~test planned for %v, want every project of the selection", tests)
	}
	assertPlanned(t, keys, "/lib:test~test", "/other:test~test", "/app:publish~artifact")
	if result.ExitCode != ExitSuccess {
		t.Fatalf("push-run session = exit %d", result.ExitCode)
	}
}
