package cloudcli

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	datacli "go.putnami.dev/cloud/extension/internal/datacli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	jobproto "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/releaseset"
)

func publishMigration(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	outbox := distributioncli.PublicationOutbox(env)
	if _, managed := params[releaseset.ContextParamName]; outbox != "" && !managed {
		return distributioncli.OutboxWithoutPlanError("Data migration publication")
	}
	target, err := datacli.ResolveMigrationTarget(params, args, workspaceRoot)
	if err != nil {
		return err
	}
	if err := requirePlannedMigrationBundle(params, target); err != nil {
		return err
	}
	prepareParams, err := migrationPreparationParams(params, target)
	if err != nil {
		return newError(err.Error(), ExitUsage)
	}
	prepared, err := target.Prepare(prepareParams, ioctx)
	if err != nil || prepared == nil {
		return err
	}
	selection, err := migrationSelection(params, prepared)
	if err != nil {
		return newError(err.Error(), ExitUsage)
	}
	if truthy(param(params, "dry-run", "dryRun")) {
		datacli.ReportMigrationDryRun(params, ioctx, prepared, selection)
		return nil
	}
	if outbox != "" {
		// publication-v1: pack the member and emit no published member. The
		// engine uploads it and reports the member itself, and Data accepts it
		// at the first deploy that selects it, so the job calls no Data
		// endpoint and resolves no credential.
		packed, err := datacli.PackPreparedMigration(outbox, prepared.ProjectPath, prepared, selection)
		if err != nil {
			return err
		}
		datacli.ReportPackedMigration(params, ioctx, packed)
		return nil
	}
	bearer, err := distributioncli.ResolvePutPublicationBearer(params, workspaceRoot, env, ioctx)
	if err != nil {
		return err
	}
	accepted, err := datacli.PublishPreparedMigration(params, workspaceRoot, env, ioctx, prepared, selection, bearer)
	if err != nil {
		return err
	}
	return emitMigrationPublishedMember(ioctx, accepted)
}

// Migration content is cacheable across releases; its build-time version is
// provenance, not the current publication identity. Bind the existing explicit
// bundle-version input to the strict plan before preparing the publication.
// Keep the cached bundle bytes intact: Data validates their content independently.
func migrationPreparationParams(params map[string]any, target *datacli.MigrationTarget) (map[string]any, error) {
	value, managed := params[releaseset.ContextParamName]
	if !managed || target.BundleMissing() {
		return params, nil
	}
	plan, err := managedReleasePlan(value)
	if err != nil {
		return nil, err
	}
	coordinate, err := target.Coordinate(params)
	if err != nil || coordinate == "" {
		return params, err
	}
	selected, err := selectedMigrationMember(plan, target.ProjectPath, coordinate)
	if err != nil {
		return nil, err
	}
	if explicit := stringParam(params, "bundle-version", "bundleVersion", "version"); explicit != "" && explicit != selected.Version {
		return nil, fmt.Errorf("migration version %s does not match selected member %s@%s", explicit, coordinate, selected.Version)
	}
	bound := maps.Clone(params)
	bound["bundle-version"] = selected.Version
	return bound, nil
}

