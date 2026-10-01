package sessionstream

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
)

const testSessionID = "20260917-120000-abc123"

func newLog(t *testing.T) (*Log, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := Create(dir, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	closeAtCleanup(t, log)
	return log, dir
}

// closeAtCleanup closes log when the test ends and fails the test when a
// subscription is still declared then. A declared subscription holds
// events.jsonl open, and Windows refuses to delete an open file, so the test's
// temp dir could not be removed.
func closeAtCleanup(t *testing.T, log *Log) {
	t.Helper()
	t.Cleanup(func() {
		_ = log.Close()
		log.mu.Lock()
		defer log.mu.Unlock()
		for name := range log.subs {
			t.Errorf("subscription %s still holds %s open when the test ends", name, EventsFile)
		}
	})
}

// subscribe declares a subscription the test owns and closes it when the test
// ends, before the log's own cleanup checks for open subscriptions.
func subscribe(t *testing.T, log *Log, name string, from int64) *Subscription {
	t.Helper()
	sub, err := log.Subscribe(name, from)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	return sub
}

// TestAppendWritesExactlyTheRecordAndItsLF pins byte identity: the stream adds
// nothing to a record — no sequence member, no framing — so events.jsonl is the
// same file the recorder wrote before the stream existed.
func TestAppendWritesExactlyTheRecordAndItsLF(t *testing.T) {
	log, dir := newLog(t)
	records := [][]byte{[]byte(`{"record":"task:start"}`), []byte(`{}`), []byte(`{"record":"session:end"}`)}
	var want []byte
	for _, record := range records {
		if err := log.Append(record); err != nil {
			t.Fatal(err)
		}
		want = append(append(want, record...), '\n')
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := log.Append([]byte(`{"after":"close"}`)); err != nil {
		t.Fatalf("append after close = %v, want the recorder's silent no-op", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("events.jsonl = %q, want %q", got, want)
	}
	end, final := log.Extent()
	if !final || end != (Position{Offset: int64(len(want)), Records: 3}) {
		t.Fatalf("extent = %+v final=%v, want %d bytes / 3 records, final", end, final, len(want))
	}
}

// TestADeadSubscriberNeverBlocksTheProducer is the scheduler-safety invariant: a
// subscriber that never reads, never drains its wake signal and never
// acknowledges cannot slow an Append.
func TestADeadSubscriberNeverBlocksTheProducer(t *testing.T) {
	log, _ := newLog(t)
	subscribe(t, log, "dead", 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			_ = log.Append([]byte(`{"record":"task:event"}`))
		}
		_ = log.Close()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("appends stalled behind a subscriber that never reads")
	}
}

// TestALiveSubscriberReadsEveryByteOnceWoken proves the wake signal is enough to
// follow concurrent producers to the final marker, reading only from disk.
func TestALiveSubscriberReadsEveryByteOnceWoken(t *testing.T) {
	log, dir := newLog(t)
	sub := subscribe(t, log, "follower", 0)
	var received bytes.Buffer
	followed := make(chan error, 1)
	go func() {
		var offset int64
		for {
			data, err := sub.Read(offset, 17)
			if err != nil {
				followed <- err
				return
			}
			received.Write(data)
			offset += int64(len(data))
			if err := sub.Ack(offset, false); err != nil {
				followed <- err
				return
			}
			if end, final := sub.Extent(); final && offset == end.Offset {
				followed <- sub.Ack(offset, true)
				return
			}
			if len(data) == 0 {
				<-sub.Wake()
			}
		}
	}()
	var producers sync.WaitGroup
	for p := 0; p < 8; p++ {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for i := 0; i < 200; i++ {
				_ = log.Append([]byte(fmt.Sprintf(`{"producer":%d,"i":%d}`, p, i)))
			}
		}()
	}
	producers.Wait()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-followed; err != nil {
		t.Fatal(err)
	}
	onDisk, _ := os.ReadFile(filepath.Join(dir, EventsFile))
	if !bytes.Equal(received.Bytes(), onDisk) {
		t.Fatalf("subscriber received %d bytes, stream holds %d", received.Len(), len(onDisk))
	}
	evidence, err := sub.Evidence()
	if err != nil || evidence.Evidence != protocolcli.SubscriberEvidenceDelivered || evidence.Lost != 0 || evidence.Acknowledged.Records != 1600 {
		t.Fatalf("evidence = %+v, %v; want delivered, 1600 records, none lost", evidence, err)
	}
}

// TestAcknowledgementsOnlyMoveForward pins "no cursor reset" and the final
// marker's precondition.
func TestAcknowledgementsOnlyMoveForward(t *testing.T) {
	log, _ := newLog(t)
	_ = log.Append([]byte(`{"a":1}`))
	_ = log.Append([]byte(`{"b":2}`))
	sub := subscribe(t, log, "acks", 0)
	if err := sub.Ack(4, false); err != nil {
		t.Fatal(err)
	}
	if err := sub.Ack(3, false); err == nil {
		t.Fatal("a backwards acknowledgement was accepted")
	}
	if err := sub.Ack(100, false); err == nil {
		t.Fatal("an acknowledgement past the committed extent was accepted")
	}
	end, _ := log.Extent()
	if err := sub.Ack(end.Offset, true); err == nil {
		t.Fatal("a final acknowledgement before the stream closed was accepted")
	}
	if _, err := sub.Evidence(); err == nil {
		t.Fatal("evidence was classified before the stream was final")
	}
	if _, err := sub.Read(end.Offset+1, 10); err == nil {
		t.Fatal("a read past the committed extent was accepted")
	}
	if _, err := log.Subscribe("acks", 0); err == nil {
		t.Fatal("a second subscriber took a declared name")
	}
	if _, err := log.Subscribe("late", end.Offset+1); err == nil {
		t.Fatal("a subscription starting past the stream was accepted")
	}
	if _, err := log.Subscribe("Bad Name", 0); err == nil {
		t.Fatal("an undeclarable subscriber name was accepted")
	}
}

// TestEvidenceClassifiesAKilledSubscriberAsPartial is the acceptance shape at
// the stream level: a subscriber that stops mid-record is partial, and its lost
// count is every record it never acknowledged.
func TestEvidenceClassifiesAKilledSubscriberAsPartial(t *testing.T) {
	log, dir := newLog(t)
	for i := 0; i < 4; i++ {
		_ = log.Append([]byte(fmt.Sprintf(`{"i":%d}`, i)))
	}
	killed := subscribe(t, log, "killed", 0)
	silent := subscribe(t, log, "silent", 0)
	finished := subscribe(t, log, "finished", 0)
	if err := killed.Ack(10, false); err != nil { // one record plus 2 bytes of the next
		t.Fatal(err)
	}
	_ = log.Close()
	end, _ := log.Extent()
	if err := finished.Ack(end.Offset, true); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []*Subscription{killed, silent, finished} {
		if err := sub.RecordEvidence(); err != nil {
			t.Fatal(err)
		}
	}
	file, err := ReadEvidence(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []protocolcli.SessionSubscriberEvidence{
		{Name: "finished", Evidence: protocolcli.SubscriberEvidenceDelivered, Acknowledged: end, Lost: 0},
		{Name: "killed", Evidence: protocolcli.SubscriberEvidencePartial, Acknowledged: Position{Offset: 10, Records: 1}, Lost: 3},
		{Name: "silent", Evidence: protocolcli.SubscriberEvidenceLost, Acknowledged: Position{}, Lost: 4},
	}
	if file.SessionID != testSessionID || file.Stream != end || fmt.Sprint(file.Subscribers) != fmt.Sprint(want) {
		t.Fatalf("subscribers.json = %+v, want stream %+v and %+v", file, end, want)
	}
}

// TestRecordEvidenceKeepsOtherSubscribersAndReplacesAStaleStream covers the
// replay case: a resumed subscriber rewrites its own entry only.
func TestRecordEvidenceKeepsOtherSubscribersAndReplacesAStaleStream(t *testing.T) {
	log, dir := newLog(t)
	_ = log.Append([]byte(`{"i":0}`))
	_ = log.Close()
	end, _ := log.Extent()
	other := subscribe(t, log, "other", 0)
	if err := other.RecordEvidence(); err != nil {
		t.Fatal(err)
	}

	replayed, err := Open(dir, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	closeAtCleanup(t, replayed)
	reporter := subscribe(t, replayed, "reporter", 3)
	if err := reporter.Ack(end.Offset, true); err != nil {
		t.Fatal(err)
	}
	if err := reporter.RecordEvidence(); err != nil {
		t.Fatal(err)
	}
	file, err := ReadEvidence(dir)
	if err != nil || len(file.Subscribers) != 2 || file.Subscribers[0].Name != "other" || file.Subscribers[1].Evidence != protocolcli.SubscriberEvidenceDelivered {
		t.Fatalf("merged evidence = %+v, %v", file, err)
	}

	stale := []byte(`{"protocolVersion":1,"sessionId":"` + testSessionID + `","stream":{"offset":1,"records":0},"subscribers":[{"name":"ghost","evidence":"lost","acknowledged":{"offset":0,"records":0},"lost":0}]}`)
	if err := os.WriteFile(filepath.Join(dir, protocolcli.SessionSubscribersFileName), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reporter.RecordEvidence(); err != nil {
		t.Fatal(err)
	}
	file, err = ReadEvidence(dir)
	if err != nil || len(file.Subscribers) != 1 || file.Subscribers[0].Name != "reporter" || file.Stream != end {
		t.Fatalf("a document for a different stream was merged: %+v, %v", file, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.Name() != EventsFile && entry.Name() != protocolcli.SessionSubscribersFileName && entry.Name() != subscribersLockName {
			t.Fatalf("evidence write left %s behind", entry.Name())
		}
	}
}

// TestOpenReadsAFinalizedStreamRecordByRecord is the reader contract the
// sessions commands use: subscribe from 0, every record in order with its line
// ordinal, and torn trailing bytes surfaced once instead of dropped.
func TestOpenReadsAFinalizedStreamRecordByRecord(t *testing.T) {
	dir := t.TempDir()
	long := bytes.Repeat([]byte("x"), 3*readChunk+5)
	content := append(append([]byte("{\"a\":1}\n\n"), long...), []byte("\n{\"torn\"")...)
	if err := os.WriteFile(filepath.Join(dir, EventsFile), content, 0o644); err != nil {
		t.Fatal(err)
	}
	log, err := Open(dir, testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if end, final := log.Extent(); !final || end != (Position{Offset: int64(len(content)), Records: 3}) {
		t.Fatalf("extent = %+v final=%v", end, final)
	}
	sub, err := log.Subscribe("reader", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	var got []Record
	for {
		record, ok, err := sub.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, record)
	}
	if len(got) != 4 ||
		string(got[0].Data) != `{"a":1}` || got[0].Position != (Position{}) || !got[0].Terminated ||
		len(got[1].Data) != 0 || got[1].Position.Records != 1 ||
		!bytes.Equal(got[2].Data, long) || got[2].Position != (Position{Offset: 9, Records: 2}) ||
		string(got[3].Data) != `{"torn"` || got[3].Terminated || got[3].Position.Records != 3 {
		t.Fatalf("records = %d: %+v", len(got), got)
	}
	if _, err := Open(t.TempDir(), testSessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing stream opened: %v", err)
	}
}

// TestAckOfAServedRangeNeedsNoDiskRead pins the acknowledgement memo: acking
// exactly the range Read just served reuses the records Read counted in those
// bytes, while any other range still counts from disk, with the same result.
func TestAckOfAServedRangeNeedsNoDiskRead(t *testing.T) {
	log, dir := newLog(t)
	for i := 0; i < 5; i++ {
		_ = log.Append([]byte(fmt.Sprintf(`{"i":%d}`, i)))
	}
	end, _ := log.Extent()
	served := subscribe(t, log, "served", 0)
	data, err := served.Read(0, 20) // 2 records and 4 bytes of the third
	if err != nil || len(data) != 20 {
		t.Fatalf("read = %q, %v", data, err)
	}

	// Make the disk unreadable for this subscription: any count from disk fails.
	real := served.file
	unreadable, err := os.Open(filepath.Join(dir, EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	_ = unreadable.Close()
	served.file = unreadable
	if err := served.Ack(20, false); err != nil {
		t.Fatalf("acking the served range read the disk: %v", err)
	}
	if served.acked != (Position{Offset: 20, Records: 2}) {
		t.Fatalf("memo count = %+v, want 20 bytes / 2 records", served.acked)
	}
	if err := served.Ack(30, false); err == nil {
		t.Fatal("a range that was never served was counted without reading the disk")
	}

	served.file = real
	if err := served.Ack(end.Offset, false); err != nil || served.acked != end {
		t.Fatalf("fallback ack = %+v, %v; want %+v", served.acked, err, end)
	}
	fromDisk := subscribe(t, log, "disk", 0)
	if err := fromDisk.Ack(20, false); err != nil || fromDisk.acked != (Position{Offset: 20, Records: 2}) {
		t.Fatalf("disk count for the served range = %+v, %v; want the memo's 2 records", fromDisk.acked, err)
	}
	if _, err := fromDisk.Read(0, 10); err != nil {
		t.Fatal(err)
	}
	if err := fromDisk.Ack(30, false); err != nil || fromDisk.acked != (Position{Offset: 30, Records: 3}) {
		t.Fatalf("an ack that is not the served range = %+v, %v; want the disk count", fromDisk.acked, err)
	}
}

// TestCloseWithdrawsTheSubscriber pins that a closed subscription leaves the
// stream: it is never woken again, its name can be declared again, and its
// evidence stays classifiable.
func TestCloseWithdrawsTheSubscriber(t *testing.T) {
	log, _ := newLog(t)
	first := subscribe(t, log, "reporter", 0)
	_ = log.Append([]byte(`{"i":0}`))
	if err := first.Ack(8, false); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for len(first.Wake()) > 0 {
		<-first.Wake()
	}
	second, err := log.Subscribe("reporter", 0)
	if err != nil {
		t.Fatalf("a closed subscriber's name could not be declared again: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	<-second.Wake() // the declaration's own signal
	_ = log.Append([]byte(`{"i":1}`))
	if len(first.Wake()) != 0 {
		t.Fatal("a closed subscription was still woken")
	}
	if len(second.Wake()) != 1 {
		t.Fatal("the redeclared subscription was not woken")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Subscribe("reporter", 0); err == nil {
		t.Fatal("closing a stale handle again withdrew the newer declaration of its name")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := log.Subscribe("reporter", 0)
	if err != nil {
		t.Fatalf("the name was not free after its current holder closed: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	evidence, err := first.Evidence()
	if err != nil || evidence.Evidence != protocolcli.SubscriberEvidencePartial || evidence.Acknowledged.Records != 1 {
		t.Fatalf("evidence after close = %+v, %v", evidence, err)
	}
}
