//go:build linux || darwin

package machineproc

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
)

// awaitRefusal plays a worker that refuses with code, and returns what await
// reports and what the command wrote on stderr meanwhile.
func awaitRefusal(t *testing.T, code string) (served bool, reason, stderr string) {
	t.Helper()
	pair, err := socketpair()
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]*conn, 2)
	for i, fd := range pair {
		f := os.NewFile(uintptr(fd), "pair")
		fc, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		uc, err := unixConn(fc)
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = &conn{c: uc}
		defer uc.Close() //nolint:errcheck // the test is over
	}
	if err := conns[1].send(event{Refused: code}); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = w
	_, served, reason = await(context.Background(), conns[0])
	os.Stderr = previous
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return served, reason, string(out)
}

// TestAWorkerOfAnotherContextIsTheOneRefusalTheCommandReports pins what a
// command prints when the machine process refuses it: one line on stderr
// when the worker started in another process context than the command's,
// which the user can act on, and nothing for any other refusal. The command
// runs in its own process either way.
func TestAWorkerOfAnotherContextIsTheOneRefusalTheCommandReports(t *testing.T) {
	served, reason, stderr := awaitRefusal(t, CodeContext)
	if served || reason != "refused: "+CodeContext || stderr != contextRefusalNotice {
		t.Fatalf("served %v (%q), wrote %q: want the command run here and one line saying why", served, reason, stderr)
	}
	for _, code := range []string{CodeBusy, CodeIdentity, CodeStopping, CodeOutdated, CodeTerminal} {
		if served, _, stderr := awaitRefusal(t, code); served || stderr != "" {
			t.Errorf("%s: served %v, wrote %q: want the command run here, silently", code, served, stderr)
		}
	}
}
