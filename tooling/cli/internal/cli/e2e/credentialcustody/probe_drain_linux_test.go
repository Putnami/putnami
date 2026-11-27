//go:build linux

package credentialcustody

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The descriptor probe reads a live pipe of its parent without taking its
// bytes: a request line on its way to the credential provider still reaches
// the provider whole once the probe has read it.
func TestTheDescriptorProbeLeavesEveryByteInAPipe(t *testing.T) {
	t.Parallel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	const line = `{"id":7,"op":"upload"}` + "\n"
	if _, err := writer.WriteString(line); err != nil {
		t.Fatal(err)
	}
	// Both ends, as the engine holds them for its provider session: the
	// write end of the provider's input and the read end of its output.
	for _, end := range []*os.File{writer, reader} {
		data, err := readDescriptor("/proc/self/fd/"+strconv.Itoa(int(end.Fd())), procBound)
		if err != nil || string(data) != line {
			t.Fatalf("the probe read %q, %v through %s; want the pending line", data, err, end.Name())
		}
	}
	_ = writer.Close()
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != line {
		t.Fatalf("the pipe's reader got %q, %v after the probe; want the whole line", got, err)
	}
}

// A pipe that holds nothing is read within descriptorReadWait and yields
// nothing.
func TestTheDescriptorProbeReturnsFromAnEmptyPipe(t *testing.T) {
	t.Parallel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	data, err := readDescriptor("/proc/self/fd/"+strconv.Itoa(int(reader.Fd())), procBound)
	if len(data) != 0 || err == nil {
		t.Fatalf("the probe read %q, %v from an empty pipe; want nothing and the wait's error", data, err)
	}
	_ = writer.Close()
	if data, err := readDescriptor("/proc/self/fd/"+strconv.Itoa(int(reader.Fd())), procBound); len(data) != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("the probe read %q, %v from an empty pipe with no writer; want io.EOF", data, err)
	}
}

// A pipe larger than the default capacity is copied whole: a credential
// written past the first 64 KiB is still found.
func TestTheDescriptorProbeCopiesAPipeLargerThanTheDefault(t *testing.T) {
	t.Parallel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, writer.Fd(), fSetPipeSize, 1<<20); errno != 0 {
		t.Skipf("the pipe cannot grow: %v", errno)
	}
	content := append(bytes.Repeat([]byte("x"), 256<<10), custodyBearer...)
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	data, err := readDescriptor("/proc/self/fd/"+strconv.Itoa(int(reader.Fd())), procBound)
	if err != nil || !bytes.HasSuffix(data, []byte(custodyBearer)) {
		t.Fatalf("the probe read %d bytes, %v; want all %d, ending with the credential", len(data), err, len(content))
	}
}

// A copy that cannot grow takes only the first bytes of a larger pipe; the
// probe says so rather than report the copy as all the pipe held. It replaces
// growCopy, so it does not run in parallel.
func TestTheDescriptorProbeReportsAPartialCopy(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, writer.Fd(), fSetPipeSize, 1<<20); errno != 0 {
		t.Skipf("the pipe cannot grow: %v", errno)
	}
	previous := growCopy
	growCopy = func(int, int) {}
	t.Cleanup(func() { growCopy = previous })

	content := append(bytes.Repeat([]byte("x"), 256<<10), custodyBearer...)
	skipUnlessADefaultPipeIsSmallerThan(t, len(content))
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	data, err := readDescriptor("/proc/self/fd/"+strconv.Itoa(int(reader.Fd())), procBound)
	var partial *partialCopyError
	if !errors.As(err, &partial) || partial.held != len(content) || partial.copied != len(data) || len(data) >= len(content) {
		t.Fatalf("the probe read %d of %d bytes with %v; want a partial copy that names both counts", len(data), len(content), err)
	}
	if got, err := io.ReadAll(io.LimitReader(reader, int64(len(content)))); err != nil || len(got) != len(content) {
		t.Fatalf("the pipe's reader got %d bytes, %v after the probe; want all %d", len(got), err, len(content))
	}
}

// The descriptor probe of a process whose pipe it copies only in part scans
// the bytes it copied and records no check, naming the partial copy: a run
// fails rather than pass on a descriptor it did not read whole. It replaces
// growCopy, so it does not run in parallel.
func TestTheDescriptorProbeRecordsNoCheckOnAPartialCopy(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, writer.Fd(), fSetPipeSize, 1<<20); errno != 0 {
		t.Skipf("the pipe cannot grow: %v", errno)
	}
	previous := growCopy
	growCopy = func(int, int) {}
	t.Cleanup(func() { growCopy = previous })

	// The credential lies in the first bytes, which the partial copy takes.
	content := append([]byte(custodyBearer), bytes.Repeat([]byte("x"), 256<<10)...)
	skipUnlessADefaultPipeIsSmallerThan(t, len(content))
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	f := probeProcFD("test", os.Getpid())
	if f.Checked || !strings.Contains(f.Detail, "partial=") {
		t.Fatalf("the probe recorded checked=%v with detail %q; want no check and the partial copy named", f.Checked, f.Detail)
	}
	if !slices.Contains(f.Found, "bearer") {
		t.Fatalf("the probe found %v; want the credential the partial copy holds", f.Found)
	}
}

// skipUnlessADefaultPipeIsSmallerThan skips a test whose copy must stay
// smaller than size: on a kernel with 64 KiB pages a pipe holds 1 MiB by
// default, and the copy is whole.
func skipUnlessADefaultPipeIsSmallerThan(t *testing.T, size int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	capacity, _, errno := syscall.Syscall(syscall.SYS_FCNTL, w.Fd(), fGetPipeSize, 0)
	if errno != 0 || int(capacity) >= size {
		t.Skipf("a default pipe holds %d bytes (%v), no less than the %d the test writes", capacity, errno, size)
	}
}
