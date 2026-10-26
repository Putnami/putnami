package abort

import (
	"sync"
	"time"
)

// InteractiveStopBudget is how long an interactive child, which shares the
// CLI's terminal, has to exit on its own once its run is canceled, before the
// CLI kills it. A first terminal signal cancels the run and the child stops on
// its own terms within the budget; a second one kills it at once
// (KillInteractive).
//
// The dev executable's compose teardown (teardownTimeout in
// tooling/dev/internal/compose/up.go) ends 15s before this budget, so a
// composition releases its lease itself: change both together.
const InteractiveStopBudget = 90 * time.Second

var (
	interactiveMu    sync.Mutex
	interactiveNext  int
	interactiveKills = map[int]func(){}
)

// HoldInteractive records that an interactive child runs, with kill ending it
// at once. The returned release removes the record once the child is gone;
// calling it more than once is harmless.
func HoldInteractive(kill func()) (release func()) {
	interactiveMu.Lock()
	defer interactiveMu.Unlock()
	interactiveNext++
	id := interactiveNext
	interactiveKills[id] = kill
	return func() {
		interactiveMu.Lock()
		defer interactiveMu.Unlock()
		delete(interactiveKills, id)
	}
}

// InteractiveHeld reports whether an interactive child runs.
func InteractiveHeld() bool {
	interactiveMu.Lock()
	defer interactiveMu.Unlock()
	return len(interactiveKills) > 0
}

// KillInteractive ends every interactive child that runs, at once.
func KillInteractive() {
	interactiveMu.Lock()
	kills := make([]func(), 0, len(interactiveKills))
	for _, kill := range interactiveKills {
		kills = append(kills, kill)
	}
	interactiveMu.Unlock()
	for _, kill := range kills {
		kill()
	}
}
