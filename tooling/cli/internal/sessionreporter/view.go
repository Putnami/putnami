package sessionreporter

import (
	"bytes"
	"container/heap"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
)

// viewHeldCap is the most records the view holds at once, a record several
// owners hold counted once (D-R2). Past it, the owner that holds the most
// releases them, as a failure would, and sends live from then on. Only tests
// lower it.
var viewHeldCap = 1_000_000

// viewReadGap and viewReadSpan bound how a frame reads its bytes back from
// events.jsonl: ranges of one frame whose raw gap is at most viewReadGap share
// one read of at most viewReadSpan bytes.
const (
	viewReadGap  = 4 * 1024
	viewReadSpan = 2 * protocolcli.SessionReportingChunkBytes
)

// errViewOutside is a view offset the view does not cover: before the
// acknowledged cursor or past what left.
var errViewOutside = errors.New("position outside the event stream view")

// view is the trimmed view of a session's events.jsonl that the session
// reporter frames (ADR 0083). It reads the stream by raw offset through the
// subscription, splits it into records itself, and decides each record once,
// whole, from the records before it: it drops progress events, holds an
// owner's task:event records from its first debug line to its task:end, and
// drops the held debug lines of an owner that passes. A record that names an
// owner the cap made live leaves live. What leaves is a queue
// of raw byte ranges, each at its view offset; a frame reads its bytes back
// from disk. The view keeps byte ranges, never record bytes, beyond the window
// it scans, the record it decodes and the frame it cuts.
//
// A Run builds its view from raw offset 0 whenever it starts, so every view
// offset names the same bytes after an engine restart or a replay. Ranges
// wholly before the acknowledged cursor are never queued.
type view struct {
	events *sessionstream.Subscription
	// cursor is the view offset the receiver acknowledged, and end the view
	// offset after the last range that left.
	cursor, end int64
	// scan is the raw offset of the first record not decided yet.
	scan int64
	// window holds committed raw bytes from windowStart. partial holds the
	// bytes of the record at scan read so far, when no window held its LF.
	window      []byte
	windowStart int64
	partial     []byte
	// owners are the owners that hold records, and the ones the cap made live.
	owners map[string]*viewOwner
	// groups are the open groups, by their key (group). key, holderKeys and
	// holders are scratch space for the record being decided.
	groups     map[string]*viewGroup
	key        []byte
	holderKeys []string
	holders    []*viewOwner
	// held counts the records held, a shared record once.
	held int
	// queue is the ranges that left and are not wholly before cursor, in view
	// order. lows is the minimum raw offset of the queue's suffixes: lows[0]
	// names the queued range with the smallest raw offset.
	queue []viewRange
	lows  []viewLow
	// done reports that the stream is final and every record left or was
	// dropped: the view's final marker may follow its last byte.
	done bool
}

// viewRange is a raw byte range of events.jsonl and the view offset it
// starts at.
type viewRange struct{ raw, size, at int64 }

// viewLow is one candidate of the queue's raw minimum: the range's raw start
// and the view offset it ends at.
type viewLow struct{ raw, end int64 }

// viewEntry is one held record: its raw range.
type viewEntry struct{ raw, size int64 }

// viewGroup holds, once, the records one set of owners holds, each list in raw
// order: debug the debug log lines, other the other records. Each of its
// owners references the group, never its records, so a record that many
// owners hold costs one entry, and an owner one reference per owner set it
// shares. members counts the owners that reference it.
//
// A group takes records while it is open, until one of its owners ends or the
// cap releases one: a record of the same owners then opens a new group. Its
// lists leave whole: other at its first owner's end, debug at the first end
// that fails, or dropped at the last one, which passes.
type viewGroup struct {
	// key is the group's key while it is open, and empty once it is closed.
	key          string
	members      int
	debug, other []viewEntry
}

// viewOwner is one owner of records (ADR 0083, rule 2): one that holds, from
// its first debug log line to its task:end, or one the cap made live.
type viewOwner struct {
	// groups are the groups the owner holds records in.
	groups []*viewGroup
	// live: the cap released this owner, which never holds again.
	live bool
}

