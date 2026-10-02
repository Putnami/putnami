//go:build unix

package regularfile

import (
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openDeadline bounds one open. An open that waits on a FIFO for a writer
// never returns, so any bound proves it does not wait.
const openDeadline = 5 * time.Second

// openPromptly opens name with open and reports whether it returned within
// openDeadline. An open that does not return keeps its goroutine, which
// touches nothing of the test.
func openPromptly(open func(string) (*os.File, error), name string) (*os.File, bool, error) {
	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		file, err := open(name)
		done <- result{file, err}
	}()
	select {
	case got := <-done:
		return got.file, true, got.err
	case <-time.After(openDeadline):
		return nil, false, nil
	}
}

// releaseFIFO opens name for writing when it is a FIFO, so an open that waits
// on it returns.
func releaseFIFO(name string) {
	if info, err := os.Lstat(name); err == nil && info.Mode()&os.ModeNamedPipe != 0 {
		if file, err := os.OpenFile(name, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = file.Close()
		}
	}
}

// The open flags refuse a symbolic link and do not wait on a FIFO, whatever
// the path held when it was checked.
func TestOpenNoFollowNeitherFollowsALinkNorWaitsOnAFIFO(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if file, err := openNoFollow(link); err == nil {
		_ = file.Close()
		t.Fatal("openNoFollow followed a symbolic link")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	file, returned, err := openPromptly(openNoFollow, fifo)
	if !returned {
		releaseFIFO(fifo)
		t.Fatal("openNoFollow waited on a FIFO for a writer")
	}
	if err != nil {
		t.Fatalf("openNoFollow(FIFO) = %v", err)
	}
	_ = file.Close()
}

func TestOpenRefusesALinkAndAFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if file, err := Open(link); err == nil {
		_ = file.Close()
		t.Fatal("Open followed a symbolic link")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	file, returned, err := openPromptly(Open, fifo)
	if !returned {
		releaseFIFO(fifo)
		t.Fatal("Open waited on a FIFO for a writer")
	}
	if err == nil {
		_ = file.Close()
		t.Fatal("Open opened a FIFO")
	}
}

// The file Open returns reads in blocking mode.
func TestOpenReturnsABlockingFile(t *testing.T) {
	name := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(name, []byte("artifact bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	conn, err := file.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var flagsErr error
	if err := conn.Control(func(fd uintptr) { flags, flagsErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil || flagsErr != nil {
		t.Fatalf("read the descriptor flags: %v, %v", err, flagsErr)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Fatal("the file Open returned is in non-blocking mode")
	}
}

// While the path is swapped between the regular file, a FIFO and a symbolic
// link, every open returns, and every file Open returns is the regular file.
func TestOpenNeverBlocksWhileThePathIsSwapped(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "artifact")
	regular := filepath.Join(dir, "regular")
	fifo := filepath.Join(dir, "fifo")
	link := filepath.Join(dir, "link")
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(name, []byte("artifact bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		for !stop.Load() {
			for _, other := range []string{fifo, link} {
				if os.Rename(name, regular) != nil || os.Rename(other, name) != nil ||
					os.Rename(name, other) != nil || os.Rename(regular, name) != nil {
					return
				}
			}
		}
	}()
	defer func() {
		stop.Store(true)
		<-swapped
	}()
	opens, successes := 0, 0
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		file, returned, err := openPromptly(Open, name)
		if !returned {
			stop.Store(true)
			releaseFIFO(name)
			releaseFIFO(fifo)
			t.Fatalf("open %d waited on a FIFO swapped in for the file", opens+1)
		}
		opens++
		if err != nil {
			continue
		}
		data, readErr := io.ReadAll(file)
		_ = file.Close()
		if readErr != nil || string(data) != "artifact bytes" {
			t.Fatalf("open %d read %q, %v; want the regular file", opens, data, readErr)
		}
		successes++
	}
	if opens == 0 || successes == 0 {
		t.Fatalf("%d opens, %d of the regular file; want both", opens, successes)
	}
}
