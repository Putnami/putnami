package cloudcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	distributioncli "go.putnami.dev/cloud/extension/internal/distributioncli"
	distributionproto "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/infra"
	"go.putnami.dev/protocol/put"
	"go.putnami.dev/sdk/extension/releaseset"
)

// A workload's deployment declaration travels as a put release-set member of
// kind "deployment". The framework language extensions own the bytes: their
// package step "deployment" writes <project>/.gen/deployment.json. Cloud owns
// the declaration (the workspace probe) and the upload (the publish step).
const (
	cloudDeploymentEcosystem     = distributionproto.Ecosystem("put")
	cloudDeploymentPackageStep   = "deployment"
	cloudDeploymentPublishStep   = "cloud-publish-deployment"
	cloudDeploymentPackageSuffix = "-deployment"
	// cloudDeploymentStepParam and cloudImageStepParam are the two parameters
	// of the framework step condition "params.deployment && !params.image".
	cloudDeploymentStepParam = "deployment"
	cloudImageStepParam      = "image"
	// cloudPackageCommand is the framework command that owns the step.
	cloudPackageCommand = "package"
	// cloudApplicationType is the one authored project type, besides none,
	// that the framework treats as a deployable workload.
	cloudApplicationType = "application"
)

// cloudDeploymentPackagePublishers are the language extensions whose package
// command carries the deployment step, in migrationPackageProducer's order.
var cloudDeploymentPackagePublishers = []string{"@putnami/go", "@putnami/typescript"}

// deploymentDeclaration returns the project's deployment member when all of
// these hold, and nothing otherwise:
//
//   - the project declares a Config member, whose namespace the caller passes;
//   - options["@putnami/cloud"].deploy.enabled is true;
//   - the project is a workload, which is the only kind the step declares;
//   - the project's own options turn its language publisher's deployment
//     package step on (deploymentStepActive).
//
// The last condition is the one switch. Declaring a member whose package step
// is off fails the whole publication, so the probe reads that step's own
// condition rather than a second Cloud option.
func (p cloudProjectProbe) deploymentDeclaration(namespace string) (releaseset.MemberDeclaration, bool, error) {
	if !p.deployEnabled() || (p.Type != "" && p.Type != cloudApplicationType) {
		return releaseset.MemberDeclaration{}, false, nil
	}
	active := false
	for _, publisher := range cloudDeploymentPackagePublishers {
		if slices.Contains(p.Extensions, publisher) && p.deploymentStepActive(publisher) {
			active = true
		}
	}
	if !active {
		return releaseset.MemberDeclaration{}, false, nil
	}
	publisher, err := p.deploymentPackagePublisher()
	if err != nil {
		return releaseset.MemberDeclaration{}, false, err
	}
	coordinate, err := deploymentMemberCoordinate(namespace, p.Name)
	if err != nil {
		return releaseset.MemberDeclaration{}, false, err
	}
	return releaseset.MemberDeclaration{
		Ecosystem: cloudDeploymentEcosystem, Coordinate: coordinate,
		PackagePublisher: publisher, PackageStep: cloudDeploymentPackageStep, PublishStep: cloudDeploymentPublishStep,
	}, true, nil
}

// deployEnabled reads options["@putnami/cloud"].deploy.enabled, the switch
// that makes a project a continuously deployed workload.
func (p cloudProjectProbe) deployEnabled() bool {
	deploy, ok := p.Options[cloudRuntimeIdentity]["deploy"].(map[string]any)
	return ok && deploy["enabled"] == true
}

