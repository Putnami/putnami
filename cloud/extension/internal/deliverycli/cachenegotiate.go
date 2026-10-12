package deliverycli

import (
	"fmt"
	"sync"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// Late negotiates. The prefetch negotiates the keys core knows when the run
// starts. Core computes the key of a task only once the tasks it depends on
// have finished, so it also restores keys the prefetch did not cover. Each of
// those restores used to send a negotiate of its own key, then a
// download-batch for the small files of its own entry: two requests per key.
//
// A finished task releases the tasks that depend on it together, so their
// restores reach the provider within a few milliseconds of each other. A
// restore that needs a negotiate now joins the open group of its mode. The
// group goes out as one negotiate once no key has joined it for
// lateNegotiateQuiet, at the latest lateNegotiateMax after its first key, and
// at once when it holds lateNegotiateKeys keys. Before the group answers its
// restores, it records one download-batch request for the small files of its
// hits, as the prefetch does (see cacheinline.go).
//
// The wait costs a restore little. A restore that negotiates already waits for
// at least two cache-server round trips, the negotiate and then a
// download-batch or a download, each tens of milliseconds from a CI runner. A
// restore that arrives alone waits lateNegotiateQuiet more, less than one
// round trip. A restore of a burst waits at most lateNegotiateMax. A restore
// keeps its slot of restoreConcurrency while it waits, so a group holds at
// most that many restores of the read loop, and the slots are back within
// lateNegotiateMax.
//
// A group whose negotiate fails leaves each of its restores to negotiate its
// own key, as a failed prefetch does. A group of one key, a closed session, or
// a refused credential has nothing to gain from that: the restore reports the
// failure, as before.

// The bounds of a group. They are variables so a test can change them; a
// session reads them once, when it is created.
var (
	// lateNegotiateQuiet sends a group once no key has joined it for this long.
	lateNegotiateQuiet = 20 * time.Millisecond
	// lateNegotiateMax sends a group at the latest this long after its first
	// key joined it. The restores that one finished task releases reach the
	// provider well within it.
	lateNegotiateMax = 50 * time.Millisecond
	// lateNegotiateKeys sends a group at once when it holds this many keys,
	// the most one negotiate takes.
	lateNegotiateKeys = cache.MaxKeysPerRequest
)

// lateNegotiates gathers the restores that need a negotiate into groups, one
// open group per mode.
type lateNegotiates struct {
	mu   sync.Mutex
	open map[cache.Mode]*lateNegotiate
	// quiet, max and keys are the bounds of a group, as the session read them
	// when it was created.
	quiet, max time.Duration
	keys       int
}

// lateNegotiate is one group: the keys of one mode that share a negotiate.
type lateNegotiate struct {
	mode cache.Mode
	// keys lists the keys of the group in the order they joined, each once.
	// It does not change once the group is sealed.
	keys []string
	has  map[string]bool
	// first and last are when the first and the newest key joined.
	first, last time.Time
	// sealed is set when the group takes no more keys. full is closed when the
	// group sealed because it holds lateNegotiateKeys keys.
	sealed bool
	full   chan struct{}
	// done is closed once results and err are set and the download-batch
	// requests for the small files of the hits are recorded.
	done    chan struct{}
	results map[string]providerCacheResult
	err     error
}

// init reads the bounds of a group. It sets the fields in place: a
// lateNegotiates holds a mutex and is never copied.
func (l *lateNegotiates) init() {
	l.quiet = lateNegotiateQuiet
	l.max = lateNegotiateMax
	l.keys = lateNegotiateKeys
}

// join adds key to the open group of mode, opening one when there is none,
// and returns the group. start reports that the group is new: the caller then
// starts its sender.
func (l *lateNegotiates) join(mode cache.Mode, key string, now time.Time) (g *lateNegotiate, start bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g = l.open[mode]
	if g == nil {
		g = &lateNegotiate{
			mode:  mode,
			has:   make(map[string]bool),
			first: now,
			full:  make(chan struct{}),
			done:  make(chan struct{}),
		}
		if l.open == nil {
			l.open = make(map[cache.Mode]*lateNegotiate)
		}
		l.open[mode] = g
		start = true
	}
	g.last = now
	if !g.has[key] {
		g.has[key] = true
		g.keys = append(g.keys, key)
	}
	if len(g.keys) >= l.keys {
		l.sealLocked(g)
		close(g.full)
	}
	return g, start
}

// sealLocked closes g to new keys: the next key of its mode opens a new
// group. The caller holds l.mu.
func (l *lateNegotiates) sealLocked(g *lateNegotiate) {
	if l.open[g.mode] == g {
		delete(l.open, g.mode)
	}
	g.sealed = true
}

// due seals g and returns zero once it is due. Otherwise it returns how long
// to wait before asking again.
func (l *lateNegotiates) due(g *lateNegotiate, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if g.sealed {
		return 0
	}
	wait := min(g.last.Add(l.quiet).Sub(now), g.first.Add(l.max).Sub(now))
	if wait <= 0 {
		l.sealLocked(g)
		return 0
	}
	return wait
}

// seal closes g to new keys at once.
func (l *lateNegotiates) seal(g *lateNegotiate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sealLocked(g)
}

// negotiateKey negotiates one key for a restore, in the open group of its
// mode, and returns the key's outcome: a key the server did not answer is a
// miss. The restore waits for the group to be sent and answered.
func (s *providerSession) negotiateKey(mode cache.Mode, key string) (providerCacheResult, error) {
	g, start := s.late.join(mode, key, time.Now())
	if start {
		s.bg.Add(1)
		go s.sendLateNegotiate(g)
	}
	select {
	case <-g.done:
	case <-s.ctx.Done():
		return providerCacheResult{}, s.ctx.Err()
	}
	if g.err == nil {
		return g.results[key], nil
	}
	if _, refused := cacheAuthRefusal(g.err); refused || len(g.keys) == 1 || s.ctx.Err() != nil {
		return providerCacheResult{}, g.err
	}
	results, err := s.negotiateKeys(mode, []string{key})
	if err != nil {
		return providerCacheResult{}, err
	}
	return results[key], nil
}

// sendLateNegotiate waits until g is due, sends it, and answers its restores.
// It records the download-batch requests for the small files of the hits
// before it answers, so a restore of a hit always finds the request that
// carries its files, and sends them after.
func (s *providerSession) sendLateNegotiate(g *lateNegotiate) {
	defer s.bg.Done()
	s.awaitLateNegotiate(g)
	g.results, g.err = s.negotiateKeys(g.mode, g.keys)
	var inline []*inlineBatch
	if g.err == nil {
		inline = s.planInlineLate(g.keys, g.results)
	} else if len(g.keys) > 1 {
		s.logCacheError(fmt.Sprintf("negotiate of %d late keys, each restore negotiates its own key", len(g.keys)), g.err)
	}
	close(g.done)
	s.fetchInline("late restores", inline)
}

// awaitLateNegotiate returns once g is due and sealed: no key has joined it
// for the quiet delay, its first key is as old as the group may get, it is
// full, or the session is closing.
func (s *providerSession) awaitLateNegotiate(g *lateNegotiate) {
	for {
		wait := s.late.due(g, time.Now())
		if wait <= 0 {
			return
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-g.full:
		case <-s.ctx.Done():
			timer.Stop()
			s.late.seal(g)
			return
		}
		timer.Stop()
	}
}
