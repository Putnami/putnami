package treecmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

const workflowFeature = "fixture/workflow"

type workflowUnitProjection struct {
	Outcome           string   `json:"outcome"`
	NextResult        string   `json:"nextResult"`
	Blockers          []string `json:"blockers"`
	NativeDiagnostics []struct {
		Code string `json:"code"`
	} `json:"nativeDiagnostics"`
	NextCycleTasks []struct {
		Kind string `json:"kind"`
		Ref  struct {
			Source string `json:"source"`
			ID     string `json:"id"`
		} `json:"ref"`
		Revision string `json:"revision"`
	} `json:"nextCycleTasks"`
	Requirements []struct {
		State           string `json:"state"`
		ProducerReports []struct {
			Task string `json:"task"`
		} `json:"producerReports"`
		Checks []struct {
			ID string `json:"id"`
		} `json:"checks"`
	} `json:"requirements"`
}

func workflowProjectedUnits(t *testing.T, result verifyResult) []workflowUnitProjection {
	t.Helper()
	var projection struct {
		Units []workflowUnitProjection `json:"units"`
	}
	if err := json.Unmarshal([]byte(result.out), &projection); err != nil {
		t.Fatalf("decode workflow projection: %v", err)
	}
	if len(projection.Units) == 0 {
		t.Fatalf("workflow projection has no units: %s", result.out)
	}
	return projection.Units
}

func newWorkflowFixture(t *testing.T) (*verifyFixture, fxObj) {
	t.Helper()
	f := newVerifyFixture(t)
	manifest := fxObj{"protocolVersion": 2, "namespace": "fixture", "features": fxArr{fxObj{
		"id": workflowFeature, "type": "feature", "name": "Workflow", "outcome": "A reviewed workflow is ready",
		"owner": "fixture", "target": "coded", "requirements": fxArr{fxObj{
			"id": "acceptance", "stage": "coded", "evidenceKinds": fxArr{"attestation"},
			"verification": fxObj{"kind": "acceptance", "checks": fxArr{"first-check", "second-check"}},
		}},
	}}}
	spec := fxObj{"protocolVersion": 1, "feature": workflowFeature, "outcomes": fxArr{"A reviewed workflow is ready"},
		"requirements": fxArr{fxObj{"id": "acceptance", "text": "Both checks pass."}}}
	f.write(".context/putnami.features.json", manifest)
	f.write(".context/specs/workflow.json", spec)
	f.git("add", "-f", ".context/putnami.features.json", ".context/specs/workflow.json")
	f.record["changedFiles"] = fxArr{".context/putnami.features.json", ".context/specs/workflow.json", "app.txt"}
	fxAppend(fxObjAt(f.record, "review"), "coverage",
		fxObj{"path": ".context/putnami.features.json", "status": "reviewed", "reason": "Native feature source."})
	fxAppend(fxObjAt(f.record, "review"), "coverage",
		fxObj{"path": ".context/specs/workflow.json", "status": "reviewed", "reason": "Native spec source."})
	f.write(".context/putnami.json", fxObj{"name": "fixture"})
	f.write(".context/companion/putnami.json", fxObj{"name": "companion"})
	f.write(".context/verification-report.json", fxObj{"protocolVersion": 1, "observations": fxArr{
		fxObj{"feature": workflowFeature, "requirement": "acceptance", "check": "first-check", "status": "passed",
			"provenance": fxObj{"path": "app_test.go", "symbol": "TestFirst"}},
		fxObj{"feature": workflowFeature, "requirement": "acceptance", "check": "second-check", "status": "passed",
			"provenance": fxObj{"path": "app_test.go", "symbol": "TestSecond"}},
	}})
	reportDigest := f.reference(".context/verification-report.json")["sha256"].(string)
	proof := fxObj{"protocolVersion": 1, "sessionId": "synthetic-gate", "generatedAt": fxEnd,
		"groups": fxArr{fxObj{"project": "/.context", "feature": workflowFeature,
			"spec": ".context/specs/workflow.json", "mode": "enforce", "automaticEvaluation": true,
			"blocked": false, "requirements": fxArr{fxObj{"requirement": "acceptance", "state": "verified",
				"checks": fxArr{fxObj{"check": "first-check", "state": "satisfied"},
					fxObj{"check": "second-check", "state": "satisfied"}}}},
			"reports": fxArr{fxObj{"task": "/application:test", "project": "/application",
				"path": ".context/verification-report.json", "digest": "sha256:" + reportDigest}},
			"counts": fxObj{"specRequirements": 1, "executable": 1, "verified": 1, "unmapped": 0,
				"unexecutable": 0, "missing": 0, "stale": 0, "contradicted": 0}}}}
	f.write(".context/sessions/synthetic-gate/session.json", f.session)
	f.write(".context/sessions/synthetic-gate/spec-verification.json", proof)
	mandate := f.note("workflow-mandate", "Ready for review of the current unit only.")
	boundary := f.note("workflow-boundary", "Current unit proves both acceptance checks.")
	stageReason := f.note("stage-nonapplicable", "This settled correction reuses earlier material.")
	stages := fxArr{}
	for _, name := range []string{"intent", "spec", "design", "prototype", "review", "adr", "candidate", "handoff"} {
		stage := fxObj{"unit": "first", "name": name, "state": "not-applicable", "entryCondition": "The bounded unit has an owner and exact inputs.", "inputs": fxArr{mandate},
			"expectedOutput": name + " decision", "exitCriteria": "The bounded unit is inspectable", "reason": stageReason}
		if name == "candidate" {
			stage["state"] = "passed"
			delete(stage, "reason")
			stage["receipt"] = f.note("candidate-stage", "Candidate reviewed.")
		}
		stages = append(stages, stage)
	}
	w := fxObj{
		"version": 1, "outcome": "fixture-outcome", "mandate": mandate,
		"sources": fxArr{fxObj{"feature": workflowFeature,
			"manifest": f.reference(".context/putnami.features.json"), "spec": f.reference(".context/specs/workflow.json")}},
		"requirements": fxArr{fxObj{"feature": workflowFeature, "id": "acceptance", "disposition": "current", "unit": "first"}},
		"units": fxArr{fxObj{"id": "first", "outcome": "Both checks pass", "boundary": boundary, "exclusions": fxArr{},
			"projects": fxArr{fxObj{"id": "/.context", "manifest": f.reference(".context/putnami.json")},
				fxObj{"id": "/.context/companion", "manifest": f.reference(".context/companion/putnami.json")}},
			"requirements": fxArr{workflowFeature + "#acceptance"}, "dependsOn": fxArr{},
			"requiredStage": "ready", "stage": "candidate", "nextResult": "Independent candidate review",
			"nextCycleTasks": fxArr{}, "decisions": fxArr{}}},
		"artifacts": fxArr{}, "experiments": fxArr{},
		"stages": stages,
		"handoff": fxObj{"mandate": mandate, "unit": "first", "scopes": fxArr{"/.context"},
			"receiver": "reviewer", "responsibility": "Inspect the candidate", "inputs": fxArr{mandate},
			"expectedOutput": "A review finding report", "acceptance": "Every current check is considered",
			"nextAction": "Inspect the candidate", "unresolvedFindings": fxArr{}},
		"proof": fxObj{"session": f.reference(".context/sessions/synthetic-gate/session.json"),
			"specVerification": f.reference(".context/sessions/synthetic-gate/spec-verification.json")},
	}
	return f, w
}

