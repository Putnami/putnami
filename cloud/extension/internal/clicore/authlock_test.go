package clicore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
)

// authRefreshHelperVariable makes the test binary a CLI process instead of a
// test run. "active" and "workspace" wait for the start file the variable
// authRefreshStartVariable names, then resolve the session with ActiveAuth or
// WorkspaceAuth and print the token it got as JSON. "hold" takes the refresh
// lock, prints "held", and keeps it until the process dies.
const (
	authRefreshHelperVariable = "PUTNAMI_AUTH_REFRESH_TEST_HELPER"
	authRefreshStartVariable  = "PUTNAMI_AUTH_REFRESH_TEST_START"
	authRefreshWorkspace      = "ws-refresh"
)

// heldAuthRefreshLock keeps the "hold" helper's lock reachable, so the
// collector never closes its file and drops the lock before the process dies.
var heldAuthRefreshLock *filelock.Lock

func TestAuthRefreshHelperProcess(t *testing.T) {
	mode := os.Getenv(authRefreshHelperVariable)
	if mode == "" {
		t.Skip("helper process only")
	}
	env := map[string]string{
		"PUTNAMI_HOME":     os.Getenv("PUTNAMI_HOME"),
		"PUTNAMI_AUTH_URL": os.Getenv("PUTNAMI_AUTH_URL"),
	}
	if mode == "hold" {
		// Take the lock directly: lockAuthRefresh would fall back to no lock,
		// and a holder that holds nothing proves nothing.
		lock, err := filelock.Acquire(authLockPath(env), true, true)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		heldAuthRefreshLock = lock
		fmt.Println("held")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	start := os.Getenv(authRefreshStartVariable)
	for {
		if _, err := os.Stat(start); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ioctx := IO{Client: &http.Client{Timeout: 10 * time.Second}}
	var token *StoredToken
	var err error
	if mode == "workspace" {
		token, err = WorkspaceAuth(map[string]any{}, env, ioctx, authRefreshWorkspace)
	} else {
		token, err = ActiveAuth(map[string]any{}, env, ioctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	_ = json.NewEncoder(os.Stdout).Encode(token)
	os.Exit(0)
}

// rotatingAuthServer is a token endpoint that rotates the refresh token on
// every refresh and refuses any other one with invalid_grant, with no reuse
// window: two processes that refresh the same token cannot both succeed.
type rotatingAuthServer struct {
	mu       sync.Mutex
	current  string
	serial   int
	requests int
	scoped   bool
}

func (s *rotatingAuthServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/token" {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	// Answer late, so the processes that start together are all past their
	// first read of auth.json before the first refresh lands.
	time.Sleep(150 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	w.Header().Set("Content-Type", "application/json")
	if body["grant_type"] != "refresh_token" || body["refresh_token"] != s.current {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "refresh token revoked"})
		return
	}
	s.serial++
	s.current = fmt.Sprintf("rt-%d", s.serial)
	claims := map[string]any{"sub": "user"}
	if s.scoped {
		claims["scope_ref"] = map[string]any{"workspace_id": authRefreshWorkspace}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  mintTestJWT(claims),
		"refresh_token": s.current,
		"token_type":    "Bearer",
		"expires_in":    3600,
	})
}

func (s *rotatingAuthServer) snapshot() (current string, requests int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.requests
}

func writeExpiredAuth(t *testing.T, home, issuer string) {
	t.Helper()
	expired := &StoredToken{
		AccessToken:  mintTestJWT(map[string]any{"sub": "user"}),
		RefreshToken: "rt-0",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		Issuer:       issuer,
		ClientID:     "putnami-cli",
	}
	if err := WriteAuth(expired, map[string]string{"PUTNAMI_HOME": home}); err != nil {
		t.Fatal(err)
	}
}

func authRefreshHelper(home, issuer, mode, start string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAuthRefreshHelperProcess$")
	cmd.Env = append(os.Environ(),
		authRefreshHelperVariable+"="+mode,
		authRefreshStartVariable+"="+start,
		"PUTNAMI_HOME="+home,
		"PUTNAMI_AUTH_URL="+issuer,
	)
	return cmd
}

func TestParallelProcessesRefreshTheStoredSessionOnce(t *testing.T) {
	for _, mode := range []string{"active", "workspace"} {
		t.Run(mode, func(t *testing.T) {
			server := &rotatingAuthServer{current: "rt-0", scoped: mode == "workspace"}
			httpServer := httptest.NewServer(server)
			defer httpServer.Close()
			home := t.TempDir()
			writeExpiredAuth(t, home, httpServer.URL)
			start := filepath.Join(t.TempDir(), "start")

			const processes = 6
			type outcome struct {
				out []byte
				err error
			}
			outcomes := make(chan outcome, processes)
			cmds := make([]*exec.Cmd, processes)
			for i := range cmds {
				cmds[i] = authRefreshHelper(home, httpServer.URL, mode, start)
				var stderr strings.Builder
				cmds[i].Stderr = &stderr
				stdout, err := cmds[i].StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmds[i].Start(); err != nil {
					t.Fatal(err)
				}
				go func(cmd *exec.Cmd, stderr *strings.Builder) {
					out, _ := io.ReadAll(stdout)
					err := cmd.Wait()
					if err != nil {
						err = fmt.Errorf("%w: %s", err, stderr.String())
					}
					outcomes <- outcome{out: out, err: err}
				}(cmds[i], &stderr)
			}
			if err := os.WriteFile(start, nil, 0o600); err != nil {
				t.Fatal(err)
			}

			for range processes {
				got := <-outcomes
				if got.err != nil {
					t.Fatalf("a process lost the session: %v", got.err)
				}
				var token StoredToken
				if err := json.Unmarshal(got.out, &token); err != nil || token.AccessToken == "" {
					t.Fatalf("a process printed no token: %q", got.out)
				}
			}

			current, requests := server.snapshot()
			if requests != 1 || current != "rt-1" {
				t.Fatalf("token endpoint answered %d requests and rotated to %s, want one refresh to rt-1", requests, current)
			}
			stored, err := ReadAuth(map[string]string{"PUTNAMI_HOME": home}, true)
			if err != nil {
				t.Fatal(err)
			}
			if stored.RefreshToken != "rt-1" {
				t.Fatalf("stored refresh token = %q, want the rotated rt-1", stored.RefreshToken)
			}
			// A workspace refresh caches the scoped token and keeps the base
			// access token as it was.
			fresh := stored.AccessToken
			expiresAt := stored.ExpiresAt
			if mode == "workspace" {
				fresh = stored.WorkspaceAccess[authRefreshWorkspace].AccessToken
				expiresAt = stored.WorkspaceAccess[authRefreshWorkspace].ExpiresAt
			}
			if !AccessTokenFresh(fresh, expiresAt, time.Now()) {
				t.Fatalf("stored session holds no fresh %s token: expires %q", mode, expiresAt)
			}
		})
	}
}

func TestAKilledRefreshLockHolderBlocksNobody(t *testing.T) {
	server := &rotatingAuthServer{current: "rt-0"}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	home := t.TempDir()
	writeExpiredAuth(t, home, httpServer.URL)

	holder := authRefreshHelper(home, httpServer.URL, "hold", "")
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "held" {
		_ = holder.Process.Kill()
		t.Fatalf("holder did not take the lock: %q %v", line, err)
	}
	if lock, err := filelock.Acquire(authLockPath(map[string]string{"PUTNAMI_HOME": home}), true, true); !errors.Is(err, filelock.ErrBusy) {
		_ = lock.Release()
		_ = holder.Process.Kill()
		t.Fatalf("the lock is free while the holder lives: %v", err)
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()

	began := time.Now()
	token, err := ActiveAuth(map[string]any{}, map[string]string{"PUTNAMI_HOME": home, "PUTNAMI_AUTH_URL": httpServer.URL}, IO{Client: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(began); waited >= authRefreshLockWait/2 {
		t.Fatalf("refresh waited %s for a dead holder", waited)
	}
	if token.RefreshToken != "rt-1" {
		t.Fatalf("refresh token = %q, want rt-1", token.RefreshToken)
	}
}

func TestALiveRefreshLockHolderMakesTheNextRefreshWait(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"PUTNAMI_HOME": home}
	release := lockAuthRefresh(env)
	acquired := make(chan struct{})
	go func() {
		lockAuthRefresh(env)()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("a second refresh took the lock while the first held it")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the second refresh never took the released lock")
	}
}

func TestARefreshLockPastItsWaitRunsUnlocked(t *testing.T) {
	previous := authRefreshLockWait
	authRefreshLockWait = 50 * time.Millisecond
	defer func() { authRefreshLockWait = previous }()
	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}
	release := lockAuthRefresh(env)
	defer release()
	done := make(chan struct{})
	go func() {
		lockAuthRefresh(env)()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a refresh waited past authRefreshLockWait")
	}
}
