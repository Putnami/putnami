package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/evidence"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	runtimeproto "go.putnami.dev/protocol/runtime"
	pctx "go.putnami.dev/sdk/extension/context"
)

// The features step of `validate-workspace` at the boundary the gate crosses:
// a workspace job context in; a result, a JSONL stream and the evidence
// fragment file out. The step states findings and never decides, so its
// whole verdict on the wire is the file: no diagnostic, and success whatever
// the findings say (ADR 0006 of this extension, ADR 0134 of the CLI).

// sddMember is ref as a member that names this extension, which the
// per-project command's activation requires.
func sddMember(ref pctx.ProjectRef) pctx.ProjectRef {
	ref.Extensions = []string{extensionName}
	return ref
}

// featuresJobContext is the context the features step receives: the
// workspace-once job's context with the step's command output and, since the
// task declares selection none, no selection.
func featuresJobContext(root string, members ...pctx.ProjectRef) *pctx.Context {
	ctx := workspaceJobContext(root, members)
	ctx.OutputPath = filepath.Join(root, ".putnami", "out", "~validate-workspace")
	ctx.Selection = nil
	ctx.SelectedProjects = nil
	return ctx
}

// writtenFragments reads the fragment file the step wrote, with the strict
// reader the CLI's verdict uses.
func writtenFragments(t *testing.T, ctx *pctx.Context) ([]byte, []evidence.Fragment) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ctx.OutputPath, evidence.FragmentFilename))
	if err != nil {
		t.Fatalf("read the declared fragment file: %v", err)
	}
	fragments, diagnostics := evidence.ParseFragments(data)
	if diagnostics != nil {
		t.Fatalf("the fragment file fails the strict reader: %v\n%s", diagnostics, data)
	}
	return data, fragments
}

// assertStatesAndDoesNotDecide holds the step's half of the contract: it
// succeeds, publishes no diagnostic and announces the fragment file it wrote.
func assertStatesAndDoesNotDecide(t *testing.T, outcome jobOutcome, ctx *pctx.Context) {
	t.Helper()
	if outcome.err != nil || outcome.status != "OK" || outcome.exitCode() != protocolcli.ExitSuccess {
		t.Fatalf("status = %q, err = %v, exit = %d; want the step to succeed whatever the findings say",
			outcome.status, outcome.err, outcome.exitCode())
	}
	if published := outcome.diagnostics(); len(published) != 0 {
		t.Errorf("the step published %d diagnostic(s); findings reach the user only through the fragments:\n%s", len(published), outcome.stream)
	}
	var announced *runtimeproto.Event
	for i, event := range outcome.events {
		if event.Type == runtimeproto.EventArtifact && event.ID == evidence.FragmentArtifactID {
			announced = &outcome.events[i]
		}
	}
	if announced == nil || announced.Path != filepath.Join(ctx.OutputPath, evidence.FragmentFilename) {
		t.Errorf("artifact event = %+v, want %s at the absolute destination", announced, evidence.FragmentArtifactID)
	}
	if _, ok := outcome.data["diagnostics"]; ok {
		t.Errorf("the result data carries diagnostics: %#v", outcome.data)
	}
}

// TestTheFeaturesStepSucceedsOnErrorFindings is the seeded violation: a
// relation to a feature no manifest mints. A per-project task failed on it.
// The step states it as an error finding at the file that declares the
// relation and succeeds, so only the CLI's verdict over the selection fails.
func TestTheFeaturesStepSucceedsOnErrorFindings(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"the-features-step-succeeds-on-error-findings")
	root := validationFixture(t, validInvoicingSpec)
	writeFixture(t, filepath.Join(root, "billing", featureproto.ManifestFilename),
		billingManifest(`{"kind": "parent", "target": "platform/nonexistent"}`))
	ctx := featuresJobContext(root, sddMember(billingMember(root)), sddMember(platformMember(root)))

	outcome := runJob(t, runFeaturesValidate, ctx)
	assertStatesAndDoesNotDecide(t, outcome, ctx)
	_, fragments := writtenFragments(t, ctx)
	var dangling *evidence.Fragment
	for i, fragment := range fragments {
		if fragment.Rule == featureproto.ErrorCodeDanglingRelation {
			dangling = &fragments[i]
		}
	}
	if dangling == nil || dangling.Severity != evidence.FindingError ||
		dangling.Subject.Path != "billing/"+featureproto.ManifestFilename || dangling.Source.Format != "putnami-sdd" {
		t.Fatalf("fragments = %+v, want the dangling relation as an error finding at billing's manifest", fragments)
	}
	findings, ok := outcome.data["findings"].(map[string]any)
	if !ok || findings["errors"] != float64(1) {
		t.Errorf("result findings = %#v, want one error counted", outcome.data["findings"])
	}
	if !strings.Contains(outcome.summaryLine(), "1 error and") {
		t.Errorf("summary line = %q, want the error counted", outcome.summaryLine())
	}
}