func (f *verifyFixture) workflow(w fxObj, final bool, reason string) verifyResult {
	f.t.Helper()
	f.write(".context/workflow.json", w)
	f.resetFingerprint(fxFingerprint{})
	if final {
		f.record["workflow"] = f.reference(".context/workflow.json")
		f.verify(reason)
		return verifyResult{}
	}
	result := f.invoke("--workflow", ".context/workflow.json")
	f.expect(result, reason, "workflow valid")
	return result
}

func (f *verifyFixture) issuedReadyReceipt() fxObj {
	f.t.Helper()
	f.write(".context/dossier.json", f.record)
	f.resetFingerprint(fxFingerprint{})
	issued := f.invoke("--record", ".context/dossier.json")
	f.expect(issued, "", "Putnami verified")
	if issued.value["stage"] != "ready" {
		f.t.Fatalf("native dossier issued no ready verdict: %s", issued.out)
	}
	f.write(".context/ready-verdict.json", issued.out)
	return f.reference(".context/ready-verdict.json")
}

func TestWorkflowNativeChecksAndDossier(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "native-workflow-validation", "workflow-versions-and-references")
	t.Parallel()
	f, w := newWorkflowFixture(t)
	result := f.workflow(w, false, "")
	if result.value["stage"] != "progress" || result.value["outcomeComplete"] != false || result.value["verifiedRequirements"] != float64(1) {
		t.Fatalf("workflow projection = %s", result.out)
	}
	unit := workflowProjectedUnits(t, result)[0]
	if unit.Outcome != "Both checks pass" || unit.NextResult != "Independent candidate review" ||
		len(unit.Blockers) == 0 {
		t.Fatalf("workflow unit projection lost capability or next step: %s", result.out)
	}
	requirement := unit.Requirements[0]
	if requirement.State != "verified" || len(requirement.ProducerReports) != 1 || len(requirement.Checks) != 2 {
		t.Fatalf("workflow projection lost native evidence: %s", result.out)
	}
	f.workflow(w, true, "")
	delete(f.record, "workflow")
	f.verify("") // Existing v2 dossiers still work with no workflow member.
}

func TestWorkflowSourcesMustBeTrackedNativeFiles(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "native-workflow-validation", "workflow-versions-and-references")
	t.Parallel()
	for _, tc := range []struct {
		name, file, reason string
	}{
		{"manifest", ".context/putnami.features.json", "workflow manifest must be tracked by Git"},
		{"spec", ".context/specs/workflow.json", "workflow spec must be tracked by Git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, w := newWorkflowFixture(t)
			f.git("rm", "--cached", "-q", tc.file)
			f.workflow(w, false, tc.reason)
		})
	}
	t.Run("untracked one-check replacement", func(t *testing.T) {
		t.Parallel()
		f, w := newWorkflowFixture(t)
		manifest := f.readJSON(".context/putnami.features.json")
		fxObjAt(manifest, "features", 0, "requirements", 0, "verification")["checks"] = fxArr{"first-check"}
		f.write(".context/putnami.features.json", manifest)
		fxObjAt(w, "sources", 0)["manifest"] = f.reference(".context/putnami.features.json")
		f.git("rm", "--cached", "-f", "-q", ".context/putnami.features.json")
		f.workflow(w, false, "workflow manifest must be tracked by Git")
	})
}

