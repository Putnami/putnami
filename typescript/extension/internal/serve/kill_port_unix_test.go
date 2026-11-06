//go:build unix

package serve

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// killPortHelperEnv selects the role of the test binary in the kill-port
// tests: "listener" listens on a free loopback port, writes it to the file
// named by killPortHelperFileEnv and reads every connection it accepts;
// "client" connects to the port killPortHelperPortEnv names, then writes the
// file named by killPortHelperFileEnv. Both run until a signal ends them, or
// exit 3 after 10 minutes.
const (
	killPortHelperEnv     = "PUTNAMI_TS_SERVE_KILL_PORT_HELPER"
	killPortHelperFileEnv = "PUTNAMI_TS_SERVE_KILL_PORT_HELPER_FILE"
	killPortHelperPortEnv = "PUTNAMI_TS_SERVE_KILL_PORT_HELPER_PORT"
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
		go func() {
			time.Sleep(10 * time.Minute)
			os.Exit(3)
		}()
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
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-kill-port", "the-listener-is-killed-and-its-client-is-not")
	if lsof, found := findTool("lsof"); found {
		if isBusyBox(lsof) {
			t.Skip("lsof is the BusyBox applet, which kill-port does not use")
		}
	} else if fuser, found := findTool("fuser"); !found || isBusyBox(fuser) {
		t.Skip("neither lsof nor a fuser other than BusyBox's is on PATH")
	}
	_, portText, listenerDone := startKillPortHelper(t, "listener")
	client, _, clientDone := startKillPortHelper(t, "client", killPortHelperPortEnv+"="+portText)
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("the listener wrote port %q: %v", portText, err)
	}

	if !KillProcessOnPort(port) {
		t.Fatalf("KillProcessOnPort(%d) = false, want true", port)
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

// An lsof that prints anything but one process id per line, such as the
// BusyBox applet that ignores its arguments and lists every open file, makes
// --kill-port kill nothing.
func TestKillProcessOnPort_KillsNothingWhenLsofPrintsMoreThanProcessIDs(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "serve-kill-port", "output-other-than-process-ids-kills-nothing")
	listener, portText, listenerDone := startKillPortHelper(t, "listener")
	pid := strconv.Itoa(listener.Process.Pid)
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("the listener wrote port %q: %v", portText, err)
	}

	bin := t.TempDir()
	busybox := "#!/bin/sh\nprintf '" + pid + "\\t/bin/sleep\\t/dev/null\\n'\n"
	fake := filepath.Join(bin, "lsof")
	if err := os.WriteFile(fake, []byte(busybox), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if lsof, _ := findTool("lsof"); lsof != fake {
		t.Skipf("the fake lsof in %s cannot run, PATH resolves lsof to %q", bin, lsof)
	}

	if KillProcessOnPort(port) {
		t.Errorf("KillProcessOnPort(%d) = true, want false for output that is not process ids", port)
	}
	if err := listener.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the listener: %v", err)
	}
	if sig := exitSignal(t, "listener", listenerDone, 30*time.Second); sig != syscall.SIGTERM {
		t.Errorf("the listener ended with %v, want %v: kill-port killed a process lsof did not name as a listener", sig, syscall.SIGTERM)
	}
}

// findTool returns the program PATH resolves name to, and whether it found one.
func findTool(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

// isBusyBox reports whether the program at path is a BusyBox applet: a link to
// the busybox binary.
func isBusyBox(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && filepath.Base(resolved) == "busybox"
}
