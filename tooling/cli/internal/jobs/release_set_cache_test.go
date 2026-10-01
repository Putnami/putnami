package jobs

import (
	"encoding/json"
	"testing"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/sdk/extension/releaseset"
)

func TestReleaseSetPlanBoundParamIsPartOfTaskCacheIdentity(t *testing.T) {
	t.Parallel()
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/library"},
		Extension: &extension.ExtensionDescription{Name: "test"},
		JobDef: &extension.JobDefinition{
			Name:        "package~npm",
			CommandName: "package",
		},
	}
	bound, _ := json.Marshal(struct {
		Plan *releaseset.Plan `json:"releaseSetPlan"`
	}{Plan: planWithHead("a")})
	if err := json.Unmarshal(bound, &job.JobDef.BoundParams); err != nil {
		t.Fatal(err)
	}
	first := taskCacheParams(nil, job, nil)
	job.JobDef.BoundParams[releaseset.ContextParamName] = planWithHead("b")
	second := taskCacheParams(nil, job, nil)
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) == string(secondJSON) {
		t.Fatalf("base release-set ref/member mapping did not change cache params: %s", firstJSON)
	}
	if first[releaseset.ContextParamName] == nil || second[releaseset.ContextParamName] == nil {
		t.Fatalf("release-set plan absent from cache params: first=%v second=%v", first, second)
	}

	withMember := planWithHead("b")
	withMember.Members = []releaseset.PlannedMember{{Ecosystem: "npm", Coordinate: "@putnami/library", Version: "1.0.0-old"}}
	job.JobDef.BoundParams[releaseset.ContextParamName] = withMember
	third := taskCacheParams(nil, job, nil)
	thirdJSON, _ := json.Marshal(third)
	if string(secondJSON) == string(thirdJSON) {
		t.Fatalf("relevant member mapping did not change cache params: %s", thirdJSON)
	}
}

// TestSelectionKeyIgnoresTheBoundReleaseSetPlan pins the other half of the
// same seam. The EXECUTION key must move with the plan, because a package task
// packages what the plan tells it to; the SELECTION key must not, because the
// plan is DERIVED from the selection fingerprints and a key that saw it would
// be defined in terms of itself.
func TestSelectionKeyIgnoresTheBoundReleaseSetPlan(t *testing.T) {
	t.Parallel()
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/library"},
		Extension: &extension.ExtensionDescription{Name: "test"},
		JobDef: &extension.JobDefinition{
			Name:        "package~npm",
			CommandName: "package",
			BoundParams: extension.ParamMap{releaseset.ContextParamName: planWithHead("a")},
		},
	}
	first := taskCacheParamsWith(nil, job, nil, true)
	job.JobDef.BoundParams[releaseset.ContextParamName] = planWithHead("b")
	second := taskCacheParamsWith(nil, job, nil, true)
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("selection params saw the bound plan: %s vs %s", firstJSON, secondJSON)
	}
	if first[releaseset.ContextParamName] != nil {
		t.Fatalf("selection params carry the release-set plan: %v", first)
	}
}

// planWithHead is a plan over the canary channel at a given head ref.
func planWithHead(hex string) *releaseset.Plan {
	ref := structRef(hex)
	return &releaseset.Plan{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Channels:        []string{"canary"},
		Heads:           map[string]*distribution.ChannelHead{"canary": {Ref: *ref, Generation: 2}},
	}
}

func structRef(hex string) *distribution.ReleaseSetRef {
	return &distribution.ReleaseSetRef{ID: "rs_" + repeatHex(hex), Digest: "sha256:" + repeatHex(hex)}
}

func repeatHex(value string) string {
	result := ""
	for len(result) < 64 {
		result += value
	}
	return result[:64]
}