func TestWorkflowRejectsUnsafeEvidenceBeforeReading(t *testing.T) {
	t.Parallel()
	f, w := newWorkflowFixture(t)
	source := fxObjAt(w, "sources", 0)
	original := source["manifest"]
	source["manifest"] = fxObj{"path": "../outside/putnami.features.json", "sha256": strings.Repeat("0", 64)}
	f.workflow(w, false, "unsafe workflow evidence path")
	source["manifest"] = original

	target := filepath.Join(f.root, ".context", "workflow-mandate.md")
	link := filepath.Join(f.root, ".context", "mandate-link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	w["mandate"] = f.reference(".context/mandate-link.md")
	f.workflow(w, false, "workflow evidence path contains symlink")
}

func TestWorkflowRestoredReportRequiresNativeCacheCorroboration(t *testing.T) {
	t.Parallel()
	f, w := newWorkflowFixture(t)
	proof := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
	fxObjAt(proof, "groups", 0, "reports", 0)["restored"] = true
	f.write(".context/sessions/synthetic-gate/spec-verification.json", proof)
	fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
	f.workflow(w, false, "restored report has no native cache verifier")
	f.recoverReport = func(_ string, _ features.VerificationReportRef) ([]byte, error) {
		return []byte("forged restored bytes"), nil
	}
	f.workflow(w, false, "restored report digest changed")
	f.recoverReport = func(_ string, report features.VerificationReportRef) ([]byte, error) {
		if report.Task != "/application:test" || !report.Restored {
			t.Fatalf("native callback received wrong report: %+v", report)
		}
		return os.ReadFile(filepath.Join(f.root, ".context/verification-report.json"))
	}
	f.workflow(w, false, "") // The native cache bridge owns producer corroboration.
	first := true
	f.recoverReport = func(_ string, _ features.VerificationReportRef) ([]byte, error) {
		if first {
			first = false
			return os.ReadFile(filepath.Join(f.root, ".context/verification-report.json"))
		}
		return []byte("cache content changed after admission"), nil
	}
	f.workflow(w, false, "workflow restored report changed during verification")
}

func TestWorkflowRejectsNativeInventoryAndProofFailures(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "native-workflow-validation", "native-requirement-check-coverage")
	t.Parallel()
	cases := []struct {
		name, reason string
		change       func(*verifyFixture, fxObj)
	}{
		{"unknown version", "unsupported local workflow version", func(_ *verifyFixture, w fxObj) { w["version"] = 2 }},
		{"unknown member", "unknown field", func(_ *verifyFixture, w fxObj) { w["complete"] = true }},
		{"task state cannot claim completion", "unknown field", func(_ *verifyFixture, w fxObj) { w["taskState"] = "done" }},
		{"unknown requirement", "unknown workflow requirement", func(_ *verifyFixture, w fxObj) {
			fxObjAt(w, "requirements", 0)["id"] = "invented"
		}},
		{"duplicate requirement", "duplicate workflow requirement", func(_ *verifyFixture, w fxObj) {
			w["requirements"] = append(fxArrAt(w, "requirements"), fxClone(fxObjAt(w, "requirements", 0)))
		}},
		{"omitted requirement", "omits native spec requirements", func(_ *verifyFixture, w fxObj) { w["requirements"] = fxArr{} }},
		{"unknown check", "check inventory differs", func(f *verifyFixture, w fxObj) {
			record := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
			fxObjAt(record, "groups", 0, "requirements", 0, "checks", 1)["check"] = "unknown"
			f.write(".context/sessions/synthetic-gate/spec-verification.json", record)
			fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
		}},
		{"duplicate check", "check inventory differs", func(f *verifyFixture, w fxObj) {
			record := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
			fxObjAt(record, "groups", 0, "requirements", 0, "checks", 1)["check"] = "first-check"
			f.write(".context/sessions/synthetic-gate/spec-verification.json", record)
			fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
		}},
		{"missing check", "check inventory differs", func(f *verifyFixture, w fxObj) {
			record := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
			checks := fxArrAt(record, "groups", 0, "requirements", 0, "checks")
			fxObjAt(record, "groups", 0, "requirements", 0)["checks"] = checks[:1]
			f.write(".context/sessions/synthetic-gate/spec-verification.json", record)
			fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
		}},
		{"failed check", "current unit lacks passing native checks", func(f *verifyFixture, w fxObj) {
			record := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
			report := f.readJSON(".context/verification-report.json")
			fxObjAt(report, "observations", 1)["status"] = "failed"
			f.write(".context/verification-report.json", report)
			fxObjAt(record, "groups", 0, "reports", 0)["digest"] = "sha256:" + f.reference(".context/verification-report.json")["sha256"].(string)
			fxObjAt(record, "groups", 0, "requirements", 0)["state"] = "contradicted"
			fxObjAt(record, "groups", 0, "requirements", 0, "checks", 1)["state"] = "violated"
			fxObjAt(record, "groups", 0)["counts"] = fxObj{"specRequirements": 1, "executable": 1, "verified": 0,
				"unmapped": 0, "unexecutable": 0, "missing": 0, "stale": 0, "contradicted": 1}
			fxObjAt(record, "groups", 0)["blocked"] = true
			f.write(".context/sessions/synthetic-gate/spec-verification.json", record)
			fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
		}},
		{"unknown observed check", "report has unknown check", func(f *verifyFixture, w fxObj) {
			record := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
			report := f.readJSON(".context/verification-report.json")
			report["observations"] = append(fxArrAt(report, "observations"), fxObj{
				"feature": workflowFeature, "requirement": "acceptance", "check": "invented", "status": "passed",
				"provenance": fxObj{"path": "app_test.go", "symbol": "TestInvented"}})
			f.write(".context/verification-report.json", report)
			fxObjAt(record, "groups", 0, "reports", 0)["digest"] = "sha256:" + f.reference(".context/verification-report.json")["sha256"].(string)
			f.write(".context/sessions/synthetic-gate/spec-verification.json", record)
			fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
		}},
		{"duplicate observed check", "invalid native producer report", func(f *verifyFixture, w fxObj) {
			record := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
			report := f.readJSON(".context/verification-report.json")
			report["observations"] = append(fxArrAt(report, "observations"), fxClone(fxObjAt(report, "observations", 0)))
			f.write(".context/verification-report.json", report)
			fxObjAt(record, "groups", 0, "reports", 0)["digest"] = "sha256:" + f.reference(".context/verification-report.json")["sha256"].(string)
			f.write(".context/sessions/synthetic-gate/spec-verification.json", record)
			fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
		}},
		{"skipped observed check", "does not match native producer observations", func(f *verifyFixture, w fxObj) {
			record := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
			report := f.readJSON(".context/verification-report.json")
			fxObjAt(report, "observations", 1)["status"] = "skipped"
			f.write(".context/verification-report.json", report)
			fxObjAt(record, "groups", 0, "reports", 0)["digest"] = "sha256:" + f.reference(".context/verification-report.json")["sha256"].(string)
			f.write(".context/sessions/synthetic-gate/spec-verification.json", record)
			fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
		}},
		{"stale session", "stale workflow proof tree", func(f *verifyFixture, w fxObj) {
			session := f.readJSON(".context/sessions/synthetic-gate/session.json")
			fxObjAt(session, "tree")["fingerprint"] = fxOtherTree
			f.write(".context/sessions/synthetic-gate/session.json", session)
			fxObjAt(w, "proof")["session"] = f.reference(".context/sessions/synthetic-gate/session.json")
		}},
		{"unrelated session", "session sibling artifact", func(f *verifyFixture, w fxObj) {
			f.write(".context/unrelated.json", f.readJSON(".context/sessions/synthetic-gate/spec-verification.json"))
			fxObjAt(w, "proof")["specVerification"] = f.reference(".context/unrelated.json")
		}},
		{"native collection error", "unresolved native collection errors", func(f *verifyFixture, w fxObj) {
			record := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
			record["diagnostics"] = fxArr{fxObj{"severity": "error", "code": "features.report_unreadable",
				"message": "The producer report could not be read.", "field": ".context/verification-report.json"}}
			f.write(".context/sessions/synthetic-gate/spec-verification.json", record)
			fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, w := newWorkflowFixture(t)
			tc.change(f, w)
			f.workflow(w, true, tc.reason)
		})
	}
}

