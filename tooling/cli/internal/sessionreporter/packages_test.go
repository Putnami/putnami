package sessionreporter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

const packagesRequirement = "task-packages-in-parallel"

// packageReceiver is an in-memory session reporter whose destination may
// accept packages. It acknowledges every frame at once, offering packages when
// offer says so, keeps every put and manifest it reads, and hands each put to
// onPut, which answers it through reply whenever, and from whichever
// goroutine, it likes: results leave in any order. A manifest is accepted
// once every address it lists was accepted, and refused "missing" otherwise.
type packageReceiver struct {
	t      *testing.T
	remote net.Conn
	offer  bool
	onPut  func(n int, put protocolcli.SessionReportingPut)

	writeMu sync.Mutex
	mu      sync.Mutex
	// lines names every line read, in order: "frame:<artifact>",
	// "put:<address>" or "manifest".
	lines     []string
	chunks    []protocolcli.SessionReportingChunk
	puts      []protocolcli.SessionReportingPut
	manifests []protocolcli.SessionReportingManifest
	// stored are the packages accepted, by address.
	stored map[string][]byte
	// outstanding counts the puts read and not answered, and most its
	// largest value.
	outstanding, most int
	done              chan struct{}
}

// servePackages hands r a piped reporter served by a packageReceiver whose
// store starts with stored, and starts its worker.
func servePackages(t *testing.T, r *Run, offer bool, stored map[string][]byte, onPut func(rec *packageReceiver, n int, put protocolcli.SessionReportingPut)) *packageReceiver {
	t.Helper()
	p, remote := pipeProcess()
	p.done = make(chan struct{})
	close(p.done)
	r.process = p
	rec := &packageReceiver{t: t, remote: remote, offer: offer, stored: map[string][]byte{}, done: make(chan struct{})}
	maps.Copy(rec.stored, stored)
	rec.onPut = func(n int, put protocolcli.SessionReportingPut) { onPut(rec, n, put) }
	t.Cleanup(func() { _ = remote.Close() })
	go rec.serve()
	go r.work()
	return rec
}

func (rec *packageReceiver) serve() {
	defer close(rec.done)
	scanner := bufio.NewScanner(rec.remote)
	scanner.Buffer(make([]byte, 4096), protocolcli.SessionReportingMaxPackageLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		switch protocolcli.SessionReportingPackageOp(line) {
		case protocolcli.SessionReportingOpPut:
			put, err := protocolcli.ParseSessionReportingPut(line)
			if err != nil {
				rec.t.Errorf("the engine sent an invalid put: %v", err)
				return
			}
			if !rec.offer {
				rec.t.Errorf("the engine sent a put to a reporter that offered no packages")
			}
			if bundle := len(protocolcli.EncodeSessionReportingBundle(put.Packages)); bundle > protocolcli.SessionReportingMaxPackageBytes {
				rec.t.Errorf("a put's bundle is %d bytes, past %d", bundle, protocolcli.SessionReportingMaxPackageBytes)
			}
			rec.mu.Lock()
			n := len(rec.puts)
			rec.lines = append(rec.lines, "put:"+put.Address)
			rec.puts = append(rec.puts, *put)
			rec.outstanding++
			rec.most = max(rec.most, rec.outstanding)
			rec.mu.Unlock()
			rec.onPut(n, *put)
		case protocolcli.SessionReportingOpManifest:
			manifest, err := protocolcli.ParseSessionReportingManifest(line)
			if err != nil {
				rec.t.Errorf("the engine sent an invalid manifest: %v", err)
				return
			}
			rec.mu.Lock()
			rec.lines = append(rec.lines, "manifest")
			rec.manifests = append(rec.manifests, *manifest)
			result := manifest.Accept()
			for _, address := range manifest.Addresses {
				if _, ok := rec.stored[address]; !ok {
					result = result.Refuse("missing", false)
				}
			}
			rec.mu.Unlock()
			rec.write(result)
		default:
			chunk, err := protocolcli.ParseSessionReportingChunk(line)
			if err != nil {
				rec.t.Errorf("the engine sent an invalid frame: %v", err)
				return
			}
			rec.mu.Lock()
			rec.lines = append(rec.lines, "frame:"+chunk.Artifact)
			rec.chunks = append(rec.chunks, *chunk)
			rec.mu.Unlock()
			ack := chunk.Ack()
			ack.Packages = rec.offer
			rec.write(ack)
		}
	}
}

// reply answers put with result; an accepted put stores each of its
// packages.
func (rec *packageReceiver) reply(put protocolcli.SessionReportingPut, result protocolcli.SessionReportingResult) {
	rec.mu.Lock()
	rec.outstanding--
	if result.OK {
		for _, pkg := range put.Packages {
			rec.stored[pkg.Address] = pkg.Data
		}
	}
	rec.mu.Unlock()
	rec.write(result)
}

func (rec *packageReceiver) write(line any) {
	rec.writeMu.Lock()
	defer rec.writeMu.Unlock()
	_ = json.NewEncoder(rec.remote).Encode(line)
}

// awaitFrame returns once the receiver read a frame of artifact.
func (rec *packageReceiver) awaitFrame(artifact string) {
	rec.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		read := slices.Contains(rec.lines, "frame:"+artifact)
		rec.mu.Unlock()
		if read {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.t.Fatalf("no %s frame left while the graph ran", artifact)
}

// acceptAll accepts every put at once.
func acceptAll(rec *packageReceiver, _ int, put protocolcli.SessionReportingPut) {
	go rec.reply(put, put.Accept())
}

func (rec *packageReceiver) snapshot() (lines []string, puts []protocolcli.SessionReportingPut, manifests []protocolcli.SessionReportingManifest, stored map[string][]byte) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.lines), slices.Clone(rec.puts), slices.Clone(rec.manifests), maps.Clone(rec.stored)
}

// streamRecord is one events.jsonl record of a test session.
func streamRecord(kind, key string, extra ...string) []byte {
	if key == "" {
		return []byte(fmt.Sprintf(`{"record":%q%s}`, kind, strings.Join(extra, "")))
	}
	return []byte(fmt.Sprintf(`{"record":%q,"identity":{"key":%q}%s}`, kind, key, strings.Join(extra, "")))
}

// sized is a record of key whose bytes, with its LF, are exactly size.
func sized(kind, key string, size int) []byte {
	base := streamRecord(kind, key, `,"pad":""`)
	pad := strings.Repeat("x", size-1-len(base))
	return streamRecord(kind, key, `,"pad":"`+pad+`"`)
}

