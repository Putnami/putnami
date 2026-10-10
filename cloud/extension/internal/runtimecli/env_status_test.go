package runtimecli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// envStatusFacts is a prod environment that declares three workloads and
// follows canary: one ready, one failed, one not deployed, and one more
// deployed that putnami.ci.json does not select.
func envStatusFacts() EnvStatusFacts {
	return EnvStatusFacts{
		Declared: []EnvDeclaration{{Name: "prod", Channel: "canary", Workloads: []EnvDoctorWorkload{
			{Path: "shop/workloads/api", Name: "shop/workloads/api"},
			{Path: "shop/workloads/web", Name: "shop-web"},
			{Path: "shop/workloads/jobs", Name: "shop/workloads/jobs"},
		}}},
		Deployments: []deploymentSummaryEntry{
			{Project: "shop/workloads/api", Environment: "prod", State: "Ready", Revision: "api-00042"},
			{Project: "shop-web", Environment: "prod", State: "Failed", Revision: "web-00007", LastReleaseID: "cm_web", LastRunReason: "container exited 1"},
			{Project: "shop/workloads/old", Environment: "prod", State: "Ready", Revision: "old-00001"},
		},
		Follow: map[string]statusChannelFollow{"prod": {Environment: "prod", Channels: []followedChannel{{
			Channel: "canary",
			Receipt: &channelMoveReceipt{Generation: 310, Disposition: "opened", Settled: true, ReleaseIDs: []string{"cm_310"}},
		}}}},
	}
}

func TestEnvStatusNodeFromNamedEnvironment(t *testing.T) {
	facts := envStatusFacts()
	facts.Named = "prod"
	node := EnvStatusNodeFrom(facts)
	if node.ID != "env" || node.Title != "env prod" || node.State != clicore.StatusFailing {
		t.Fatalf("node = %+v", node)
	}
	if want := "1 of 3 workloads ready, 1 not deployed, 1 deployed and not declared, canary gen 310"; node.Detail != want {
		t.Fatalf("detail = %q, want %q", node.Detail, want)
	}
	if node.Fix != "putnami cloud deploy status cm_web" {
		t.Fatalf("fix = %q, want the failed workload's release", node.Fix)
	}
	want := []struct {
		id     string
		state  clicore.StatusState
		detail string
		fix    string
	}{
		{"env.prod.channel", clicore.StatusOK, "gen 310 opened, release cm_310", ""},
		{"env.prod.shop/workloads/api", clicore.StatusOK, "ready, revision api-00042", ""},
		{"env.prod.shop-web", clicore.StatusFailing, "failed, revision web-00007: container exited 1", "putnami cloud deploy status cm_web"},
		{"env.prod.shop/workloads/jobs", clicore.StatusDegraded, "declared, not deployed", "putnami cloud env doctor prod"},
		{"env.prod.shop/workloads/old", clicore.StatusDegraded, "deployed, not declared in putnami.ci.json, revision old-00001", ""},
	}
	if len(node.Children) != len(want) {
		t.Fatalf("children = %+v", node.Children)
	}
	for index, child := range node.Children {
		if child.ID != want[index].id || child.State != want[index].state || child.Detail != want[index].detail || child.Fix != want[index].fix {
			t.Errorf("child %d = %+v, want %+v", index, child, want[index])
		}
	}
	metrics := map[string]float64{}
	for _, metric := range node.Metrics {
		metrics[metric.ID] = metric.Value
	}
	if metrics["workloads_declared"] != 3 || metrics["workloads_ready"] != 1 || metrics["workloads_not_deployed"] != 1 || metrics["workloads_not_declared"] != 1 || metrics["cd_generation"] != 310 {
		t.Fatalf("metrics = %+v", node.Metrics)
	}
}