func TestWorkflowLaterScopeAndUnitReceipts(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "native-workflow-validation", "delivery-receipts-and-parent-completeness")
	t.Parallel()
	f, w := newWorkflowFixture(t)
	manifest := f.readJSON(".context/putnami.features.json")
	feature := fxObjAt(manifest, "features", 0)
	feature["requirements"] = append(fxArrAt(feature, "requirements"), fxObj{
		"id": "large-data", "stage": "coded", "evidenceKinds": fxArr{"attestation"},
		"verification": fxObj{"kind": "acceptance", "checks": fxArr{"large-data-export"}},
	})
	f.write(".context/putnami.features.json", manifest)
	spec := f.readJSON(".context/specs/workflow.json")
	spec["requirements"] = append(fxArrAt(spec, "requirements"), fxObj{"id": "large-data", "text": "Large data remains requested."})
	f.write(".context/specs/workflow.json", spec)
	source := fxObjAt(w, "sources", 0)
	source["manifest"], source["spec"] = f.reference(".context/putnami.features.json"), f.reference(".context/specs/workflow.json")
	w["requirements"] = append(fxArrAt(w, "requirements"), fxObj{"feature": workflowFeature, "id": "large-data", "disposition": "later", "unit": "later"})
	w["units"] = append(fxArrAt(w, "units"), fxObj{"id": "later", "outcome": "Full large-data export", "boundary": f.note("large-boundary", "Large data remains in scope"),
		"exclusions": fxArr{}, "projects": fxArr{fxObj{"id": "/.context", "manifest": f.reference(".context/putnami.json")}},
		"requirements": fxArr{workflowFeature + "#large-data"}, "dependsOn": fxArr{"first"},
		"requiredStage": "ready", "stage": "planned", "nextResult": "Bounded large-data prototype",
		"nextCycleTasks": fxArr{fxObj{"kind": "provider", "ref": fxObj{"source": "github:fixture", "id": "2"}}}, "decisions": fxArr{}})
	for _, item := range fxArrAt(w, "stages") {
		original, ok := item.(fxObj)
		if !ok {
			t.Fatal("fixture stage is not an object")
		}
		stage := fxObj{}
		for key, value := range original {
			stage[key] = value
		}
		stage["unit"], stage["state"] = "later", "pending"
		delete(stage, "receipt")
		delete(stage, "reason")
		w["stages"] = append(fxArrAt(w, "stages"), stage)
	}
	record := f.readJSON(".context/sessions/synthetic-gate/spec-verification.json")
	group := fxObjAt(record, "groups", 0)
	group["requirements"] = append(fxArrAt(group, "requirements"), fxObj{"requirement": "large-data", "state": "missing",
		"checks": fxArr{fxObj{"check": "large-data-export", "state": "missing"}}})
	group["counts"] = fxObj{"specRequirements": 2, "executable": 2, "verified": 1,
		"unmapped": 0, "unexecutable": 0, "missing": 1, "stale": 0, "contradicted": 0}
	group["mode"] = "report"
	group["blocked"] = false
	group["diagnostics"] = fxArr{fxObj{"severity": "warning", "code": "features.unobserved_requirement",
		"message": "The large-export check was not observed.", "field": ".context/specs/workflow.json"}}
	f.write(".context/sessions/synthetic-gate/spec-verification.json", record)
	fxObjAt(w, "proof")["specVerification"] = f.reference(".context/sessions/synthetic-gate/spec-verification.json")
	result := f.workflow(w, false, "")
	if result.value["outcomeComplete"] != false || result.value["laterRequirements"] != float64(1) {
		t.Fatalf("later unit was lost: %s", result.out)
	}
	laterStatus := workflowProjectedUnits(t, result)[1]
	if len(laterStatus.NativeDiagnostics) != 1 || laterStatus.NativeDiagnostics[0].Code != "features.unobserved_requirement" ||
		!strings.Contains(strings.Join(laterStatus.Blockers, " "), "features.unobserved_requirement") {
		t.Fatalf("native warning was lost from later unit: %s", result.out)
	}
	w["artifacts"] = fxArr{
		fxObj{"id": "later-design", "kind": "design", "contractVersion": "1", "lifecycle": "proposed", "units": fxArr{"later"},
			"scopes": fxArr{"/.context"}, "source": f.note("later-design", "Later design is not yet reviewed."),
			"context": fxArr{}, "author": "later-author", "material": true},
		fxObj{"id": "replaced-design", "kind": "design", "contractVersion": "1", "lifecycle": "superseded", "units": fxArr{"first"},
			"scopes": fxArr{"/.context"}, "source": f.note("replaced-design", "The current unit replaced this design."),
			"context": fxArr{}, "author": "first-author", "material": true},
	}
	f.workflow(w, true, "") // Ready is for the current unit, not the parent outcome.
	w["artifacts"] = append(fxArrAt(w, "artifacts"), fxObj{"id": "current-design", "kind": "design", "contractVersion": "1",
		"lifecycle": "accepted", "units": fxArr{"first"}, "scopes": fxArr{"/.context"},
		"source": f.note("current-design", "Material design needs independent review."), "context": fxArr{},
		"author": "first-author", "material": true})
	f.workflow(w, true, "material workflow artifact lacks independent challenge")
	w["artifacts"] = fxArrAt(w, "artifacts")[:2]
	fxObjAt(w, "units", 1)["dependsOn"] = fxArr{}
	fxObjAt(w, "units", 0)["dependsOn"] = fxArr{"later"}
	f.workflow(w, true, "workflow current unit has incomplete dependency")
	fxObjAt(w, "units", 0)["dependsOn"] = fxArr{}
	fxObjAt(w, "units", 1)["dependsOn"] = fxArr{"first"}
	fxObjAt(w, "requirements", 1)["disposition"] = "excluded"
	delete(fxObjAt(w, "requirements", 1), "unit")
	f.workflow(w, false, "missing evidence reference")
	w["requirements"] = fxArr{fxObjAt(w, "requirements", 0), fxObj{
		"feature": workflowFeature, "id": "large-data", "disposition": "excluded",
		"reason": f.note("exclusion-reason", "A proposed size limit."), "authority": f.note("exclusion-authority", "Actual scope authority approved exclusion.")}}
	fxObjAt(w, "units", 1)["requirements"] = fxArr{}
	f.workflow(w, false, "workflow unit has no requirements")
}

