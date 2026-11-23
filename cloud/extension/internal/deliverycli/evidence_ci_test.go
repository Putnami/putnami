package deliverycli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

// evidenceCIMembers are the members of the run that name its CI run.
var evidenceCIMembers = []string{"ciSystem", "runId", "runAttempt", "runUrl"}

// ciEvidenceDoc writes fixture with ci in place of its own.
func ciEvidenceDoc(t *testing.T, fixture string, ci map[string]any) string {
	t.Helper()
	data, err := os.ReadFile(evidenceFixture(fixture))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["ci"] = ci
	if subject := doc["subject"].(map[string]any); subject["projectId"] == nil {
		subject["projectId"] = "4521"
	}
	return writeEvidenceDoc(t, doc)
}

// wantCIRun fails unless run carries exactly the CI members want names.
func wantCIRun(t *testing.T, run map[string]any, want map[string]any) {
	t.Helper()
	for _, member := range evidenceCIMembers {
		got, present := run[member]
		expected, wanted := want[member]
		switch {
		case wanted && got != expected:
			t.Errorf("%s = %v, want %v", member, got, expected)
		case !wanted && present:
			t.Errorf("%s = %v, want none", member, got)
		}
	}
}

func TestEvidenceReportNamesTheGitHubActionsRun(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-ci-run", "a-github-actions-run-names-its-ci")
	fake := &evidenceFakeServer{}
	if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("turbo-pull-request.json")); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	wantCIRun(t, fake.requests[0].Body, map[string]any{
		"ciSystem":   "github-actions",
		"runId":      "9876543210",
		"runAttempt": float64(1),
		"runUrl":     "https://github.com/acme/storefront/actions/runs/9876543210",
	})
}

func TestEvidenceReportNamesTheGitLabPipeline(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-ci-run", "a-gitlab-ci-run-names-its-ci")
	fake := &evidenceFakeServer{}
	if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", evidenceFixture("symfony-merge-request.json")); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	// A GitLab pipeline names no attempt: Delivery reads attempt 1.
	wantCIRun(t, fake.requests[0].Body, map[string]any{
		"ciSystem": "gitlab-ci",
		"runId":    "123456",
		"runUrl":   "https://gitlab.example.com/shop/backoffice/-/pipelines/123456",
	})
}

func TestEvidenceReportNamesTheCIAGenericDocumentRunsIn(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-ci-run", "a-generic-document-in-a-ci-names-the-ci-it-detects")
	for name, test := range map[string]struct {
		system string
		vars   map[string]string
		want   string
	}{
		"a CI marker":         {evidence.CISystemGeneric, map[string]string{"TEAMCITY_VERSION": "2024.03"}, "teamcity"},
		"CI=true":             {evidence.CISystemGeneric, map[string]string{"CI": "true"}, "generic"},
		"a document's own CI": {evidence.CISystemJenkins, nil, "jenkins"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &evidenceFakeServer{}
			if _, err := runEvidenceReport(t, fake, evidenceEnvWith(test.vars), "--file", genericEvidenceDoc(t, test.system)); err != nil {
				t.Fatalf("evidence-report: %v", err)
			}
			wantCIRun(t, fake.requests[0].Body, map[string]any{"ciSystem": test.want, "runId": "512"})
		})
	}
}

func TestEvidenceReportSendsTheRunURLWithoutItsQuery(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-ci-run", "a-github-actions-run-names-its-ci")
	fake := &evidenceFakeServer{}
	doc := ciEvidenceDoc(t, "turbo-pull-request.json", map[string]any{"system": "github-actions", "runId": "9876543210", "url": "https://github.com/acme/storefront/actions/runs/9876543210?token=s3cr3t#log"})
	if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", doc); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	wantCIRun(t, fake.requests[0].Body, map[string]any{"ciSystem": "github-actions", "runId": "9876543210", "runUrl": "https://github.com/acme/storefront/actions/runs/9876543210"})
	if strings.Contains(string(fake.raw[0]), "s3cr3t") {
		t.Fatalf("the run carries the query: %s", fake.raw[0])
	}
}

func TestEvidenceReportNamesNoCIForALocalRun(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-ci-run", "a-local-run-names-no-ci")
	fake := &evidenceFakeServer{}
	doc := ciEvidenceDoc(t, "generic-minimal.json", map[string]any{"system": "generic", "runId": "512", "attempt": 2, "url": "https://ci.example.com/runs/512"})
	if _, err := runEvidenceReport(t, fake, evidenceEnv(), "--file", doc); err != nil {
		t.Fatalf("evidence-report: %v", err)
	}
	run := fake.requests[0].Body
	if run["provenance"] != "local" {
		t.Fatalf("provenance = %v, want local", run["provenance"])
	}
	wantCIRun(t, run, nil)
}

