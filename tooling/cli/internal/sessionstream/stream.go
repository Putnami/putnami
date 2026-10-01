// Package sessionstream is the one event stream of a recorded session.
//
// The stream IS the session's events.jsonl. The engine's session recorder is its
// only producer (Create): it appends one LF-terminated record per write and
// never rewrites a byte. Nothing is added to a record: a position is a byte
// offset, and its record count is derived from the log itself, so the file stays
// byte-identical to what the recorder wrote before this package existed.
//
// Every consumer is a declared subscriber (Subscribe). A subscriber starts from a
// position, reads bytes from the file on disk by offset, and acknowledges
// positions explicitly. The producer never waits for one: it wakes subscribers
// with a non-blocking send on a one-slot channel, so a slow or dead subscriber
// cannot delay a task. Close is the terminal final marker.
//
// After a subscriber stops, RecordEvidence states in subscribers.json how much of
// the stream it acknowledged (delivered, partial or lost). A reader of a stream
// recorded by another process (Open) uses the same subscription and records no
// evidence.
package sessionstream

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	protocolcli "go.putnami.dev/protocol/cli"
)

// EventsFile is the stream's file name inside the session directory.
const EventsFile = "events.jsonl"

// readChunk bounds one read from disk while counting or scanning records.
const readChunk = 64 * 1024

// Position is a stream position: a byte offset and the LF-terminated records
// that end at or before it.
type Position = protocolcli.SessionStreamPosition

var subscriberName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// Log is one session's event stream.
type Log struct {
	dir       string
	sessionID string

	mu    sync.Mutex
	file  *os.File // the producer's append handle; nil once closed or when opened read-only
	end   Position // committed extent: every byte a completed write put on disk
	final bool
	subs  map[string]*Subscription
}

// Create opens the stream of a new session for append. The caller is the
// session's only producer.
func Create(dir, sessionID string) (*Log, error) {
	f, err := os.OpenFile(filepath.Join(dir, EventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	log := &Log{dir: dir, sessionID: sessionID, file: f, subs: map[string]*Subscription{}}
	if info.Size() > 0 {
		records, err := countRecords(f, 0, info.Size())
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		log.end = Position{Offset: info.Size(), Records: records}
	}
	return log, nil
}

// Open returns a stream another process recorded, as it is on disk now. Its
// extent is fixed and it is already final, so it never wakes a subscriber.
func Open(dir, sessionID string) (*Log, error) {
	f, err := os.Open(filepath.Join(dir, EventsFile))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("event stream is not a regular file")
	}
	records, err := countRecords(f, 0, info.Size())
	if err != nil {
		return nil, err
	}
	return &Log{dir: dir, sessionID: sessionID, end: Position{Offset: info.Size(), Records: records}, final: true, subs: map[string]*Subscription{}}, nil
}

// Dir is the session directory the stream lives in.
func (l *Log) Dir() string { return l.dir }

// Append writes one record and its LF in a single write, then wakes every
// subscriber without waiting for any of them. Appending to a closed stream is a
// silent no-op, which is what the session recorder has always done.
func (l *Log) Append(record []byte) error {
	line := make([]byte, len(record)+1)
	copy(line, record)
	line[len(record)] = '\n'

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	n, err := l.file.Write(line)
	l.end.Offset += int64(n)
	l.end.Records += int64(bytes.Count(line[:n], []byte{'\n'}))
	l.wakeLocked()
	return err
}

// Close is the terminal final marker: no record follows it.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.final {
		return nil
	}
	l.final = true
	var err error
	if l.file != nil {
		err = l.file.Close()
		l.file = nil
	}
	l.wakeLocked()
	return err
}

// Extent returns the committed end of the stream and whether it is final.
func (l *Log) Extent() (Position, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.end, l.final
}

func (l *Log) wakeLocked() {
	for _, sub := range l.subs {
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
}

// Subscribe declares a named subscriber that starts at byte offset from. Every
// byte before from counts as already acknowledged: a subscriber resuming a
// durable cursor passes that cursor, a new reader passes 0.
func (l *Log) Subscribe(name string, from int64) (*Subscription, error) {
	if !subscriberName.MatchString(name) {
		return nil, fmt.Errorf("invalid subscriber name %q", name)
	}
	end, _ := l.Extent()
	if from < 0 || from > end.Offset {
		return nil, fmt.Errorf("subscriber %s starts outside the event stream", name)
	}
	f, err := os.Open(filepath.Join(l.dir, EventsFile))
	if err != nil {
		return nil, err
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() < end.Offset {
		_ = f.Close()
		return nil, fmt.Errorf("event stream changed on disk")
	}
	records, err := countRecords(f, 0, from)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	start := Position{Offset: from, Records: records}
	sub := &Subscription{log: l, name: name, file: f, wake: make(chan struct{}, 1), acked: start, cursor: start}
	sub.wake <- struct{}{}

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.subs[name]; exists {
		_ = f.Close()
		return nil, fmt.Errorf("subscriber %s is already declared", name)
	}
	l.subs[name] = sub
	return sub, nil
}

// countRecords counts the LF bytes in [from, to) of f.
func countRecords(f *os.File, from, to int64) (int64, error) {
	var count int64
	buf := make([]byte, readChunk)
	for offset := from; offset < to; {
		n := min(int64(len(buf)), to-offset)
		read, err := f.ReadAt(buf[:n], offset)
		count += int64(bytes.Count(buf[:read], []byte{'\n'}))
		offset += int64(read)
		if int64(read) < n {
			if err == nil || errors.Is(err, io.EOF) {
				err = fmt.Errorf("event stream changed on disk")
			}
			return 0, err
		}
	}
	return count, nil
}