func TestEnvStatusNodeFromEveryEnvironment(t *testing.T) {
	facts := envStatusFacts()
	facts.Declared = append(facts.Declared, EnvDeclaration{Name: "staging", Channel: "canary"})
	facts.Deployments = append(facts.Deployments, deploymentSummaryEntry{Project: "shop/workloads/api", Environment: "preview", State: "Ready"})
	facts.Follow["staging"] = statusChannelFollow{Environment: "staging", Unavailable: "503 Service Unavailable"}
	node := EnvStatusNodeFrom(facts)
	if node.ID != "env" || node.Title != "env" || node.State != clicore.StatusFailing {
		t.Fatalf("node = %+v", node)
	}
	wantDetail := "prod: 1 of 3 workloads ready, 1 not deployed, 1 deployed and not declared, canary gen 310; " +
		"staging: no workload declared; " +
		"preview: not declared in putnami.ci.json, 1 workload deployed"
	if node.Detail != wantDetail {
		t.Fatalf("detail = %q, want %q", node.Detail, wantDetail)
	}
	if len(node.Children) != 3 || node.Children[0].ID != "env.prod" || node.Children[1].ID != "env.staging" || node.Children[2].ID != "env.preview" {
		t.Fatalf("children = %+v", node.Children)
	}
	staging := node.Children[1]
	if staging.State != clicore.StatusUnknown || staging.Children[0].Detail != "channel follow not read: 503 Service Unavailable" {
		t.Fatalf("staging = %+v", staging)
	}
	preview := node.Children[2]
	if preview.State != clicore.StatusDegraded || preview.Fix != "" || preview.Children[0].Fix != "" {
		t.Fatalf("preview = %+v", preview)
	}
	if node.Fix != "putnami cloud deploy status cm_web" {
		t.Fatalf("fix = %q", node.Fix)
	}
	for _, metric := range node.Metrics {
		if metric.ID == "cd_generation_prod" && metric.Value == 310 && metric.Title == "prod CD generation" {
			return
		}
	}
	t.Fatalf("metrics lack the prod CD generation: %+v", node.Metrics)
}

func TestEnvStatusNodeFromKeepsTheNodeWhenTheSummaryFails(t *testing.T) {
	facts := envStatusFacts()
	facts.Named = "prod"
	facts.Deployments = nil
	facts.DeploymentsErr = errors.New("status: 503 Service Unavailable")
	node := EnvStatusNodeFrom(facts)
	if node.State != clicore.StatusUnknown || node.Detail != "3 workloads declared, deployments not read, canary gen 310" {
		t.Fatalf("node = %+v", node)
	}
	if len(node.Children) != 2 || node.Children[1].ID != "env.prod.deployments" || node.Children[1].State != clicore.StatusUnknown {
		t.Fatalf("children = %+v", node.Children)
	}
}

func TestEnvStatusNodeFromReadsEachWorkloadState(t *testing.T) {
	cases := []struct {
		deployment deploymentSummaryEntry
		state      clicore.StatusState
		detail     string
	}{
		{deploymentSummaryEntry{State: "ready", LastRunStatus: "Failed", LastRunReason: "no image"}, clicore.StatusDegraded, "ready, last run failed: no image"},
		{deploymentSummaryEntry{State: "Provisioning"}, clicore.StatusDegraded, "provisioning"},
		{deploymentSummaryEntry{State: "Partial"}, clicore.StatusFailing, "partial"},
		{deploymentSummaryEntry{}, clicore.StatusUnknown, "no state reported"},
		{deploymentSummaryEntry{State: "Paused"}, clicore.StatusUnknown, "state paused"},
	}
	for _, tc := range cases {
		node := envWorkloadNode("prod", "api", tc.deployment)
		if node.State != tc.state || node.Detail != tc.detail {
			t.Errorf("%+v: node = %+v, want %s %q", tc.deployment, node, tc.state, tc.detail)
		}
	}
}

func TestEnvChannelNodeJudgesTheMove(t *testing.T) {
	declaration := EnvDeclaration{Name: "prod", Channel: "canary"}
	cases := []struct {
		name   string
		follow statusChannelFollow
		state  clicore.StatusState
		fix    string
	}{
		{"converged", statusChannelFollow{Environment: "prod", Channels: []followedChannel{{Channel: "canary", Receipt: &channelMoveReceipt{Generation: 1, Disposition: "converged", Settled: true}}}}, clicore.StatusOK, ""},
		{"in flight", statusChannelFollow{Environment: "prod", Channels: []followedChannel{{Channel: "canary", Receipt: &channelMoveReceipt{Generation: 2, Disposition: "opened"}}}}, clicore.StatusOK, ""},
		{"partially opened", statusChannelFollow{Environment: "prod", Channels: []followedChannel{{Channel: "canary", Receipt: &channelMoveReceipt{Generation: 3, Disposition: "partially_opened", Settled: true, ReleaseIDs: []string{"cm_a", "cm_b"}}}}}, clicore.StatusDegraded, "putnami cloud deploy status cm_b"},
		{"not deployable", statusChannelFollow{Environment: "prod", Channels: []followedChannel{{Channel: "canary", Receipt: &channelMoveReceipt{Generation: 4, Disposition: "not_deployable", Settled: true}}}}, clicore.StatusDegraded, "putnami cloud env doctor prod"},
		{"follows another channel", statusChannelFollow{Environment: "prod", Channels: []followedChannel{{Channel: "stable"}}}, clicore.StatusDegraded, "putnami cloud env doctor prod"},
		{"unavailable", statusChannelFollow{Environment: "prod", Unavailable: "404 Not Found"}, clicore.StatusUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node, _, _ := envChannelNode("prod", declaration, tc.follow)
			if node.State != tc.state || node.Fix != tc.fix {
				t.Fatalf("node = %+v, want %s fix %q", node, tc.state, tc.fix)
			}
		})
	}
	// An environment declared without a channel follows none, as env doctor
	// reports: ok, with nothing to fix.
	node, _, _ := envChannelNode("dev", EnvDeclaration{Name: "dev"}, statusChannelFollow{Environment: "dev"})
	if node.State != clicore.StatusOK || node.Fix != "" || node.Detail != "follows no channel" {
		t.Fatalf("no channel declared: node = %+v, want ok", node)
	}
}

