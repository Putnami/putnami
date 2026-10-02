package jobs

import (
	"path/filepath"
	"slices"
	"testing"

	modelextension "go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/credentialprovider/providertest"
)

// planEdges maps each planned key to its sorted dependsOn and serializeAfter
// edges, the parts of a plan an expected plan compares beside the task.
func planEdges(planned []*ScheduledJob) map[string][2][]string {
	edges := make(map[string][2][]string, len(planned))
	for _, job := range planned {
		if job != nil {
			edges[job.Key()] = [2][]string{dedupeSorted(slices.Clone(job.DependsOn)), dedupeSorted(slices.Clone(job.SerializeAfter))}
		}
	}
	return edges
}

// cloneJobs copies each job and its edges, so a second attachment of the same
// jobs starts from the edges the planner gave them.
func cloneJobs(planned []*ScheduledJob) []*ScheduledJob {
	clones := make([]*ScheduledJob, 0, len(planned))
	for _, job := range planned {
		copied := *job
		copied.DependsOn, copied.SerializeAfter = slices.Clone(job.DependsOn), slices.Clone(job.SerializeAfter)
		clones = append(clones, &copied)
	}
	return clones
}

// A bound request's submitter plans without knowing whether the provider
// echoes publication-v1. Without the open and upload nodes, a publication-v1
// plan is the plan the legacy attachment builds from the same jobs, a deploy
// barrier included: the release depends on the publication jobs again. The
// plan the scheduler runs keeps every node and edge.
func TestWithoutPublicationNodesIsTheLegacyPlan(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "bound-requests-publish-through-the-provider", "the-expected-plan-is-the-legacy-plan")
	for name, barrier := range map[string][]string{"local run": nil, "bound request": {"test"}} {
		t.Run(name, func(t *testing.T) {
			h := newPublicationHarness(t, providertest.Config{})
			web := h.addMember("npm", "@putnami/web", "1.2.3", "web", nil, nil)
			mod := h.addMember("go", "example.com/mod", "v1.0.0", "mod", nil, nil)
			gate := h.addGate("web", filepath.Join(h.root, "gate"))
			deploy := &ScheduledJob{
				Project: web.Project, Extension: h.publisher, SelectedProjects: []*workspace.Project{web.Project, mod.Project},
				DependsOn: []string{gate.Key()},
				JobDef:    &modelextension.JobDefinition{Name: "deploy~ship", CommandName: "deploy", StepID: "ship", ExtensionName: h.publisher.Name},
			}
			h.jobs = append(h.jobs, deploy)
			legacyJobs := cloneJobs(h.jobs)

			planned, _ := h.plan(nil, fixedAncestry{testRevision}, barrier)
			legacyRun := &ReleaseSetRun{
				provider: sessionResolver{h.session}, plan: h.run.plan, routes: h.routes, sourceRevision: testRevision, root: h.root,
			}
			legacy, err := legacyRun.AttachPlan(legacyJobs)
			if err != nil {
				t.Fatal(err)
			}
			if err := legacyRun.BindPublication(nil, barrier); err != nil {
				t.Fatal(err)
			}
			legacy, _, err = legacyRun.AttachBarrier(legacy)
			if err != nil {
				t.Fatal(err)
			}

			want, got := planEdges(legacy), planEdges(h.run.WithoutPublicationNodes(planned))
			if len(got) != len(want) {
				t.Fatalf("projection plans %d nodes, the legacy attachment %d:\nprojection %v\nlegacy     %v", len(got), len(want), got, want)
			}
			for key, edges := range want {
				if projected, ok := got[key]; !ok || !slices.Equal(projected[0], edges[0]) || !slices.Equal(projected[1], edges[1]) {
					t.Errorf("%s: projection edges %v, legacy %v", key, projected, edges)
				}
			}
			if release := planEdges(legacy)[releaseSetResultKey]; !slices.Equal(release[0], []string{mod.Key(), web.Key()}) {
				t.Errorf("the legacy release depends on %v, want both publication jobs", release[0])
			}

			scheduled := planEdges(planned)
			uploads := []string{h.uploadKey(mod), h.uploadKey(web)}
			if _, open := scheduled[publicationOpenKey]; !open || !slices.Equal(scheduled[releaseSetResultKey][0], dedupeSorted(append([]string{publicationOpenKey, mod.Key(), web.Key()}, uploads...))) {
				t.Errorf("the plan the scheduler runs lost a publication node or edge: release depends on %v", scheduled[releaseSetResultKey][0])
			}
			if same := legacyRun.WithoutPublicationNodes(legacy); len(same) != len(legacy) || &same[0] != &legacy[0] {
				t.Error("the projection of a legacy run is not that run's plan")
			}
		})
	}
}

// An edge to an upload node names the publication job it uploads for, an
// edge to open goes, and the rewritten edges are deduplicated and sorted. A
// job with such an edge is copied: the planned job keeps its edges.
func TestWithoutPublicationNodesRewritesEveryEdge(t *testing.T) {
	spectest.Proves(t, "cli/provider-publication", "bound-requests-publish-through-the-provider", "the-expected-plan-is-the-legacy-plan")
	h := newPublicationHarness(t, providertest.Config{})
	web := h.addMember("npm", "@putnami/web", "1.2.3", "web", nil, nil)
	gate := h.addGate("web", filepath.Join(h.root, "gate"))
	planned, _ := h.plan(nil, fixedAncestry{testRevision}, nil)
	upload := h.uploadKey(web)
	report := &ScheduledJob{
		Project: web.Project, Extension: h.publisher,
		DependsOn:      []string{upload, web.Key(), gate.Key()},
		SerializeAfter: []string{publicationOpenKey, upload},
		JobDef:         &modelextension.JobDefinition{Name: "report~summary", CommandName: "report", StepID: "summary", ExtensionName: h.publisher.Name},
	}
	planned = append(planned, report)

	projected := planEdges(h.run.WithoutPublicationNodes(planned))
	if _, kept := projected[upload]; kept {
		t.Errorf("the projection keeps the upload node %s", upload)
	}
	if _, kept := projected[publicationOpenKey]; kept {
		t.Error("the projection keeps the open node")
	}
	edges := projected[report.Key()]
	if want := dedupeSorted([]string{gate.Key(), web.Key()}); !slices.Equal(edges[0], want) {
		t.Errorf("projected dependsOn %v, want %v", edges[0], want)
	}
	if want := []string{web.Key()}; !slices.Equal(edges[1], want) {
		t.Errorf("projected serializeAfter %v, want %v", edges[1], want)
	}
	if !slices.Equal(report.DependsOn, []string{upload, web.Key(), gate.Key()}) || !slices.Equal(report.SerializeAfter, []string{publicationOpenKey, upload}) {
		t.Errorf("the projection changed the planned job's edges to %v and %v", report.DependsOn, report.SerializeAfter)
	}
	if got := (*ReleaseSetRun)(nil).WithoutPublicationNodes(planned); len(got) != len(planned) {
		t.Errorf("a run without a release set projected %d of %d nodes", len(got), len(planned))
	}
}