func TestWorkflowArtifactAndHandoffBindings(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "native-workflow-validation", "relevant-artifact-invalidation")
	t.Parallel()
	f, w := newWorkflowFixture(t)
	source := f.note("design-v1", "Chosen interaction design.")
	context := f.note("design-context", "Relevant constraints.")
	challengeReport := f.note("design-challenge", "Substantive independent challenge.")
	f.write(".context/original-design-challenge.json", fxObj{"version": 1, "reviewer": "reviewer-session", "author": "author-session",
		"artifact": source, "context": fxArr{context}, "report": challengeReport,
		"findings": fxArr{fxObj{"id": "F1", "scenario": "State can be lost",
			"consequence": "The handoff cannot resume", "correction": "Bind exact input refs"}}})
	w["artifacts"] = fxArr{fxObj{"id": "design", "kind": "design", "contractVersion": "1", "lifecycle": "accepted",
		"units": fxArr{"first"}, "scopes": fxArr{"/.context"}, "source": source, "context": fxArr{context},
		"author": "author-session", "material": true,
		"challenge": fxObj{"original": f.reference(".context/original-design-challenge.json"),
			"resolutions": fxArr{fxObj{"id": "F1", "state": "refuted",
				"note": f.note("finding-resolution", "Counterevidence for the original concern.")}}}}}
	w["experiments"] = fxArr{fxObj{"id": "prototype", "question": "Does the interaction clarify a choice?",
		"effortCeiling": "One day", "inputs": fxArr{context}, "asset": f.note("prototype-asset", "Inspectable mockup"),
		"instructions": f.note("prototype-instructions", "Open asset and inspect"), "observed": f.note("prototype-result", "The flow is understandable"),
		"simulated": fxArr{"Large-data throughput"}, "assumptions": fxArr{"Customer bandwidth"},
		"conclusion": "Continue with a bounded implementation", "uncertainty": "Production throughput remains unknown"}}
	f.workflow(w, true, "")
	f.note("unrelated", "Unrelated edit does not change the examined design.")
	f.workflow(w, true, "")
	fxObjAt(w, "artifacts", 0)["source"] = f.note("design-v2", "A changed design that needs a new challenge.")
	f.workflow(w, false, "stale workflow artifact challenge")
	fxObjAt(w, "artifacts", 0)["source"] = source
	fxObjAt(w, "artifacts", 0)["context"] = fxArr{f.note("design-context-v2", "Revised relevant constraint.")}
	f.workflow(w, false, "stale workflow artifact context challenge")
	fxObjAt(w, "artifacts", 0)["context"] = fxArr{context}
	f.write(".context/design-v1.md", "Changed design.")
	f.workflow(w, false, "evidence digest changed")
	w["artifacts"] = fxArr{}
	f.remove(".context/workflow-mandate.md")
	f.workflow(w, false, "missing or empty evidence file")
}

