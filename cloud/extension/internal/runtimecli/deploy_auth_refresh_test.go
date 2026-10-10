package runtimecli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// A --wait can outlive the access token minted at deploy start: the control
// plane keeps provisioning, but the status poll dies with 401
// "Authentication required" (hit live on the npm-server deploy — the release
// went Ready server-side while the CLI exited non-zero). doAuthed must
// re-mint the session once on 401, replay the request with the fresh bearer,
// and keep the fresh token on ctx for subsequent polls.
func TestDeployStatusRequestRefreshesExpiredSession(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Authentication required"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"Ready"}`))
	}))
	defer srv.Close()

	ctx := &deployCtx{
		WorkspaceContext: &clicore.WorkspaceContext{
			WorkspaceID:  "ws-acme",
			ControlPlane: srv.URL,
			AuthToken:    clicore.NewBearer("stale"),
			IO:           clicore.IO{Client: srv.Client()},
			RefreshAuth:  func() (clicore.Bearer, error) { return clicore.NewBearer("fresh"), nil },
		},
	}

	parsed, status, err := deployStatusRequest(context.Background(), ctx, "rel_test")
	if err != nil {
		t.Fatalf("deployStatusRequest after refresh: %v", err)
	}
	if status != http.StatusOK || parsed.State != "Ready" {
		t.Fatalf("status=%d state=%v, want 200 Ready", status, parsed.State)
	}
	if len(got) != 2 || got[0] != "Bearer stale" || got[1] != "Bearer fresh" {
		t.Fatalf("authorization sequence = %v, want [Bearer stale, Bearer fresh]", got)
	}
	if ctx.AuthToken.Authorization() != "Bearer fresh" {
		t.Fatal("ctx.AuthToken not updated — the next poll would 401 again")
	}
}

// When the re-mint itself fails (no refresh token, auth server down), the
// original 401 must surface unchanged — the server's message is what the
// user can act on.
func TestDeployStatusRequestSurfacesOriginal401WhenRefreshFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Authentication required"}`))
	}))
	defer srv.Close()

	ctx := &deployCtx{
		WorkspaceContext: &clicore.WorkspaceContext{
			WorkspaceID:  "ws-acme",
			ControlPlane: srv.URL,
			AuthToken:    clicore.NewBearer("stale"),
			IO:           clicore.IO{Client: srv.Client()},
			RefreshAuth: func() (clicore.Bearer, error) {
				return clicore.Bearer{}, clicore.NewError("session is not refreshable; run putnami cloud login", clicore.ExitAuth)
			},
		},
	}

	_, status, err := deployStatusRequest(context.Background(), ctx, "rel_test")
	if status != http.StatusUnauthorized || err == nil {
		t.Fatalf("status=%d err=%v, want the original 401 error", status, err)
	}
}

// The deploy POST replays its JSON body on the refresh retry (GetBody rewind).
func TestDeployRequestReplaysBodyAfterRefresh(t *testing.T) {
	var bodies []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodies = append(bodies, r.ContentLength)
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Authentication required"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"Ready"}`))
	}))
	defer srv.Close()

	ctx := &deployCtx{
		WorkspaceContext: &clicore.WorkspaceContext{
			WorkspaceID:  "ws-acme",
			ControlPlane: srv.URL,
			AuthToken:    clicore.NewBearer("stale"),
			IO:           clicore.IO{Client: srv.Client()},
			RefreshAuth:  func() (clicore.Bearer, error) { return clicore.NewBearer("fresh"), nil },
		},
	}

	_, status, err := deployRequest(context.Background(), ctx, map[string]any{"release_id": "rel_test"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("deployRequest after refresh: status=%d err=%v", status, err)
	}
	if len(bodies) != 2 || bodies[0] == 0 || bodies[0] != bodies[1] {
		t.Fatalf("body lengths = %v, want the same non-empty body on both attempts", bodies)
	}
}

func TestDeployRequestSurfacesPersistent401AsExitAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Authentication required"}`))
	}))
	defer srv.Close()

	ctx := &deployCtx{
		WorkspaceContext: &clicore.WorkspaceContext{
			WorkspaceID:  "ws-acme",
			ControlPlane: srv.URL,
			AuthToken:    clicore.NewBearer("stale"),
			IO:           clicore.IO{Client: srv.Client()},
			RefreshAuth: func() (clicore.Bearer, error) {
				return clicore.Bearer{}, clicore.NewError("session is not refreshable; run putnami cloud login", clicore.ExitAuth)
			},
		},
	}

	_, status, err := deployRequest(context.Background(), ctx, map[string]any{"release_id": "rel_test"})
	if status != http.StatusUnauthorized || err == nil {
		t.Fatalf("status=%d err=%v, want a persistent 401 error", status, err)
	}
	if clicore.ExitCode(err) != clicore.ExitAuth {
		t.Fatalf("exit code = %d, want ExitAuth (err=%v)", clicore.ExitCode(err), err)
	}
}

func TestDeployRequestPrefersMessageAndAppendsGCPBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"control.cloud_failure","error":"coarse","message":"specific","details":{"gcp_response_body":"quota detail"}}`))
	}))
	defer srv.Close()

	ctx := &deployCtx{
		WorkspaceContext: &clicore.WorkspaceContext{
			WorkspaceID:  "ws-acme",
			ControlPlane: srv.URL,
			AuthToken:    clicore.NewBearer("token"),
			IO:           clicore.IO{Client: srv.Client()},
		},
	}

	_, status, err := deployRequest(context.Background(), ctx, map[string]any{"release_id": "rel_test"})
	if status != http.StatusInternalServerError || err == nil {
		t.Fatalf("status=%d err=%v, want a surfaced 500 error", status, err)
	}
	if !strings.Contains(err.Error(), "deploy: specific") || !strings.Contains(err.Error(), "GCP response body: quota detail") {
		t.Fatalf("error %q should prefer message and append the GCP body", err.Error())
	}
}
