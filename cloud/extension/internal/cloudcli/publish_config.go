package cloudcli

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	configcli "go.putnami.dev/cloud/extension/internal/configcli"
	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	jobproto "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/releaseset"
)

func validateConfig(params map[string]any, args []string, workspaceRoot string, ioctx IO) error {
	// This is an explicit validation command, so an inherited packaging hint
	// must never turn a missing schema or namespace into a successful skip.
	delete(params, "if-present")
	delete(params, "ifPresent")
	if stringParam(params, "env", "environment") == "" {
		params["env"] = configcli.AuthoredConfigEnvironment
	}
	prepared, err := configcli.PrepareAuthoredConfig(params, args, workspaceRoot, ioctx)
	if err != nil {
		return err
	}
	if prepared == nil {
		return newError("native Config validation produced no authored member", ExitUsage)
	}
	configcli.WritePublishResult(map[string]any{
		"status": "validated", "coordinate": prepared.Coordinate, "version": prepared.Version,
		"schema": prepared.SchemaPath, "values": prepared.ValuesPaths,
	}, params, ioctx, fmt.Sprintf("Validated authored Config %s@%s for prod.", prepared.Coordinate, prepared.Version), nil)
	return nil
}

func validateConfigProject(params map[string]any, args []string, workspaceRoot string, ioctx IO) error {
	adoptPositionalApp(params, args)
	project, err := resolveApp(params, workspaceRoot)
	if err != nil {
		return err
	}
	if err := refuseConfigCheckOverrides(params, project); err != nil {
		return err
	}
	namespace, declared, err := configcli.DeclaredConfigNamespace(workspaceRoot, project)
	if err != nil {
		return err
	}
	if !declared {
		configcli.WritePublishResult(map[string]any{"status": "skipped", "reason": "no declared Config namespace"}, params, ioctx, "No declared Config namespace — skipping authored Config validation.", nil)
		return nil
	}
	// Ordinary validation checks the same production member as native packaging.
	// A declared member must have a schema; missing artifacts are never a skip.
	params["env"] = configcli.AuthoredConfigEnvironment
	params["namespace"] = namespace
	return validateConfig(params, nil, workspaceRoot, ioctx)
}