// viewRecord decodes the members of a record the view decides on.
type viewRecord struct {
	Record   string `json:"record"`
	Identity struct {
		Key  string `json:"key"`
		Task struct {
			Name string `json:"name"`
		} `json:"task"`
	} `json:"identity"`
	Event struct {
		Type    string                     `json:"type"`
		Level   string                     `json:"level"`
		Context map[string]json.RawMessage `json:"context"`
	} `json:"event"`
	Task struct {
		Status string `json:"status"`
	} `json:"task"`
}

// newView starts a view of events at raw offset 0, its acknowledged cursor
// at view offset cursor.
func newView(events *sessionstream.Subscription, cursor int64) *view {
	return &view{events: events, cursor: cursor, owners: map[string]*viewOwner{}, groups: map[string]*viewGroup{}}
}

// available is how many view bytes past offset are ready to frame. It scans
// only as far as one frame past offset needs.
func (v *view) available(offset int64) (int64, error) {
	if err := v.fill(offset + protocolcli.SessionReportingChunkBytes); err != nil {
		return 0, err
	}
	return v.end - offset, nil
}

// frame returns up to one chunk of view bytes at offset, and whether the view
// is done, so that a final marker may follow its last byte.
func (v *view) frame(offset int64) ([]byte, bool, error) {
	if err := v.fill(offset + protocolcli.SessionReportingChunkBytes); err != nil {
		return nil, false, err
	}
	data, err := v.read(offset, protocolcli.SessionReportingChunkBytes)
	return data, v.done, err
}

// acknowledged moves the cursor to the view offset the receiver acknowledged
// and returns the raw offset the subscription may acknowledge: every record
// before it is decided, and every byte of it that left is before cursor. It
// is the smallest of the first undecided record, the first held record, and
// the first queued range in raw order. It never moves back.
func (v *view) acknowledged(cursor int64) int64 {
	v.advance(cursor)
	low := v.scan
	if len(v.lows) > 0 {
		low = min(low, v.lows[0].raw)
	}
	for _, o := range v.owners {
		for _, g := range o.groups {
			if raw, ok := g.first(); ok {
				low = min(low, raw)
			}
		}
	}
	return low
}

// advance moves the cursor forward and drops the ranges wholly before it.
func (v *view) advance(cursor int64) {
	v.cursor = max(v.cursor, cursor)
	for len(v.queue) > 0 && v.queue[0].at+v.queue[0].size <= v.cursor {
		v.queue = v.queue[1:]
	}
	for len(v.lows) > 0 && v.lows[0].end <= v.cursor {
		v.lows = v.lows[1:]
	}
}

// fill decides records until the view holds target bytes, the committed
// extent ends, or the view is done. On a final stream, the last record
// releases what is still held, then any torn tail leaves.
func (v *view) fill(target int64) error {
	for !v.done && v.end < target {
		extent, final := v.events.Extent()
		line, ok, err := v.nextLine(extent.Offset)
		if err != nil {
			return err
		}
		if !ok {
			if !final {
				return nil
			}
			v.finish(extent.Offset)
			continue
		}
		raw, size := v.scan, int64(len(line))+1
		v.scan += size
		v.decide(line, raw, size)
		if cap(v.partial) > 1<<20 {
			v.partial = nil
		} else {
			v.partial = v.partial[:0]
		}
	}
	return nil
}

// nextLine returns the record at scan without its LF, or false when no LF
// before end ends it. A record may span any number of windows.
func (v *view) nextLine(end int64) ([]byte, bool, error) {
	for from := v.scan + int64(len(v.partial)); from < end; {
		chunk, err := v.windowAt(from)
		if err != nil {
			return nil, false, err
		}
		if i := bytes.IndexByte(chunk, '\n'); i >= 0 {
			if len(v.partial) == 0 {
				return chunk[:i], true, nil
			}
			v.partial = append(v.partial, chunk[:i]...)
			return v.partial, true, nil
		}
		v.partial = append(v.partial, chunk...)
		from += int64(len(chunk))
	}
	return nil, false, nil
}

