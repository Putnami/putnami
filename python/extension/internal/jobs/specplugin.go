package jobs

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/sdk/extension/specreport"
)

// The producer seam of the executable-spec loop for Python: pytest has
// no ambient way to observe test verdicts from outside the process, so the
// adapter ships a pytest plugin (embedded below), materializes it onto
// PYTHONPATH for the run, and activates it with `-p putnami_spectest`. Tests
// bind themselves with the `putnami_proves` marker — inert metadata for plain
// pytest — and the plugin writes one fragment per observed verdict into the
// directory the adapter provisions. The merge into the reserved report
// artifact is the shared extension-sdk/specreport half.

//go:embed putnami_spectest.py
var specPluginSource []byte

// specPluginModule is the module name the plugin is imported under; it must
// match the embedded file's basename.
const specPluginModule = "putnami_spectest"

// specProvision carries the per-run spec-verification scratch state: the
// fragment directory handed to the pytest process and the directory holding
// the materialized plugin module.
type specProvision struct {
	fragmentsDir string
	pluginDir    string
}

// provisionSpecPlugin materializes the embedded pytest plugin into a scratch
// directory, once per extension process. The fragment directory is minted
// separately (per project under batching) so attribution never depends on
// sharing.
func provisionSpecPlugin() (string, func(), error) {
	directory, err := scratch.New("putnami-spec-plugin-")
	if err != nil {
		return "", nil, fmt.Errorf("create spec plugin directory: %w", err)
	}
	cleanup := func() { _ = directory.Remove() }
	if err := os.WriteFile(filepath.Join(directory.Path(), specPluginModule+".py"), specPluginSource, 0o644); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write spec plugin: %w", err)
	}
	return directory.Path(), cleanup, nil
}

// provisionSpecVerification mints one project's fragment directory next to an
// already-materialized plugin directory.
func provisionSpecVerification(pluginDir string) (specProvision, func(), error) {
	fragmentsDir, cleanup, err := specreport.NewFragmentDirectory()
	if err != nil {
		return specProvision{}, nil, err
	}
	return specProvision{fragmentsDir: fragmentsDir, pluginDir: pluginDir}, cleanup, nil
}

// specPytestArgs returns the pytest arguments that activate the plugin.
func (p specProvision) specPytestArgs() []string {
	return []string{"-p", specPluginModule}
}

// decorateSpecEnv adds the fragment directory and puts the plugin module on
// PYTHONPATH, preserving whatever path the job already assembled.
func (p specProvision) decorateSpecEnv(extra map[string]string) {
	extra[spectest.FragmentDirEnv] = p.fragmentsDir
	if existing := extra["PYTHONPATH"]; existing != "" {
		extra["PYTHONPATH"] = p.pluginDir + string(os.PathListSeparator) + existing
	} else {
		extra["PYTHONPATH"] = p.pluginDir
	}
}

// batchOutputPath resolves the per-project captured output directory,
// preferring the explicit OutputPath the orchestrator supplies (protocol/job
// ProjectRef.outputPath) and reconstructing the same location when an older
// orchestrator omits it — the batch runs only for the `test` job.
func batchOutputPath(wsRoot string, proj pctx.ProjectRef) string {
	if proj.OutputPath != "" {
		return proj.OutputPath
	}
	return filepath.Join(wsRoot, ".putnami", "out", proj.Path, "test")
}

// attachBatchSpecVerification merges the fragments one member's suite wrote
// into its own report artifact, keeping per-project attribution exactly as a
// solo run produces it. Reporting problems become warning diagnostics and
// never change the member's test verdict.
func attachBatchSpecVerification(result *pyTestBatchProjectResult, wsRoot, fragmentsDir, projectRoot, outputPath string) {
	fragments, warnings := specreport.ReadFragments(fragmentsDir)
	report, foreign, buildWarnings := specreport.ProjectReport(fragments, projectRoot)
	warnings = append(warnings, buildWarnings...)
	for _, fragment := range foreign {
		warnings = append(warnings, fmt.Sprintf(
			"spec observation (%s, %s, %s) dropped: declaration %s is outside the reporting project",
			fragment.Feature, fragment.Requirement, fragment.Check, fragment.File))
	}
	for _, warning := range warnings {
		result.Diagnostics = append(result.Diagnostics, pyTestBatchDiagnostic{
			Severity: "warning", Description: warning,
		})
	}
	if report == nil {
		return
	}
	destination, err := specreport.EmitReport(nil, report, outputPath)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, pyTestBatchDiagnostic{
			Severity: "warning", Description: err.Error(),
		})
		return
	}
	displayPath := destination
	if relative, err := filepath.Rel(wsRoot, destination); err == nil {
		displayPath = filepath.ToSlash(relative)
	}
	result.Artifacts = append(result.Artifacts, pyTestBatchArtifact{
		ID:   features.VerificationReportArtifactID,
		Name: "Feature Verification Report",
		Kind: "report",
		Path: displayPath,
	})
}