// taskRecords is the records of n tasks, each started, logging and ended,
// then the session's end.
func taskRecords(n int) [][]byte {
	var records [][]byte
	for i := range n {
		key := fmt.Sprintf("task-%02d", i)
		records = append(records, streamRecord(protocolcli.RecordTaskStart, key), streamRecord(protocolcli.RecordTaskEvent, key, `,"event":{"type":"log"}`), streamRecord(protocolcli.RecordTaskEnd, key))
	}
	return append(records, streamRecord(protocolcli.RecordSessionEnd, ""))
}

// decoded is one task package a receiver stored.
type decoded struct {
	header  protocolcli.SessionReportingPackageHeader
	records [][]byte
}

// decodeTask parses a task package and splits its records, each with its LF
// when it has one.
func decodeTask(t *testing.T, data []byte) decoded {
	t.Helper()
	header, body, err := protocolcli.ParseSessionReportingPackage(data)
	if err != nil {
		t.Fatalf("invalid package: %v", err)
	}
	var records [][]byte
	for len(body) > 0 {
		i := bytes.IndexByte(body, '\n')
		if i < 0 {
			records = append(records, body)
			break
		}
		records = append(records, body[:i+1])
		body = body[i+1:]
	}
	return decoded{header: header, records: records}
}

// rebuildEvents places every record of every task package at its raw offset
// and joins them: the receiver's copy of events.jsonl. Every byte must be
// covered once.
func rebuildEvents(t *testing.T, packages [][]byte) []byte {
	t.Helper()
	type placed struct {
		offset int64
		data   []byte
	}
	var all []placed
	for _, data := range packages {
		p := decodeTask(t, data)
		for i, r := range p.records {
			all = append(all, placed{p.header.Offsets[i], r})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].offset < all[j].offset })
	var out bytes.Buffer
	for _, p := range all {
		if p.offset != int64(out.Len()) {
			t.Fatalf("a record at raw offset %d follows %d rebuilt bytes", p.offset, out.Len())
		}
		out.Write(p.data)
	}
	return out.Bytes()
}

// packagesOfPuts is every package of puts, in the order read.
func packagesOfPuts(puts []protocolcli.SessionReportingPut) []protocolcli.SessionReportingPackage {
	var out []protocolcli.SessionReportingPackage
	for _, put := range puts {
		out = append(out, put.Packages...)
	}
	return out
}

// taskPuts is the data of every task package of puts, in the order read.
func taskPuts(puts []protocolcli.SessionReportingPut) [][]byte {
	var out [][]byte
	for _, pkg := range packagesOfPuts(puts) {
		if pkg.Kind == protocolcli.SessionReportingPackageTask {
			out = append(out, pkg.Data)
		}
	}
	return out
}

// addressesOf is the address of each package, in order.
func addressesOf(packages []protocolcli.SessionReportingPackage) []string {
	out := make([]string, len(packages))
	for i, pkg := range packages {
		out[i] = pkg.Address
	}
	return out
}

// appendRecords commits records to log, as a running graph does.
func appendRecords(t *testing.T, log *sessionstream.Log, records [][]byte) {
	for _, r := range records {
		if err := log.Append(r); err != nil {
			t.Error(err)
		}
	}
}

// taskOf is the records of task i of taskRecords.
func taskOf(i int) [][]byte {
	return taskRecords(i + 1)[3*i : 3*i+3]
}

// awaitPuts returns once the receiver read n puts.
func (rec *packageReceiver) awaitPuts(n int) {
	rec.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		read := len(rec.puts)
		rec.mu.Unlock()
		if read >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.t.Fatalf("the receiver read fewer than %d puts", n)
}

