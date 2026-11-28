package sessionreporter

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/sessionrefs"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// A session on packages (ADR 0135). Once a plan.json acknowledgement offers
// packages (observePackages) and plan.json closed, the worker stops framing:
// a collector cuts events.jsonl into task packages as a subscriber reads it,
// and whenever one of maxPackagesInFlight slots is free, every package sealed
// since leaves in one put over the reporter's pipe, so the count of puts
// follows the round trips, not the tasks. session.json and
// spec-verification.json leave as packages of their own, cut in parts, and a
// final manifest of every address closes the session.
const (
	// maxPackagesInFlight bounds the puts outstanding at once.
	maxPackagesInFlight = 8
	// documentPartBytes bounds the bytes of a document each part of it
	// carries: a document of any size is delivered, in parts numbered from 0.
	documentPartBytes = protocolcli.SessionReportingPackageRecordBytes
	// putDeadlinePerMiB is what a put's deadline adds to operationTimeout for
	// each MiB its packages begin: eight puts share one uplink.
	putDeadlinePerMiB = 2 * time.Second
	// putEnvelopeBytes and putPackageBytes bound the JSON a put line adds to
	// the base64 of its packages: its identity, and each package's kind,
	// address and punctuation.
	putEnvelopeBytes = 512
	putPackageBytes  = 128
	// packageRetryDelay is how long a put waits before its next attempt
	// after a refusal with retry or a failure in transport.
	packageRetryDelay = 100 * time.Millisecond
	// collectorScanBytes bounds the raw bytes one collector step reads
	// without sealing a package, so the worker keeps taking results while a
	// long task accumulates records.
	collectorScanBytes = 1 << 20
	// collectorReadGap and collectorReadSpan bound how a sealed package reads
	// its records back: records of one package whose raw gap is at most
	// collectorReadGap share one read of at most collectorReadSpan bytes.
	collectorReadGap  = 4 * 1024
	collectorReadSpan = 1 << 20
)

// errPackageTooLarge is an events.jsonl record whose package, with its
// bundle line, exceeds protocolcli.SessionReportingMaxPackageBytes: no put
// can carry it.
var errPackageTooLarge = errors.New("an events.jsonl record exceeds the reporting package bound")

// packagesState is what a checkpoint records of a session on packages. Every
// package of the session has an index: the task packages in sealing order,
// then session.json, then spec-verification.json. Acknowledged counts the
// packages from index 0 that the reporter acknowledged, every one of them, and
// Addresses names each later package it acknowledged. A replay rebuilds the
// same packages from raw offset 0 and sends only the others: a package's
// bytes, and so its address, depend on the stream's bytes only. Manifest
// records that the reporter acknowledged the final manifest, which completes
// the delivery.
type packagesState struct {
	Acknowledged int      `json:"acknowledged"`
	Addresses    []string `json:"addresses,omitempty"`
	Manifest     bool     `json:"manifest,omitempty"`
}

// progressed reports whether the reporter acknowledged a package or the
// manifest.
func (p *packagesState) progressed() bool {
	return p.Acknowledged > 0 || len(p.Addresses) > 0 || p.Manifest
}

// validatePackages checks a checkpoint on packages: no artifact but plan.json
// was framed, and every acknowledged address is a digest named once.
func (r *Run) validatePackages() error {
	s := r.state
	if s.Events != (cursor{}) || s.Session != (cursor{}) || s.SpecVerification != (cursor{}) || s.Pending != nil && s.Pending.Artifact != planFile {
		return fmt.Errorf("invalid reporting cursor")
	}
	if s.SpecVerificationOmitted != "" && !r.capability.sends(specVerificationFile) {
		return fmt.Errorf("invalid reporting cursor")
	}
	p := s.Packages
	if p.Acknowledged < 0 {
		return fmt.Errorf("invalid reporting packages")
	}
	seen := make(map[string]bool, len(p.Addresses))
	for _, address := range p.Addresses {
		if !validAddress(address) || seen[address] {
			return fmt.Errorf("invalid reporting package address")
		}
		seen[address] = true
	}
	return nil
}