func migrationSelection(params map[string]any, prepared *datacli.PreparedMigration) (datacli.MigrationSelection, error) {
	value, managed := params[releaseset.ContextParamName]
	if !managed {
		sourceRevision := firstString(stringParam(params, "source-revision", "sourceRevision"))
		fingerprint := firstString(stringParam(params, "selection-fingerprint", "selectionFingerprint"))
		if sourceRevision == "" || fingerprint == "" {
			return datacli.MigrationSelection{}, fmt.Errorf("migration publication requires a selected release-set member or explicit source provenance")
		}
		if _, carriesChannel := params["channel"]; carriesChannel {
			return datacli.MigrationSelection{}, fmt.Errorf("--channel is available only through a selected release-set plan")
		}
		return datacli.MigrationSelection{SourceRevision: sourceRevision, SelectionFingerprint: fingerprint}, nil
	}
	plan, err := managedReleasePlan(value)
	if err != nil {
		return datacli.MigrationSelection{}, err
	}
	if channel, present := params["channel"]; present {
		requested, ok := channel.(string)
		if !ok {
			return datacli.MigrationSelection{}, fmt.Errorf("migration --channel must match the strict release-set plan")
		}
		matched := false
		for _, part := range strings.Split(requested, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			if !slices.Contains(plan.Channels, name) {
				return datacli.MigrationSelection{}, fmt.Errorf("migration --channel must match the strict release-set plan")
			}
			matched = true
		}
		if !matched {
			return datacli.MigrationSelection{}, fmt.Errorf("migration --channel must match the strict release-set plan")
		}
	}
	selected, err := selectedMigrationMember(plan, prepared.ProjectPath, prepared.ExpectedCoordinate)
	if err != nil {
		return datacli.MigrationSelection{}, err
	}
	if selected.Coordinate != prepared.ExpectedCoordinate || selected.Version != prepared.Version {
		return datacli.MigrationSelection{}, fmt.Errorf("migration %s@%s does not match selected member %s@%s",
			prepared.ExpectedCoordinate, prepared.Version, selected.Coordinate, selected.Version)
	}
	return datacli.MigrationSelection{SourceRevision: selected.SourceRevision, SelectionFingerprint: selected.SelectionFingerprint}, nil
}

func selectedMigrationMember(plan *releaseset.Plan, projectPath, coordinate string) (*releaseset.PlannedMember, error) {
	var selected *releaseset.PlannedMember
	for _, member := range plan.SelectedMembers() {
		if member.ProjectID != projectPath || member.Ecosystem != distributionproto.Ecosystem("put") ||
			member.Coordinate != coordinate {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("release-set plan selects migration member %s more than once", coordinate)
		}
		copy := member
		selected = &copy
	}
	if selected == nil {
		return nil, fmt.Errorf("release-set plan does not select the migration member for project %s", projectPath)
	}
	return selected, nil
}

// requirePlannedMigrationBundle refuses to skip a missing migration bundle
// when the managed release-set plan selects this project's migration member.
// A skip emits no PublishedMember event, and the release-set coordinator then
// fails far from the cause with "partial release-set publication".
func requirePlannedMigrationBundle(params map[string]any, target *datacli.MigrationTarget) error {
	value, managed := params[releaseset.ContextParamName]
	if !managed || !target.BundleMissing() {
		return nil
	}
	plan, err := managedReleasePlan(value)
	if err != nil {
		return newError(err.Error(), ExitUsage)
	}
	coordinate, err := target.Coordinate(params)
	if err != nil || coordinate == "" {
		return err
	}
	for _, member := range plan.SelectedMembers() {
		if member.Ecosystem == distributionproto.Ecosystem("put") && member.Coordinate == coordinate {
			return newError(fmt.Sprintf(
				"release-set plan selects migration member %s but %s does not exist; the build produced no migration bundle for %s",
				coordinate, target.BundleManifestPath(), target.Application), ExitUsage)
		}
	}
	return nil
}

// managedReleasePlan strictly parses the orchestrator's release-set plan
// parameter value.
func managedReleasePlan(value any) (*releaseset.Plan, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", releaseset.ContextParamName, err)
	}
	return releaseset.ParseParams(jobproto.Params{releaseset.ContextParamName: raw})
}

func emitMigrationPublishedMember(ioctx IO, accepted *datacli.PublishedMigration) error {
	if accepted == nil || ioctx.Artifact == nil {
		return nil
	}
	member := &extensionproto.PublishedMember{
		Ecosystem: accepted.Member.Ecosystem, Coordinate: accepted.Member.Coordinate,
		Version: accepted.Member.Version, ArtifactDigest: accepted.Member.ArtifactDigest,
	}
	if diagnostics := extensionproto.ValidatePublishedMember(member); len(diagnostics) != 0 {
		return fmt.Errorf("refuse invalid Data migration publication result: %s", diagnostics[0].String())
	}
	encoded, err := json.Marshal(member)
	if err != nil {
		return fmt.Errorf("encode migration publication result: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		return fmt.Errorf("encode migration publication event data: %w", err)
	}
	return ioctx.Artifact(
		"migration-"+strings.NewReplacer("/", "-", "@", "").Replace(member.Coordinate),
		member.Coordinate, extensionproto.PublishedMemberEventKind, "", data,
	)
}