// deploymentStepActive evaluates the framework condition of the publisher's
// package step "deployment", "params.deployment && !params.image", over the
// parameter layers a project's own putnami.json sets, in the framework's
// ascending precedence: options.package, options[<publisher>],
// options[<publisher>:package], then each publish channel, which forces its
// parameter to true. A higher layer replaces a lower value, null included.
//
// Workspace-level defaults and command-line flags are invisible to a project
// probe. One that turns the step on where the project is silent leaves the
// step without a member, which publishes nothing. One that sets "image" turns
// the step off under a declared member and fails the publication, so a
// workload keeps both parameters in its own options.
func (p cloudProjectProbe) deploymentStepActive(publisher string) bool {
	params := make(map[string]any, 2)
	for _, layer := range []string{cloudPackageCommand, publisher, publisher + ":" + cloudPackageCommand} {
		for _, name := range []string{cloudDeploymentStepParam, cloudImageStepParam} {
			if value, found := p.Options[layer][name]; found {
				params[name] = value
			}
		}
	}
	for _, channel := range p.Publish {
		if channel == cloudDeploymentStepParam || channel == cloudImageStepParam {
			params[channel] = true
		}
	}
	return stepParamTruthy(params[cloudDeploymentStepParam]) && !stepParamTruthy(params[cloudImageStepParam])
}

// stepParamTruthy is the framework's truthiness for a step condition operand:
// null and false are false, a number is true when it is not zero, a string is
// true unless it is empty or "false", and any object or array is true.
func stepParamTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0
	case string:
		return typed != "" && typed != "false"
	default:
		return true
	}
}

// deploymentPackagePublisher chooses the language extension that packages the
// declaration the way migrationPackageProducer chooses the bundle producer:
// exactly one of the project's extensions must be a language publisher.
func (p cloudProjectProbe) deploymentPackagePublisher() (string, error) {
	publisher := ""
	for _, candidate := range cloudDeploymentPackagePublishers {
		if !slices.Contains(p.Extensions, candidate) {
			continue
		}
		if publisher != "" {
			return "", fmt.Errorf("a deployment member requires exactly one deployment language publisher")
		}
		publisher = candidate
	}
	if publisher == "" {
		return "", fmt.Errorf("a deployment member requires exactly one deployment language publisher")
	}
	return publisher, nil
}

// deploymentMemberCoordinate is the one Put coordinate of a workload's
// deployment member: the Config namespace, then the project name with "/"
// replaced by "-" and the "-deployment" suffix. It applies the Config
// coordinate's canonical-name rule, so a project whose Config coordinate is
// valid maps here too unless the longer suffix exceeds the segment bound.
func deploymentMemberCoordinate(namespace, project string) (string, error) {
	if !nativeMigrationPart.MatchString(namespace) || project == "" || project != strings.TrimSpace(project) ||
		strings.HasPrefix(project, "/") || strings.HasSuffix(project, "/") || strings.Contains(project, "//") || strings.Contains(project, "..") {
		return "", fmt.Errorf("a deployment member requires a canonical namespace and project name")
	}
	packageName := strings.ReplaceAll(project, "/", "-") + cloudDeploymentPackageSuffix
	if !nativeMigrationPart.MatchString(packageName) {
		return "", fmt.Errorf("project %q does not map to a native Put package", project)
	}
	return namespace + "/" + packageName, nil
}

// deploymentTarget is the project a publish step runs for. Coordinate is the
// deployment coordinate the probe derives for it, and empty when the project
// names no Config namespace or its name maps to no Put package: the probe then
// declares no deployment member for it, or fails the plan before this step.
type deploymentTarget struct {
	ProjectID   string // the plan's ProjectID: "/" + the path without grouping folders (projectIDFromPath)
	Name        string
	Namespace   string
	Package     string
	Coordinate  string
	Declaration string // absolute path of <project>/.gen/deployment.json
}

// workload is the value the project's deployment declaration must name: the
// project ID without its leading slash. The release set records the same value
// as the member's project, and Control compares the declaration with it. A
// project at the workspace root has an empty ID and keeps its name, as the
// framework writes it (distribution.MemberProjectID).
func (t deploymentTarget) workload() string {
	if id := strings.TrimPrefix(t.ProjectID, "/"); id != "" {
		return id
	}
	return t.Name
}

