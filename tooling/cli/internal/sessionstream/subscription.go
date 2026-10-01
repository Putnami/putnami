package sessionstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/cli/internal/flock"
)

// Subscription is one declared subscriber of a Log.
type Subscription struct {
	log  *Log
	name string
	wake chan struct{}

	mu          sync.Mutex
	file        *os.File // read-only handle on the stream; nil after Close
	acked       Position
	finalAcked  bool
	cursor      Position // Next's read position; independent of acknowledgements
	buf         []byte   // Next's reusable read-ahead buffer
	window      []byte   // the committed bytes buf currently holds
	windowStart int64    // the stream offset of window[0]
	served      servedRange
}

// servedRange is the last range Read returned, with the records it counted in
// the bytes it served. Ack reuses the count when it acknowledges exactly that
// range, so a subscriber that acknowledges what it just read costs no second
// read from disk. The count is the stream's own, never a caller's.
type servedRange struct {
	start, end, records int64
	valid               bool
}

// Record is one record of the stream, without its LF.
type Record struct {
	// Position is where the record starts. Position.Records is its zero-based
	// line ordinal.
	Position Position
	// Data is the record's bytes.
	Data []byte
	// Terminated is false only for trailing bytes of a final stream that no LF
	// ends: a torn last write.
	Terminated bool
}

// Name is the subscriber's declared name.
func (s *Subscription) Name() string { return s.name }

// Wake signals that the stream may have grown or become final. A signal is
// never queued twice, so a subscriber re-reads the extent after each one.
func (s *Subscription) Wake() <-chan struct{} { return s.wake }

// Extent is the stream's committed end and whether it is final.
func (s *Subscription) Extent() (Position, bool) { return s.log.Extent() }

// Read returns up to max bytes of the stream starting at offset, read from the
// file on disk and never past the committed extent. An empty result at the end
// of a stream that is not final means "wait for Wake".
func (s *Subscription) Read(offset int64, max int) ([]byte, error) {
	end, _ := s.log.Extent()
	if offset < 0 || offset > end.Offset {
		return nil, fmt.Errorf("event stream position %d is outside the stream", offset)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil, fmt.Errorf("subscription %s is closed", s.name)
	}
	data := make([]byte, min(int64(max), end.Offset-offset))
	read, err := s.file.ReadAt(data, offset)
	if read < len(data) {
		if err == nil || errors.Is(err, io.EOF) {
			err = fmt.Errorf("event stream changed on disk")
		}
		return nil, err
	}
	s.served = servedRange{start: offset, end: offset + int64(len(data)), records: int64(bytes.Count(data, []byte{'\n'})), valid: true}
	return data, nil
}

// Next returns the record at the read cursor and advances past it. It reports
// false when no complete record is committed yet; on a final stream it returns
// trailing unterminated bytes once, as a record with Terminated false.
func (s *Subscription) Next() (Record, bool, error) {
	end, final := s.log.Extent()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return Record{}, false, fmt.Errorf("subscription %s is closed", s.name)
	}
	start := s.cursor
	var data []byte
	for offset := start.Offset; offset < end.Offset; {
		chunk, err := s.windowAt(offset, end.Offset)
		if err != nil {
			return Record{}, false, err
		}
		if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
			data = append(data, chunk[:i]...)
			s.cursor = Position{Offset: offset + int64(i) + 1, Records: start.Records + 1}
			return Record{Position: start, Data: data, Terminated: true}, true, nil
		}
		data = append(data, chunk...)
		offset += int64(len(chunk))
	}
	if final && end.Offset > start.Offset {
		s.cursor = Position{Offset: end.Offset, Records: start.Records}
		return Record{Position: start, Data: data}, true, nil
	}
	return Record{}, false, nil
}

// windowAt returns committed bytes from offset out of a read-ahead window,
// refilling the window from disk when offset lies outside it. The stream is
// append-only, so bytes already in the window never go stale; a record copies
// out of the window and never aliases it.
func (s *Subscription) windowAt(offset, end int64) ([]byte, error) {
	if offset >= s.windowStart && offset < s.windowStart+int64(len(s.window)) {
		return s.window[offset-s.windowStart:], nil
	}
	if s.buf == nil {
		s.buf = make([]byte, readChunk)
	}
	n := min(int64(len(s.buf)), end-offset)
	read, err := s.file.ReadAt(s.buf[:n], offset)
	if int64(read) < n {
		if err == nil || errors.Is(err, io.EOF) {
			err = fmt.Errorf("event stream changed on disk")
		}
		return nil, err
	}
	s.window, s.windowStart = s.buf[:n], offset
	return s.window, nil
}