// windowAt returns committed raw bytes from offset, read from disk through
// the subscription, which never reads past the committed extent.
func (v *view) windowAt(offset int64) ([]byte, error) {
	if offset >= v.windowStart && offset < v.windowStart+int64(len(v.window)) {
		return v.window[offset-v.windowStart:], nil
	}
	data, err := v.events.Read(offset, protocolcli.SessionReportingChunkBytes)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errViewOutside
	}
	v.window, v.windowStart = data, offset
	return data, nil
}

// decide places one whole record, raw bytes [raw, raw+size), in the view.
func (v *view) decide(line []byte, raw, size int64) {
	var record viewRecord
	if json.Unmarshal(line, &record) != nil {
		// Rule 9: a line that is not a JSON record leaves as it is.
		v.emit(raw, size)
		return
	}
	switch record.Record {
	case protocolcli.RecordTaskEvent:
		v.taskEvent(&record, raw, size)
		return
	case protocolcli.RecordTaskEnd:
		v.taskEnd(record.Identity.Key, record.Task.Status)
	case protocolcli.RecordSessionEnd:
		v.releaseAll()
	}
	v.emit(raw, size)
}

// taskEvent drops a progress event, sends live a record that names an owner
// the cap made live or that no owner holds, and holds any other one in the
// group of the owners that hold it. An owner starts holding at its first debug
// log line, unless the cap made it live.
func (v *view) taskEvent(record *viewRecord, raw, size int64) {
	if record.Event.Type == string(runtimeproto.EventProgress) {
		return
	}
	debug := record.Event.Type == string(runtimeproto.EventLog) && record.Event.Level == string(runtimeproto.LevelDebug)
	live := false
	v.holderKeys, v.holders = v.holderKeys[:0], v.holders[:0]
	for _, key := range record.owners() {
		o := v.owners[key]
		if debug && o == nil {
			o = &viewOwner{}
			v.owners[key] = o
		}
		switch {
		case o == nil:
		case o.live:
			live = true
		default:
			v.holderKeys, v.holders = append(v.holderKeys, key), append(v.holders, o)
		}
	}
	if live || len(v.holders) == 0 {
		v.emit(raw, size)
		return
	}
	g := v.group()
	if debug {
		g.debug = append(g.debug, viewEntry{raw: raw, size: size})
	} else {
		g.other = append(g.other, viewEntry{raw: raw, size: size})
	}
	v.held++
	if v.held > viewHeldCap {
		v.releaseLargest()
	}
}

// group returns the open group of the holders of the record being decided,
// and opens it, referenced by each of them, at its first record. Its key lists
// the owner keys in record order, each prefixed with its length.
func (v *view) group() *viewGroup {
	v.key = v.key[:0]
	for _, key := range v.holderKeys {
		v.key = strconv.AppendInt(v.key, int64(len(key)), 10)
		v.key = append(v.key, ':')
		v.key = append(v.key, key...)
	}
	if g, ok := v.groups[string(v.key)]; ok {
		return g
	}
	g := &viewGroup{key: string(v.key), members: len(v.holders)}
	v.groups[g.key] = g
	for _, o := range v.holders {
		o.join(g)
	}
	return g
}

// close stops g from taking records.
func (v *view) close(g *viewGroup) {
	if g.key != "" {
		delete(v.groups, g.key)
		g.key = ""
	}
}

// taskEnd releases what the ending owner holds, in raw order, before its
// task:end leaves. A record leaves now unless the end passes and it is a
// debug line; such a line stays held for the other owners that hold it, or is
// dropped when none does.
func (v *view) taskEnd(key, status string) {
	o := v.owners[key]
	if o == nil || o.live {
		return
	}
	passed := status == protocolcli.TaskStatusSuccess || status == protocolcli.TaskStatusSkipped
	var lists [][]viewEntry
	for _, g := range o.groups {
		v.close(g)
		g.members--
		lists = append(lists, g.other)
		v.held -= len(g.other)
		g.other = nil
		if !passed {
			lists = append(lists, g.debug)
		}
		if !passed || g.members == 0 {
			v.held -= len(g.debug)
			g.debug = nil
		}
	}
	delete(v.owners, key)
	v.emitMerged(lists)
}