// publishDeployment backs the publish step "cloud-publish-deployment". It
// publishes exactly the deployment member the release-set plan selected for
// this project, byte for byte from the language package step's artifact, and
// reports it to the coordinator. Without a plan, or when the plan selects no
// deployment member of this project, it publishes nothing.
//
// Under the engine's publication-v1 capability (PUTNAMI_PUBLICATION_OUTBOX
// set) it packs the same bytes into the outbox instead, like the Config step:
// it uploads nothing, emits no published member, and refuses an outbox
// without a plan.
func publishDeployment(params map[string]any, args []string, workspaceRoot string, env map[string]string, ioctx IO) error {
	outbox := distributioncli.PublicationOutbox(env)
	value, managed := params[releaseset.ContextParamName]
	if !managed {
		if outbox != "" {
			return distributioncli.OutboxWithoutPlanError("Deployment member publication")
		}
		writeResult(map[string]any{"status": "skipped", "reason": "no release-set plan"}, params, ioctx,
			"Deployment members publish only through a release-set plan — skipping the deployment publish.")
		return nil
	}
	plan, err := managedReleasePlan(value)
	if err != nil {
		return newError(err.Error(), ExitUsage)
	}
	target, err := resolveDeploymentTarget(params, args, workspaceRoot)
	if err != nil {
		return err
	}
	selected, found, err := selectedDeploymentMember(params, plan, target)
	if err != nil {
		return newError(err.Error(), ExitUsage)
	}
	if !found {
		writeResult(map[string]any{"status": "skipped", "reason": "no selected deployment member"}, params, ioctx,
			"The release-set plan selects no deployment member of "+target.ProjectID+" — skipping the deployment publish.")
		return nil
	}
	payload, err := readDeploymentDeclaration(target)
	if err != nil {
		return err
	}
	digest := put.Digest(payload)
	if truthy(param(params, "dry-run", "dryRun")) {
		writeResult(map[string]any{
			"status": "dry-run", "coordinate": target.Coordinate, "version": selected.Version, "artifact_digest": digest,
			"source_revision": selected.SourceRevision, "selection_fingerprint": selected.SelectionFingerprint,
		}, params, ioctx, fmt.Sprintf("Dry run: deployment member %s@%s is ready for publication.", target.Coordinate, selected.Version))
		return nil
	}
	publication := distributioncli.DeploymentMemberPublication{
		Namespace: target.Namespace, Package: target.Package, Version: selected.Version, Payload: payload,
	}
	if outbox != "" {
		// publication-v1: pack the member and emit no published member; the
		// engine uploads it and reports the member itself.
		packed, err := distributioncli.PackDeploymentMember(outbox, selected.ProjectID, publication)
		if err != nil {
			return err
		}
		if !sameDeploymentMember(packed, target, selected, digest) {
			return newError("Distribution packed a different deployment member identity", ExitAPI)
		}
		writeResult(map[string]any{
			"status": "packed", "coordinate": packed.Coordinate, "version": packed.Version, "artifact_digest": packed.ArtifactDigest,
		}, params, ioctx, fmt.Sprintf("Packed deployment member %s@%s into the publication outbox.", packed.Coordinate, packed.Version))
		return nil
	}
	published, err := distributioncli.PublishDeploymentMember(params, workspaceRoot, env, ioctx, publication)
	if err != nil {
		return err
	}
	if !sameDeploymentMember(published, target, selected, digest) {
		return newError("Distribution confirmed a different deployment member identity", ExitAPI)
	}
	writeResult(map[string]any{
		"status": "published", "coordinate": published.Coordinate, "version": published.Version, "artifact_digest": published.ArtifactDigest,
	}, params, ioctx, fmt.Sprintf("Published deployment member %s@%s.", published.Coordinate, published.Version))
	return emitDeploymentPublishedMember(ioctx, published)
}

