package remotecache

import (
	"context"
	"sync/atomic"
)

// firstWrite orders the writes of a client until the cache server has accepted
// one. The server lets only some credentials write (a developer's reads the
// shared cache and writes nothing), and it says so with a 403 on the first
// write. upload-batch is the one write that carries blob bytes under the
// bearer, so a client that opened with it paid for megabytes to learn that it
// may not write.
//
// Until one write is accepted, upload-batch therefore waits for a find-missing,
// which carries no bytes. One caller at a time holds the slot and sends that
// find-missing; the others wait for its answer instead of each sending their
// own. After the first accepted write no caller waits and none probes.
type firstWrite struct {
	accepted atomic.Bool
	// slot is held by the one caller that sends the first write.
	slot chan struct{}
}

func newFirstWrite() *firstWrite {
	return &firstWrite{slot: make(chan struct{}, 1)}
}

// enter reports whether the caller must send a write without bytes before its
// upload-batch. When it reports true the caller holds the slot and must call
// leave once that write is answered. It waits while another caller holds the
// slot, and gives up when ctx is done.
func (f *firstWrite) enter(ctx context.Context) (bool, error) {
	if f.accepted.Load() {
		return false, nil
	}
	select {
	case f.slot <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if f.accepted.Load() {
		<-f.slot
		return false, nil
	}
	return true, nil
}

// leave releases the slot.
func (f *firstWrite) leave() { <-f.slot }

// accept records that the server accepted a write of this client's bearer.
func (f *firstWrite) accept() { f.accepted.Store(true) }

// reset forgets the verdict when the client's bearer changes.
func (f *firstWrite) reset() { f.accepted.Store(false) }