func TestWorkflowPendingCannotMintReady(t *testing.T) {
	t.Parallel()
	f, w := newWorkflowFixture(t)
	delete(w, "proof")
	fxObjAt(w, "units", 0)["stage"] = "building"
	result := f.workflow(w, false, "")
	if result.value["verifiedRequirements"] != float64(0) || result.value["outcomeComplete"] != false {
		t.Fatalf("pending proof advanced outcome: %s", result.out)
	}
	f.workflow(w, true, "current unit lacks passing native checks")
	if slices.Contains([]string{"ready", "Putnami verified"}, result.value["stage"].(string)) {
		t.Fatalf("standalone workflow minted candidate readiness: %s", result.out)
	}
}

func TestWorkflowDeliveryRequiresExactNativeReceipt(t *testing.T) {
	t.Parallel()
	f, w := newWorkflowFixture(t)
	unit := fxObjAt(w, "units", 0)
	unit["requiredStage"], unit["stage"] = "merged", "merged"
	unit["proposal"] = fxObj{"source": "github:fixture/repo", "id": "7"}
	f.write(".context/merged-status.json", fxObj{"contract": "proposals", "version": 1, "operation": "status", "outcome": "ok", "result": fxObj{"proposal": fxObj{
		"ref":      fxObj{"source": "github:fixture/repo", "id": "7"},
		"revision": "r2", "change": fxObj{"repository": "fixture/repo", "base": "main", "head": "feature", "headCommit": f.head},
		"title": "feat: workflow", "state": "merged"}, "checks": fxObj{"state": "passing"}}})
	unit["receipt"] = f.reference(".context/merged-status.json")
	result := f.workflow(w, false, "")
	if result.value["outcomeComplete"] != true {
		t.Fatalf("merged native receipt was not recognized: %s", result.out)
	}
	status := f.readJSON(".context/merged-status.json")
	fxObjAt(status, "result", "proposal", "change")["headCommit"] = strings.Repeat("b", 40)
	f.write(".context/merged-status.json", status)
	unit["receipt"] = f.reference(".context/merged-status.json")
	f.workflow(w, false, "workflow merged receipt names another head")
	fxObjAt(status, "result", "proposal", "change")["headCommit"] = f.head
	fxObjAt(status, "result", "proposal")["state"] = "open"
	f.write(".context/merged-status.json", status)
	unit["receipt"] = f.reference(".context/merged-status.json")
	f.workflow(w, false, "does not show merged state")
	status["operation"] = "get"
	f.write(".context/merged-status.json", status)
	unit["receipt"] = f.reference(".context/merged-status.json")
	f.workflow(w, false, "invalid native collaboration envelope")
	status["operation"] = "status"
	status["outcome"] = "not_found"
	delete(status, "result")
	status["error"] = fxObj{"message": "missing proposal", "retryable": false}
	f.write(".context/merged-status.json", status)
	unit["receipt"] = f.reference(".context/merged-status.json")
	f.workflow(w, false, "not a successful native proposals.status result")
	unit["requiredStage"], unit["stage"] = "adopted", "adopted"
	unit["receipt"] = f.note("opaque-adoption", "An unsupported claim")
	result = f.workflow(w, false, "")
	if result.value["outcomeComplete"] != false {
		t.Fatalf("opaque adoption was treated as complete: %s", result.out)
	}
	unitStatus := workflowProjectedUnits(t, result)[0]
	if !strings.Contains(unitStatus.Blockers[0], "adapter unavailable") {
		t.Fatalf("missing unsupported receipt diagnostic: %s", result.out)
	}
}

func TestWorkflowDeliveryAcceptsProvenLaterStage(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "native-workflow-validation", "delivery-receipts-and-parent-completeness")
	t.Parallel()
	f, w := newWorkflowFixture(t)
	unit := fxObjAt(w, "units", 0)
	unit["requiredStage"], unit["stage"] = "candidate", "ready"
	unit["receipt"] = f.issuedReadyReceipt()
	result := f.workflow(w, false, "")
	if result.value["outcomeComplete"] != true {
		t.Fatalf("native ready receipt failed to cover candidate: %s", result.out)
	}
	f.workflow(w, true, "")

	unit["requiredStage"], unit["stage"] = "ready", "merged"
	unit["proposal"] = fxObj{"source": "github:fixture/repo", "id": "7"}
	f.write(".context/merged-beyond-ready.json", fxObj{"contract": "proposals", "version": 1, "operation": "status", "outcome": "ok", "result": fxObj{"proposal": fxObj{
		"ref": fxObj{"source": "github:fixture/repo", "id": "7"}, "revision": "r2",
		"change": fxObj{"repository": "fixture/repo", "base": "main", "head": "feature", "headCommit": f.head},
		"title":  "feat: workflow", "state": "merged"}, "checks": fxObj{"state": "passing"}}})
	unit["receipt"] = f.reference(".context/merged-beyond-ready.json")
	result = f.workflow(w, false, "")
	if result.value["outcomeComplete"] != true {
		t.Fatalf("native merged receipt failed to cover ready: %s", result.out)
	}
	f.workflow(w, true, "")
	status := f.readJSON(".context/merged-beyond-ready.json")
	fxObjAt(status, "result", "proposal")["state"] = "open"
	f.write(".context/merged-beyond-ready.json", status)
	unit["receipt"] = f.reference(".context/merged-beyond-ready.json")
	f.workflow(w, false, "does not show merged state")
}

