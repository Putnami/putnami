// Package gocacheprog backs the Go build cache with the run's object cache.
//
// It implements GOCACHEPROG — the protocol the `go` command uses to delegate
// its build cache to a helper program (Go 1.24 and later). The helper answers
// every lookup from a local directory first, asks the cache provider's object
// cache only on a miss, and uploads what a build produced behind the compiler
// rather than in front of it. Nothing here is on the network's critical path:
// a provider that is absent, slow or broken degrades the helper to a plain
// local cache, which is what the go command would have used anyway.
//
// The entry point is hidden (`putnami-go gocacheprog`) because its caller is
// the go command, never a user and never the orchestrator: toolchain.
// GoCommandEnv points GOCACHEPROG at this binary for a run whose job
// environment carries a provider socket, and at nothing otherwise.
package gocacheprog

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"go.putnami.dev/go/extension/internal/toolchain"
	cache "go.putnami.dev/protocol/cache"
)

// progDirName is the helper's record directory under the resolved Go cache
// root, beside "build" (GOCACHE, where the helper keeps its data files) and
// "mod" (GOMODCACHE).
const progDirName = "prog"

// debugEnv turns the one-line socket failure report on. The helper is a child
// of every `go` invocation a build makes, so an unconditional message would
// print dozens of times per run for a condition the build recovers from.
const debugEnv = "PUTNAMI_DEBUG"

// Run serves the GOCACHEPROG protocol until the go command closes the helper.
//
// It writes the capability message FIRST, before resolving anything that can
// fail. The go command aborts the whole build when a helper does not announce
// itself, so a misconfigured cache root must degrade this process, never take
// the build down with it.
func Run(stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) error {
	server := &server{
		out:    bufio.NewWriterSize(stdout, 64*1024),
		stderr: stderr,
		getenv: getenv,
	}
	if err := server.respond(&response{ID: 0, KnownCommands: knownCommands}); err != nil {
		return err
	}

	local, err := openStore(storeDirs(getenv))
	if err != nil {
		// Answer every request honestly instead of exiting: a helper that dies
		// mid-build fails the build, while one that reports "no cache" only
		// makes it slower.
		server.reportOnce(err)
	}
	server.store = local
	server.remote = openRemote(getenv, server.reportOnce)

	err = server.serve(stdin)
	server.reportExchange()
	return err
}

// storeDirs resolves the helper's two cache directories: records under
// <goCacheRoot>/prog, data files under <goCacheRoot>/build.
//
// Both come from the SAME root resolution as GOCACHE and GOMODCACHE
// (toolchain.ResolveGoCacheRoot), so one PUTNAMI_GO_CACHE_DIR moves all of them
// and the extension's collector finds all of them. The data directory is
// toolchain.GoBuildCacheDir, the function GoCommandEnv sets GOCACHE from, so
// the helper's data files land exactly where a go command running without the
// helper looks for its own.
//
// A machine where no root resolves at all yields "", and the helper then runs
// with no local store: every lookup misses and every put is refused.
// GoCommandEnv never points GOCACHEPROG at this helper without a resolved root,
// so that branch is reachable only by running the helper by hand. A
// temporary-directory fallback would be a cache tree that `putnami cache
// clean` and `cache gc` never see.
func storeDirs(getenv func(string) string) (records, data string) {
	root := toolchain.ResolveGoCacheRoot(getenv)
	if root == "" {
		return "", ""
	}
	return filepath.Join(root, progDirName), toolchain.GoBuildCacheDir(root)
}

// server holds the state one helper process serves a single go command with.
type server struct {
	store  *store
	remote *remote
	getenv func(string) string

	writeMu sync.Mutex
	out     *bufio.Writer

	stderr     io.Writer
	reportOnly sync.Once

	handlers     sync.WaitGroup
	shutdownOnce sync.Once
}

