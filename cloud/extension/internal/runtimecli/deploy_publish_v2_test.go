package runtimecli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

func publishV2Fixture() publishV2DeployInput {
	hex := strings.Repeat("a", 64)
	return publishV2DeployInput{
		ProtocolVersion:                       publishV2InputVersion,
		SourceRevision:                        strings.Repeat("b", 40),
		ExpectedEnvironmentDefinitionRevision: 3,
		ReleaseSetRef:                         publishV2ReleaseSetRef{ID: "rs_" + hex, Digest: "sha256:" + hex},
		Projects: []publishV2ProjectInput{{
			Name: "accounts/workloads/auth-server",
			Image: publishV2MemberSelector{
				Ecosystem: "oci", Coordinate: "putnami/accounts-workloads-auth-server", Version: "0.2.0-immutable",
				ArtifactDigest: "sha256:" + strings.Repeat("c", 64),
			},
			Config: publishV2MemberSelector{
				Ecosystem: "put", Coordinate: "config/accounts/workloads/auth-server", Version: "0.2.0-immutable",
				ArtifactDigest: "sha256:" + strings.Repeat("d", 64),
			},
			Migrations: []publishV2MemberSelector{{
				Ecosystem: "put", Coordinate: "cloud/accounts-workloads-auth-server", Version: "0.2.0-immutable",
				ArtifactDigest: "sha256:" + strings.Repeat("e", 64),
			}},
		}},
	}
}

func TestDeployPublishV2AcceptsDefinitionThenPostsOnlyOwnerReferences(t *testing.T) {
	input := publishV2Fixture()
	var calls int
	var deployBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls++
		if request.Header.Get("Authorization") != "Bearer workspace-token" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers = %+v", request.Header)
		}
		switch calls {
		case 1:
			if request.Method != http.MethodPost || request.URL.Path != "/v1/workspaces/ws-acme/environment-definitions/accept" {
				t.Fatalf("accept request = %s %s", request.Method, request.URL.Path)
			}
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 2 || body["expectedRevision"] != float64(3) || body["sourceRevision"] != input.SourceRevision {
				t.Fatalf("accept body = %#v", body)
			}
			writeRuntimeCLIJSON(t, w, map[string]any{
				"workspace_id": "ws-acme", "revision": 4,
				"digest": "sha256:" + strings.Repeat("e", 64), "source_revision": input.SourceRevision,
				"accepted_at": "2026-09-07T06:30:00Z",
			})
		case 2:
			if request.Method != http.MethodPost || request.URL.Path != "/v1/workspaces/ws-acme/deploy" {
				t.Fatalf("deploy request = %s %s", request.Method, request.URL.Path)
			}
			if err := json.NewDecoder(request.Body).Decode(&deployBody); err != nil {
				t.Fatal(err)
			}
			if len(deployBody) != 3 || deployBody["environment"] != "prod" || !strings.HasPrefix(deployBody["release_id"].(string), "rel_") {
				t.Fatalf("deploy envelope = %#v", deployBody)
			}
			publish, ok := deployBody["publish_v2"].(map[string]any)
			if !ok || len(publish) != 3 || publish["environment_definition_revision"] != float64(4) {
				t.Fatalf("publish_v2 = %#v", deployBody["publish_v2"])
			}
			if _, legacy := deployBody["projects"]; legacy {
				t.Fatal("ordinary request mixed legacy projects into the envelope")
			}
			ref := publish["release_set_ref"].(map[string]any)
			if ref["id"] != input.ReleaseSetRef.ID || ref["digest"] != input.ReleaseSetRef.Digest {
				t.Fatalf("release set ref = %#v", ref)
			}
			projects := publish["projects"].([]any)
			project := projects[0].(map[string]any)
			image := project["image"].(map[string]any)
			config := project["config"].(map[string]any)
			if project["name"] != input.Projects[0].Name || image["artifact_digest"] != input.Projects[0].Image.ArtifactDigest || config["artifact_digest"] != input.Projects[0].Config.ArtifactDigest {
				t.Fatalf("project selectors = %#v", project)
			}
			// The ordered migration members travel to Control exactly as authored:
			// they are what lets the ordinary path apply a bundle the workload owns.
			migrations, _ := project["migrations"].([]any)
			if len(migrations) != 1 || migrations[0].(map[string]any)["artifact_digest"] != input.Projects[0].Migrations[0].ArtifactDigest ||
				migrations[0].(map[string]any)["ecosystem"] != "put" {
				t.Fatalf("project migrations = %#v", project["migrations"])
			}
			writeRuntimeCLIJSON(t, w, map[string]any{
				"release_id": deployBody["release_id"], "state": "Ready",
				"projects": []any{map[string]any{"name": input.Projects[0].Name, "status": "Ready", "action": "roll"}},
			})
		default:
			t.Fatalf("unexpected request %d", calls)
		}
	}))
	defer server.Close()

	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{
		WorkspaceID: "ws-acme", ControlPlane: server.URL, AuthToken: clicore.NewBearer("workspace-token"),
		IO: clicore.IO{Client: server.Client()},
	}, environment: "prod"}
	var output []string
	ioctx := clicore.IO{Client: server.Client(), Now: func() time.Time { return time.Date(2026, 9, 7, 6, 31, 0, 0, time.UTC) }, Stdout: func(line string) { output = append(output, line) }}
	if err := deployPublishV2(map[string]any{"json": true}, ctx, input, ioctx); err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Status string `json:"status"`
		Data   struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.Join(output, "\n")), &rendered); err != nil {
		t.Fatalf("decode output %q: %v", output, err)
	}
	if calls != 2 || rendered.Status != "success" || rendered.Data.Status != "ready" {
		t.Fatalf("calls=%d output=%+v", calls, rendered)
	}
}

