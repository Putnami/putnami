package clicore

import (
	"errors"
	"testing"
)

func TestWorkspaceAuthAcceptsOnlyAnExplicitWorkspaceBearer(t *testing.T) {
	env := map[string]string{
		"PUTNAMI_HOME":        t.TempDir(),
		"PUTNAMI_CLOUD_TOKEN": "pkt_workspace_ci",
	}

	auth, err := WorkspaceAuth(nil, env, IO{}, "workspace-a")
	if err != nil {
		t.Fatalf("WorkspaceAuth: %v", err)
	}
	if auth.AccessToken != env["PUTNAMI_CLOUD_TOKEN"] || auth.TokenType != "Bearer" {
		t.Fatal("WorkspaceAuth did not return the exact explicit bearer with Bearer type")
	}
	if auth.RefreshToken != "" || auth.WorkspaceAccess != nil {
		t.Fatal("WorkspaceAuth explicit bearer gained refresh or cached-session authority")
	}
	if stored, readErr := ReadAuth(env, false); readErr != nil || stored != nil {
		t.Fatalf("WorkspaceAuth persisted the explicit bearer: stored=%t err=%v", stored != nil, readErr)
	}

	if _, err := ActiveAuth(nil, env, IO{}); err == nil || ExitCode(err) != ExitAuth {
		t.Fatalf("ActiveAuth accepted PUTNAMI_CLOUD_TOKEN outside workspace auth: %v", err)
	}
	if _, err := WorkspaceAuth(nil, env, IO{}, ""); err == nil || ExitCode(err) != ExitAuth {
		t.Fatalf("empty-workspace auth accepted PUTNAMI_CLOUD_TOKEN: %v", err)
	}
}

func TestWorkspaceAuthRejectsACompositeExplicitBearer(t *testing.T) {
	env := map[string]string{
		"PUTNAMI_HOME":        t.TempDir(),
		"PUTNAMI_CLOUD_TOKEN": "pkt_one pkt_two",
	}
	if _, err := WorkspaceAuth(nil, env, IO{}, "workspace-a"); err == nil || ExitCode(err) != ExitAuth {
		t.Fatalf("WorkspaceAuth accepted a composite bearer: %v", err)
	}
}

// TestUserSessionErrorNamesTheMachineToken: on PUTNAMI_CLOUD_TOKEN alone, a
// read that needs a user says so instead of asking to sign in again.
func TestUserSessionErrorNamesTheMachineToken(t *testing.T) {
	signIn := NewError("not authenticated; run putnami cloud login", ExitAuth)
	home := t.TempDir()
	machine := map[string]string{"PUTNAMI_HOME": home, "PUTNAMI_CLOUD_TOKEN": "machine-token"}
	if err := UserSessionError(signIn, machine); ExitCode(err) != ExitAuth || err.Error() != "needs a signed-in user; PUTNAMI_CLOUD_TOKEN cannot read it" {
		t.Fatalf("machine token alone = %v", err)
	}
	if err := UserSessionError(signIn, map[string]string{"PUTNAMI_HOME": home}); !errors.Is(err, signIn) {
		t.Fatalf("no machine token = %v, want the sign-in error", err)
	}
	other := NewError("boom", ExitAPI)
	if err := UserSessionError(other, machine); !errors.Is(err, other) {
		t.Fatalf("non-auth error = %v, want it unchanged", err)
	}
	if err := UserSessionError(nil, machine); err != nil {
		t.Fatalf("nil = %v", err)
	}
	if node := UserSessionStatus("token", "token", signIn, machine); node.State != StatusUnknown || node.Fix != "" ||
		node.Detail != "needs a signed-in user; PUTNAMI_CLOUD_TOKEN cannot read it" {
		t.Fatalf("machine token node = %+v", node)
	}
	if node := UserSessionStatus("token", "token", signIn, map[string]string{"PUTNAMI_HOME": home}); node.Fix != "putnami cloud login" {
		t.Fatalf("signed-out node = %+v, want the login fix", node)
	}
	if node := UserSessionStatus("token", "token", other, machine); node.Fix != "" || node.Detail != "boom" {
		t.Fatalf("api error node = %+v", node)
	}
}
