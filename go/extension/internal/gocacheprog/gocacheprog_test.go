package gocacheprog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// helper drives a running gocacheprog exactly as the go command does: JSON
// lines in, JSON lines out, a put body as a base64 string on its own line.
//
// It writes the wire form by hand instead of reusing this package's own request
// type, so a renamed field fails here rather than silently agreeing with
// itself.
type helper struct {
	t      *testing.T
	stdin  *io.PipeWriter
	stdout *bufio.Reader
	stderr *bytes.Buffer
	done   chan error

	waitOnce sync.Once
	waitErr  error

	mu     sync.Mutex
	nextID int64
}

func startHelper(t *testing.T, env map[string]string) *helper {
	t.Helper()
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	h := &helper{
		t:      t,
		stdin:  stdinWriter,
		stdout: bufio.NewReader(stdoutReader),
		stderr: &bytes.Buffer{},
		done:   make(chan error, 1),
	}
	go func() {
		err := Run(stdinReader, stdoutWriter, h.stderr, func(key string) string { return env[key] })
		_ = stdoutWriter.Close()
		h.done <- err
	}()
	t.Cleanup(func() {
		_ = stdinWriter.Close()
		h.wait()
	})

	// The capability message must be the first thing on stdout.
	first := h.read()
	if first.ID != 0 || len(first.KnownCommands) == 0 {
		t.Fatalf("first message = %+v, want ID 0 with KnownCommands", first)
	}
	want := map[command]bool{commandGet: false, commandPut: false, commandClose: false}
	for _, cmd := range first.KnownCommands {
		if _, ok := want[cmd]; !ok {
			t.Fatalf("advertised unknown command %q", cmd)
		}
		want[cmd] = true
	}
	for cmd, seen := range want {
		if !seen {
			t.Fatalf("command %q is not advertised", cmd)
		}
	}
	return h
}

// wait blocks until the helper process function returns, once. Both close() and
// the test cleanup call it, so it cannot consume the completion channel twice.
func (h *helper) wait() {
	h.waitOnce.Do(func() {
		select {
		case h.waitErr = <-h.done:
		case <-time.After(30 * time.Second):
			h.waitErr = fmt.Errorf("helper did not exit")
		}
	})
	if h.waitErr != nil {
		h.t.Errorf("helper exited with %v", h.waitErr)
	}
}

func (h *helper) id() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	return h.nextID
}

func (h *helper) write(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.stdin, line); err != nil {
		h.t.Fatalf("write request: %v", err)
	}
}

// writeRequest frames one request the way the go command does: a JSON line
// followed by a SECOND newline. The go command encodes with a json.Encoder,
// which already terminates its output, and then writes a newline of its own, so
// every request is followed by a blank line — including the one that precedes a
// put body.
func (h *helper) writeRequest(line string) {
	h.t.Helper()
	h.write(line + "\n")
}

func (h *helper) read() *response {
	h.t.Helper()
	line, err := h.stdout.ReadBytes('\n')
	if err != nil {
		h.t.Fatalf("read response: %v (line %q)", err, line)
	}
	resp := &response{}
	if err := json.Unmarshal(line, resp); err != nil {
		h.t.Fatalf("decode response %q: %v", line, err)
	}
	return resp
}

// get issues one lookup and returns the answer.
func (h *helper) get(actionID []byte) *response {
	h.t.Helper()
	id := h.id()
	h.writeRequest(fmt.Sprintf("{\"ID\":%d,\"Command\":\"get\",\"ActionID\":%q}", id, base64.StdEncoding.EncodeToString(actionID)))
	resp := h.read()
	if resp.ID != id {
		h.t.Fatalf("get answered id %d, asked %d", resp.ID, id)
	}
	return resp
}

// put stores one object, framing the body the way the go command frames it.
func (h *helper) put(actionID, body []byte) *response {
	h.t.Helper()
	outputID := sha256.Sum256(body)
	id := h.id()
	h.writeRequest(fmt.Sprintf("{\"ID\":%d,\"Command\":\"put\",\"ActionID\":%q,\"OutputID\":%q,\"BodySize\":%d}",
		id,
		base64.StdEncoding.EncodeToString(actionID),
		base64.StdEncoding.EncodeToString(outputID[:]),
		len(body),
	))
	if len(body) > 0 {
		h.write("\"" + base64.StdEncoding.EncodeToString(body) + "\"\n")
	}
	resp := h.read()
	if resp.ID != id {
		h.t.Fatalf("put answered id %d, asked %d", resp.ID, id)
	}
	return resp
}

