package fileindex

import (
	"os"
	"sync"
)

// A sample of the record's answers is collected again (Sample, tooling/cli
// ADR 0113 §1). A process that serves one run checks a sampled answer before
// the run uses it. A machine worker defers the checks (DeferSamples): its run
// uses a sampled answer as it uses any other, and the worker checks it once
// the run returned and before it takes its next request, in the time it would
// otherwise wait idle (TakeDeferredSamples). An answer that differs then
// discards the record on disk, so the next run starts from an empty record.

// deferredSample is a sampled answer a run used: the record that answered
// it, the file index the run read files through, the witness the record
// answered it under, and its check.
type deferredSample struct {
	record *Record
	index  *Index
	parts  []RecordPart
	check  func() string
}

var deferred struct {
	sync.Mutex
	on      bool
	pending []deferredSample
	// discarded holds, by record file, the reason a deferred check discarded
	// the record, until a run of that record takes it (TakeDeferredDiscard).
	discarded map[string]string
}

// DeferSamples makes this process check the answers Sample picks once its run
// returned (TakeDeferredSamples) rather than before the run uses them, when on
// is set: a machine worker sets it before it serves its first run. A process
// that never sets it, in CI, in a hosted run or with the machine process off,
// checks each one when the record answers it. Turning it off drops the checks
// still queued.
func DeferSamples(on bool) {
	deferred.Lock()
	defer deferred.Unlock()
	deferred.on = on
	if !on {
		deferred.pending = nil
	}
}

// Defer queues the check of an answer Sample picked, and reports whether it
// did. parts is the witness the record answered it under; check collects the
// value again and names how it differs from the answer, "" when it does not,
// and runs through the file index current now. Nothing is queued, and the
// caller checks the answer now, in a process that does not defer samples
// (DeferSamples) and for a record that writes nothing, which a discard could
// not empty for the next run.
func (r *Record) Defer(parts []RecordPart, check func() string) bool {
	if !r.enabled() || r.readOnly || r.file == "" || check == nil {
		return false
	}
	deferred.Lock()
	defer deferred.Unlock()
	if !deferred.on {
		return false
	}
	deferred.pending = append(deferred.pending, deferredSample{
		record: r, index: Current(), parts: append([]RecordPart(nil), parts...), check: check,
	})
	return true
}

// TakeDeferredSamples takes the checks Defer queued, and returns what runs
// them in order: it reports how many collected a value again, and the reason
// of the first answer that differed, "" when none did.
//
// A check collects only while the answer's witness holds, stated anew: a
// part that moved since the answer, before or while the value is collected
// again, was written after the run read it, and the record refuses that
// answer to the next run by the same witness. An answer that differs under a
// witness that held throughout is one the record should not have trusted:
// its record is discarded and saved empty, as a sampled answer that differs
// within a run discards it, so every later run starts from an empty record
// and collects every value again, and the next run of that record reports
// why (TakeDeferredDiscard). A record that cannot be saved is removed.
func TakeDeferredSamples() (run func() (checked int, reason string)) {
	deferred.Lock()
	pending := deferred.pending
	deferred.pending = nil
	deferred.Unlock()
	return func() (int, string) {
		checked, first := 0, ""
		for _, s := range pending {
			if s.record.Discarded() != "" || !holdsBy(s.parts, freshStat) {
				continue
			}
			restore := Use(s.index)
			reason := s.check()
			restore()
			checked++
			if reason == "" || !holdsBy(s.parts, freshStat) {
				continue
			}
			s.record.discard(reason, false)
			if err := s.record.Save(); err != nil {
				_ = os.Remove(s.record.file)
			}
			deferred.Lock()
			if deferred.discarded == nil {
				deferred.discarded = map[string]string{}
			}
			deferred.discarded[s.record.file] = reason
			deferred.Unlock()
			if first == "" {
				first = reason
			}
		}
		return checked, first
	}
}

// TakeDeferredDiscard returns the reason a deferred check discarded the
// record file at path since a run of it last took one, and "" when none did.
// The run that opens the record next reports it: the run that used the answer
// had already returned.
func TakeDeferredDiscard(path string) string {
	deferred.Lock()
	defer deferred.Unlock()
	reason := deferred.discarded[path]
	delete(deferred.discarded, path)
	return reason
}
