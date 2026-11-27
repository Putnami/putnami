//go:build race

package repository

// raceEnabled is true when this test binary runs under the race detector, so
// the marked tests run under it too.
const raceEnabled = true
