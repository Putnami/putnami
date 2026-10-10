package deliverycli

import (
	"net/http"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

const subscriptionTestPath = "/v1/workspaces/ws-acme/ci/subscription"

func subscriptionFixture() map[string]any {
	return map[string]any{"workspace_id": "ws-acme", "revision": 7, "enabled": true, "runner_channel": "canary",
		"tracks":          []map[string]any{{"producer_workspace_id": "producer", "producer_namespace": "putnami", "channel": "canary"}, {"producer_namespace": "other", "release_set_id": "rs-pinned"}},
		"required_checks": map[string]any{"rules": []map[string]string{{"repo_pattern": "acme/*", "branch_pattern": "main"}}},
		"drift":           map[string]any{"apps": []string{"app:prod"}},
	}
}

func TestTrackAtomicUpdatePreservesSettings(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + subscriptionTestPath: {status: http.StatusOK, body: subscriptionFixture()},
		"PUT " + subscriptionTestPath: {status: http.StatusOK, body: subscriptionFixture()},
	}}
	ioctx, root := newCITestIO(t, fake)
	params := map[string]any{"namespace": "putnami", "channel": "stable", "runner-channel": "stable"}
	if err := Track(params, nil, root, ioctx.Env, ioctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %v", fake.requests)
	}
	body := fake.requests[1].Body
	if body["expected_revision"] != float64(7) || body["runner_channel"] != "stable" || body["enabled"] != true {
		t.Fatalf("atomic request = %v", body)
	}
	tracks := body["tracks"].([]any)
	if len(tracks) != 2 || tracks[0].(map[string]any)["channel"] != "stable" || tracks[1].(map[string]any)["release_set_id"] != "rs-pinned" {
		t.Fatalf("tracks = %v", tracks)
	}
	if _, ok := tracks[0].(map[string]any)["producer_workspace_id"]; ok {
		t.Fatal("client supplied producer authority")
	}
	if !strings.Contains(ciCompactJSON(body["required_checks"]), "acme/*") || !strings.Contains(ciCompactJSON(body["drift"]), "app:prod") {
		t.Fatalf("lost settings: %v", body)
	}
}

func TestTrackConflictDoesNotOverwriteOrRetry(t *testing.T) {
	fake := &ciFakeServer{responses: map[string]ciStubResponse{
		"GET " + subscriptionTestPath: {status: 200, body: subscriptionFixture()},
		"PUT " + subscriptionTestPath: {status: 409, body: map[string]string{"error": "subscription revision changed"}},
	}}
	ioctx, root := newCITestIO(t, fake)
	err := Track(map[string]any{"namespace": "putnami", "release-set": "rs-exact"}, nil, root, ioctx.Env, ioctx)
	if err == nil || len(fake.requests) != 2 {
		t.Fatalf("conflict: %v, requests %v", err, fake.requests)
	}
	track := fake.requests[1].Body["tracks"].([]any)[0].(map[string]any)
	if track["release_set_id"] != "rs-exact" || track["channel"] != nil {
		t.Fatalf("pin: %v", track)
	}
}

func TestSubscriptionEnableCreatesAndDisableUsesCAS(t *testing.T) {
	for _, verb := range []string{"enable", "disable", "subscription"} {
		t.Run(verb, func(t *testing.T) {
			get := ciStubResponse{status: 200, body: subscriptionFixture()}
			if verb == "enable" {
				get = ciStubResponse{status: 404}
			}
			fake := &ciFakeServer{responses: map[string]ciStubResponse{
				"GET " + subscriptionTestPath:               get,
				"PUT " + subscriptionTestPath:               {status: 200, body: subscriptionFixture()},
				"POST " + subscriptionTestPath + "/disable": {status: 200, body: subscriptionFixture()},
			}}
			_, err := captureCI(t, fake, []string{verb})
			if err != nil {
				t.Fatal(err)
			}
			if verb == "subscription" {
				if len(fake.requests) != 1 {
					t.Fatal("read mutated")
				}
				return
			}
			body := fake.requests[1].Body
			if verb == "enable" && (body["expected_revision"] != float64(0) || body["enabled"] != true || body["runner_channel"] != "canary") {
				t.Fatalf("create %v", body)
			}
			if verb == "disable" && (len(body) != 1 || body["expected_revision"] != float64(7)) {
				t.Fatalf("disable %v", body)
			}
		})
	}
}

func TestTrackRejectsInvalidSelectorBeforeNetwork(t *testing.T) {
	for _, p := range []map[string]any{{}, {"namespace": "x", "channel": "preview"}, {"namespace": "x", "channel": "stable", "release-set": "rs"}, {"namespace": "x", "channel": "stable", "runner-channel": "preview"}} {
		if err := Track(p, nil, "", nil, clicore.IO{}); err == nil {
			t.Fatalf("accepted %v", p)
		}
	}
}