// validAddress reports whether address is a lowercase hexadecimal SHA-256.
func validAddress(address string) bool {
	if len(address) != 64 {
		return false
	}
	for _, c := range address {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	_, err := hex.DecodeString(address)
	return err == nil
}

// collector cuts events.jsonl into task packages. It reads the stream by raw
// offset through the subscription, never past what the producer committed,
// splits it into records itself, LF-terminated lines and, once the stream is
// final, its unterminated tail, and keeps one open package per identity key:
// every record goes, byte for byte, to the package of its identity.key, ""
// for a record that has none or is not JSON. A package seals at its task's
// task:end record, before one more record would take its records past
// protocolcli.SessionReportingPackageRecordBytes (a single larger record
// travels alone), or when the stream is final, which seals every open package
// in the order of their first records. A later record of the key opens its
// next part. Its decisions depend on the stream's bytes only, never on time
// or on how much was committed when it read, so a collector built again from
// raw offset 0 seals the same packages, in the same order, with the same
// bytes.
//
// An open package keeps the raw range of each of its records, never their
// bytes: they are read back from the stream when it seals.
type collector struct {
	events *sessionstream.Subscription
	// scan is the raw offset of the first record not placed yet.
	scan int64
	// window holds committed raw bytes from windowStart. partial holds the
	// bytes of the record at scan read so far, when no window held its LF.
	window      []byte
	windowStart int64
	partial     []byte
	open        map[string]*taskPackage
	parts       map[string]int
	// unacknowledged is every package, open or sealed, in the order of its
	// first record, until the reporter acknowledged it and every one before
	// it (low).
	unacknowledged []*taskPackage
	// sealed are the packages sealed and not handed out yet, in sealing
	// order.
	sealed []*taskPackage
	// done reports that the stream is final and every package sealed.
	done bool
	// rebuild, when set, rebuilds what a task:end that names a cache entry
	// stands for (sessionrefs.Resolve): the records the entry holds, each
	// without its LF, and the task:end without its reference (ADR 0137).
	rebuild func(line []byte) ([][]byte, []byte, error)
	// err is the error that stopped the collector: a reference this CLI
	// cannot rebuild as its producer would have. No package past it seals.
	err error
}

// taskPackage is one package of a key's records, and their raw ranges while
// it is open. A record whose bytes are not the stream's, one rebuilt from a
// cache entry or a task:end without its reference, keeps them in data, at the
// raw offset of the task:end it stands before; rebuilt lists the indexes of
// those the stream does not hold.
type taskPackage struct {
	key     string
	part    int
	first   int64
	offsets []int64
	sizes   []int64
	data    [][]byte
	rebuilt []int
	bytes   int64
	open    bool
	acked   bool
}

// collectedRecord decodes the members a collector places a record by.
type collectedRecord struct {
	Record   string `json:"record"`
	Identity struct {
		Key string `json:"key"`
	} `json:"identity"`
	CacheReference json.RawMessage `json:"cacheReference"`
}

func newCollector(events *sessionstream.Subscription) *collector {
	return &collector{events: events, open: map[string]*taskPackage{}, parts: map[string]int{}}
}

// next returns the next sealed package, in sealing order. Without one, more
// reports whether the scan stopped at its limit of raw bytes, so that next
// may be called again at once, rather than at what is committed: the stream
// must grow first.
func (c *collector) next(limit int64) (pkg *taskPackage, more bool, err error) {
	if c.err != nil {
		return nil, false, c.err
	}
	start := c.scan
	for len(c.sealed) == 0 && !c.done {
		if c.scan-start >= limit {
			return nil, true, nil
		}
		extent, final := c.events.Extent()
		line, ok, err := c.nextLine(extent.Offset)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			if !final {
				return nil, false, nil
			}
			c.finish(extent.Offset)
			break
		}
		raw, size := c.scan, int64(len(line))+1
		c.scan += size
		c.place(line, raw, size)
		if c.err != nil {
			return nil, false, c.err
		}
		if cap(c.partial) > 1<<20 {
			c.partial = nil
		} else {
			c.partial = c.partial[:0]
		}
	}
	if len(c.sealed) == 0 {
		return nil, false, nil
	}
	pkg, c.sealed = c.sealed[0], c.sealed[1:]
	return pkg, true, nil
}

// nextLine returns the record at scan without its LF, or false when no LF
// before end ends it. A record may span any number of windows; one whose
// bytes exceed a package's bound fails.
func (c *collector) nextLine(end int64) ([]byte, bool, error) {
	for from := c.scan + int64(len(c.partial)); from < end; {
		chunk, err := c.windowAt(from, end)
		if err != nil {
			return nil, false, err
		}
		if i := indexLF(chunk); i >= 0 {
			if len(c.partial) == 0 {
				return chunk[:i], true, nil
			}
			c.partial = append(c.partial, chunk[:i]...)
			return c.partial, true, nil
		}
		if len(c.partial)+len(chunk) > protocolcli.SessionReportingMaxPackageBytes {
			return nil, false, errPackageTooLarge
		}
		c.partial = append(c.partial, chunk...)
		from += int64(len(chunk))
	}
	return nil, false, nil
}

func indexLF(data []byte) int {
	for i, b := range data {
		if b == '\n' {
			return i
		}
	}
	return -1
}

// windowAt returns committed raw bytes from offset, read through the
// subscription, which never reads past the committed extent.
func (c *collector) windowAt(offset, end int64) ([]byte, error) {
	if offset >= c.windowStart && offset < c.windowStart+int64(len(c.window)) {
		return c.window[offset-c.windowStart:], nil
	}
	data, err := c.events.Read(offset, int(min(int64(protocolcli.SessionReportingChunkBytes), end-offset)))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("event stream changed on disk")
	}
	c.window, c.windowStart = data, offset
	return data, nil
}

// finish places the unterminated tail of a final stream that ends at end, as
// one record without its LF, then seals every open package in the order of
// its first record.
func (c *collector) finish(end int64) {
	if c.scan < end {
		raw := c.scan
		c.scan = end
		c.place(c.partial, raw, end-raw)
		c.partial = nil
	}
	for _, p := range c.unacknowledged {
		if p.open {
			c.seal(p)
		}
	}
	c.done = true
}

