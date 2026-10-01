// Package specgate is core's half of the executable-spec verification gate
// (fixed decision 9): the SDD extension judges what a run is
// expected to prove and emits the executable-criteria projection; this package
// collects what the run observed, joins the two through the pure evaluator in
// protocols/features, and hands the engine the one sanction decision. It
// imports protocol types and the CLI's own result plumbing only — it never
// parses putnami.features.json or specs/*.json.
//
// This file owns the POLICY half: reading the committed, domain-keyed
// options.sdd.verification object. The vocabulary and the one resolver live in
// protocols/features so the extension's renderers and this gate can never
// disagree about the same committed policy; this file only binds that resolver
// to the loaded workspace and classifies its refusals into the CLI's error
// taxonomy.
package specgate

import (
	"fmt"

	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	features "go.putnami.dev/protocol/features"
)

// ValidatePolicy strictly validates every committed options.sdd.verification
// block — the workspace's and every project's — before any job executes. An
// unknown domain, an unknown value, or a wrong type is an invalid-config error
// (exit 2), never a silent fallback: this policy decides whether a run may
// fail, so the only safe reading of an unreadable one is to stop.
//
// Every project is validated, not only the selected ones, because the policy
// is committed configuration: a broken block a narrowed run happens not to
// read is still broken, and letting it ride until the wrong selection finds it
// would make the failure depend on what somebody else ran.
func ValidatePolicy(ws *workspace.Workspace) error {
	if ws == nil {
		return nil
	}
	if ws.Config != nil {
		if _, err := features.DecodeVerificationPolicy("workspace", ws.Config.Options); err != nil {
			return classifyPolicyError(err)
		}
	}
	for _, project := range ws.Projects {
		if project == nil || project.Config == nil {
			continue
		}
		if _, err := features.DecodeVerificationPolicy(projectScope(project), project.Config.Options); err != nil {
			return classifyPolicyError(err)
		}
	}
	return nil
}

// EffectiveMode resolves one spec-hosting project's effective verification
// mode for a domain, with provenance. It delegates to the protocol's single
// resolver; project may be nil for a workspace-rooted subject.
func EffectiveMode(domain features.VerificationDomain, ws *workspace.Workspace, project *workspace.Project) (features.VerificationMode, features.VerificationModeSource, error) {
	var workspaceOptions, projectOptions map[string]map[string]any
	if ws != nil && ws.Config != nil {
		workspaceOptions = ws.Config.Options
	}
	scope := "workspace"
	if project != nil && project.Config != nil {
		projectOptions = project.Config.Options
	}
	projectScopeName := scope
	if project != nil {
		projectScopeName = projectScope(project)
	}
	mode, source, err := features.ResolveVerificationMode(domain, scope, workspaceOptions, projectScopeName, projectOptions)
	if err != nil {
		return "", "", classifyPolicyError(err)
	}
	return mode, source, nil
}

func projectScope(project *workspace.Project) string {
	if project.ID != "" {
		return project.ID
	}
	return project.Name
}

func classifyPolicyError(err error) error {
	return protocolcli.Classify(fmt.Errorf("verification policy: %w", err), protocolcli.ErrInvalidConfig)
}
