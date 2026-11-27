//go:build !race

package repository

// raceEnabled is false when this test binary runs without the race detector.
const raceEnabled = false