// place adds one whole record, raw bytes [raw, raw+size), to the package of
// its key, sealing that package before the record when the record would take
// it past its bound, and after it at the record's task:end. A task:end that
// names a cache entry the collector rebuilds is placed as the records the
// entry holds, then itself without its reference, all at its raw offset; one
// whose entry is gone is placed as it is, and one this CLI cannot rebuild as
// its producer would have stops the collector.
func (c *collector) place(line []byte, raw, size int64) {
	var record collectedRecord
	key, end := "", false
	if json.Unmarshal(line, &record) == nil {
		key, end = record.Identity.Key, record.Record == protocolcli.RecordTaskEnd
	}
	if end && c.rebuild != nil && len(record.CacheReference) > 0 {
		records, stripped, err := c.rebuild(line)
		var projection *sessionrefs.ProjectionError
		switch {
		case errors.As(err, &projection):
			c.err = err
			return
		case err == nil:
			for _, rebuilt := range records {
				c.add(key, raw, int64(len(rebuilt))+1, append(rebuilt, '\n'), true)
			}
			c.seal(c.add(key, raw, int64(len(stripped))+1, append(stripped, '\n'), false))
			return
		}
	}
	p := c.add(key, raw, size, nil, false)
	if end {
		c.seal(p)
	}
}

// add appends one record of size bytes at raw offset raw to the open package
// of key, sealing that package first when the record would take it past its
// bound, and returns the package. data is the record's bytes when they are
// not the stream's, and rebuilt reports a record the stream does not hold.
func (c *collector) add(key string, raw, size int64, data []byte, rebuilt bool) *taskPackage {
	p := c.open[key]
	if p != nil && p.bytes+size > protocolcli.SessionReportingPackageRecordBytes {
		c.seal(p)
		p = nil
	}
	if p == nil {
		p = &taskPackage{key: key, part: c.parts[key], first: raw, open: true}
		c.parts[key]++
		c.open[key] = p
		c.unacknowledged = append(c.unacknowledged, p)
	}
	if rebuilt {
		p.rebuilt = append(p.rebuilt, len(p.offsets))
	}
	p.offsets = append(p.offsets, raw)
	p.sizes = append(p.sizes, size)
	p.data = append(p.data, data)
	p.bytes += size
	return p
}

// drained reports that every package sealed and was handed out.
func (c *collector) drained() bool {
	return c.done && len(c.sealed) == 0
}

func (c *collector) seal(p *taskPackage) {
	p.open = false
	delete(c.open, p.key)
	c.sealed = append(c.sealed, p)
}

// encode returns a sealed package's bytes: its header line, naming its key,
// its part and the raw offset of each record, then its records as persisted,
// read back from the stream. The package then keeps no ranges.
func (c *collector) encode(p *taskPackage) ([]byte, error) {
	key := p.key
	header := protocolcli.SessionReportingPackageHeader{Kind: protocolcli.SessionReportingPackageTask, Key: &key, Part: p.part, Offsets: p.offsets, Rebuilt: p.rebuilt}
	headerLine, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("encode reporting package")
	}
	if int64(len(headerLine))+1+p.bytes > protocolcli.SessionReportingMaxPackageBytes {
		return nil, errPackageTooLarge
	}
	body := make([]byte, 0, p.bytes)
	for i := 0; i < len(p.offsets); {
		if p.data[i] != nil {
			body = append(body, p.data[i]...)
			i++
			continue
		}
		start, end := p.offsets[i], p.offsets[i]+p.sizes[i]
		j := i + 1
		for j < len(p.offsets) && p.data[j] == nil && p.offsets[j]-end <= collectorReadGap && p.offsets[j]+p.sizes[j]-start <= collectorReadSpan {
			end = p.offsets[j] + p.sizes[j]
			j++
		}
		data, err := c.events.Read(start, int(end-start))
		if err != nil || int64(len(data)) != end-start {
			return nil, fmt.Errorf("reporting artifact changed: %s", sessionstream.EventsFile)
		}
		for k := i; k < j; k++ {
			body = append(body, data[p.offsets[k]-start:p.offsets[k]-start+p.sizes[k]]...)
		}
		i = j
	}
	data := protocolcli.EncodeSessionReportingPackage(header, body)
	p.offsets, p.sizes, p.data, p.rebuilt = nil, nil, nil, nil
	return data, nil
}

// low is the raw offset the subscription may acknowledge: the first record
// of the oldest package that is open or not acknowledged, or, without one,
// the first record not placed yet. It never moves back.
func (c *collector) low() int64 {
	for len(c.unacknowledged) > 0 && c.unacknowledged[0].acked {
		c.unacknowledged[0] = nil
		c.unacknowledged = c.unacknowledged[1:]
	}
	if len(c.unacknowledged) > 0 {
		return c.unacknowledged[0].first
	}
	return c.scan
}