// serve reads requests until stdin ends or a close request is answered.
//
// The read loop is single threaded: a put's body follows its request line and
// is consumed before the next request is decoded. Everything after that is
// handed to a goroutine, so parallel lookups reach the socket in parallel.
func (s *server) serve(stdin io.Reader) error {
	reader := bufio.NewReaderSize(stdin, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			req := &request{}
			if decodeErr := json.Unmarshal(line, req); decodeErr != nil {
				return fmt.Errorf("gocacheprog: decode request: %w", decodeErr)
			}
			done, handleErr := s.dispatch(req, reader)
			if handleErr != nil {
				return handleErr
			}
			if done {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The go command closed our stdin without a close request; it
				// is shutting down and no longer reads our answers.
				s.shutdown()
				return nil
			}
			return fmt.Errorf("gocacheprog: read request: %w", err)
		}
	}
}

// dispatch handles one request and reports whether the helper is done.
func (s *server) dispatch(req *request, reader *bufio.Reader) (bool, error) {
	switch req.Command {
	case commandGet:
		s.handlers.Add(1)
		go func() {
			defer s.handlers.Done()
			_ = s.respond(s.handleGet(req))
		}()
		return false, nil
	case commandPut:
		// The body is framed inside the request stream, so it is read here,
		// in the read loop, and stored before the next request is decoded.
		resp, err := s.handlePut(req, reader)
		if err != nil {
			return false, err
		}
		return false, s.respond(resp)
	case commandClose:
		s.shutdown()
		return true, s.respond(&response{ID: req.ID})
	default:
		// The go command only sends commands this helper advertised, so an
		// unknown one is a protocol break rather than a request to refuse
		// silently.
		return false, s.respond(&response{ID: req.ID, Err: fmt.Sprintf("unknown command %q", req.Command)})
	}
}

// handleGet answers one lookup: local store, then the object cache.
func (s *server) handleGet(req *request) *response {
	miss := &response{ID: req.ID, Miss: true}
	if s.store == nil {
		return miss
	}
	actionID := hex.EncodeToString(req.ActionID)
	if found, ok := s.store.get(actionID); ok {
		return hitResponse(req.ID, found)
	}
	hit, blob, ok := s.remote.get(actionID)
	if !ok {
		return miss
	}
	// Meta carries the output id the object was stored with, and the digest
	// the address its bytes are staged at. Both come from the provider, so
	// their agreement proves only that the provider is consistent with itself;
	// it is a cheap way to refuse a malformed answer before touching the disk.
	// The check that matters is in ingest, which re-hashes the bytes and
	// refuses any that do not sum to the output id — the go command never
	// re-hashes what a helper hands it, so nothing downstream would.
	outputID := strings.TrimSpace(hit.Meta)
	if !validID(outputID) || hit.Digest != cache.DigestAlgorithm+":"+outputID {
		return miss
	}
	found, err := s.store.ingest(actionID, outputID, hit.Size, blob)
	if err != nil {
		s.reportOnce(err)
		return miss
	}
	s.remote.markServed()
	return hitResponse(req.ID, found)
}

// hitResponse renders a found entry the way the go command reads it.
func hitResponse(id int64, found entry) *response {
	outputID, err := hex.DecodeString(found.outputID)
	if err != nil {
		return &response{ID: id, Miss: true}
	}
	when := found.time
	return &response{
		ID:       id,
		OutputID: outputID,
		Size:     found.size,
		Time:     &when,
		DiskPath: found.diskPath,
	}
}

// handlePut stores one object locally, answers, and queues the upload.
//
// The local write happens before the answer, and the upload after it.
func (s *server) handlePut(req *request, reader *bufio.Reader) (*response, error) {
	body, err := bodyReader(reader, req.BodySize)
	if err != nil {
		return nil, err
	}
	if s.store == nil {
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return nil, drainErr
		}
		return &response{ID: req.ID, Err: "no local cache directory"}, nil
	}
	actionID := hex.EncodeToString(req.ActionID)
	outputID := hex.EncodeToString(req.OutputID)
	stored, err := s.store.put(actionID, outputID, req.BodySize, body)
	if err != nil {
		// A failed store must still leave the stream aligned: bodyReader
		// consumes the framing even when the write below it failed.
		if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
			return nil, drainErr
		}
		s.reportOnce(err)
		return &response{ID: req.ID, Err: err.Error()}, nil
	}
	if _, drainErr := io.Copy(io.Discard, body); drainErr != nil {
		return nil, drainErr
	}
	s.remote.queuePut(pendingPut{
		id:       actionID,
		outputID: outputID,
		size:     stored.size,
		source:   stored.diskPath,
	})
	return &response{ID: req.ID, DiskPath: stored.diskPath}, nil
}