// sameDeploymentMember reports that Distribution published, or packed, exactly
// the selected member: its coordinate, version and the digest of the
// declaration bytes the step read.
func sameDeploymentMember(result *distributioncli.DeploymentMemberPublicationResult, target deploymentTarget, selected releaseset.PlannedMember, digest string) bool {
	return result != nil && result.Coordinate == target.Coordinate && result.Version == selected.Version && result.ArtifactDigest == digest
}

// resolveDeploymentTarget reads the project the step runs for and derives its
// plan ProjectID the way the Config producer does.
func resolveDeploymentTarget(params map[string]any, args []string, workspaceRoot string) (deploymentTarget, error) {
	adoptPositionalApp(params, args)
	app, err := resolveApp(params, workspaceRoot)
	if err != nil {
		return deploymentTarget{}, err
	}
	dir, err := findAppDir(workspaceRoot, app)
	if err != nil {
		return deploymentTarget{}, err
	}
	manifest := filepath.Join(dir, cloudProjectMarker)
	project, found, err := readCloudProject(manifest)
	if err != nil || !found {
		return deploymentTarget{}, newError(fmt.Sprintf("read %s: the project manifest is missing or invalid", manifest), ExitUsage)
	}
	root := workspaceRoot
	if resolved, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return deploymentTarget{}, newError("deployment project "+project.Name+" is outside the workspace", ExitUsage)
	}
	target := deploymentTarget{
		ProjectID: projectIDFromPath(filepath.ToSlash(rel)), Name: project.Name,
		Declaration: filepath.Join(dir, infra.AggregatedManifestDir, infra.DeploymentFilename),
	}
	namespace, declared, err := project.configNamespace()
	if err != nil {
		return deploymentTarget{}, newError(err.Error(), ExitUsage)
	}
	if !declared {
		return target, nil
	}
	if coordinate, err := deploymentMemberCoordinate(namespace, project.Name); err == nil {
		target.Namespace, target.Coordinate = namespace, coordinate
		target.Package = strings.TrimPrefix(coordinate, namespace+"/")
	}
	return target, nil
}

// selectedDeploymentMember finds the plan's one selected deployment member of
// the project: the put member at its deployment coordinate, or a put member
// of the project that the plan classifies as its deployment. Any disagreement
// between the two, another owner, another kind or a repeat is refused, so a
// selected member is never skipped silently.
func selectedDeploymentMember(params map[string]any, plan *releaseset.Plan, target deploymentTarget) (releaseset.PlannedMember, bool, error) {
	if channel, present := params["channel"]; present {
		requested, ok := channel.(string)
		if !ok {
			return releaseset.PlannedMember{}, false, fmt.Errorf("deployment --channel must match the strict release-set plan")
		}
		matched := false
		for _, part := range strings.Split(requested, ",") {
			name := strings.TrimSpace(part)
			if name == "" {
				continue
			}
			if !slices.Contains(plan.Channels, name) {
				return releaseset.PlannedMember{}, false, fmt.Errorf("deployment --channel must match the strict release-set plan")
			}
			matched = true
		}
		if !matched {
			return releaseset.PlannedMember{}, false, fmt.Errorf("deployment --channel must match the strict release-set plan")
		}
	}
	var selected *releaseset.PlannedMember
	for _, member := range plan.SelectedMembers() {
		if member.Ecosystem != cloudDeploymentEcosystem {
			continue
		}
		atCoordinate := target.Coordinate != "" && member.Coordinate == target.Coordinate
		classified := member.Kind == distributionproto.KindDeployment && member.ProjectID == target.ProjectID
		if !atCoordinate && !classified {
			continue
		}
		switch {
		case !atCoordinate:
			return releaseset.PlannedMember{}, false, fmt.Errorf("release-set plan selects %s as the deployment member of %s, whose deployment coordinate is %q",
				member.Coordinate, target.ProjectID, target.Coordinate)
		case member.ProjectID != target.ProjectID:
			return releaseset.PlannedMember{}, false, fmt.Errorf("selected deployment member %s belongs to project %s, not %s", member.Coordinate, member.ProjectID, target.ProjectID)
		case member.Kind != "" && member.Kind != distributionproto.KindDeployment:
			return releaseset.PlannedMember{}, false, fmt.Errorf("release-set plan classifies %s as %q, not %q", member.Coordinate, member.Kind, distributionproto.KindDeployment)
		case selected != nil:
			return releaseset.PlannedMember{}, false, fmt.Errorf("release-set plan selects deployment member %s more than once", member.Coordinate)
		}
		copy := member
		selected = &copy
	}
	if selected == nil {
		return releaseset.PlannedMember{}, false, nil
	}
	return *selected, true, nil
}

