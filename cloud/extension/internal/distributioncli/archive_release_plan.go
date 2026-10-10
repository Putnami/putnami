package distributioncli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	distributionproto "go.putnami.dev/protocol/distribution"
	jobproto "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/releaseset"
)

// validateArchiveReleaseSetPlan binds the last local publication decision to
// the coordinator's strict plan before credentials are resolved or any Put
// request is made. The generic publish --channel flag is orchestration input;
// it is tolerated only when that plan carries the same channel and is never
// translated into an archive channel write. It returns the one planned member
// the publication matches, or nil when no plan is present.
func validateArchiveReleaseSetPlan(params map[string]any, projectPath, coordinate, version string) (*releaseset.PlannedMember, error) {
	plan, present, err := archiveReleaseSetPlan(params)
	if err != nil {
		return nil, err
	}
	if err := validatePlannedPublishChannels(params, plan, present, "archive"); err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}

	projectID := "/" + filepath.ToSlash(strings.TrimPrefix(projectPath, "./"))
	if projectPath == "." || projectPath == "" {
		projectID = "/"
	}
	var matched []releaseset.PlannedMember
	for _, member := range plan.SelectedMembers() {
		if member.ProjectID != projectID || member.Ecosystem != distributionproto.Ecosystem("archive") {
			continue
		}
		matched = append(matched, member)
		if member.Coordinate != coordinate || member.Version != version {
			return nil, fmt.Errorf("archive publication %s@%s does not match planned member %s@%s for project %s",
				coordinate, version, member.Coordinate, member.Version, projectID)
		}
	}
	if len(matched) != 1 {
		return nil, fmt.Errorf("release-set plan selects %d archive members for project %s; exactly one is required", len(matched), projectID)
	}
	return &matched[0], nil
}

// The framework accepts a comma-separated --channel list and may add an
// immutable tag channel to the plan. Every explicitly requested channel must
// be in the strict plan; this parameter never authorizes a native channel write.
func validatePlannedPublishChannels(params map[string]any, plan *releaseset.Plan, present bool, artifact string) error {
	channel, carriesChannel := params["channel"]
	if !carriesChannel {
		return nil
	}
	value, ok := channel.(string)
	refusal := fmt.Errorf("--channel is unavailable for direct %s publishing; it must match a strict release-set plan", artifact)
	if !present || plan == nil || !ok {
		return refusal
	}
	matched := false
	for _, part := range strings.Split(value, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		if !slices.Contains(plan.Channels, name) {
			return refusal
		}
		matched = true
	}
	if !matched {
		return refusal
	}
	return nil
}

func archiveReleaseSetPlan(params map[string]any) (*releaseset.Plan, bool, error) {
	value, present := params[releaseset.ContextParamName]
	if !present {
		return nil, false, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, true, fmt.Errorf("encode %s: %w", releaseset.ContextParamName, err)
	}
	plan, err := releaseset.ParseParams(jobproto.Params{releaseset.ContextParamName: raw})
	if err != nil {
		return nil, true, err
	}
	return plan, true, nil
}
