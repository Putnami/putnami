package serve

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// killPortHelperEnv selects the role of the test binary in the kill-port
// tests: "listener" listens on a free loopback port, writes it to the file
// named by killPortHelperFileEnv and holds every connection it accepts;
// "client" connects to the port killPortHelperPortEnv names, then writes the
// file named by killPortHelperFileEnv. Both run until a signal ends them.
const (
	killPortHelperEnv     = "PUTNAMI_GO_SERVE_KILL_PORT_HELPER"
	killPortHelperFileEnv = "PUTNAMI_GO_SERVE_KILL_PORT_HELPER_FILE"
	killPortHelperPortEnv = "PUTNAMI_GO_SERVE_KILL_PORT_HELPER_PORT"
)

// TestKillPortHelperProcess is not a test: it is one of the kill-port
// helpers, and it returns at once unless a test started it as one.
func TestKillPortHelperProcess(t *testing.T) {
	file := os.Getenv(killPortHelperFileEnv)
	switch os.Getenv(killPortHelperEnv) {
	case "listener":
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(5)
		}
		port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		writeHelperFile(file, port)
		for {
			conn, err := listener.Accept()
			if err != nil {
				os.Exit(6)
			}
			go func() { _, _ = io.Copy(io.Discard, conn) }()
		}
	case "client":
		conn, err := net.Dial("tcp", "127.0.0.1:"+os.Getenv(killPortHelperPortEnv))
		if err != nil {
			os.Exit(5)
		}
		writeHelperFile(file, "connected")
		time.Sleep(10 * time.Minute)
		_ = conn.Close()
		os.Exit(3)
	}
}

// writeHelperFile publishes content at path in one rename, so a reader never
// sees a partial file.
func writeHelperFile(path, content string) {
	if err := os.WriteFile(path+".tmp", []byte(content), 0o644); err != nil {
		os.Exit(5)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		os.Exit(5)
	}
}

// startKillPortHelper starts the helper role and returns it once it wrote its
// file, with the file's content and a channel that receives its exit.
func startKillPortHelper(t *testing.T, role string, env ...string) (*exec.Cmd, string, <-chan error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), role)
	cmd := exec.Command(os.Args[0], "-test.run=^TestKillPortHelperProcess$")
	cmd.Env = append(append(os.Environ(), killPortHelperEnv+"="+role, killPortHelperFileEnv+"="+file), env...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the %s: %v", role, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		if data, err := os.ReadFile(file); err == nil {
			return cmd, strings.TrimSpace(string(data)), done
		}
		select {
		case err := <-done:
			t.Fatalf("the %s ended before it was ready: %v", role, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s was not ready within 30s", role)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// exitSignal returns the signal that ended a helper, or fails the test when
// none did within bound.
func exitSignal(t *testing.T, role string, done <-chan error, bound time.Duration) syscall.Signal {
	t.Helper()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("the %s exited with %v, want a signal", role, err)
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() {
			t.Fatalf("the %s exited with %v, want a signal", role, err)
		}
		return status.Signal()
	case <-time.After(bound):
		t.Fatalf("the %s did not end within %v", role, bound)
		return 0
	}
}

// --kill-port ends the process that listens on the port, and only that one: a
// client connected to the port is left running.
func TestKillProcessOnPort_KillsTheListenerAndNotItsClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("kill-port is not available on Windows")
	}
	if _, lsofErr := exec.LookPath("lsof"); lsofErr != nil {
		if _, fuserErr := exec.LookPath("fuser"); fuserErr != nil {
			t.Skip("neither lsof nor fuser is on PATH")
		}
	}
	_, port, listenerDone := startKillPortHelper(t, "listener")
	client, _, clientDone := startKillPortHelper(t, "client", killPortHelperPortEnv+"="+port)

	if !killProcessOnPort(port) {
		t.Fatalf("killProcessOnPort(%s) = false, want true", port)
	}
	if sig := exitSignal(t, "listener", listenerDone, 30*time.Second); sig != syscall.SIGKILL {
		t.Errorf("the listener ended with %v, want %v", sig, syscall.SIGKILL)
	}

	// A SIGKILL sent to the client before this SIGTERM ends it first, so the
	// signal that ended it tells whether kill-port reached it.
	if err := client.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the client: %v", err)
	}
	if sig := exitSignal(t, "client", clientDone, 30*time.Second); sig != syscall.SIGTERM {
		t.Errorf("the client ended with %v, want %v: kill-port killed a process that does not listen on the port", sig, syscall.SIGTERM)
	}
}