// readDeploymentDeclaration returns the bytes the publish step uploads: the
// language package step's artifact, accepted only when it is a strict,
// canonical deployment declaration of this workload (deploymentTarget.workload)
// and a valid Put payload. A selected member with a missing or invalid artifact fails the step.
func readDeploymentDeclaration(target deploymentTarget) ([]byte, error) {
	file, err := os.Open(target.Declaration)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, newError(fmt.Sprintf(
				"release-set plan selects deployment member %s but %s does not exist; the language package step %q wrote no declaration — read its warnings",
				target.Coordinate, target.Declaration, cloudDeploymentPackageStep), ExitUsage)
		}
		return nil, newError(fmt.Sprintf("read deployment declaration %s: %v", target.Declaration, err), ExitUsage)
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, put.MaxManifestBytes+1))
	if err != nil {
		return nil, newError(fmt.Sprintf("read deployment declaration %s: %v", target.Declaration, err), ExitUsage)
	}
	if len(payload) > put.MaxManifestBytes {
		return nil, newError(fmt.Sprintf("deployment declaration %s exceeds %d bytes", target.Declaration, put.MaxManifestBytes), ExitUsage)
	}
	manifest, diagnostics := infra.ParseDeployment(payload)
	if manifest == nil {
		messages := make([]string, 0, len(diagnostics))
		for _, diagnostic := range diagnostics {
			messages = append(messages, diagnostic.String())
		}
		return nil, newError(fmt.Sprintf("deployment declaration %s is invalid: %s", target.Declaration, strings.Join(messages, "; ")), ExitUsage)
	}
	if diagnostics := put.ValidatePayload(payload); len(diagnostics) != 0 {
		return nil, newError(fmt.Sprintf("deployment declaration %s is not a valid Put payload: %s", target.Declaration, diagnostics[0].String()), ExitUsage)
	}
	if workload := target.workload(); manifest.Workload != workload {
		return nil, newError(fmt.Sprintf("deployment declaration %s declares workload %q, not %q", target.Declaration, manifest.Workload, workload), ExitUsage)
	}
	return payload, nil
}

func emitDeploymentPublishedMember(ioctx IO, published *distributioncli.DeploymentMemberPublicationResult) error {
	if published == nil || ioctx.Artifact == nil {
		return nil
	}
	member := &extensionproto.PublishedMember{
		Ecosystem: string(cloudDeploymentEcosystem), Coordinate: published.Coordinate,
		Version: published.Version, ArtifactDigest: published.ArtifactDigest,
	}
	if diagnostics := extensionproto.ValidatePublishedMember(member); len(diagnostics) != 0 {
		return fmt.Errorf("refuse invalid deployment publication result: %s", diagnostics[0].String())
	}
	encoded, err := json.Marshal(member)
	if err != nil {
		return fmt.Errorf("encode deployment publication result: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		return fmt.Errorf("encode deployment publication event data: %w", err)
	}
	return ioctx.Artifact(
		"deployment-"+strings.NewReplacer("/", "-", "@", "").Replace(member.Coordinate),
		member.Coordinate, extensionproto.PublishedMemberEventKind, "", data,
	)
}