// packageItem is one package of the session, at its index: a task package,
// or a part of a document after them.
type packageItem struct {
	index   int
	kind    string
	task    *taskPackage
	address string
	// data is the package's bytes until the reporter acknowledges it.
	data []byte
	// bundle is the bytes the package takes in the bundle a reporter posts:
	// its line, then its bytes (protocolcli.EncodeSessionReportingBundle).
	bundle int
	acked  bool
}

// bundleLineBytes is the length of the line, its LF included, that
// protocolcli.EncodeSessionReportingBundle writes before a package of kind
// holding length bytes. Every address has the same length, so the line's
// does not depend on which.
func bundleLineBytes(kind string, length int) int {
	line, _ := json.Marshal(struct {
		Kind    string `json:"kind"`
		Address string `json:"address"`
		Length  int    `json:"length"`
	}{kind, zeroAddress, length})
	return len(line) + 1
}

// zeroAddress stands for any package address in bundleLineBytes.
var zeroAddress = strings.Repeat("0", 64)

// putTimeout is how long the engine waits for the result of a line whose
// packages hold size bytes: base, then putDeadlinePerMiB for each MiB they
// begin.
func putTimeout(base time.Duration, size int) time.Duration {
	return base + time.Duration((size+1<<20-1)>>20)*putDeadlinePerMiB
}

// packageLine is one line the worker sends: a put of a batch of packages, or
// the manifest. A put keeps its packages, and so its address, across its
// attempts.
type packageLine struct {
	op      string
	address string
	items   []*packageItem
	// manifest is the manifest's line.
	manifest []byte
	// size is the bytes of its packages, or of the manifest's line, which
	// its deadline follows (putTimeout).
	size     int
	attempts int
	deadline time.Time
	retryAt  time.Time
	// abandoned: spec-verification.json was omitted while the line was in
	// flight; its result, whatever it is, changes nothing.
	abandoned bool
}

// key identifies the line's result: its op and its address.
func (l *packageLine) key() string {
	return l.op + ":" + l.address
}

// order places the line among the lines to send: by its first package's
// index, the manifest last.
func (l *packageLine) order() int {
	if len(l.items) == 0 {
		return math.MaxInt
	}
	return l.items[0].index
}

// specVerification reports whether the line is a put of parts of
// spec-verification.json, which travel apart from every other package, so
// that failing it omits that document alone (failed).
func (l *packageLine) specVerification() bool {
	return len(l.items) > 0 && l.items[0].kind == protocolcli.SessionReportingPackageSpecVerification
}

// packageWorker sends a session's packages. It runs on the worker's
// goroutine only; the pipeline's reader hands it results.
type packageWorker struct {
	r *Run
	c *collector
	// items are the packages sealed so far, by index.
	items []*packageItem
	// pending holds the packages sealed and in no put yet, by index, and
	// pendingBytes their bytes. queue holds the lines to send again, and the
	// manifest, by order; inFlight the lines sent and not answered, by key.
	pending      []*packageItem
	pendingBytes int
	queue        []*packageLine
	inFlight     map[string]*packageLine
	// acknowledged counts the items from index 0 that the reporter
	// acknowledged, at least what the checkpoint recorded.
	acknowledged int
	// restored names the packages past acknowledged that the checkpoint
	// recorded and no rebuilt package matched yet, in its order.
	restored      map[string]bool
	restoredOrder []string
	// raw is the raw offset the subscription acknowledged.
	raw int64
	// documents lists the documents not sealed whole yet, the next first,
	// once the stream drained and Finish was called; part is the next part
	// of documents[0], and size its size, read at its first part. sealed:
	// every document was sealed whole, or omitted.
	documents []string
	part      int
	size      int64
	sealed    bool
	manifest  *packageLine
	terminal  bool
	// starved: the collector read every committed record; the worker waits
	// for the stream to grow.
	starved bool
}

func (r *Run) newPackageWorker(terminal bool) *packageWorker {
	p := r.state.Packages
	c := newCollector(r.events)
	if entries, ok := r.referenceEntries(); ok {
		c.rebuild = func(line []byte) ([][]byte, []byte, error) { return sessionrefs.Resolve(entries, line) }
	}
	w := &packageWorker{r: r, c: c, inFlight: map[string]*packageLine{}, acknowledged: p.Acknowledged, restored: map[string]bool{}, raw: r.state.EventsRaw, terminal: terminal}
	for _, address := range p.Addresses {
		w.restored[address] = true
		w.restoredOrder = append(w.restoredOrder, address)
	}
	return w
}

// deliverPackages delivers the rest of a session on packages, from a
// collector built from raw offset 0: a package the checkpoint records as
// acknowledged is rebuilt, never sent again, and the others are batched
// anew. It returns once the reporter acknowledged the manifest, or with the
// error that stops the delivery; the checkpoint then records every package
// acknowledged so far.
func (r *Run) deliverPackages(terminal bool) error {
	w := r.newPackageWorker(terminal)
	defer w.record()
	for !r.state.Complete {
		if r.ctx.Err() != nil {
			return errBudgetExhausted
		}
		if !w.terminal {
			select {
			case <-r.finish:
				w.terminal = true
			default:
			}
		}
		more, err := w.produce()
		if err != nil {
			return err
		}
		// A new put takes every package sealed since the last one: it is cut
		// once the collector read what is committed, or sealed a put's worth.
		if err := w.dispatch(!more); err != nil {
			return err
		}
		if r.state.Complete {
			break
		}
		if err := w.await(more); err != nil {
			return err
		}
	}
	return nil
}