func TestEvidenceReportLeavesOutACIMemberDeliveryWouldRefuse(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-ci-run", "a-member-delivery-would-refuse-is-left-out")
	long := "https://ci.example.com/runs/" + strings.Repeat("a", maxEvidenceCIRunURLLength)
	for name, test := range map[string]struct {
		ci      map[string]any
		want    map[string]any
		warning string
	}{
		"a run id with a space": {
			map[string]any{"system": "buildkite", "runId": "run 512", "url": "https://buildkite.com/acme/runs/512"},
			map[string]any{"ciSystem": "buildkite", "runUrl": "https://buildkite.com/acme/runs/512"},
			"sends no runId",
		},
		"a run id over the bound": {
			map[string]any{"system": "buildkite", "runId": strings.Repeat("9", maxEvidenceCIRunIDLength+1)},
			map[string]any{"ciSystem": "buildkite"},
			"sends no runId",
		},
		"an attempt over the bound": {
			map[string]any{"system": "buildkite", "runId": "512", "attempt": maxEvidenceCIRunAttempt + 1},
			map[string]any{"ciSystem": "buildkite", "runId": "512"},
			"sends no runAttempt",
		},
		"a url over the bound": {
			map[string]any{"system": "buildkite", "runId": "512", "url": long},
			map[string]any{"ciSystem": "buildkite", "runId": "512"},
			"sends no runUrl",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &evidenceFakeServer{}
			_, stderr, err := runEvidenceReportLogged(t, fake, evidenceEnv(), "--file", ciEvidenceDoc(t, "generic-minimal.json", test.ci))
			if err != nil {
				t.Fatalf("evidence-report: %v", err)
			}
			wantCIRun(t, fake.requests[0].Body, test.want)
			if !strings.Contains(stderr, "warning: cloud evidence-report "+test.warning) {
				t.Fatalf("stderr = %q, want a warning that it %s", stderr, test.warning)
			}
		})
	}

	t.Run("a CI system Delivery refuses", func(t *testing.T) {
		for _, system := range []string{"putnami-ci", "Jenkins", "a-name-longer-than-thirty-two-chars"} {
			run := evidenceSubmitRequest{Provenance: provenanceExternal}
			attempt := 2
			doc := &evidence.Document{CI: evidence.CI{System: system, RunID: "512", Attempt: &attempt, URL: "https://ci.example.com/runs/512"}}
			leftOut := evidenceCIRun(&run, doc, map[string]string{})
			if run.CISystem != "" || run.RunID != "" || run.RunAttempt != 0 || run.RunURL != "" {
				t.Fatalf("%s: run = %+v, want no CI member", system, run)
			}
			if len(leftOut) != 1 || !strings.HasPrefix(leftOut[0], "ciSystem, runId, runAttempt or runUrl:") {
				t.Fatalf("%s: left out = %q, want the four members", system, leftOut)
			}
		}
	})

	t.Run("every CI system of the protocol", func(t *testing.T) {
		for _, system := range evidence.CISystems {
			run := evidenceSubmitRequest{Provenance: provenanceExternal}
			if leftOut := evidenceCIRun(&run, &evidence.Document{CI: evidence.CI{System: system}}, map[string]string{}); len(leftOut) != 0 || run.CISystem != system {
				t.Errorf("%s: run = %+v, left out %q, want the system sent", system, run, leftOut)
			}
		}
	})

	t.Run("a generic document names the CI of the upload", func(t *testing.T) {
		run := evidenceSubmitRequest{Provenance: provenanceExternal}
		evidenceCIRun(&run, &evidence.Document{CI: evidence.CI{System: evidence.CISystemGeneric}}, map[string]string{"GITLAB_CI": "true"})
		if run.CISystem != evidence.CISystemGitLabCI {
			t.Fatalf("ciSystem = %q, want gitlab-ci", run.CISystem)
		}
	})

	// The evidence protocol already refuses these, so no document reaches the
	// upload with them; the run still leaves them out.
	t.Run("what the protocol refuses", func(t *testing.T) {
		zero := 0
		run := evidenceSubmitRequest{Provenance: provenanceExternal}
		doc := &evidence.Document{CI: evidence.CI{System: "buildkite", RunID: "512", Attempt: &zero, URL: "https://user:secret@ci.example.com/runs/512"}}
		leftOut := evidenceCIRun(&run, doc, map[string]string{})
		if run.CISystem != "buildkite" || run.RunID != "512" || run.RunAttempt != 0 || run.RunURL != "" {
			t.Fatalf("run = %+v, want the system and the run id alone", run)
		}
		if len(leftOut) != 2 || !strings.HasPrefix(leftOut[0], "runAttempt:") || !strings.HasPrefix(leftOut[1], "runUrl:") {
			t.Fatalf("left out = %q, want runAttempt and runUrl", leftOut)
		}
		http := evidenceSubmitRequest{Provenance: provenanceExternal}
		evidenceCIRun(&http, &evidence.Document{CI: evidence.CI{System: "buildkite", URL: "http://ci.example.com/runs/512"}}, map[string]string{})
		if http.RunURL != "" {
			t.Fatalf("runUrl = %q, want an http address left out", http.RunURL)
		}
	})
}
