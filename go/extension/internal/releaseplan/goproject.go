// Package releaseplan binds the provider-neutral release-set plan carried by
// the job context to the Go module owned by the current project.
package releaseplan

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	distribution "go.putnami.dev/protocol/distribution"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/releaseset"
)

// GoEcosystem is the id of the ecosystem this extension OWNS: its profile —
// the shape of a module path, the shape and ordering of a version, its native
// `@v/<channel>.info` projection, and the shape of its `registries` entry — is
// declared in putnami.extension.json. This constant is the same id in code: the
// value the probe writes, the publish job reports a member under, and every
// release-set reader compares against.
const GoEcosystem distribution.Ecosystem = "go"

// OCIEcosystem is the id of the container-image ecosystem this extension USES
// and does not own: the extension SDK ships the profile, and this extension
// lists it under `uses` in its manifest. A Go server that also publishes an
// image contributes a member here, published by the SDK's shared Docker
// publisher, so the probe must declare it or the coordinator would reject a
// member no plan expected.
const OCIEcosystem distribution.Ecosystem = "oci"

// GoProject is the exact release-set member bound to the current Go project.
// A nil result means the context carries no release-set plan and the caller
// must preserve the legacy/full/cloudless behavior.
type GoProject struct {
	Plan   *releaseset.Plan
	Member releaseset.PlannedMember
}

// ResolveGoProject validates the context plan, reads the project's canonical
// module coordinate, and requires that coordinate to exist in the full set.
func ResolveGoProject(ctx *pctx.Context) (*GoProject, error) {
	plan, err := releaseset.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, nil
	}

	projectRoot := ctx.Project.FullPath
	if projectRoot == "" {
		projectRoot = filepath.Join(ctx.WorkspaceRoot, ctx.Project.Path)
	}
	data, err := os.ReadFile(filepath.Join(projectRoot, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("read project go.mod for release-set plan: %w", err)
	}
	modulePath := modfile.ModulePath(data)
	if modulePath == "" {
		return nil, fmt.Errorf("project go.mod has no module directive")
	}
	if err := module.CheckPath(modulePath); err != nil {
		return nil, fmt.Errorf("project go.mod has malformed module path %q: %w", modulePath, err)
	}
	member, ok := plan.Member(GoEcosystem, modulePath)
	if !ok {
		return nil, fmt.Errorf("go module %q is absent from release-set plan", modulePath)
	}
	if member.Selected && ctx.Identity != nil && member.ProjectID != ctx.Identity.Project.ID {
		return nil, fmt.Errorf("go release-set member %q belongs to project %q, not %q", modulePath, member.ProjectID, ctx.Identity.Project.ID)
	}
	return &GoProject{Plan: plan, Member: member}, nil
}