// pendingFull reports whether the packages in no put yet fill one.
func (w *packageWorker) pendingFull() bool {
	return w.pendingBytes >= protocolcli.SessionReportingMaxPackageBytes || len(w.pending) >= protocolcli.SessionReportingMaxPutPackages
}

// produce seals packages until the collector read every committed record or
// the packages in no put yet fill one, then, once the stream is final and
// Finish was called, the parts of the documents, as many as fill a put, then,
// once every package is acknowledged, queues the manifest. more reports that the collector stopped
// at its scan limit.
func (w *packageWorker) produce() (more bool, err error) {
	w.starved = false
	for !w.pendingFull() && !w.c.drained() {
		pkg, more, err := w.c.next(collectorScanBytes)
		if errors.Is(err, errPackageTooLarge) {
			return false, err
		}
		if err != nil {
			return false, eventsError(err)
		}
		if pkg == nil {
			if w.c.drained() {
				// The stream ended with no package open: the documents follow.
				break
			}
			w.starved = !more
			return more, nil
		}
		data, err := w.c.encode(pkg)
		if err != nil {
			return false, err
		}
		if len(data)+bundleLineBytes(protocolcli.SessionReportingPackageTask, len(data)) > protocolcli.SessionReportingMaxPackageBytes {
			return false, errPackageTooLarge
		}
		w.add(protocolcli.SessionReportingPackageTask, pkg, data)
	}
	if w.c.drained() && w.terminal && !w.sealed {
		for !w.sealed && !w.pendingFull() {
			if err := w.sealDocumentPart(); err != nil {
				return false, err
			}
		}
	}
	if w.sealed && w.manifest == nil && w.acknowledged >= len(w.items) {
		if err := w.queueManifest(); err != nil {
			return false, err
		}
	}
	return false, nil
}

// add takes the package of index len(items). One the checkpoint records as
// acknowledged, within the count it restored or by address, waits for no
// put.
func (w *packageWorker) add(kind string, task *taskPackage, data []byte) {
	it := &packageItem{index: len(w.items), kind: kind, task: task, address: protocolcli.SessionReportingDigest(data), data: data, bundle: len(data) + bundleLineBytes(kind, len(data))}
	w.items = append(w.items, it)
	if it.index < w.acknowledged || w.restored[it.address] {
		delete(w.restored, it.address)
		w.ack(it)
		return
	}
	w.pending = append(w.pending, it)
	w.pendingBytes += it.bundle
}

// sealDocumentPart seals the next part of the next document: session.json,
// then spec-verification.json, each cut in parts of documentPartBytes,
// numbered from 0, a package of its own kind each; an empty document is one
// empty part. session.json must stay readable, and as large as at its first
// part. spec-verification.json is best-effort as on frames: one that is
// absent, past its bound or unreadable at any part is omitted, every part
// of it (omitSpecVerification).
func (w *packageWorker) sealDocumentPart() error {
	r := w.r
	if w.documents == nil {
		w.documents = []string{sessionFile}
		if r.capability.sends(specVerificationFile) && r.state.SpecVerificationOmitted == "" {
			w.documents = append(w.documents, specVerificationFile)
		}
	}
	if len(w.documents) == 0 {
		w.sealed = true
		return nil
	}
	artifact := w.documents[0]
	kind := protocolcli.SessionReportingPackageSession
	if artifact == specVerificationFile {
		kind = protocolcli.SessionReportingPackageSpecVerification
	}
	if w.part == 0 {
		size, err := documentSize(r.dir, artifact)
		if err != nil {
			if artifact == specVerificationFile {
				return w.omitSpecVerification(specVerificationOmission(err))
			}
			return err
		}
		w.size = size
	}
	offset := int64(w.part) * documentPartBytes
	n := min(documentPartBytes, w.size-offset)
	data, err := readArtifact(r.dir, artifact, offset, int(n))
	if err == nil && int64(len(data)) != n {
		err = fmt.Errorf("reporting artifact changed: %s", artifact)
	}
	if err != nil {
		if artifact == specVerificationFile {
			return w.omitSpecVerification(specVerificationOmission(fmt.Errorf("%w: %w", errDocumentUnreadable, err)))
		}
		return err
	}
	w.add(kind, nil, protocolcli.EncodeSessionReportingPackage(protocolcli.SessionReportingPackageHeader{Kind: kind, Part: w.part}, data))
	w.part++
	if offset+n >= w.size {
		w.documents, w.part, w.size = w.documents[1:], 0, 0
		w.sealed = len(w.documents) == 0
	}
	return nil
}