func TestWorkflowReadyReceiptResumesNativeDossier(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "native-workflow-validation", "delivery-receipts-and-parent-completeness")
	t.Parallel()
	f, w := newWorkflowFixture(t)
	unit := fxObjAt(w, "units", 0)
	unit["stage"] = "ready"
	f.workflow(w, false, "missing evidence reference") // A declared stage cannot issue its own receipt.
	unit["receipt"] = f.issuedReadyReceipt()
	result := f.workflow(w, false, "")
	if result.value["stage"] != "progress" || result.value["outcomeComplete"] != true {
		t.Fatalf("native ready receipt did not resume progress: %s", result.out)
	}
	f.workflow(w, true, "")

	delete(w, "proof")
	f.workflow(w, true, "workflow current unit lacks passing native checks")
	w["proof"] = fxObj{"session": f.reference(".context/sessions/synthetic-gate/session.json"),
		"specVerification": f.reference(".context/sessions/synthetic-gate/spec-verification.json")}
	f.write(".context/blocked-ready.md", "A current decision remains open.")
	unit["decisions"] = fxArr{f.reference(".context/blocked-ready.md")}
	f.workflow(w, true, "workflow current unit has unresolved decisions")
}

func TestWorkflowReadyReceiptRejectsBareAndStaleClaims(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		change       func(*verifyFixture, fxObj)
	}{
		{"bare verdict", "workflow ready receipt missing assurance", func(f *verifyFixture, _ fxObj) {
			f.write(".context/ready-verdict.json", fxObj{"verdict": "Putnami verified", "stage": "ready"})
		}},
		{"wrong verdict", "workflow ready receipt is not a native ready verdict", func(f *verifyFixture, receipt fxObj) {
			receipt["verdict"] = "claimed ready"
			f.write(".context/ready-verdict.json", receipt)
		}},
		{"stale tree", "stale workflow ready receipt tree", func(f *verifyFixture, receipt fxObj) {
			fxObjAt(receipt, "binding")["fingerprint"] = fxOtherTree
			f.write(".context/ready-verdict.json", receipt)
		}},
		{"another head", "stale workflow ready receipt tree", func(f *verifyFixture, receipt fxObj) {
			fxObjAt(receipt, "binding")["headSHA"] = strings.Repeat("a", 40)
			f.write(".context/ready-verdict.json", receipt)
		}},
		{"invalid base", "workflow ready receipt has invalid base", func(f *verifyFixture, receipt fxObj) {
			fxObjAt(receipt, "binding")["baseSHA"] = strings.Repeat("a", 40)
			f.write(".context/ready-verdict.json", receipt)
		}},
		{"malformed document", "JSON Parse error", func(f *verifyFixture, _ fxObj) {
			f.write(".context/ready-verdict.json", "{invalid")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, w := newWorkflowFixture(t)
			unit := fxObjAt(w, "units", 0)
			unit["stage"] = "ready"
			unit["receipt"] = f.issuedReadyReceipt()
			receipt := f.readJSON(".context/ready-verdict.json")
			tc.change(f, receipt)
			unit["receipt"] = f.reference(".context/ready-verdict.json")
			f.workflow(w, false, tc.reason)
		})
	}
}

func TestWorkflowRejectsUnknownNativeProjectAndMissingHandoff(t *testing.T) {
	spectest.Proves(t, "cli/contributor-workflows", "native-workflow-validation", "experiment-and-handoff-inputs")
	t.Parallel()
	f, w := newWorkflowFixture(t)
	fxObjAt(w, "units", 0, "projects", 0)["id"] = "/fabricated"
	f.workflow(w, false, "unknown or duplicate workflow project target")
	fxObjAt(w, "units", 0, "projects", 0)["id"] = "/.context"
	fxObjAt(w, "handoff")["inputs"] = fxArr{fxObj{"path": ".context/absent.md", "sha256": strings.Repeat("0", 64)}}
	result := f.workflow(w, false, "missing or empty evidence file")
	reason, ok := result.value["reason"].(string)
	if !ok || !strings.Contains(reason, ".context/absent.md") {
		t.Fatalf("missing handoff input did not name its path: %s", result.out)
	}
}

func TestWorkflowCandidateStageCannotBeSkipped(t *testing.T) {
	t.Parallel()
	f, w := newWorkflowFixture(t)
	candidate := fxObjAt(w, "stages", 6)
	candidate["state"] = "not-applicable"
	delete(candidate, "receipt")
	candidate["reason"] = f.note("candidate-waiver", "No actual candidate proof.")
	f.workflow(w, true, "workflow candidate stage incomplete")
}

func TestWorkflowStageEntryConditionIsRequired(t *testing.T) {
	t.Parallel()
	f, w := newWorkflowFixture(t)
	stage := fxObjAt(w, "stages", 0)
	delete(stage, "entryCondition")
	f.workflow(w, false, "workflow stage missing entryCondition")
	stage["entryCondition"] = ""
	f.workflow(w, false, "workflow stage entry condition must be nonempty text")
}

func TestWorkflowUnknownStageDiagnosticIsDeterministic(t *testing.T) {
	t.Parallel()
	f, w := newWorkflowFixture(t)
	original := fxArrAt(w, "stages")
	for _, unit := range []string{"zzz", "aaa"} {
		for _, item := range original {
			stage, ok := item.(fxObj)
			if !ok {
				t.Fatal("stage fixture is not an object")
			}
			copy := fxObj{}
			for key, value := range stage {
				copy[key] = value
			}
			copy["unit"] = unit
			w["stages"] = append(fxArrAt(w, "stages"), copy)
		}
	}
	for range 20 {
		result := f.workflow(w, false, "workflow stage names unknown unit aaa")
		if got := result.value["reason"]; got != "workflow stage names unknown unit aaa" {
			t.Fatalf("nondeterministic unknown-stage diagnostic: %v", got)
		}
	}
}