func TestReadPublishV2DeployInputIsClosedAndFailClosed(t *testing.T) {
	input := publishV2Fixture()
	valid, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, body []byte) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if got, err := readPublishV2DeployInput(write("request.json", valid)); err != nil || got.ReleaseSetRef != input.ReleaseSetRef {
		t.Fatalf("valid request = %+v, %v", got, err)
	}

	cases := map[string][]byte{
		"unknown managed data": bytes.Replace(valid, []byte(`"projects"`), []byte(`"managedData":{},"projects"`), 1),
		"trailing data":        append(append([]byte(nil), valid...), []byte(` {}`)...),
		"mismatched ref":       bytes.Replace(valid, []byte(input.ReleaseSetRef.Digest), []byte("sha256:"+strings.Repeat("f", 64)), 1),
		"wrong ecosystem":      bytes.Replace(valid, []byte(`"ecosystem":"oci"`), []byte(`"ecosystem":"docker"`), 1),
		"migration not a put member": bytes.Replace(valid,
			[]byte(`"ecosystem":"put","coordinate":"cloud/accounts-workloads-auth-server"`),
			[]byte(`"ecosystem":"oci","coordinate":"cloud/accounts-workloads-auth-server"`), 1),
		"migration with a malformed digest": bytes.Replace(valid, []byte(strings.Repeat("e", 64)), []byte(strings.Repeat("e", 63)+"g"), 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := readPublishV2DeployInput(write("invalid.json", body)); err == nil || clicore.ExitCode(err) != clicore.ExitUsage {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAcceptPublishV2EnvironmentAllowsHistoricalReplayAndRejectsDifferentIdentity(t *testing.T) {
	input := publishV2Fixture()
	input.ExpectedEnvironmentDefinitionRevision = 9
	response := map[string]any{
		"workspace_id": "ws-acme", "revision": 2, "digest": "sha256:" + strings.Repeat("e", 64),
		"source_revision": input.SourceRevision, "accepted_at": "2026-09-07T06:30:00Z",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeRuntimeCLIJSON(t, w, response) }))
	defer server.Close()
	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{
		WorkspaceID: "ws-acme", ControlPlane: server.URL, AuthToken: clicore.NewBearer("token"), IO: clicore.IO{Client: server.Client()},
	}}
	if receipt, err := acceptPublishV2Environment(context.Background(), ctx, input); err != nil || receipt.Revision != 2 {
		t.Fatalf("historical replay = %+v, %v", receipt, err)
	}
	response["revision"] = float64(11)
	if _, err := acceptPublishV2Environment(context.Background(), ctx, input); err == nil {
		t.Fatal("acceptance newer than expected CAS was accepted")
	}
	response["revision"] = float64(2)
	response["source_revision"] = strings.Repeat("f", 40)
	if _, err := acceptPublishV2Environment(context.Background(), ctx, input); err == nil {
		t.Fatal("different accepted source identity was accepted")
	}
}

func TestAcceptPublishV2EnvironmentRejectsOpenResponseBeforeDeploy(t *testing.T) {
	input := publishV2Fixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"workspace_id":"ws-acme","revision":4,"digest":"sha256:`+strings.Repeat("e", 64)+`","source_revision":"`+input.SourceRevision+`","accepted_at":"2026-09-07T06:30:00Z","repository":"private"}`)
	}))
	defer server.Close()
	ctx := &deployCtx{WorkspaceContext: &clicore.WorkspaceContext{
		WorkspaceID: "ws-acme", ControlPlane: server.URL, AuthToken: clicore.NewBearer("token"), IO: clicore.IO{Client: server.Client()},
	}}
	if _, err := acceptPublishV2Environment(context.Background(), ctx, input); err == nil {
		t.Fatal("open acceptance response was accepted")
	}
}

func TestDeployDispatchesPublishV2(t *testing.T) {
	err := Deploy(map[string]any{}, []string{"publish-v2"}, t.TempDir(), nil, clicore.IO{})
	if err == nil || !strings.Contains(err.Error(), "--request-file") {
		t.Fatalf("dispatch error = %v", err)
	}
}

// TestDeployHasOnlyStatusAndPublishV2 pins that `cloud deploy` with no
// subcommand, or any other, is a usage error that names the two it has: the
// classic release and the hand deploy are gone.
func TestDeployHasOnlyStatusAndPublishV2(t *testing.T) {
	for _, args := range [][]string{nil, {"auth-server"}, {"--image-version", "1.2.3"}} {
		err := Deploy(map[string]any{"image-version": "1.2.3"}, args, t.TempDir(), nil, clicore.IO{})
		if clicore.ExitCode(err) != clicore.ExitUsage || err.Error() != deployUsage {
			t.Errorf("Deploy(%q) = %v (exit %d), want the usage error", args, err, clicore.ExitCode(err))
		}
	}
}

func writeRuntimeCLIJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}