// shutdown drains what is queued and waits for the in-flight lookups, so the
// process exits with its answers written and its uploads offered.
func (s *server) shutdown() {
	s.shutdownOnce.Do(func() {
		s.handlers.Wait()
		s.remote.close()
	})
}

// respond writes one response line. Writes are serialized.
func (s *server) respond(resp *response) error {
	line, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.out.Write(append(line, '\n')); err != nil {
		return err
	}
	return s.out.Flush()
}

// reportOnce prints the first degradation to stderr, and only under the debug
// flag. The go command wires our stderr to its own, so an unconditional line
// would land in the output of every job that compiles anything.
func (s *server) reportOnce(err error) {
	if err == nil || s.stderr == nil {
		return
	}
	if !debugEnabled(s.getenv) {
		return
	}
	s.reportOnly.Do(func() {
		fmt.Fprintf(s.stderr, "putnami-go gocacheprog: continuing with the local cache only: %v\n", err)
	})
}

// reportExchange prints what this process traded with the provider, under the
// same debug flag as the failure report.
//
// It is the answer to "is the shared build cache actually doing anything?",
// which is otherwise invisible: a run served entirely from the object cache and
// a run that recompiled everything produce the same build output, the same
// local cache and the same exit code. The line is written after the protocol
// stream is done, so it can never land between two responses.
func (s *server) reportExchange() {
	if s.remote == nil || s.stderr == nil || !debugEnabled(s.getenv) {
		return
	}
	served, offered := s.remote.counts()
	fmt.Fprintf(s.stderr, "putnami-go gocacheprog: object cache served %d objects, offered %d\n", served, offered)
}

func debugEnabled(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(getenv(debugEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// bodyReader returns a reader over the base64 body line that follows a put
// request, or an empty reader when the request carries no body.
//
// The body is STREAMED rather than decoded into memory: the go command puts
// linked binaries through this path, and a helper that buffered each one twice
// would be the reason a parallel build runs out of memory. Base64's alphabet
// contains no quote, so the closing quote of the JSON string literal is an
// unambiguous end marker.
//
// Leading blank space is SKIPPED, and that is not defensive tidiness: the go
// command encodes the request with a json.Encoder — which already ends its
// output with a newline — and then writes a newline of its own, so a real body
// line is preceded by an empty line. A reader that demanded the quote
// immediately would reject every put the go command sends.
func bodyReader(reader *bufio.Reader, size int64) (io.Reader, error) {
	if size <= 0 {
		return strings.NewReader(""), nil
	}
	for {
		opening, err := reader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("gocacheprog: read body: %w", err)
		}
		switch opening {
		case '\n', '\r', ' ', '\t':
			continue
		case '"':
			return base64.NewDecoder(base64.StdEncoding, &quotedReader{reader: reader}), nil
		default:
			return nil, fmt.Errorf("gocacheprog: body is not a JSON string (got %q)", string(opening))
		}
	}
}

// quotedReader reads the contents of a JSON string literal whose closing quote
// has not been consumed yet, and consumes the quote and its trailing newline
// when it reaches them.
type quotedReader struct {
	reader *bufio.Reader
	done   bool
}

func (q *quotedReader) Read(p []byte) (int, error) {
	if q.done {
		return 0, io.EOF
	}
	for i := range p {
		b, err := q.reader.ReadByte()
		if err != nil {
			return i, err
		}
		if b == '"' {
			q.done = true
			// The go command writes "\n" after the closing quote; leaving it
			// unread would make the next request line start with a blank.
			if next, peekErr := q.reader.Peek(1); peekErr == nil && next[0] == '\n' {
				_, _ = q.reader.Discard(1)
			}
			return i, io.EOF
		}
		p[i] = b
	}
	return len(p), nil
}