func TestWorkflowReadyRejectsBlockedStagesAndDecisions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reason string
		change       func(*verifyFixture, fxObj)
	}{
		{"blocked design", "workflow design stage incomplete", func(f *verifyFixture, w fxObj) {
			stage := fxObjAt(w, "stages", 2)
			stage["state"] = "blocked"
			stage["reason"] = f.note("blocked-design", "A design decision is reserved.")
		}},
		{"pending review", "workflow review stage incomplete", func(_ *verifyFixture, w fxObj) {
			stage := fxObjAt(w, "stages", 4)
			stage["state"] = "pending"
			delete(stage, "reason")
		}},
		{"unresolved decision", "workflow current unit has unresolved decisions", func(f *verifyFixture, w fxObj) {
			fxObjAt(w, "units", 0)["decisions"] = fxArr{f.note("reserved-decision", "Still awaiting an owner decision.")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, w := newWorkflowFixture(t)
			tc.change(f, w)
			f.workflow(w, true, tc.reason)
		})
	}
}

func TestWorkflowRetainsOriginalChallengeAcrossSamePathRepair(t *testing.T) {
	t.Parallel()
	f, w := newWorkflowFixture(t)
	originalSource := f.note("repaired-design", "The first design loses handoff state.")
	context := f.note("repair-context", "A durable handoff is required.")
	f.write(".context/repair-original-review.json", fxObj{"version": 1, "reviewer": "first-reviewer", "author": "author",
		"artifact": originalSource, "context": fxArr{context}, "report": f.note("repair-first-report", "F1: handoff state can be lost."),
		"findings": fxArr{fxObj{"id": "F1", "scenario": "Host resumes after transfer",
			"consequence": "The state is lost", "correction": "Carry the exact input snapshot"}}})
	artifact := fxObj{"id": "design", "kind": "design", "contractVersion": "1", "lifecycle": "accepted",
		"units": fxArr{"first"}, "scopes": fxArr{"/.context"}, "source": originalSource, "context": fxArr{context},
		"author": "author", "material": true,
		"challenge": fxObj{"original": f.reference(".context/repair-original-review.json"),
			"resolutions": fxArr{fxObj{"id": "F1", "state": "refuted", "note": f.note("repair-initial-refutation", "Initial counterevidence.")}}}}
	w["artifacts"] = fxArr{artifact}
	f.workflow(w, true, "")
	f.write(".context/repaired-design-original-snapshot.md", "The first design loses handoff state.")
	snapshot := f.reference(".context/repaired-design-original-snapshot.md")
	f.write(".context/repaired-design.md", "The repaired design carries the exact handoff input.")
	currentSource := f.reference(".context/repaired-design.md")
	artifact["source"] = currentSource
	f.workflow(w, false, "evidence digest changed") // No index edit can renew the original review.
	f.write(".context/repair-current-review.json", fxObj{"version": 1, "reviewer": "replacement-reviewer", "author": "author",
		"artifact": currentSource, "context": fxArr{context}, "report": f.note("repair-current-report", "The corrected handoff retains its state."),
		"findings": fxArr{}})
	challenge, ok := artifact["challenge"].(fxObj)
	if !ok {
		t.Fatal("fixture challenge is not an object")
	}
	challenge["snapshots"] = fxArr{fxObj{"original": originalSource, "copy": snapshot}}
	challenge["revisions"] = fxArr{f.reference(".context/repair-current-review.json")}
	challenge["resolutions"] = fxArr{fxObj{"id": "F1", "state": "fixed", "note": f.note("repair-resolution", "Replacement reviewer inspected the correction.")}}
	f.workflow(w, true, "")
	delete(challenge, "revisions")
	f.workflow(w, true, "evidence digest changed")
}

func TestWorkflowNativeTaskRefsRoundTrip(t *testing.T) {
	t.Parallel()
	f, w := newWorkflowFixture(t)
	input := f.note("task-input", "An exact handoff input.")
	f.write(".context/local-task.json", fxObj{"version": 1, "id": "local:follow-up", "inputs": fxArr{input}, "nextResult": "Inspect the repaired handoff"})
	providerRef := fxObj{"source": "github:one/repo", "id": "issue#2/with:punctuation"}
	f.write(".context/task-get.json", fxObj{"contract": "tasks", "version": 1, "operation": "get", "outcome": "ok",
		"result": fxObj{"task": fxObj{"ref": providerRef, "revision": "v3", "title": "Review next unit", "state": "open"}}})
	fxObjAt(w, "units", 0)["nextCycleTasks"] = fxArr{
		fxObj{"kind": "provider", "ref": fxObj{"source": "github:one/repo", "id": "same-id"}},
		fxObj{"kind": "provider", "ref": fxObj{"source": "github:two/repo", "id": "same-id"}},
		fxObj{"kind": "provider", "ref": providerRef, "revision": "v3", "receipt": f.reference(".context/task-get.json")},
		fxObj{"kind": "local", "record": f.reference(".context/local-task.json")},
	}
	fxObjAt(w, "handoff")["inputs"] = fxArr{input, f.reference(".context/task-get.json"), f.reference(".context/local-task.json")}
	result := f.workflow(w, false, "")
	tasks := workflowProjectedUnits(t, result)[0].NextCycleTasks
	if len(tasks) != 4 || tasks[0].Ref.Source == tasks[1].Ref.Source || tasks[2].Ref.ID != "issue#2/with:punctuation" || tasks[2].Revision != "v3" {
		t.Fatalf("native task identity lost in projection: %s", result.out)
	}
	f.workflow(w, true, "")
	fxObjAt(w, "units", 0, "nextCycleTasks", 2)["revision"] = "v4"
	f.workflow(w, false, "workflow task receipt does not match native ref and revision")
}