// Ack acknowledges every byte before offset. Acknowledgements only move
// forward: there is no reset. final additionally acknowledges the terminal
// marker, which requires a final stream and offset at its end.
func (s *Subscription) Ack(offset int64, final bool) error {
	end, closed := s.log.Extent()
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset < s.acked.Offset || offset > end.Offset {
		return fmt.Errorf("subscriber %s acknowledged position %d outside [%d, %d]", s.name, offset, s.acked.Offset, end.Offset)
	}
	if final && (!closed || offset != end.Offset) {
		return fmt.Errorf("subscriber %s acknowledged a final marker the stream has not reached", s.name)
	}
	if s.file == nil {
		return fmt.Errorf("subscription %s is closed", s.name)
	}
	var records int64
	if served := s.served; served.valid && served.start == s.acked.Offset && served.end == offset {
		records = served.records
	} else {
		counted, err := countRecords(s.file, s.acked.Offset, offset)
		if err != nil {
			return err
		}
		records = counted
	}
	s.acked = Position{Offset: offset, Records: s.acked.Records + records}
	s.finalAcked = s.finalAcked || final
	return nil
}

// Close withdraws the subscriber from the stream — it is no longer woken, and its
// name can be declared again — and releases its file handle. Its
// acknowledgements stay, so its evidence can still be classified and recorded.
func (s *Subscription) Close() error {
	// The log lock is taken and released on its own, never nested inside s.mu:
	// no path may hold s.mu while waiting for l.mu.
	s.log.mu.Lock()
	if s.log.subs[s.name] == s {
		delete(s.log.subs, s.name)
	}
	s.log.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Evidence classifies the subscriber against the final stream.
func (s *Subscription) Evidence() (protocolcli.SessionSubscriberEvidence, error) {
	end, final := s.log.Extent()
	if !final {
		return protocolcli.SessionSubscriberEvidence{}, fmt.Errorf("event stream is not final")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return protocolcli.NewSessionSubscriberEvidence(s.name, end, s.acked, s.finalAcked), nil
}

// RecordEvidence writes this subscriber's evidence into subscribers.json beside
// the session, keeping every other subscriber's entry. subscribers.lock beside
// it is locked for the read-merge-write, and the document is replaced
// atomically.
func (s *Subscription) RecordEvidence() error {
	evidence, err := s.Evidence()
	if err != nil {
		return err
	}
	end, _ := s.log.Extent()
	return mergeEvidence(s.log.dir, s.log.sessionID, end, evidence)
}

// ReadEvidence reads a session directory's subscribers.json. A session without
// live subscribers has none, which is reported as os.ErrNotExist.
func ReadEvidence(dir string) (*protocolcli.SessionSubscribersFile, error) {
	f, err := os.Open(filepath.Join(dir, protocolcli.SessionSubscribersFileName))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, protocolcli.SessionSubscribersMaxBytes+1))
	if err != nil {
		return nil, err
	}
	return protocolcli.ParseSessionSubscribersFile(data)
}

// subscribersLockName is the file whose exclusive lock serializes the writers
// of subscribers.json in one session directory.
const subscribersLockName = "subscribers.lock"

func mergeEvidence(dir, sessionID string, stream Position, evidence protocolcli.SessionSubscriberEvidence) error {
	lock, err := flock.Acquire(filepath.Join(dir, subscribersLockName), true, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	document := protocolcli.SessionSubscribersFile{ProtocolVersion: protocolcli.SessionSubscribersVersion, SessionID: sessionID, Stream: stream}
	// A prior document for the same session and extent keeps its other
	// subscribers. Any other prior document describes a different stream and is
	// replaced rather than merged into a claim it never made.
	if prior, err := ReadEvidence(dir); err == nil && prior.SessionID == sessionID && prior.Stream == stream {
		for _, entry := range prior.Subscribers {
			if entry.Name != evidence.Name {
				document.Subscribers = append(document.Subscribers, entry)
			}
		}
	}
	document.Subscribers = append(document.Subscribers, evidence)
	slices.SortFunc(document.Subscribers, func(a, b protocolcli.SessionSubscriberEvidence) int {
		return strings.Compare(a.Name, b.Name)
	})
	if err := document.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(dir, protocolcli.SessionSubscribersFileName, append(data, '\n'))
}

// writeAtomic replaces dir/name with data through a staged file and a rename.
// The rename waits for a reader that holds the document open, which fails it
// on Windows (robustio): ReadEvidence reads without the writers' lock.
func writeAtomic(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".subscribers-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := robustio.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return flock.SyncDir(dir)
}
