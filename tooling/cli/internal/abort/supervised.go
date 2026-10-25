package abort

import "sync/atomic"

var supervised atomic.Bool

// Supervise records that a supervisor started this run and stops it: a
// supervised serve run, whose stderr its supervisor passes through as a serve
// log. The first signal then prints no interrupt notice, which addresses a
// user at a terminal: the supervisor reports its own stop.
func Supervise() { supervised.Store(true) }

// Supervised reports whether Supervise was called.
func Supervised() bool { return supervised.Load() }