func (h *helper) close() {
	h.t.Helper()
	id := h.id()
	h.writeRequest(fmt.Sprintf("{\"ID\":%d,\"Command\":\"close\"}", id))
	resp := h.read()
	if resp.ID != id {
		h.t.Fatalf("close answered id %d, asked %d", resp.ID, id)
	}
	if resp.Err != "" {
		h.t.Fatalf("close answered error %q", resp.Err)
	}
	h.wait()
}

func actionID(seed byte) []byte {
	id := make([]byte, 32)
	for i := range id {
		id[i] = seed
	}
	return id
}

func localEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{"PUTNAMI_GO_CACHE_DIR": t.TempDir()}
}

// TestPutThenGetRoundTripsThroughTheLocalStore is the helper's whole contract
// with the go command: an object that was put comes back, with the output id it
// was stored under and a DiskPath whose CONTENT is the body. The go command
// links what that file holds, so a wrong path is a wrong build, not a slow one.
func TestPutThenGetRoundTripsThroughTheLocalStore(t *testing.T) {
	env := localEnv(t)
	h := startHelper(t, env)
	body := []byte("compiled package archive")
	action := actionID(0xa1)

	put := h.put(action, body)
	if put.Err != "" {
		t.Fatalf("put failed: %s", put.Err)
	}
	if !filepath.IsAbs(put.DiskPath) {
		t.Fatalf("put DiskPath = %q, want an absolute path", put.DiskPath)
	}
	stored, err := os.ReadFile(put.DiskPath)
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("DiskPath content = %q (%v), want the body", stored, err)
	}

	got := h.get(action)
	if got.Miss {
		t.Fatal("get missed an object the same process just put")
	}
	sum := sha256.Sum256(body)
	if !bytes.Equal(got.OutputID, sum[:]) {
		t.Fatalf("get OutputID = %x, want %x", got.OutputID, sum)
	}
	if got.Size != int64(len(body)) {
		t.Fatalf("get Size = %d, want %d", got.Size, len(body))
	}
	if got.DiskPath != put.DiskPath {
		t.Fatalf("get DiskPath = %q, want %q", got.DiskPath, put.DiskPath)
	}
	if got.Time == nil {
		t.Fatal("get returned no Time; the go command would treat every entry as brand new")
	}
	h.close()
}

// TestGetMissesAnUnknownActionWithoutFailing pins the miss contract: absence is
// Miss, never Err. An error answer propagates out of the go command's cache
// layer as a build failure.
func TestGetMissesAnUnknownActionWithoutFailing(t *testing.T) {
	h := startHelper(t, localEnv(t))
	got := h.get(actionID(0xb2))
	if !got.Miss || got.Err != "" {
		t.Fatalf("unknown action answered %+v, want Miss with no Err", got)
	}
	h.close()
}

// TestStoreSurvivesTheHelperProcess pins that the local directory, not process
// memory, is the cache: a second helper serves what the first one stored.
func TestStoreSurvivesTheHelperProcess(t *testing.T) {
	env := localEnv(t)
	body := []byte("archive that outlives its writer")
	action := actionID(0xc3)

	first := startHelper(t, env)
	first.put(action, body)
	first.close()

	second := startHelper(t, env)
	got := second.get(action)
	if got.Miss {
		t.Fatal("a fresh helper missed an entry the previous one stored")
	}
	stored, err := os.ReadFile(got.DiskPath)
	if err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("restored content = %q (%v), want the body", stored, err)
	}
	second.close()
}

