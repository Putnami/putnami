package infraagg

import (
	"fmt"
	"os"
	"path/filepath"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/robustio"
)

// DeploymentFile is the project-relative, slash-separated path of a workload's
// deployment declaration: <project>/.gen/deployment.json.
const DeploymentFile = infra.AggregatedManifestDir + "/" + infra.DeploymentFilename

// OutcomeWithheld means the workload's aggregate has an error finding, so no
// deployment declaration was written and any declaration left by an earlier
// run was removed.
const OutcomeWithheld Outcome = "withheld"

// DeploymentPath is the workload's deployment declaration location.
func DeploymentPath(workloadRoot string) string {
	return filepath.Join(workloadRoot, filepath.FromSlash(DeploymentFile))
}

// Deployment writes the workload's deployment declaration: the manifest
// Aggregate resolves from the same committed files, in the canonical bytes of
// infra.MarshalDeployment, at DeploymentPath.
//
// It reads only committed files and writes only the declaration. It neither
// writes nor removes the runtime defaults sidecar, which belongs to Aggregate:
// a workload without infra/runtime.json is declared with infra.DefaultRuntime()
// after the compatibility hook, whatever the sidecar holds.
//
// A file at DeploymentPath is always the declaration of the current committed
// files, or absent:
//
//   - A project that is not a workload is skipped and loses a declaration left
//     by an earlier run.
//   - An aggregate with an error finding, such as an unreadable contribution,
//     a same-precedence conflict or an invalid runtime block, is withheld: no
//     declaration is written and an earlier one is removed. Its findings are
//     returned for the caller to report.
//   - A failed write or removal is returned as an error, because it can leave
//     an earlier declaration in place.
func Deployment(ctx *pctx.Context, opts Options) (Result, error) {
	if ctx == nil {
		return Result{Outcome: OutcomeSkipped}, nil
	}
	workloadRoot := projectRoot(ctx.WorkspaceRoot, ctx.Project.Path, ctx.Project.FullPath)
	declarationPath := DeploymentPath(workloadRoot)

	if !IsWorkload(ctx.Project.Type) {
		if err := removeDeclaration(declarationPath); err != nil {
			return Result{Outcome: OutcomeSkipped}, err
		}
		return Result{Outcome: OutcomeSkipped}, nil
	}

	contributions, diags := collectContributions(ctx)
	runtime, _, runtimeDiags := resolveRuntime(workloadRoot, opts)
	diags = append(diags, runtimeDiags...)
	merged, assembleDiags := assemble(ctx.Project.Name, workloadRoot, contributions, runtime)
	diags = append(diags, assembleDiags...)

	withheld := Result{
		Outcome:       OutcomeWithheld,
		ManifestPath:  declarationPath,
		Contributions: len(contributions),
		Diagnostics:   diags,
	}
	if diag.HasErrors(diags) {
		return withheld, removeDeclaration(declarationPath)
	}
	data, marshalDiags := infra.MarshalDeployment(&merged)
	if data == nil {
		withheld.Diagnostics = append(withheld.Diagnostics, marshalDiags...)
		return withheld, removeDeclaration(declarationPath)
	}

	if err := os.MkdirAll(filepath.Dir(declarationPath), 0o755); err != nil {
		return withheld, fmt.Errorf("write deployment declaration %s: %w", declarationPath, err)
	}
	if err := writeFileAtomic(declarationPath, data); err != nil {
		return withheld, fmt.Errorf("write deployment declaration %s: %w", declarationPath, err)
	}
	return Result{
		Outcome:       OutcomeEmitted,
		ManifestPath:  declarationPath,
		Contributions: len(contributions),
		Diagnostics:   diags,
	}, nil
}

// removeDeclaration deletes a deployment declaration left by an earlier run. A
// missing file is not an error.
func removeDeclaration(declarationPath string) error {
	if err := robustio.Remove(declarationPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale deployment declaration %s: %w", declarationPath, err)
	}
	return nil
}