// releaseLargest releases every record of the owner that holds the most, as
// a failure would, and makes it live (D-R2). A tie goes to the owner whose
// first held record comes first, then to the smaller owner key.
func (v *view) releaseLargest() {
	var largest *viewOwner
	largestKey, largestCount, largestFirst := "", 0, int64(0)
	for key, o := range v.owners {
		count, first := o.holds()
		if count == 0 {
			continue
		}
		if largest == nil || count > largestCount ||
			count == largestCount && (first < largestFirst || first == largestFirst && key < largestKey) {
			largest, largestKey, largestCount, largestFirst = o, key, count, first
		}
	}
	if largest == nil {
		return
	}
	lists := make([][]viewEntry, 0, 2*len(largest.groups))
	for _, g := range largest.groups {
		v.close(g)
		g.members--
		lists = append(lists, g.debug, g.other)
		v.held -= len(g.debug) + len(g.other)
		g.debug, g.other = nil, nil
	}
	largest.groups, largest.live = nil, true
	v.emitMerged(lists)
}

// releaseAll releases every held record, in raw order, as a session:end
// record or the final marker does. Owners the cap made live stay live.
func (v *view) releaseAll() {
	var lists [][]viewEntry
	for key, o := range v.owners {
		for _, g := range o.groups {
			v.close(g)
			if !g.empty() {
				lists = append(lists, g.debug, g.other)
				v.held -= len(g.debug) + len(g.other)
				g.debug, g.other = nil, nil
			}
		}
		o.groups = nil
		if !o.live {
			delete(v.owners, key)
		}
	}
	v.emitMerged(lists)
}

// emitMerged sends the records of lists, each in raw order, merged in raw
// order. No two records share a raw offset: each is held in one group only.
func (v *view) emitMerged(lists [][]viewEntry) {
	merge := make(viewMerge, 0, len(lists))
	for _, list := range lists {
		if len(list) > 0 {
			heap.Push(&merge, list)
		}
	}
	for len(merge) > 0 {
		e := merge[0][0]
		v.emit(e.raw, e.size)
		if merge[0] = merge[0][1:]; len(merge[0]) == 0 {
			heap.Pop(&merge)
		} else {
			heap.Fix(&merge, 0)
		}
	}
}

// viewMerge is a heap of non-empty record lists by their first raw offset.
type viewMerge [][]viewEntry

func (m viewMerge) Len() int           { return len(m) }
func (m viewMerge) Less(i, j int) bool { return m[i][0].raw < m[j][0].raw }
func (m viewMerge) Swap(i, j int)      { m[i], m[j] = m[j], m[i] }

func (m *viewMerge) Push(x any) {
	if list, ok := x.([]viewEntry); ok {
		*m = append(*m, list)
	}
}

func (m *viewMerge) Pop() any {
	old := *m
	last := old[len(old)-1]
	*m = old[:len(old)-1]
	return last
}

// finish ends the view of a final stream that ends at end: what is still
// held leaves in raw order, then any torn unterminated tail, as it is.
func (v *view) finish(end int64) {
	v.releaseAll()
	if v.scan < end {
		v.emit(v.scan, end-v.scan)
		v.scan = end
	}
	v.partial, v.window = nil, nil
	v.done = true
}

// emit appends raw bytes [raw, raw+size) to the view. A range wholly before
// the cursor is skipped; one that continues the last queued range in raw
// order extends it.
func (v *view) emit(raw, size int64) {
	at := v.end
	v.end += size
	if v.end <= v.cursor {
		return
	}
	if n := len(v.queue); n > 0 && v.queue[n-1].raw+v.queue[n-1].size == raw {
		// The last queued range is always the last candidate of lows.
		v.queue[n-1].size += size
		v.lows[len(v.lows)-1].end = v.end
		return
	}
	for len(v.lows) > 0 && v.lows[len(v.lows)-1].raw >= raw {
		v.lows = v.lows[:len(v.lows)-1]
	}
	v.lows = append(v.lows, viewLow{raw: raw, end: v.end})
	v.queue = append(v.queue, viewRange{raw: raw, size: size, at: at})
}