// refuseConfigCheckOverrides keeps the cached ordinary check on its own
// project. The task runs from {projectRoot} and its cache key hashes that
// directory's files, so a schema override or another project's name would
// store a verdict on files the key never hashed. The own project comes from
// the working directory's putnami.json, never from a workspace walk, and
// clicore.FindAppDir resolves that name to the working directory, so a
// same-name copy elsewhere in the tree cannot change the result.
// `putnami cloud config validate` is uncached and keeps both overrides.
func refuseConfigCheckOverrides(params map[string]any, project string) error {
	if schema := stringParam(params, "schema-from", "schemaFrom"); schema != "" {
		return newError(fmt.Sprintf("validate~cloud-config-member does not accept --schema-from (%s); run `putnami cloud config validate --schema-from %s` instead", schema, schema), ExitUsage)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	own, _ := clicore.ProjectNameAt(cwd)
	if own == "" || project != own {
		return newError(fmt.Sprintf("validate~cloud-config-member checks only the project in its working directory %s, not %s; run `putnami cloud config validate %s` instead", cwd, project, project), ExitUsage)
	}
	return nil
}

func packageConfigMember(params map[string]any, args []string, workspaceRoot string, ioctx IO) error {
	prepared, err := configcli.PackageAuthoredConfig(params, args, workspaceRoot, ioctx)
	if err != nil {
		return err
	}
	if prepared == nil {
		configcli.WritePublishResult(map[string]any{"status": "skipped", "reason": "no config schema or declared namespace"}, params, ioctx, "No config schema or declared Config namespace — skipping Config member package.", nil)
		return nil
	}
	configcli.WritePublishResult(map[string]any{
		"status": "packaged", "coordinate": prepared.Coordinate, "version": prepared.Version,
		"artifact_digest": prepared.Member.Descriptor.ContentDigest,
	}, params, ioctx, fmt.Sprintf("Packaged Config member %s@%s.", prepared.Coordinate, prepared.Version), nil)
	return nil
}

func publishConfig(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	outbox := distributioncli.PublicationOutbox(env)
	if _, managed := params[releaseset.ContextParamName]; !managed {
		if outbox != "" {
			return distributioncli.OutboxWithoutPlanError("Config publication")
		}
		return configcli.PublishConfig(params, args, workspaceRoot, env, ioctx)
	}
	prepared, err := configcli.PrepareAuthoredConfig(params, args, workspaceRoot, ioctx)
	if err != nil {
		return err
	}
	if prepared == nil {
		configcli.WritePublishResult(map[string]any{"status": "skipped", "reason": "no config schema or declared namespace"}, params, ioctx, "No config schema or declared Config namespace — skipping Config member publish.", nil)
		return nil
	}
	selected, err := configMemberSelection(params, prepared)
	if err != nil {
		return newError(err.Error(), ExitUsage)
	}
	if err := configcli.VerifyPackagedAuthoredConfig(ioctx.Context, prepared); err != nil {
		return err
	}
	if truthy(param(params, "dry-run", "dryRun")) {
		configcli.WritePublishResult(map[string]any{
			"status": "dry-run", "coordinate": prepared.Coordinate, "version": prepared.Version,
			"artifact_digest": prepared.Member.Descriptor.ContentDigest,
			"source_revision": selected.SourceRevision, "selection_fingerprint": selected.SelectionFingerprint,
		}, params, ioctx, fmt.Sprintf("Dry run: Config member %s@%s is ready for publication.", prepared.Coordinate, prepared.Version), nil)
		return nil
	}
	publication := distributioncli.ConfigMemberPublication{
		Namespace: prepared.Namespace, Package: prepared.Package, Version: prepared.Version, Payload: prepared.Member.Bytes,
	}
	if outbox != "" {
		// publication-v1: pack the member and emit no published member; the
		// engine uploads it and reports the member itself.
		packed, err := distributioncli.PackConfigMember(outbox, selected.ProjectID, publication)
		if err != nil {
			return err
		}
		if !sameConfigMember(packed, prepared) {
			return newError("Distribution packed a different Config member identity", ExitAPI)
		}
		configcli.WritePublishResult(map[string]any{
			"status": "packed", "coordinate": packed.Coordinate, "version": packed.Version, "artifact_digest": packed.ArtifactDigest,
		}, params, ioctx, fmt.Sprintf("Packed Config member %s@%s into the publication outbox.", packed.Coordinate, packed.Version), nil)
		return nil
	}
	published, err := distributioncli.PublishConfigMember(params, workspaceRoot, env, ioctx, publication)
	if err != nil {
		return err
	}
	if !sameConfigMember(published, prepared) {
		return newError("Distribution confirmed a different Config member identity", ExitAPI)
	}
	return emitConfigPublishedMember(ioctx, published)
}

// sameConfigMember reports that Distribution published, or packed, exactly
// the prepared member: its coordinate, version and content digest.
func sameConfigMember(result *distributioncli.ConfigMemberPublicationResult, prepared *configcli.PreparedAuthoredConfig) bool {
	return result != nil && result.Coordinate == prepared.Coordinate && result.Version == prepared.Version &&
		result.ArtifactDigest == prepared.Member.Descriptor.ContentDigest
}

func configMemberSelection(params map[string]any, prepared *configcli.PreparedAuthoredConfig) (releaseset.PlannedMember, error) {
	raw, err := json.Marshal(params[releaseset.ContextParamName])
	if err != nil {
		return releaseset.PlannedMember{}, fmt.Errorf("encode %s: %w", releaseset.ContextParamName, err)
	}
	plan, err := releaseset.ParseParams(jobproto.Params{releaseset.ContextParamName: raw})
	if err != nil {
		return releaseset.PlannedMember{}, err
	}
	if channel, present := params["channel"]; present {
		requested, ok := channel.(string)
		if !ok {
			return releaseset.PlannedMember{}, fmt.Errorf("config --channel must match the strict release-set plan")
		}
		matched := false
		for _, part := range strings.Split(requested, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			if !slices.Contains(plan.Channels, name) {
				return releaseset.PlannedMember{}, fmt.Errorf("config --channel must match the strict release-set plan")
			}
			matched = true
		}
		if !matched {
			return releaseset.PlannedMember{}, fmt.Errorf("config --channel must match the strict release-set plan")
		}
	}
	var selected *releaseset.PlannedMember
	for _, member := range plan.SelectedMembers() {
		if member.Ecosystem != distributionproto.Ecosystem("put") || member.Coordinate != prepared.Coordinate {
			continue
		}
		if member.ProjectID != prepared.ProjectID {
			return releaseset.PlannedMember{}, fmt.Errorf("selected Config member belongs to a different project")
		}
		if selected != nil {
			return releaseset.PlannedMember{}, fmt.Errorf("release-set plan selects the Config member more than once")
		}
		copy := member
		selected = &copy
	}
	if selected == nil {
		return releaseset.PlannedMember{}, fmt.Errorf("release-set plan does not select Config member %s for project %s", prepared.Coordinate, prepared.ProjectID)
	}
	if selected.Version != prepared.Version || selected.SourceRevision != prepared.Member.Descriptor.SourceProvenance.Revision {
		return releaseset.PlannedMember{}, fmt.Errorf("authored Config member %s@%s does not match the selected immutable source", prepared.Coordinate, prepared.Version)
	}
	return *selected, nil
}

func emitConfigPublishedMember(ioctx IO, published *distributioncli.ConfigMemberPublicationResult) error {
	if published == nil || ioctx.Artifact == nil {
		return nil
	}
	member := &extensionproto.PublishedMember{
		Ecosystem: string(cloudConfigEcosystem), Coordinate: published.Coordinate,
		Version: published.Version, ArtifactDigest: published.ArtifactDigest,
	}
	if diagnostics := extensionproto.ValidatePublishedMember(member); len(diagnostics) != 0 {
		return fmt.Errorf("refuse invalid Config publication result: %s", diagnostics[0].String())
	}
	encoded, err := json.Marshal(member)
	if err != nil {
		return fmt.Errorf("encode Config publication result: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		return fmt.Errorf("encode Config publication event data: %w", err)
	}
	return ioctx.Artifact(
		"config-"+strings.NewReplacer("/", "-", "@", "").Replace(member.Coordinate),
		member.Coordinate, extensionproto.PublishedMemberEventKind, "", data,
	)
}