func readEvents(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, sessionstream.EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func subscriberEvidence(t *testing.T, dir string) string {
	t.Helper()
	document, err := sessionstream.ReadEvidence(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range document.Subscribers {
		if entry.Name == SessionReporter.Name {
			return entry.Evidence
		}
	}
	t.Fatal("no session reporter evidence")
	return ""
}

// TestATaskPackageHoldsItsRecordsByteForByte interleaves two tasks, a batch
// key that never ends, records without a key, a line that is not JSON and a
// task whose records pass 4 MiB: each task's records leave in packages of
// its own, sealed at its task:end, at 4 MiB, or at the stream's end, and the
// receiver rebuilds events.jsonl byte for byte from their header offsets.
func TestATaskPackageHoldsItsRecordsByteForByte(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "a-task-package-holds-its-records-byte-for-byte")
	mib := 1 << 20
	events := [][]byte{
		streamRecord(protocolcli.RecordPlanEnd, ""),
		streamRecord(protocolcli.RecordTaskStart, "a"),
		streamRecord(protocolcli.RecordTaskStart, "b"),
		streamRecord(protocolcli.RecordTaskEvent, "a", `,"n":1`),
		streamRecord(protocolcli.RecordTaskEvent, "batch", `,"n":1`),
		streamRecord(protocolcli.RecordTaskEvent, "b", `,"n":1`),
		streamRecord(protocolcli.RecordTaskEnd, "a"),
		sized(protocolcli.RecordTaskEvent, "big", mib),
		sized(protocolcli.RecordTaskEvent, "big", mib),
		sized(protocolcli.RecordTaskEvent, "big", mib),
		sized(protocolcli.RecordTaskEvent, "big", mib),
		sized(protocolcli.RecordTaskEvent, "big", mib),
		streamRecord(protocolcli.RecordTaskEvent, "batch", `,"n":2`),
		[]byte("not json"),
		streamRecord(protocolcli.RecordTaskEnd, "big"),
		streamRecord(protocolcli.RecordTaskEvent, "b", `,"n":2`),
		streamRecord(protocolcli.RecordTaskEnd, "b"),
		streamRecord(protocolcli.RecordTaskEvent, "batch", `,"n":3`),
		streamRecord(protocolcli.RecordSessionEnd, ""),
	}
	h := newPlanHarness(t, planOf(10), events...)
	rec := servePackages(t, h.run, true, nil, acceptAll)
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	_, puts, manifests, _ := rec.snapshot()
	packages := taskPuts(puts)
	if got, want := rebuildEvents(t, packages), readEvents(t, h.dir); !bytes.Equal(got, want) {
		t.Fatalf("rebuilt events.jsonl is %d bytes, want the %d persisted bytes", len(got), len(want))
	}
	var order []string
	for _, data := range packages {
		p := decodeTask(t, data)
		key := *p.header.Key
		order = append(order, fmt.Sprintf("%s/%d", key, p.header.Part))
		for _, r := range p.records {
			var probe collectedRecord
			got := ""
			if json.Unmarshal(r, &probe) == nil {
				got = probe.Identity.Key
			}
			if got != key {
				t.Fatalf("package %s/%d holds a record of key %q", key, p.header.Part, got)
			}
		}
		var size int
		for _, r := range p.records {
			size += len(r)
		}
		if size > protocolcli.SessionReportingPackageRecordBytes && len(p.records) > 1 {
			t.Fatalf("package %s/%d holds %d bytes of records", key, p.header.Part, size)
		}
		last := p.records[len(p.records)-1]
		ended := bytes.Contains(last, []byte(`"record":"task:end"`))
		switch key {
		case "a", "b":
			if !ended || p.header.Part != 0 {
				t.Fatalf("task %s: package part %d does not end at its task:end", key, p.header.Part)
			}
		case "big":
			if want := map[int]int{0: 4, 1: 2}[p.header.Part]; len(p.records) != want || ended != (p.header.Part == 1) {
				t.Fatalf("big part %d holds %d records (ended %v)", p.header.Part, len(p.records), ended)
			}
		case "batch":
			if len(p.records) != 3 || ended {
				t.Fatalf("the batch key's package holds %d records", len(p.records))
			}
		}
	}
	// Sealing order: at each task:end or 4 MiB as the stream is read, then,
	// at its end, the packages still open by their first record.
	if got, want := strings.Join(order, ","), "a/0,big/0,big/1,b/0,/0,batch/0"; got != want {
		t.Fatalf("packages sealed as %s, want %s", got, want)
	}
	if len(manifests) != 1 {
		t.Fatalf("%d manifests sent", len(manifests))
	}
}

// TestTheCollectorSealsTheSameBytesFromAnyStart reads a final stream whole,
// as a replay does, and the same records as a live run commits them, one at a
// time, with a scan limit far below one record: both seal the same packages.
// A record past 4 MiB travels alone, and the unterminated tail of a final
// stream is a record of its own.
func TestTheCollectorSealsTheSameBytesFromAnyStart(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "a-task-package-holds-its-records-byte-for-byte")
	huge := sized(protocolcli.RecordTaskEvent, "k", protocolcli.SessionReportingPackageRecordBytes+10)
	records := [][]byte{streamRecord(protocolcli.RecordTaskStart, "k"), huge, streamRecord(protocolcli.RecordTaskEvent, "k"), streamRecord(protocolcli.RecordTaskEnd, "k")}
	var stream []byte
	for _, r := range records {
		stream = append(append(stream, r...), '\n')
	}
	tail := []byte(`{"record":"task:event","identity":{"key":"k"}`)
	stream = append(stream, tail...)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sessionstream.EventsFile), stream, 0o644); err != nil {
		t.Fatal(err)
	}
	whole := collectAll(t, dir)
	// k/0 holds its start alone: the huge record would take it past 4 MiB.
	// k/1 is the huge record alone, k/2 the rest through task:end. The tail,
	// not JSON, is the run's package, key "".
	want := []string{"k/0:1", "k/1:1", "k/2:2", "/0:1"}
	if len(whole) != len(want) {
		t.Fatalf("%d packages, want %d", len(whole), len(want))
	}
	for i, data := range whole {
		p := decodeTask(t, data)
		if got := fmt.Sprintf("%s/%d:%d", *p.header.Key, p.header.Part, len(p.records)); got != want[i] {
			t.Fatalf("package %d is %s, want %s", i, got, want[i])
		}
	}
	if got := rebuildEvents(t, whole); !bytes.Equal(got, stream) {
		t.Fatal("the packages do not rebuild the stream, its tail included")
	}

	live := t.TempDir()
	log, err := sessionstream.Create(live, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	sub, err := log.Subscribe(SessionReporter.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	c := newCollector(sub)
	var sealed [][]byte
	drain := func() {
		for {
			pkg, more, err := c.next(64)
			if err != nil {
				t.Fatal(err)
			}
			if pkg == nil && !more {
				return
			}
			if pkg != nil {
				data, err := c.encode(pkg)
				if err != nil {
					t.Fatal(err)
				}
				sealed = append(sealed, data)
			}
		}
	}
	for _, r := range records {
		if err := log.Append(r); err != nil {
			t.Fatal(err)
		}
		drain()
	}
	if len(sealed) != 3 {
		t.Fatalf("the live stream sealed %d packages, want 3", len(sealed))
	}
	for i, data := range sealed {
		if !bytes.Equal(data, whole[i]) {
			t.Fatalf("live package %d differs from the whole stream's", i)
		}
	}
}

// collectAll seals every package of the final stream in dir.
func collectAll(t *testing.T, dir string) [][]byte {
	t.Helper()
	log, err := sessionstream.Open(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := log.Subscribe(SessionReporter.Name, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	c := newCollector(sub)
	var out [][]byte
	for !c.done || len(c.sealed) > 0 {
		pkg, _, err := c.next(collectorScanBytes)
		if err != nil {
			t.Fatal(err)
		}
		if pkg == nil {
			continue
		}
		data, err := c.encode(pkg)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, data)
	}
	return out
}

// TestPackagesTravelInParallelAndAManifestClosesTheSession ends each task
// while the puts before it are held, so each leaves in a put of its own, then
// answers the four puts last first: the engine keeps several puts, at most
// eight, in flight, takes results in any order, and once every package and
// document is acknowledged sends one final manifest of every address, which
// completes the delivery.
func TestPackagesTravelInParallelAndAManifestClosesTheSession(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "packages-travel-in-parallel-and-a-manifest-closes-the-session")
	h := newPlanHarness(t, planOf(100), taskOf(0)...)
	if err := os.WriteFile(filepath.Join(h.dir, specVerificationFile), []byte(`{"verdict":"passed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	held := map[int]protocolcli.SessionReportingPut{}
	rec := servePackages(t, h.run, true, nil, func(rec *packageReceiver, n int, put protocolcli.SessionReportingPut) {
		if n >= 4 {
			go rec.reply(put, put.Accept())
			return
		}
		mu.Lock()
		held[n] = put
		release := len(held) == 4
		mu.Unlock()
		if !release {
			// The next task ends while this put is in flight.
			appendRecords(t, h.log, taskOf(n+1))
			return
		}
		go func() {
			for i := 3; i >= 0; i-- {
				rec.reply(held[i], held[i].Accept())
			}
		}()
	})
	rec.awaitPuts(4)
	appendRecords(t, h.log, [][]byte{streamRecord(protocolcli.RecordSessionEnd, "")})
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	lines, puts, manifests, _ := rec.snapshot()
	rec.mu.Lock()
	most := rec.most
	rec.mu.Unlock()
	if most < 4 || most > maxPackagesInFlight {
		t.Fatalf("at most %d puts were outstanding at once, want between 4 and %d", most, maxPackagesInFlight)
	}
	for i := range 4 {
		if len(puts[i].Packages) != 1 {
			t.Fatalf("put %d carries %d packages, want task-%02d's alone", i, len(puts[i].Packages), i)
		}
	}
	// plan.json frames, then puts, then the manifest, last.
	firstPut := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "put:") })
	for i, line := range lines {
		if strings.HasPrefix(line, "frame:") && (i > firstPut || line != "frame:"+planFile) {
			t.Fatalf("line %d is %s: only plan.json frames, before every put", i, line)
		}
	}
	if len(manifests) != 1 || lines[len(lines)-1] != "manifest" {
		t.Fatalf("lines end %v, want a single manifest last", lines[max(0, len(lines)-3):])
	}
	manifest := manifests[0]
	if !manifest.Final {
		t.Fatal("the manifest is not final")
	}
	sent := packagesOfPuts(puts)
	if !slices.Equal(manifest.Addresses, addressesOf(sent)) {
		t.Fatalf("the manifest lists %d addresses, the puts carried %d, in sealing order", len(manifest.Addresses), len(sent))
	}
	n := len(sent)
	if n != 4+1+2 || sent[n-2].Kind != protocolcli.SessionReportingPackageSession || sent[n-1].Kind != protocolcli.SessionReportingPackageSpecVerification {
		t.Fatalf("the manifest lists %d addresses, the documents not last", n)
	}
	if got, want := rebuildEvents(t, taskPuts(puts)), readEvents(t, h.dir); !bytes.Equal(got, want) {
		t.Fatal("the task packages do not rebuild events.jsonl")
	}
	s := h.checkpoint()
	if !s.Complete || s.Version != packagesCheckpointVersion || s.Packages == nil || !s.Packages.Manifest || s.Packages.Acknowledged != n || len(s.Packages.Addresses) != 0 {
		t.Fatalf("checkpoint = %+v, packages %+v", s, s.Packages)
	}
	if s.Events != (cursor{}) || s.Session != (cursor{}) || s.SpecVerification != (cursor{}) || s.SpecVerificationOmitted != "" {
		t.Fatalf("a session on packages framed more than plan.json: %+v", s)
	}
	if got := subscriberEvidence(t, h.dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence = %s, want delivered", got)
	}
}

// TestABurstOfPackagesLeavesInAFewPuts commits two hundred tasks before the
// worker reads any: they leave together, in a handful of puts, not one put
// per package.
func TestABurstOfPackagesLeavesInAFewPuts(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "packages-travel-in-parallel-and-a-manifest-closes-the-session")
	const tasks = 200
	h := newPlanHarness(t, planOf(100), taskRecords(tasks)...)
	if err := os.WriteFile(filepath.Join(h.dir, specVerificationFile), []byte(`{"verdict":"passed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := servePackages(t, h.run, true, nil, acceptAll)
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	_, puts, manifests, _ := rec.snapshot()
	sent := packagesOfPuts(puts)
	largest := 0
	for _, put := range puts {
		largest = max(largest, len(put.Packages))
	}
	if len(sent) != tasks+1+2 || len(puts) > maxPackagesInFlight || largest < tasks {
		t.Fatalf("%d packages left in %d puts, the largest of %d packages; want %d in at most %d puts", len(sent), len(puts), largest, tasks+3, maxPackagesInFlight)
	}
	if len(manifests) != 1 || !slices.Equal(manifests[0].Addresses, addressesOf(sent)) {
		t.Fatal("the manifest does not list every package in sealing order")
	}
	if got := subscriberEvidence(t, h.dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence = %s, want delivered", got)
	}
}

// TestAReplaySendsOnlyUnacknowledgedPackages ends one task per put read and,
// once eight are in flight, acknowledges five of the first six out of order
// and refuses the second, which stops the delivery. A replay rebuilds the
// same packages from the spool, batches anew only those not acknowledged, at
// the same addresses, then sends the manifest.
func TestAReplaySendsOnlyUnacknowledgedPackages(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "a-replay-sends-only-unacknowledged-packages")
	const tasks = 12
	h := newPlanHarness(t, planOf(100), taskOf(0)...)
	var mu sync.Mutex
	held := map[int]protocolcli.SessionReportingPut{}
	first := servePackages(t, h.run, true, nil, func(rec *packageReceiver, n int, put protocolcli.SessionReportingPut) {
		if n+1 < tasks {
			appendRecords(t, h.log, taskOf(n+1))
		}
		mu.Lock()
		if n < 6 {
			held[n] = put
		}
		mu.Unlock()
		// Eight puts are in flight, the last two never answered.
		if n == 7 {
			go func() {
				for _, i := range []int{5, 4, 3, 2, 0} {
					rec.reply(held[i], held[i].Accept())
				}
				rec.reply(held[1], held[1].Accept().Refuse("invalid_package", false))
			}()
		}
	})
	first.awaitPuts(8)
	err := finishWith(t, h, []byte(`{"protocolVersion":2}`))
	if err == nil || !strings.Contains(err.Error(), "invalid_package") {
		t.Fatalf("Finish = %v, want the refusal", err)
	}
	_, interruptedPuts, _, stored := first.snapshot()
	for i := range 6 {
		if len(interruptedPuts[i].Packages) != 1 {
			t.Fatalf("put %d carries %d packages, want one task's", i, len(interruptedPuts[i].Packages))
		}
	}
	interrupted := packagesOfPuts(interruptedPuts)
	s := h.checkpoint()
	if s.Version != packagesCheckpointVersion || s.Packages == nil || s.Complete {
		t.Fatalf("checkpoint after the refusal = %+v", s)
	}
	want := addressesOf(interrupted[2:6])
	if s.Packages.Acknowledged != 1 || !slices.Equal(s.Packages.Addresses, want) {
		t.Fatalf("checkpoint records %d acknowledged and %v, want 1 and the packages 2 to 5", s.Packages.Acknowledged, s.Packages.Addresses)
	}
	// The subscription acknowledged up to the first record of the oldest
	// package not acknowledged, task-01's, and no further.
	events := readEvents(t, h.dir)
	key := bytes.Index(events, []byte(`"key":"task-01"`))
	if key < 0 {
		t.Fatal("task-01 is not in events.jsonl")
	}
	second := int64(bytes.LastIndexByte(events[:key], '\n')) + 1
	if s.EventsRaw != second {
		t.Fatalf("checkpoint acknowledged raw offset %d, want %d", s.EventsRaw, second)
	}
	if document, err := sessionstream.ReadEvidence(h.dir); err != nil || document.Subscribers[0].Acknowledged.Offset != second || document.Subscribers[0].Evidence == protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence after the refusal = %+v (%v)", document, err)
	}

	log, err := sessionstream.Open(h.dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openRun(context.Background(), SessionReporter, h.dir, batchSessionID, "@test/provider")
	if err != nil {
		t.Fatalf("replay refused the checkpoint: %v", err)
	}
	if err := r.subscribe(log); err != nil {
		r.release()
		t.Fatal(err)
	}
	replay := servePackages(t, r, true, stored, acceptAll)
	if err := r.Finish(); err != nil {
		t.Fatalf("replay Finish = %v", err)
	}
	lines, resent, manifests, _ := replay.snapshot()
	for _, line := range lines {
		if strings.HasPrefix(line, "frame:") {
			t.Fatalf("the replay sent %s: plan.json had closed", line)
		}
	}
	if len(manifests) != 1 {
		t.Fatalf("the replay sent %d manifests", len(manifests))
	}
	addresses := manifests[0].Addresses
	acknowledged := map[string]bool{}
	for _, i := range []int{0, 2, 3, 4, 5} {
		acknowledged[interrupted[i].Address] = true
	}
	var unacknowledged []string
	for _, address := range addresses {
		if !acknowledged[address] {
			unacknowledged = append(unacknowledged, address)
		}
	}
	if again := addressesOf(packagesOfPuts(resent)); !slices.Equal(again, unacknowledged) {
		t.Fatalf("the replay sent %d packages, want the %d not acknowledged", len(again), len(unacknowledged))
	}
	if len(resent) >= len(unacknowledged) {
		t.Fatalf("the replay sent %d packages in %d puts: it did not batch them", len(unacknowledged), len(resent))
	}
	for i, pkg := range interrupted {
		if addresses[i] != pkg.Address {
			t.Fatalf("package %d has another address after the replay", i)
		}
	}
	if got := subscriberEvidence(t, h.dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("replay evidence = %s, want delivered", got)
	}
	if s := h.checkpoint(); !s.Complete || !s.Packages.Manifest {
		t.Fatalf("replay checkpoint = %+v", s)
	}
}

// TestASessionWithoutAPackagesOfferStaysOnFrames acknowledges every frame
// without offering packages: the session sends today's frames, in today's
// order. An offer that first arrives on an events.jsonl frame, as when no
// plan.json precedes it, comes too late: the session stays on frames.
func TestASessionWithoutAPackagesOfferStaysOnFrames(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "a-session-without-a-packages-offer-stays-on-frames")
	plan := planOf(2*protocolcli.SessionReportingChunkBytes + 21)
	h := newPlanHarness(t, plan, fullFrame, []byte(`{"record":"task:start"}`))
	rec := servePackages(t, h.run, false, nil, acceptAll)
	rec.awaitFrame(sessionstream.EventsFile)
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	_, puts, manifests, _ := rec.snapshot()
	rec.mu.Lock()
	chunks := slices.Clone(rec.chunks)
	rec.mu.Unlock()
	if len(puts) != 0 || len(manifests) != 0 {
		t.Fatalf("%d puts and %d manifests without an offer", len(puts), len(manifests))
	}
	if got, want := frameOrder(chunks), "plan.json,plan.json,plan.json,plan.json:final,events.jsonl,session.json,session.json:final,events.jsonl,events.jsonl:final"; got != want {
		t.Fatalf("frames = %s\nwant %s", got, want)
	}
	if s := h.checkpoint(); !s.Complete || s.Version != viewCheckpointVersion || s.Packages != nil {
		t.Fatalf("checkpoint = %+v", s)
	}

	// No plan.json: the first frame is events.jsonl, and the offer its
	// acknowledgement carries does not switch the session.
	late := newPlanHarness(t, nil, fullFrame)
	rec = servePackages(t, late.run, true, nil, func(rec *packageReceiver, _ int, put protocolcli.SessionReportingPut) {
		t.Errorf("the session sent a put after an events.jsonl frame left")
		go rec.reply(put, put.Accept())
	})
	rec.awaitFrame(sessionstream.EventsFile)
	if err := finishWith(t, late, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	rec.mu.Lock()
	chunks = slices.Clone(rec.chunks)
	rec.mu.Unlock()
	if got, want := frameOrder(chunks), "events.jsonl,session.json,session.json:final,events.jsonl:final"; got != want {
		t.Fatalf("frames = %s\nwant %s", got, want)
	}
	if s := late.checkpoint(); !s.Complete || s.Version != viewCheckpointVersion || s.Packages != nil {
		t.Fatalf("checkpoint = %+v", s)
	}
	if got := subscriberEvidence(t, late.dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence = %s, want delivered", got)
	}
}

// TestARefusedPutIsSentAgainAndSpecVerificationIsBestEffort refuses the first
// put once with retry and the put of spec-verification.json without: the
// first put leaves again with the same packages at the same address,
// spec-verification.json, which travels alone, is omitted as refused, and the
// manifest, without it, completes the delivery. A put refused with retry on
// every attempt stops the delivery after three.
func TestARefusedPutIsSentAgainAndSpecVerificationIsBestEffort(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "packages-travel-in-parallel-and-a-manifest-closes-the-session")
	h := newPlanHarness(t, planOf(10), taskRecords(3)...)
	if err := os.WriteFile(filepath.Join(h.dir, specVerificationFile), []byte(`{"verdict":"passed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	specOnly := func(put protocolcli.SessionReportingPut) bool {
		return slices.ContainsFunc(put.Packages, func(p protocolcli.SessionReportingPackage) bool {
			return p.Kind == protocolcli.SessionReportingPackageSpecVerification
		})
	}
	rec := servePackages(t, h.run, true, nil, func(rec *packageReceiver, n int, put protocolcli.SessionReportingPut) {
		switch {
		case n == 0:
			go rec.reply(put, put.Accept().Refuse("receiver_unavailable", true))
		case specOnly(put):
			if len(put.Packages) != 1 {
				t.Errorf("spec-verification.json shares a put with %d packages", len(put.Packages)-1)
			}
			go rec.reply(put, put.Accept().Refuse("invalid_package", false))
		default:
			go rec.reply(put, put.Accept())
		}
	})
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	_, puts, manifests, _ := rec.snapshot()
	sent := map[string]int{}
	var spec string
	for _, put := range puts {
		sent[put.Address]++
		if specOnly(put) {
			spec = put.Packages[0].Address
			sent[spec]++
		}
	}
	again := slices.IndexFunc(puts[1:], func(p protocolcli.SessionReportingPut) bool { return p.Address == puts[0].Address })
	if sent[puts[0].Address] != 2 || again < 0 || !slices.Equal(addressesOf(puts[again+1].Packages), addressesOf(puts[0].Packages)) {
		t.Fatalf("the refused put left %d times", sent[puts[0].Address])
	}
	if spec == "" || sent[spec] != 1 {
		t.Fatalf("spec-verification.json left %d times", sent[spec])
	}
	if len(manifests) != 1 || slices.Contains(manifests[0].Addresses, spec) || len(manifests[0].Addresses) != 4+1 {
		t.Fatalf("manifest = %+v", manifests)
	}
	if s := h.checkpoint(); !s.Complete || s.SpecVerificationOmitted != specVerificationRefused || !s.Packages.Manifest {
		t.Fatalf("checkpoint = %+v", s)
	}

	stubborn := newPlanHarness(t, planOf(10), taskRecords(1)...)
	rec = servePackages(t, stubborn.run, true, nil, func(rec *packageReceiver, _ int, put protocolcli.SessionReportingPut) {
		go rec.reply(put, put.Accept().Refuse("receiver_unavailable", true))
	})
	if err := finishWith(t, stubborn, []byte(`{"protocolVersion":2}`)); err == nil || !strings.Contains(err.Error(), errRetryBudget.Error()) {
		t.Fatalf("Finish = %v, want the retry budget exhausted", err)
	}
	_, puts, _, _ = rec.snapshot()
	attempts := map[string]int{}
	for _, put := range puts {
		attempts[put.Address]++
	}
	for address, n := range attempts {
		if n > maxAttempts {
			t.Fatalf("put %s left %d times", address, n)
		}
	}
	if s := stubborn.checkpoint(); s.Complete || s.Packages == nil || s.Packages.Manifest {
		t.Fatalf("checkpoint after the retry budget = %+v", s)
	}
}

// packageReporterRecordEnv names the file the package reporter helper
// appends every line it reads to.
const packageReporterRecordEnv = "SESSIONREPORTER_PACKAGE_RECORD"

// TestPackageReporterHelperProcess is a session reporter subprocess that
// accepts every put and manifest. It appends, for each line, its process, the
// line's op and address, and the address of each package a put carries, to
// the record.
func TestPackageReporterHelperProcess(t *testing.T) {
	path := os.Getenv(packageReporterRecordEnv)
	if path == "" {
		return
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 4096), protocolcli.SessionReportingMaxPackageLineBytes)
	for in.Scan() {
		var result protocolcli.SessionReportingResult
		var carried []string
		switch protocolcli.SessionReportingPackageOp(in.Bytes()) {
		case protocolcli.SessionReportingOpPut:
			put, err := protocolcli.ParseSessionReportingPut(in.Bytes())
			if err != nil {
				os.Exit(20)
			}
			result, carried = put.Accept(), addressesOf(put.Packages)
		case protocolcli.SessionReportingOpManifest:
			manifest, err := protocolcli.ParseSessionReportingManifest(in.Bytes())
			if err != nil {
				os.Exit(20)
			}
			result = manifest.Accept()
		default:
			os.Exit(20)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(21)
		}
		_, _ = fmt.Fprintln(f, strings.Join(append([]string{fmt.Sprint(os.Getpid()), result.Op, result.Address}, carried...), " "))
		_ = f.Close()
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
	os.Exit(0)
}

// TestAReporterThatQuitsIsStartedAgain accepts the first put, of the tasks,
// then closes the reporter's pipe on the second, of session.json, once the
// checkpoint holds the first: the engine starts the reporter again and sends
// the same put again, and only it, then the manifest.
func TestAReporterThatQuitsIsStartedAgain(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "packages-travel-in-parallel-and-a-manifest-closes-the-session")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record.txt")
	// No record without a key: nothing seals between the tasks' put and the
	// documents.
	h := newPlanHarness(t, planOf(10), taskRecords(4)[:12]...)
	h.run.launch = LaunchSpec{Command: self, Args: []string{"-test.run=^TestPackageReporterHelperProcess$"}, Env: append(os.Environ(), packageReporterRecordEnv+"="+record)}
	rec := servePackages(t, h.run, true, nil, func(rec *packageReceiver, n int, put protocolcli.SessionReportingPut) {
		switch n {
		case 0:
			rec.reply(put, put.Accept())
		case 1:
			go func() {
				for range 6000 {
					var s state
					if data, err := os.ReadFile(filepath.Join(h.dir, StateFile)); err == nil && json.Unmarshal(data, &s) == nil && s.Packages != nil && s.Packages.Acknowledged == 4 {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				_ = rec.remote.Close()
			}()
		}
	})
	// The stream ends, and session.json is written, once the tasks' put left.
	rec.awaitPuts(1)
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	_, first, _, _ := rec.snapshot()
	if len(first) != 2 || len(first[0].Packages) != 4 || len(first[1].Packages) != 1 {
		t.Fatalf("the first reporter read %d puts", len(first))
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var again [][]string
	manifests := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			t.Fatalf("helper record %q", line)
		}
		if fields[1] == protocolcli.SessionReportingOpManifest {
			manifests++
			continue
		}
		again = append(again, fields[2:])
	}
	if manifests != 1 || len(again) != 1 {
		t.Fatalf("the started reporter read %d puts and %d manifests", len(again), manifests)
	}
	if again[0][0] != first[1].Address || !slices.Equal(again[0][1:], addressesOf(first[1].Packages)) {
		t.Fatal("the started reporter did not read the put in flight when the first quit, with its packages")
	}
	if got := subscriberEvidence(t, h.dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence = %s, want delivered", got)
	}
}

// documentOf reassembles the parts of the documents of kind among packages,
// in order, checking that their parts count from 0 and stay within a part's
// bound.
func documentOf(t *testing.T, packages []protocolcli.SessionReportingPackage, kind string) ([]byte, int) {
	t.Helper()
	var out []byte
	parts := 0
	for _, pkg := range packages {
		if pkg.Kind != kind {
			continue
		}
		header, body, err := protocolcli.ParseSessionReportingPackage(pkg.Data)
		if err != nil || header.Kind != kind || header.Part != parts || len(body) > documentPartBytes {
			t.Fatalf("%s part %d: header %+v, %d bytes (%v)", kind, parts, header, len(body), err)
		}
		out = append(out, body...)
		parts++
	}
	return out, parts
}

// patterned is size bytes that differ from part to part.
func patterned(size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte('a' + i/7%26)
	}
	return out
}

// TestADocumentOfAnySizeTravelsInParts delivers a session.json past 8 MiB
// and a spec-verification.json at its 8 MiB bound: each leaves in parts of at
// most 4 MiB, numbered from 0, listed in order in the manifest, that
// reassemble byte for byte. Refusing a put of spec-verification.json omits
// every part of it, and the delivery completes without it.
func TestADocumentOfAnySizeTravelsInParts(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "a-task-package-holds-its-records-byte-for-byte")
	session := patterned(9<<20 + 123)
	spec := patterned(protocolcli.SessionReportingSpecVerificationBytes)
	h := newPlanHarness(t, planOf(10), taskRecords(2)...)
	if err := os.WriteFile(filepath.Join(h.dir, specVerificationFile), spec, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := servePackages(t, h.run, true, nil, acceptAll)
	if err := finishWith(t, h, session); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	_, puts, manifests, _ := rec.snapshot()
	sent := packagesOfPuts(puts)
	if got, parts := documentOf(t, sent, protocolcli.SessionReportingPackageSession); !bytes.Equal(got, session) || parts != 3 {
		t.Fatalf("session.json arrived as %d bytes in %d parts, want %d in 3", len(got), parts, len(session))
	}
	if got, parts := documentOf(t, sent, protocolcli.SessionReportingPackageSpecVerification); !bytes.Equal(got, spec) || parts != 2 {
		t.Fatalf("spec-verification.json arrived as %d bytes in %d parts, want %d in 2", len(got), parts, len(spec))
	}
	if len(manifests) != 1 || !slices.Equal(manifests[0].Addresses, addressesOf(sent)) {
		t.Fatal("the manifest does not list every part in order")
	}
	if got := subscriberEvidence(t, h.dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence = %s, want delivered", got)
	}

	// Refuse the first put of spec-verification.json: every part of it is
	// omitted, the one in flight included.
	refused := newPlanHarness(t, planOf(10), taskRecords(2)...)
	if err := os.WriteFile(filepath.Join(refused.dir, specVerificationFile), spec, 0o644); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	rec = servePackages(t, refused.run, true, nil, func(rec *packageReceiver, _ int, put protocolcli.SessionReportingPut) {
		if put.Packages[0].Kind == protocolcli.SessionReportingPackageSpecVerification {
			first := false
			once.Do(func() { first = true })
			if first {
				go rec.reply(put, put.Accept().Refuse("invalid_package", false))
				return
			}
		}
		go rec.reply(put, put.Accept())
	})
	if err := finishWith(t, refused, session); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	_, puts, manifests, _ = rec.snapshot()
	if len(manifests) != 1 {
		t.Fatalf("%d manifests", len(manifests))
	}
	kinds := map[string]string{}
	for _, pkg := range packagesOfPuts(puts) {
		kinds[pkg.Address] = pkg.Kind
	}
	sessionParts := 0
	for _, address := range manifests[0].Addresses {
		switch kinds[address] {
		case protocolcli.SessionReportingPackageSpecVerification:
			t.Fatal("the manifest lists a part of the refused spec-verification.json")
		case protocolcli.SessionReportingPackageSession:
			sessionParts++
		}
	}
	if s := refused.checkpoint(); sessionParts != 3 || !s.Complete || s.SpecVerificationOmitted != specVerificationRefused {
		t.Fatalf("%d session parts listed, checkpoint %+v", sessionParts, s)
	}
}

// TestACutBundleStaysWithinItsBound cuts puts from many tiny packages and
// from packages of nearly 4 MiB: every bundle, each package's line included,
// stays within 8 MiB, and a put takes every package that fits.
func TestACutBundleStaysWithinItsBound(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "packages-travel-in-parallel-and-a-manifest-closes-the-session")
	for _, length := range []int{1, 99, 1 << 20, 4 << 20} {
		pkg := protocolcli.NewSessionReportingPackage(protocolcli.SessionReportingPackageSpecVerification, patterned(length))
		if got, want := bundleLineBytes(pkg.Kind, length)+length, len(protocolcli.EncodeSessionReportingBundle([]protocolcli.SessionReportingPackage{pkg})); got != want {
			t.Fatalf("a package of %d bytes takes %d bundle bytes, counted %d", length, want, got)
		}
	}
	for _, tc := range []struct {
		name         string
		count, bytes int
	}{
		{"4096 tiny packages", protocolcli.SessionReportingMaxPutPackages, 16},
		{"packages that fill 8 MiB without their lines", protocolcli.SessionReportingMaxPutPackages + 100, 2 << 10},
		{"packages of nearly 4 MiB", 3, 4<<20 - 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &packageWorker{r: &Run{state: state{SessionID: batchSessionID, Packages: &packagesState{}}}, inFlight: map[string]*packageLine{}}
			for i := range tc.count {
				data := patterned(tc.bytes)
				copy(data, fmt.Sprintf("%08d", i))
				w.add(protocolcli.SessionReportingPackageTask, nil, data)
			}
			largest := 0
			for len(w.pending) > 0 {
				l := w.cut()
				bundle := len(protocolcli.EncodeSessionReportingBundle(packagesOf(l.items)))
				if bundle > protocolcli.SessionReportingMaxPackageBytes {
					t.Fatalf("a put of %d packages has a bundle of %d bytes", len(l.items), bundle)
				}
				if line, err := w.line(l); err != nil || len(line) > protocolcli.SessionReportingMaxPackageLineBytes {
					t.Fatalf("a put of %d packages has a line of %d bytes (%v)", len(l.items), len(line), err)
				}
				if len(w.pending) > 0 && len(l.items) < protocolcli.SessionReportingMaxPutPackages && bundle+w.pending[0].bundle <= protocolcli.SessionReportingMaxPackageBytes {
					t.Fatalf("a put of %d packages left one that fit", len(l.items))
				}
				largest = max(largest, bundle)
			}
			if tc.bytes == 16 && largest == 0 {
				t.Fatal("nothing was cut")
			}
		})
	}
}

// TestAPutsDeadlineFollowsItsSize gives a put 5 s and 2 s more per MiB its
// packages begin.
func TestAPutsDeadlineFollowsItsSize(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "packages-travel-in-parallel-and-a-manifest-closes-the-session")
	for size, want := range map[int]time.Duration{
		0:           5 * time.Second,
		1:           7 * time.Second,
		1 << 20:     7 * time.Second,
		1<<20 + 1:   9 * time.Second,
		8 << 20:     21 * time.Second,
		11<<20 - 10: 27 * time.Second,
	} {
		if got := putTimeout(operationTimeout, size); got != want {
			t.Errorf("a put of %d bytes waits %s, want %s", size, got, want)
		}
	}
}

// TestAPutPastItsDeadlineGoesAgainOnANewReporter never answers a put: once
// the first one's deadline passed, the engine starts the reporter again and
// sends the same puts, the oldest first, which completes the delivery.
func TestAPutPastItsDeadlineGoesAgainOnANewReporter(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "packages-travel-in-parallel-and-a-manifest-closes-the-session")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "record.txt")
	h := newPlanHarness(t, planOf(10), taskRecords(2)[:6]...)
	h.run.opTimeout = 100 * time.Millisecond
	h.run.launch = LaunchSpec{Command: self, Args: []string{"-test.run=^TestPackageReporterHelperProcess$"}, Env: append(os.Environ(), packageReporterRecordEnv+"="+record)}
	rec := servePackages(t, h.run, true, nil, func(*packageReceiver, int, protocolcli.SessionReportingPut) {})
	rec.awaitPuts(1)
	started := time.Now()
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	if elapsed := time.Since(started); elapsed < putDeadlinePerMiB {
		t.Fatalf("the put went again after %s, before its deadline", elapsed)
	}
	_, first, _, _ := rec.snapshot()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(strings.SplitN(string(data), "\n", 2)[0])
	// Every put in flight goes again, the oldest first.
	if len(fields) < 3 || fields[1] != protocolcli.SessionReportingOpPut || fields[2] != first[0].Address {
		t.Fatalf("the new reporter's first line is %v, want the put past its deadline", fields)
	}
	if got := subscriberEvidence(t, h.dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence = %s, want delivered", got)
	}
}

// TestNoMoreThanEightPutsAreOutstanding holds every answer while tasks keep
// ending: eight puts leave, and no ninth until a slot frees.
func TestNoMoreThanEightPutsAreOutstanding(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "packages-travel-in-parallel-and-a-manifest-closes-the-session")
	const tasks = 14
	h := newPlanHarness(t, planOf(10), taskOf(0)...)
	var mu sync.Mutex
	var held []protocolcli.SessionReportingPut
	readAtCheck := make(chan int, 1)
	rec := servePackages(t, h.run, true, nil, func(rec *packageReceiver, n int, put protocolcli.SessionReportingPut) {
		if n >= maxPackagesInFlight {
			go rec.reply(put, put.Accept())
			return
		}
		mu.Lock()
		held = append(held, put)
		mu.Unlock()
		if n+1 < maxPackagesInFlight {
			appendRecords(t, h.log, taskOf(n+1))
			return
		}
		go func() {
			for i := maxPackagesInFlight; i < tasks; i++ {
				appendRecords(t, h.log, taskOf(i))
			}
			// Give a ninth put every chance to leave.
			time.Sleep(300 * time.Millisecond)
			rec.mu.Lock()
			readAtCheck <- len(rec.puts)
			rec.mu.Unlock()
			for _, put := range held {
				rec.reply(put, put.Accept())
			}
		}()
	})
	read := <-readAtCheck
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	rec.mu.Lock()
	most := rec.most
	rec.mu.Unlock()
	if read != maxPackagesInFlight || most != maxPackagesInFlight {
		t.Fatalf("%d puts read with eight unanswered, at most %d outstanding; want %d", read, most, maxPackagesInFlight)
	}
	_, puts, _, _ := rec.snapshot()
	if got := len(taskPuts(puts)); got != tasks {
		t.Fatalf("%d task packages left, want %d", got, tasks)
	}
	if got := subscriberEvidence(t, h.dir); got != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("evidence = %s, want delivered", got)
	}
}

// TestAPutsDeadlineFollowsItsBundle cuts a put whose packages stay under
// 1 MiB while their bundle lines take it past: its deadline follows the
// bundle, the size the reporter's own request deadline follows, so the
// reporter answers before the engine gives up on the put.
func TestAPutsDeadlineFollowsItsBundle(t *testing.T) {
	spectest.Proves(t, framesFeature, packagesRequirement, "packages-travel-in-parallel-and-a-manifest-closes-the-session")
	w := &packageWorker{r: &Run{state: state{SessionID: batchSessionID, Packages: &packagesState{}}}, inFlight: map[string]*packageLine{}}
	data := 0
	for i := range 1000 {
		payload := patterned(1000)
		copy(payload, fmt.Sprintf("%08d", i))
		w.add(protocolcli.SessionReportingPackageTask, nil, payload)
		data += len(payload)
	}
	l := w.cut()
	bundle := len(protocolcli.EncodeSessionReportingBundle(packagesOf(l.items)))
	if len(l.items) != 1000 || data > 1<<20 || bundle <= 1<<20 {
		t.Fatalf("the fixture does not cross 1 MiB with its lines only: %d packages, %d data bytes, %d bundle bytes", len(l.items), data, bundle)
	}
	if l.size != bundle || putTimeout(operationTimeout, l.size) != putTimeout(operationTimeout, bundle) {
		t.Fatalf("the put's deadline follows %d bytes, its bundle has %d", l.size, bundle)
	}
}
