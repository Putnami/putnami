package extension

import (
	"path/filepath"
	"slices"
	"sync"
)

// A machine worker holds what extension discovery loaded under the stats of
// what discovery read (CLI ADR 0116 §6a). WitnessProbes takes each of them
// before discovery reads the path: a write after the read moves the path
// away from a stat taken before it, so no write since the read hides under
// the witness. Every directory discovery probes for an extension is
// witnessed with its manifest and its putnami.json, also where none of them
// exists: an extension that arrives there later moves the witness of that
// absence.

// probeWitnesses is one WitnessProbes in progress.
type probeWitnesses struct {
	witness func(path string) string
	taken   map[string]string
}

var probes struct {
	sync.Mutex
	active []*probeWitnesses
}

// WitnessProbes makes discovery take, with witness, the stat of each
// directory it probes for an extension and of the manifest and putnami.json
// there, before it reads any of them, until the function it returns is
// called; that function returns them by path, each path's first. witness
// takes the stat of an absent path too.
func WitnessProbes(witness func(path string) string) (taken func() map[string]string) {
	w := &probeWitnesses{witness: witness, taken: map[string]string{}}
	probes.Lock()
	probes.active = append(probes.active, w)
	probes.Unlock()
	return func() map[string]string {
		probes.Lock()
		defer probes.Unlock()
		probes.active = slices.DeleteFunc(probes.active, func(a *probeWitnesses) bool { return a == w })
		return w.taken
	}
}

// probing witnesses dir, a directory discovery is about to probe for an
// extension, for every WitnessProbes in progress.
func probing(dir string) {
	probes.Lock()
	defer probes.Unlock()
	if len(probes.active) == 0 || dir == "" {
		return
	}
	dir = filepath.Clean(dir)
	for _, path := range []string{dir, filepath.Join(dir, ManifestFilename), filepath.Join(dir, "putnami.json")} {
		for _, w := range probes.active {
			if _, ok := w.taken[path]; !ok {
				w.taken[path] = w.witness(path)
			}
		}
	}
}
