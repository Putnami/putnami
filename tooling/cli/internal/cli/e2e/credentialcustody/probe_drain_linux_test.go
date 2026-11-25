//go:build linux

package credentialcustody

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
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