// TestEnvStatusFixIsTheWorstChecks: the env line names the fix of its worst
// check, not of the first one that is not ok.
func TestEnvStatusFixIsTheWorstChecks(t *testing.T) {
	children := []clicore.StatusNode{
		{ID: "env.dev", State: clicore.StatusDegraded, Fix: "putnami cloud env doctor dev"},
		{ID: "env.prod", State: clicore.StatusFailing, Children: []clicore.StatusNode{
			{ID: "env.prod.web", State: clicore.StatusFailing, Fix: "putnami cloud deploy status cm_web"},
		}},
	}
	if fix := firstFix(children); fix != "putnami cloud deploy status cm_web" {
		t.Fatalf("fix = %q, want the failing check's", fix)
	}
}

func TestEnvStatusWithNothingDeclaredOrDeployed(t *testing.T) {
	node := EnvStatusNodeFrom(EnvStatusFacts{})
	if node.State != clicore.StatusDegraded || node.Fix != "putnami cloud env doctor" {
		t.Fatalf("node = %+v", node)
	}
	unread := EnvStatusNodeFrom(EnvStatusFacts{DeploymentsErr: errors.New("status: 503 Service Unavailable")})
	if unread.State != clicore.StatusUnknown || unread.Detail != "status: 503 Service Unavailable" {
		t.Fatalf("nothing declared, deployments unread: node = %+v, want unknown", unread)
	}
	named := EnvStatusNodeFrom(EnvStatusFacts{Named: "qa"})
	if named.State != clicore.StatusFailing || named.Fix != "putnami cloud env doctor qa" {
		t.Fatalf("named = %+v", named)
	}
}

// envStatusServer answers the deployments summary and the channel-follow
// read of the doctor workspace, and fails the test on any other path.
func envStatusServer(t *testing.T, readinessCalls *int) *httptest.Server {
	t.Helper()
	base := "/v1/workspaces/" + doctorWorkspaceID
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case base + "/deployments":
			_, _ = w.Write([]byte(`{"workspace_id":"` + doctorWorkspaceID + `","deployments":[
				{"project":"shop/workloads/api","environment":"prod","revision":"api-00042","state":"Ready","last_release_id":"cm_310"},
				{"project":"shop-web","environment":"prod","revision":"web-00007","state":"Ready","last_release_id":"cm_310"}]}`))
		case base + "/environments/prod/channel-follow":
			_, _ = w.Write([]byte(`{"workspace_id":"` + doctorWorkspaceID + `","environment":"prod","definition_revision":3,"channels":[
				{"channel":"canary","receipt":{"namespace":"acme","channel":"canary","generation":310,"release_set_id":"rs_ab12cd34ef567890","moved_at":"2026-10-04T10:02:30Z","received_at":"2026-10-04T10:02:31Z","disposition":"opened","release_ids":["cm_310"],"settled":true}}]}`))
		default:
			if strings.HasSuffix(r.URL.Path, "/readiness") {
				*readinessCalls++
			}
			http.NotFound(w, r)
		}
	}))
}