// documentSize is the size of artifact, read before its first part.
// spec-verification.json past its bound, or absent, is refused as on frames
// (readDocumentFrame); session.json has no bound beyond its parts.
func documentSize(dir, artifact string) (int64, error) {
	info, err := os.Stat(filepath.Join(dir, artifact))
	if artifact == specVerificationFile {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return 0, errDocumentAbsent
		case err == nil && info.Size() > documentBounds[artifact]:
			return 0, errDocumentTooLarge
		}
	}
	if err != nil || !info.Mode().IsRegular() {
		if artifact == specVerificationFile {
			return 0, errDocumentUnreadable
		}
		return 0, fmt.Errorf("reporting artifact unavailable: %s", artifact)
	}
	return info.Size(), nil
}

// omitSpecVerification omits spec-verification.json, every part of it, and
// records why: its packages leave the session's, its puts waiting to leave
// are dropped, and a put of it in flight is abandoned. The delivery goes on
// without it.
func (w *packageWorker) omitSpecVerification(reason string) error {
	spec := func(it *packageItem) bool { return it.kind == protocolcli.SessionReportingPackageSpecVerification }
	for len(w.items) > 0 && spec(w.items[len(w.items)-1]) {
		w.items = w.items[:len(w.items)-1]
	}
	w.acknowledged = min(w.acknowledged, len(w.items))
	w.pending = slices.DeleteFunc(w.pending, spec)
	w.pendingBytes = 0
	for _, it := range w.pending {
		w.pendingBytes += it.bundle
	}
	w.queue = slices.DeleteFunc(w.queue, (*packageLine).specVerification)
	for _, l := range w.inFlight {
		if l.specVerification() {
			l.abandoned = true
		}
	}
	w.documents = slices.DeleteFunc(w.documents, func(d string) bool { return d == specVerificationFile })
	w.part, w.size = 0, 0
	if w.documents != nil && len(w.documents) == 0 {
		w.sealed = true
	}
	w.record()
	return w.r.omit(specVerificationFile, reason)
}

// queueManifest queues the final manifest: every package's address, in
// sealing order, the documents last.
func (w *packageWorker) queueManifest() error {
	addresses := make([]string, len(w.items))
	for i, it := range w.items {
		addresses[i] = it.address
	}
	manifest := protocolcli.NewSessionReportingManifest(w.r.state.SessionID, addresses)
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("invalid reporting manifest")
	}
	line, err := json.Marshal(manifest)
	if err != nil || len(line) > protocolcli.SessionReportingMaxPackageLineBytes {
		return fmt.Errorf("reporting manifest exceeds its bound")
	}
	w.manifest = &packageLine{op: protocolcli.SessionReportingOpManifest, address: manifest.Address(), manifest: line, size: len(line)}
	w.queue = append(w.queue, w.manifest)
	return nil
}

// cut takes, from the packages in no put yet, a put of as many as one put
// carries, by index: at most SessionReportingMaxPutPackages packages, whose
// bundle, each package's line and bytes, stays within
// SessionReportingMaxPackageBytes, on a line within
// SessionReportingMaxPackageLineBytes. The parts of spec-verification.json
// travel apart from every other package, so that a refusal of their put
// omits that document and nothing else. It returns nil when no package
// waits.
func (w *packageWorker) cut() *packageLine {
	if len(w.pending) == 0 {
		return nil
	}
	spec := w.pending[0].kind == protocolcli.SessionReportingPackageSpecVerification
	n, bundle, line := 0, 0, putEnvelopeBytes
	for n < len(w.pending) && n < protocolcli.SessionReportingMaxPutPackages {
		it := w.pending[n]
		if (it.kind == protocolcli.SessionReportingPackageSpecVerification) != spec {
			break
		}
		encoded := putPackageBytes + 4*((len(it.data)+2)/3)
		if n > 0 && (bundle+it.bundle > protocolcli.SessionReportingMaxPackageBytes || line+encoded > protocolcli.SessionReportingMaxPackageLineBytes) {
			break
		}
		n, bundle, line = n+1, bundle+it.bundle, line+encoded
	}
	items := slices.Clone(w.pending[:n])
	w.pending = slices.Delete(w.pending, 0, n)
	w.pendingBytes -= bundle
	put := protocolcli.NewSessionReportingPut(w.r.state.SessionID, packagesOf(items))
	// The deadline follows the decoded bundle, the size the reporter's own
	// request deadline follows, so the reporter answers first (ADR 0135 §2).
	return &packageLine{op: protocolcli.SessionReportingOpPut, address: put.Address, items: items, size: bundle}
}

// packagesOf is the wire form of items.
func packagesOf(items []*packageItem) []protocolcli.SessionReportingPackage {
	packages := make([]protocolcli.SessionReportingPackage, len(items))
	for i, it := range items {
		packages[i] = protocolcli.SessionReportingPackage{Kind: it.kind, Address: it.address, Data: it.data}
	}
	return packages
}

