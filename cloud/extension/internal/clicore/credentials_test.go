package clicore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestPutnamiHomeFollowsTheOSUserHomeRule pins decision D-W7: the Putnami home
// resolves through the os.UserHomeDir rule on every platform. On Windows that
// is %USERPROFILE%, and a HOME that a POSIX shell exports there is ignored.
func TestPutnamiHomeFollowsTheOSUserHomeRule(t *testing.T) {
	env := map[string]string{"HOME": "/posix-home", "USERPROFILE": `C:\Users\dev`, "home": "/plan9-home"}
	cases := []struct {
		goos string
		want string
	}{
		{"windows", filepath.Join(`C:\Users\dev`, ".putnami")},
		{"linux", filepath.Join("/posix-home", ".putnami")},
		{"darwin", filepath.Join("/posix-home", ".putnami")},
		{"plan9", filepath.Join("/plan9-home", ".putnami")},
	}
	for _, tc := range cases {
		if got := putnamiHome(env, tc.goos); got != tc.want {
			t.Errorf("putnamiHome on %s = %q, want %q", tc.goos, got, tc.want)
		}
	}
}

func TestPutnamiHomePrefersItsOwnOverride(t *testing.T) {
	env := map[string]string{"PUTNAMI_HOME": "/state", "HOME": "/posix-home", "USERPROFILE": `C:\Users\dev`}
	for _, goos := range []string{"windows", "linux", "darwin"} {
		if got := putnamiHome(env, goos); got != "/state" {
			t.Errorf("putnamiHome on %s = %q, want PUTNAMI_HOME /state", goos, got)
		}
	}
}

func TestUserHomeDirFallsBackToTheProcessHome(t *testing.T) {
	want, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user home directory on this host: %v", err)
	}
	if got := UserHomeDirFor(map[string]string{}, runtime.GOOS); got != want {
		t.Fatalf("UserHomeDirFor with an empty env = %q, want os.UserHomeDir() %q", got, want)
	}
	if got := UserHomeDirFor(map[string]string{userHomeEnv(runtime.GOOS): "/injected"}, runtime.GOOS); got != "/injected" {
		t.Fatalf("UserHomeDirFor = %q, want the injected /injected", got)
	}
}

// TestWriteAuthKeepsMembersItDoesNotDeclare pins the shared auth.json contract:
// a member another Putnami CLI wrote survives this CLI's rewrite, and a
// declared member the new token omits is dropped.
func TestWriteAuthKeepsMembersItDoesNotDeclare(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"PUTNAMI_HOME": home}
	previous := `{"access_token":"old","refresh_token":"old-refresh","token_type":"Bearer","other_cli":{"setting":1}}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAuth(&StoredToken{AccessToken: "new", TokenType: "Bearer"}, env); err != nil {
		t.Fatalf("WriteAuth: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("auth.json is not a JSON object: %v\n%s", err, data)
	}
	if string(got["access_token"]) != `"new"` {
		t.Fatalf("access_token = %s, want \"new\"", got["access_token"])
	}
	if _, ok := got["refresh_token"]; ok {
		t.Fatalf("refresh_token survived although the new token omits it: %s", data)
	}
	var other bytes.Buffer
	if err := json.Compact(&other, got["other_cli"]); err != nil || other.String() != `{"setting":1}` {
		t.Fatalf("other_cli = %s, want the member written by the other CLI", got["other_cli"])
	}
	auth, err := ReadAuth(env, true)
	if err != nil || auth.AccessToken != "new" {
		t.Fatalf("ReadAuth after rewrite = %+v, %v", auth, err)
	}
}