func TestEnvStatusPrintsTheNodeWithoutTheReadinessRoute(t *testing.T) {
	readiness := 0
	srv := envStatusServer(t, &readiness)
	defer srv.Close()
	env := map[string]string{"PUTNAMI_CLOUD_TOKEN": "user-token", "PUTNAMI_CONTROL_PLANE_URL": srv.URL}

	cap := &captureIO{}
	if err := EnvStatus(map[string]any{}, []string{"prod"}, doctorWorkspace(t), env, cap.io(srv.Client())); err != nil {
		t.Fatalf("EnvStatus: %v", err)
	}
	text := strings.Join(cap.stdout, "\n")
	for _, want := range []string{
		"env prod  ok  2 of 2 workloads ready, canary gen 310",
		"channel canary",
		"gen 310 opened 2026-10-04T10:02Z (rs_ab12cd34…), release cm_310",
		"shop/workloads/api",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if readiness != 0 {
		t.Fatalf("env status read the readiness route %d times", readiness)
	}

	cap = &captureIO{}
	if err := EnvStatus(map[string]any{"output": "json"}, nil, doctorWorkspace(t), env, cap.io(srv.Client())); err != nil {
		t.Fatalf("EnvStatus every environment: %v", err)
	}
	var node clicore.StatusNode
	if err := decodeResultData([]byte(strings.Join(cap.stdout, "\n")), &node); err != nil {
		t.Fatal(err)
	}
	if node.ID != "env" || node.State != clicore.StatusOK || len(node.Children) != 1 || node.Children[0].ID != "env.prod" {
		t.Fatalf("node = %+v", node)
	}
}

// TestEnvStatusNodeNarrowsToTheEnvFlag: `cloud status --env prod` checks only
// prod on the env line; without --env the line covers every environment.
func TestEnvStatusNodeNarrowsToTheEnvFlag(t *testing.T) {
	readiness := 0
	srv := envStatusServer(t, &readiness)
	defer srv.Close()
	env := map[string]string{"PUTNAMI_CLOUD_TOKEN": "user-token", "PUTNAMI_CONTROL_PLANE_URL": srv.URL}
	root := doctorWorkspace(t)

	named := EnvStatusNode(map[string]any{"env": "prod"}, root, env, (&captureIO{}).io(srv.Client()))
	if named.ID != "env" || named.Title != "env prod" || named.State != clicore.StatusOK {
		t.Fatalf("--env prod node = %+v, want the prod environment alone", named)
	}
	every := EnvStatusNode(map[string]any{}, root, env, (&captureIO{}).io(srv.Client()))
	if every.Title != "env" || len(every.Children) != 1 || every.Children[0].ID != "env.prod" {
		t.Fatalf("node = %+v, want every declared environment", every)
	}
}

func TestEnvStatusRefusesTwoEnvironments(t *testing.T) {
	for name, tc := range map[string]struct {
		params map[string]any
		args   []string
	}{
		"two positionals":           {map[string]any{}, []string{"prod", "staging"}},
		"positional and other flag": {map[string]any{"env": "staging"}, []string{"prod"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := EnvStatus(tc.params, tc.args, t.TempDir(), map[string]string{}, (&captureIO{}).io(nil))
			if clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("exit = %d (%v), want usage", clicore.ExitCode(err), err)
			}
		})
	}
}

func TestEnvStatusLensesKeepTheTable(t *testing.T) {
	readiness := 0
	srv := envStatusServer(t, &readiness)
	defer srv.Close()
	env := map[string]string{"PUTNAMI_CLOUD_TOKEN": "user-token", "PUTNAMI_CONTROL_PLANE_URL": srv.URL}
	root := doctorWorkspace(t)
	writeDoctorFile(t, filepath.Join(root, "putnami.workspace.json"), map[string]any{
		"options": map[string]any{"@putnami/cloud": map[string]any{"workspace": map[string]any{
			"workspace_id": doctorWorkspaceID, "control_plane_url": "https://control.invalid", "environment": "staging",
		}}},
	})

	cap := &captureIO{}
	params := map[string]any{"provenance": true}
	if err := EnvStatus(params, nil, root, env, cap.io(srv.Client())); err != nil {
		t.Fatalf("EnvStatus --provenance: %v", err)
	}
	if params["env"] != "staging" {
		t.Fatalf("lens environment = %v, want the linked staging", params["env"])
	}
	if text := strings.Join(cap.stdout, "\n"); !strings.Contains(text, "PROJECT") || !strings.Contains(text, "CONFIG VERSION") {
		t.Fatalf("--provenance must print the table:\n%s", text)
	}
}

func TestEnvStatusNodeIsUnknownWithoutAWorkspace(t *testing.T) {
	node := EnvStatusNode(map[string]any{}, t.TempDir(), map[string]string{"HOME": t.TempDir()}, clicore.IO{})
	if node.State != clicore.StatusUnknown || node.Detail == "" || node.ID != "env" {
		t.Fatalf("node = %+v, want unknown with the reason", node)
	}
}