// dispatch sends lines while fewer than maxPackagesInFlight are in flight,
// starting the reporter when no process serves the capability: first the
// queued lines whose retry delay ended, by order, then, when cut allows, a
// new put of the packages sealed since.
func (w *packageWorker) dispatch(cut bool) error {
	r := w.r
	now := time.Now()
	for len(w.inFlight) < maxPackagesInFlight {
		var l *packageLine
		if i := slices.IndexFunc(w.queue, func(q *packageLine) bool { return !q.retryAt.After(now) }); i >= 0 {
			l = w.queue[i]
			w.queue = slices.Delete(w.queue, i, i+1)
		} else if cut {
			l = w.cut()
		}
		if l == nil {
			return nil
		}
		if r.pipe == nil {
			if r.process == nil {
				p, err := r.start()
				if err != nil {
					return err
				}
				r.process = p
			}
			r.pipe = r.process.pipelined(maxPackagesInFlight + 1)
		}
		line, err := w.line(l)
		if err != nil {
			return err
		}
		l.attempts++
		timeout := putTimeout(r.opTimeout, l.size)
		l.deadline = time.Now().Add(timeout)
		w.inFlight[l.key()] = l
		opCtx, cancel := context.WithTimeout(r.ctx, timeout)
		err = r.pipe.write(opCtx, line)
		cancel()
		if err != nil {
			return w.transportFailure()
		}
	}
	return nil
}

// line is the bytes of l: a put of its packages, or the manifest.
func (w *packageWorker) line(l *packageLine) ([]byte, error) {
	if l.op == protocolcli.SessionReportingOpManifest {
		return l.manifest, nil
	}
	line, err := json.Marshal(protocolcli.NewSessionReportingPut(w.r.state.SessionID, packagesOf(l.items)))
	if err != nil || len(line) > protocolcli.SessionReportingMaxPackageLineBytes {
		return nil, fmt.Errorf("reporting put exceeds its bound")
	}
	return line, nil
}

// await waits for what moves the delivery on: a result, the reporter's exit,
// a line past its deadline or a retry delay that ended, a stream that grew
// when the collector waits for it, Finish, or the budget. With more, it only
// takes the results already there.
func (w *packageWorker) await(more bool) error {
	r := w.r
	var results <-chan *protocolcli.SessionReportingResult
	var closed <-chan struct{}
	if r.pipe != nil {
		results, closed = r.pipe.results, r.pipe.closed
	}
	if more {
		select {
		case result := <-results:
			return w.answer(result)
		case <-closed:
			return w.transportFailure()
		default:
			return nil
		}
	}
	var wake, finish <-chan struct{}
	if w.starved {
		wake = r.events.Wake()
	}
	if !w.terminal {
		finish = r.finish
	}
	var timer <-chan time.Time
	if next, ok := w.nextTimer(); ok {
		t := time.NewTimer(time.Until(next))
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-r.ctx.Done():
		return errBudgetExhausted
	case result := <-results:
		return w.answer(result)
	case <-closed:
		return w.transportFailure()
	case <-wake:
	case <-finish:
		w.terminal = true
	case <-timer:
		now := time.Now()
		for _, l := range w.inFlight {
			if !now.Before(l.deadline) {
				return w.transportFailure()
			}
		}
	}
	return nil
}

// nextTimer is the earliest deadline of a line in flight or, while a line
// may still leave, end of a retry delay to come.
func (w *packageWorker) nextTimer() (time.Time, bool) {
	var next time.Time
	for _, l := range w.inFlight {
		if next.IsZero() || l.deadline.Before(next) {
			next = l.deadline
		}
	}
	if len(w.inFlight) < maxPackagesInFlight {
		now := time.Now()
		for _, l := range w.queue {
			if l.retryAt.After(now) && (next.IsZero() || l.retryAt.Before(next)) {
				next = l.retryAt
			}
		}
	}
	return next, !next.IsZero()
}

// answer takes result and every result already handed over after it.
func (w *packageWorker) answer(result *protocolcli.SessionReportingResult) error {
	results := []*protocolcli.SessionReportingResult{result}
	for more := true; more; {
		select {
		case next := <-w.r.pipe.results:
			results = append(results, next)
		default:
			more = false
		}
	}
	return w.take(results)
}

// take records results, in order, then saves the checkpoint once and
// acknowledges, only then, the raw offset of the subscription. A result that
// stops the delivery ends the batch: the acceptances before it are still
// saved, and acknowledged on the subscription, before its error returns.
func (w *packageWorker) take(results []*protocolcli.SessionReportingResult) error {
	accepted := false
	var stop error
	for _, result := range results {
		if stop = w.result(result, &accepted); stop != nil || w.r.state.Complete {
			break
		}
	}
	if accepted {
		if err := w.save(); err != nil {
			return errors.Join(stop, err)
		}
	}
	return stop
}

// result records one result: an accepted put acknowledges each of its
// packages. accepted is set when it did.
func (w *packageWorker) result(result *protocolcli.SessionReportingResult, accepted *bool) error {
	l := w.inFlight[result.Op+":"+result.Address]
	if l == nil || result.SessionID != w.r.state.SessionID {
		return errAckMismatch
	}
	delete(w.inFlight, l.key())
	switch {
	case l.abandoned:
	case result.OK && l == w.manifest:
		return w.complete()
	case result.OK:
		for _, it := range l.items {
			w.ack(it)
		}
		*accepted = true
	case result.Retryable && l.attempts < maxAttempts:
		l.retryAt = time.Now().Add(packageRetryDelay)
		w.requeue(l)
	case result.Retryable:
		return w.failed(l, errRetryBudget)
	default:
		return w.failed(l, &chunkFailure{refused: true, reason: packageRefusal(l, result.Code)})
	}
	return nil
}