// read returns up to max view bytes at offset, read back from events.jsonl.
func (v *view) read(offset int64, max int) ([]byte, error) {
	if offset < v.cursor || offset > v.end {
		return nil, errViewOutside
	}
	stop := min(offset+int64(max), v.end)
	var pieces []viewRange
	i := sort.Search(len(v.queue), func(i int) bool { return v.queue[i].at+v.queue[i].size > offset })
	for at := offset; at < stop; i++ {
		if i >= len(v.queue) || v.queue[i].at > at {
			return nil, errViewOutside
		}
		r := v.queue[i]
		skip := at - r.at
		take := min(r.size-skip, stop-at)
		pieces = append(pieces, viewRange{raw: r.raw + skip, size: take})
		at += take
	}
	data := make([]byte, 0, stop-offset)
	for len(pieces) > 0 {
		start, end, n := pieces[0].raw, pieces[0].raw+pieces[0].size, 1
		for ; n < len(pieces); n++ {
			p := pieces[n]
			if p.raw < end || p.raw-end > viewReadGap || p.raw+p.size-start > viewReadSpan {
				break
			}
			end = p.raw + p.size
		}
		raw, err := v.events.Read(start, int(end-start))
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) != end-start {
			return nil, errViewOutside
		}
		for _, p := range pieces[:n] {
			data = append(data, raw[p.raw-start:p.raw-start+p.size]...)
		}
		pieces = pieces[n:]
	}
	return data, nil
}

// owners are the owners of a task:event record (rule 2): each member a batch
// line names, as <projectId>:<task name without -batch>, or else its
// identity key. A record without either has no owner, and is never held.
func (r *viewRecord) owners() []string {
	var ids []string
	if raw, ok := r.Event.Context[protocolcli.BatchProjectLogContextKey]; ok {
		var id string
		if json.Unmarshal(raw, &id) == nil && id != "" {
			ids = append(ids, id)
		}
	}
	if raw, ok := r.Event.Context[protocolcli.BatchProjectLogsContextKey]; ok {
		var shared []any
		if json.Unmarshal(raw, &shared) == nil {
			for _, value := range shared {
				if id, ok := value.(string); ok && id != "" && !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
	}
	if len(ids) == 0 {
		if r.Identity.Key == "" {
			return nil
		}
		return []string{r.Identity.Key}
	}
	name := strings.TrimSuffix(r.Identity.Task.Name, "-batch")
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = id + ":" + name
	}
	return keys
}

// join references g. A full list first drops the groups that closed empty,
// so the groups of owner sets that ended do not accumulate, then keeps room
// for as many again.
func (o *viewOwner) join(g *viewGroup) {
	if len(o.groups) == cap(o.groups) {
		o.groups = slices.DeleteFunc(o.groups, func(g *viewGroup) bool { return g.key == "" && g.empty() })
		o.groups = slices.Grow(o.groups, len(o.groups)+1)
	}
	o.groups = append(o.groups, g)
}

// holds is how many records the owner holds, and the raw offset of the first.
func (o *viewOwner) holds() (int, int64) {
	count, first := 0, int64(0)
	for _, g := range o.groups {
		raw, ok := g.first()
		if !ok {
			continue
		}
		if count == 0 || raw < first {
			first = raw
		}
		count += len(g.debug) + len(g.other)
	}
	return count, first
}

// empty reports whether the group holds no record.
func (g *viewGroup) empty() bool { return len(g.debug) == 0 && len(g.other) == 0 }

// first is the raw offset of the group's first record, or false when it holds
// none.
func (g *viewGroup) first() (int64, bool) {
	switch {
	case g.empty():
		return 0, false
	case len(g.debug) == 0:
		return g.other[0].raw, true
	case len(g.other) == 0:
		return g.debug[0].raw, true
	}
	return min(g.debug[0].raw, g.other[0].raw), true
}