// TestBodyFramingSurvivesBackToBackRequests is the framing regression this
// protocol invites: the body is a SEPARATE line after the request, so a helper
// that under- or over-reads it desynchronizes every request that follows.
func TestBodyFramingSurvivesBackToBackRequests(t *testing.T) {
	h := startHelper(t, localEnv(t))
	bodies := [][]byte{
		[]byte("a"),
		bytes.Repeat([]byte("b"), 4096),
		[]byte("{\"looks\":\"like a request line\"}"),
		{},
	}
	for i, body := range bodies {
		action := actionID(byte(0xd0 + i))
		put := h.put(action, body)
		if put.Err != "" {
			t.Fatalf("put %d failed: %s", i, put.Err)
		}
		got := h.get(action)
		if got.Miss {
			t.Fatalf("get %d missed after a put of %d bytes", i, len(body))
		}
		stored, err := os.ReadFile(got.DiskPath)
		if err != nil || !bytes.Equal(stored, body) {
			t.Fatalf("body %d round-tripped as %q (%v)", i, stored, err)
		}
	}
	h.close()
}

// TestConcurrentGetsAreAnsweredOutOfOrder pins that lookups are handled
// concurrently. The go command keeps many in flight and matches answers by id,
// so nothing here may depend on answering in arrival order.
func TestConcurrentGetsAreAnsweredOutOfOrder(t *testing.T) {
	env := localEnv(t)
	h := startHelper(t, env)
	const count = 16
	for i := 0; i < count; i++ {
		h.put(actionID(byte(i)), []byte(fmt.Sprintf("body %d", i)))
	}

	asked := map[int64]bool{}
	for i := 0; i < count; i++ {
		id := h.id()
		asked[id] = true
		h.writeRequest(fmt.Sprintf("{\"ID\":%d,\"Command\":\"get\",\"ActionID\":%q}",
			id, base64.StdEncoding.EncodeToString(actionID(byte(i)))))
	}
	for i := 0; i < count; i++ {
		resp := h.read()
		if !asked[resp.ID] {
			t.Fatalf("response id %d answers nothing that was asked", resp.ID)
		}
		delete(asked, resp.ID)
		if resp.Miss {
			t.Fatalf("concurrent get %d missed", resp.ID)
		}
	}
	h.close()
}

// TestUnknownCommandIsRefusedNotIgnored keeps the helper honest about the
// version negotiation: it only advertises three commands, and a fourth is a
// protocol break the go command must be told about.
func TestUnknownCommandIsRefusedNotIgnored(t *testing.T) {
	h := startHelper(t, localEnv(t))
	id := h.id()
	h.writeRequest(fmt.Sprintf("{\"ID\":%d,\"Command\":\"get2\"}", id))
	resp := h.read()
	if resp.ID != id || resp.Err == "" {
		t.Fatalf("unknown command answered %+v, want an error", resp)
	}
	h.close()
}

// TestClosedStdinEndsTheHelper covers the shutdown the go command performs when
// it does not send a close request: it closes the helper's stdin and waits for
// stdout to end.
func TestClosedStdinEndsTheHelper(t *testing.T) {
	h := startHelper(t, localEnv(t))
	h.put(actionID(0xe5), []byte("body"))
	if err := h.stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	h.wait()
}

// TestNoSocketMeansNoRemoteTraffic pins the off switch: without the socket
// variable the helper is a purely local cache and never tries to reach one.
func TestNoSocketMeansNoRemoteTraffic(t *testing.T) {
	if remote := openRemote(func(string) string { return "" }, nil); remote != nil {
		t.Fatal("openRemote built a client with no socket variable")
	}
	h := startHelper(t, localEnv(t))
	h.put(actionID(0xf6), []byte("local only"))
	h.close()
	if h.stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want silence", h.stderr.String())
	}
}

// TestQuotedReaderStopsAtTheClosingQuote exercises the body decoder directly at
// the boundary the streaming makes easy to get wrong: the closing quote and the
// newline behind it belong to the framing, not to the body.
func TestQuotedReaderStopsAtTheClosingQuote(t *testing.T) {
	body := []byte("streamed body")
	encoded := "\n\"" + base64.StdEncoding.EncodeToString(body) + "\"\n" + "{\"ID\":2}\n"
	reader := bufio.NewReader(strings.NewReader(encoded))
	decoded, err := bodyReader(reader, int64(len(body)))
	if err != nil {
		t.Fatalf("bodyReader: %v", err)
	}
	got, err := io.ReadAll(decoded)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("decoded %q (%v), want %q", got, err, body)
	}
	rest, err := reader.ReadString('\n')
	if err != nil || rest != "{\"ID\":2}\n" {
		t.Fatalf("stream left at %q (%v), want the next request line", rest, err)
	}
}
