package sessionreporter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"slices"
	"sort"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/sessionrefs"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// receivedEvents is the stream a receiver rebuilds from task packages: every
// record at its raw offset, a package's records in their order, so the
// rebuilt records a header names land before the task:end at their offset.
func receivedEvents(t *testing.T, packages [][]byte) []byte {
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
	sort.SliceStable(all, func(i, j int) bool { return all[i].offset < all[j].offset })
	var out []byte
	for _, p := range all {
		out = append(out, p.data...)
	}
	return out
}

// serveRebuiltPackages is servePackages with a receiver that offers packages,
// and, when rebuilt is set, packages that name rebuilt records too: it adds
// packagesRebuilt to each acknowledgement that offers packages.
func serveRebuiltPackages(t *testing.T, r *Run, rebuilt bool, onPut func(rec *packageReceiver, n int, put protocolcli.SessionReportingPut)) *packageReceiver {
	t.Helper()
	p, engine := pipeProcess()
	p.done = make(chan struct{})
	close(p.done)
	r.process = p
	receiverSide, proxy := net.Pipe()
	rec := &packageReceiver{t: t, remote: receiverSide, offer: true, stored: map[string][]byte{}, done: make(chan struct{})}
	rec.onPut = func(n int, put protocolcli.SessionReportingPut) { onPut(rec, n, put) }
	t.Cleanup(func() {
		_ = engine.Close()
		_ = proxy.Close()
	})
	go func() { _, _ = io.Copy(proxy, engine) }()
	go func() {
		scanner := bufio.NewScanner(proxy)
		scanner.Buffer(make([]byte, 4096), protocolcli.SessionReportingMaxPackageLineBytes)
		for scanner.Scan() {
			line := append(append([]byte(nil), scanner.Bytes()...), '\n')
			if ack, err := protocolcli.ParseSessionReportingAck(scanner.Bytes()); err == nil && rebuilt && ack.Packages {
				ack.PackagesRebuilt = true
				encoded, err := json.Marshal(ack)
				if err != nil {
					return
				}
				line = append(append(line[:0], encoded...), '\n')
			}
			if _, err := engine.Write(line); err != nil {
				return
			}
		}
	}()
	go rec.serve()
	go r.work()
	return rec
}

// TestAReusedTasksPackageHoldsItsRebuiltRecords delivers, on packages, a
// session whose reused task's task:end names a cache entry to a receiver that
// takes rebuilt records: the new checkpoint of the session, version 4, moves
// to 5, and the task's package holds the records the entry holds, named
// rebuilt at the task:end's raw offset, right before the task:end without its
// reference. A receiver that places them there reads the records a recorder
// that kept every record would have written. The packages are the same when
// sealed again, and a reference that does not resolve leaves the task:end in
// its package as it is.
func TestAReusedTasksPackageHoldsItsRebuiltRecords(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "the-session-reporter-sends-the-rebuilt-records")
	entries := referenceStore(t)
	a := "/a:test"
	end := []byte(referencedEnd(a))
	events := [][]byte{
		streamRecord(protocolcli.RecordTaskStart, a),
		streamRecord(protocolcli.RecordTaskStart, "/b:build"),
		end,
		streamRecord(protocolcli.RecordTaskEnd, "/b:build"),
		streamRecord(protocolcli.RecordSessionEnd, ""),
	}
	h := newPlanHarness(t, planOf(10), events...)
	h.run.entries = entries
	h.run.state.Version = SessionReporter.checkpointVersionOf(true)
	rec := serveRebuiltPackages(t, h.run, true, acceptAll)
	if err := finishWith(t, h, []byte(`{"protocolVersion":2}`)); err != nil {
		t.Fatalf("Finish = %v", err)
	}
	if s := h.checkpoint(); !s.Complete || s.Version != resolvedPackagesCheckpointVersion || s.Packages == nil {
		t.Fatalf("checkpoint = %+v, want a complete version 5 session on packages", s)
	}
	_, puts, _, _ := rec.snapshot()
	packages := taskPuts(puts)

	endAt := int64(len(stream(string(events[0]), string(events[1]))))
	rebuilt := resolvedBytes(t, entries, string(end))
	var found bool
	for _, data := range packages {
		p := decodeTask(t, data)
		if *p.header.Key != a {
			if len(p.header.Rebuilt) != 0 {
				t.Fatalf("package %s names rebuilt records %v", *p.header.Key, p.header.Rebuilt)
			}
			continue
		}
		found = true
		if !slices.Equal(p.header.Rebuilt, []int{1, 2, 3}) || !slices.Equal(p.header.Offsets, []int64{0, endAt, endAt, endAt, endAt}) {
			t.Fatalf("the reused task's package names rebuilt %v at offsets %v, want [1 2 3] at the task:end's %d", p.header.Rebuilt, p.header.Offsets, endAt)
		}
		if got := bytes.Join(p.records[1:], nil); !bytes.Equal(got, rebuilt) {
			t.Fatalf("the reused task's package holds\n%s\nwant the entry's records and the task:end without its reference\n%s", got, rebuilt)
		}
	}
	if !found {
		t.Fatal("no package holds the reused task")
	}
	want := bytes.Replace(readEvents(t, h.dir), append(end, '\n'), rebuilt, 1)
	if got := receivedEvents(t, packages); !bytes.Equal(got, want) {
		t.Fatalf("the receiver rebuilds\n%s\nwant\n%s", got, want)
	}

	sealed := sealAll(t, h.dir, entries)
	if again := sealAll(t, h.dir, entries); !slices.EqualFunc(sealed, again, bytes.Equal) {
		t.Fatal("sealing the stream again gave other packages")
	}
	for _, data := range sealAll(t, h.dir, referenceEntries{}) {
		if p := decodeTask(t, data); *p.header.Key == a && (len(p.header.Rebuilt) != 0 || !bytes.Equal(p.records[len(p.records)-1], append(end, '\n'))) {
			t.Fatalf("a reference that does not resolve sealed as %s", data)
		}
	}
}

// sealAll seals every package of the final stream in dir with a collector that
// rebuilds references from entries, and encodes them in sealing order.
func sealAll(t *testing.T, dir string, entries sessionrefs.Entries) [][]byte {
	t.Helper()
	log, err := sessionstream.Open(dir, batchSessionID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := log.Subscribe("seal-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	c := newCollector(sub)
	c.rebuild = func(line []byte) ([][]byte, []byte, error) { return sessionrefs.Resolve(entries, line) }
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