// save records the packages acknowledged in the checkpoint, makes it
// durable, then acknowledges the subscription's raw offset.
func (w *packageWorker) save() error {
	r := w.r
	w.record()
	r.state.EventsRaw = max(r.state.EventsRaw, w.c.low())
	if err := r.save(); err != nil {
		return err
	}
	r.acknowledged()
	if r.state.EventsRaw > w.raw {
		// Acknowledged only once the checkpoint is durable. A refused
		// acknowledgement can only make the evidence under-report delivery.
		if r.events.Ack(r.state.EventsRaw, false) == nil {
			w.raw = r.state.EventsRaw
		}
	}
	return nil
}

// complete records the acknowledged manifest, which completes the delivery,
// and acknowledges the whole stream and its final marker.
func (w *packageWorker) complete() error {
	r := w.r
	w.record()
	end, _ := r.events.Extent()
	r.state.Packages.Manifest = true
	r.state.EventsRaw = end.Offset
	r.state.Complete = r.complete()
	if err := r.save(); err != nil {
		return err
	}
	r.acknowledged()
	_ = r.events.Ack(end.Offset, true)
	return nil
}

// ack marks it acknowledged and releases its bytes.
func (w *packageWorker) ack(it *packageItem) {
	it.acked, it.data = true, nil
	if it.task != nil {
		it.task.acked = true
	}
	for w.acknowledged < len(w.items) && w.items[w.acknowledged].acked {
		w.acknowledged++
	}
}

// record writes into the checkpoint state what the reporter acknowledged:
// the count from index 0, then each later package acknowledged, then those
// the checkpoint recorded that no rebuilt package matched yet.
func (w *packageWorker) record() {
	p := w.r.state.Packages
	p.Acknowledged = w.acknowledged
	p.Addresses = nil
	for _, it := range w.items[min(w.acknowledged, len(w.items)):] {
		if it.acked {
			p.Addresses = append(p.Addresses, it.address)
		}
	}
	for _, address := range w.restoredOrder {
		if w.restored[address] {
			p.Addresses = append(p.Addresses, address)
		}
	}
}

// requeue queues l to be sent again, by order.
func (w *packageWorker) requeue(l *packageLine) {
	at, _ := slices.BinarySearchFunc(w.queue, l.order(), func(q *packageLine, order int) int { return cmp.Compare(q.order(), order) })
	w.queue = slices.Insert(w.queue, at, l)
}

// failed ends the attempts of l after err. A put of parts of
// spec-verification.json omits that document, as on frames, and the delivery
// goes on without it; any other line stops the delivery, and a replay sends
// its packages again.
func (w *packageWorker) failed(l *packageLine, err error) error {
	if !l.specVerification() {
		return err
	}
	return w.omitSpecVerification(specVerificationOmission(err))
}

// transportFailure closes a reporter that exited, closed its pipe, wrote a
// line that is no result, or let a line pass its deadline, after taking the
// results it wrote before. Every line in flight counts a failed attempt and
// goes again, the same line, on a new reporter, after packageRetryDelay; one
// that used its last attempt fails.
func (w *packageWorker) transportFailure() error {
	r := w.r
	var results []*protocolcli.SessionReportingResult
	for more := true; more; {
		select {
		case result := <-r.pipe.results:
			results = append(results, result)
		default:
			more = false
		}
	}
	r.pipe.close()
	r.pipe, r.process = nil, nil
	if len(results) > 0 {
		if err := w.take(results); err != nil {
			return err
		}
	}
	if r.ctx.Err() != nil {
		return errBudgetExhausted
	}
	lost := make([]*packageLine, 0, len(w.inFlight))
	for _, l := range w.inFlight {
		lost = append(lost, l)
	}
	slices.SortFunc(lost, func(a, b *packageLine) int { return cmp.Compare(a.order(), b.order()) })
	clear(w.inFlight)
	retryAt := time.Now().Add(packageRetryDelay)
	for _, l := range lost {
		if l.abandoned {
			continue
		}
		if l.attempts >= maxAttempts {
			if err := w.failed(l, &chunkFailure{reason: errRetryBudget.Error()}); err != nil {
				return err
			}
			continue
		}
		l.retryAt = retryAt
		w.requeue(l)
	}
	return nil
}

// packageRefusal names the line a reporter refused without retry, and the
// machine code it gave (ParseSessionReportingResult bounds it).
func packageRefusal(l *packageLine, code string) string {
	if l.op == protocolcli.SessionReportingOpManifest {
		return fmt.Sprintf("reporter refused the manifest: %s", code)
	}
	return fmt.Sprintf("reporter refused a put of %d packages from package %d: %s", len(l.items), l.items[0].index, code)
}