// TestTheFeaturesStepWritesAnEmptyFileWhenClean pins the empty file: the step
// writes it on every run, so the CLI never reads an earlier run's file.
func TestTheFeaturesStepWritesAnEmptyFileWhenClean(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"the-features-step-succeeds-on-error-findings")
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "billing", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{"id": "billing/invoicing", "type": "feature", "name": "Invoicing", "outcome": "Invoices", "owner": "billing", "target": "modeled"}]
}`)
	ctx := featuresJobContext(root, sddMember(billingMember(root)))
	// A stale file from an earlier run is replaced.
	writeFixture(t, filepath.Join(ctx.OutputPath, evidence.FragmentFilename), "stale\n")

	outcome := runJob(t, runFeaturesValidate, ctx)
	assertStatesAndDoesNotDecide(t, outcome, ctx)
	data, fragments := writtenFragments(t, ctx)
	if len(data) != 0 || len(fragments) != 0 {
		t.Fatalf("fragment file = %q, want an empty file", data)
	}
	if projects, _ := outcome.data["projects"].([]any); len(projects) != 1 || projects[0] != "/billing" {
		t.Errorf("result projects = %#v, want the validated project", outcome.data["projects"])
	}
}

// TestTheFeaturesStepReadsNoSelection pins that the step writes the same file
// and the same result whatever the run selected: it always validates every
// project, and the CLI applies the selection afterwards. The CLI hands it no
// selection; a context that carries one changes nothing.
func TestTheFeaturesStepReadsNoSelection(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"the-features-step-succeeds-on-error-findings")
	root := validationFixture(t, validInvoicingSpec)
	writeFixture(t, filepath.Join(root, "billing", featureproto.ManifestFilename),
		billingManifest(`{"kind": "parent", "target": "platform/nonexistent"}`))
	members := []pctx.ProjectRef{sddMember(billingMember(root)), sddMember(platformMember(root))}

	unscoped := featuresJobContext(root, members...)
	first := runJob(t, runFeaturesValidate, unscoped)
	firstBytes, _ := writtenFragments(t, unscoped)

	narrowed := featuresJobContext(root, members...)
	narrowed.SelectedProjects = members[1:]
	narrowed.Selection = &pctx.Selection{Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/platform"}}
	second := runJob(t, runFeaturesValidate, narrowed)
	secondBytes, _ := writtenFragments(t, narrowed)

	if string(firstBytes) != string(secondBytes) || !reflect.DeepEqual(first.data, second.data) {
		t.Fatalf("the step answered differently under another selection:\n%s\n---\n%s", firstBytes, secondBytes)
	}
	if !strings.Contains(string(firstBytes), "billing/"+featureproto.ManifestFilename) {
		t.Fatalf("a run that selected /platform did not state billing's finding:\n%s", firstBytes)
	}
}

// TestTheFeaturesStepRefusesAContextWithoutMembership pins the guard a
// workspace-once step needs: without the membership the wire would hand it
// the selection, and its answer would depend on it.
func TestTheFeaturesStepRefusesAContextWithoutMembership(t *testing.T) {
	root := validationFixture(t, validInvoicingSpec)
	ctx := featuresJobContext(root, sddMember(billingMember(root)))
	ctx.WorkspaceProjects = nil
	outcome := runJob(t, runFeaturesValidate, ctx)
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), "workspaceProjects") {
		t.Fatalf("err = %v, want a refusal naming the missing membership", outcome.err)
	}
	if _, err := os.Stat(filepath.Join(ctx.OutputPath, evidence.FragmentFilename)); err == nil {
		t.Fatal("a refused run wrote a fragment file")
	}
}

// TestExtensionReferencesNameTheExtensionAsAProjectDoes pins the references a
// project's resolved extensions name this extension by.
func TestExtensionReferencesNameTheExtensionAsAProjectDoes(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name      string
		extension job.Extension
		want      []string
	}{
		{"loaded from the workspace", job.Extension{Name: extensionName, Root: filepath.Join(root, "tooling", "sdd-extension")},
			[]string{extensionName, "/tooling/sdd-extension"}},
		{"installed outside it", job.Extension{Name: extensionName, Root: filepath.Join(filepath.Dir(root), "installed")},
			[]string{extensionName}},
		{"named by its path", job.Extension{Name: "/tooling/sdd-extension"},
			[]string{extensionName, "/tooling/sdd-extension"}},
		{"at the workspace root", job.Extension{Name: extensionName, Root: root},
			[]string{extensionName}},
	} {
		got := extensionReferences(&pctx.Context{WorkspaceRoot: root, Extension: tc.extension})
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: references = %v, want %v", tc.name, got, tc.want)
		}
	}
}
