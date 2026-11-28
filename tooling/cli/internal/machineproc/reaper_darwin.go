package machineproc

// macOS has no subreaper: the processes a worker's run started outlive a
// worker that ends during the run, as they outlive a CLI that ends then.

func becomeSubreaper() {}

func endOrphans() {}

func reapOrphans() {}

func reapOnExit() {}

var liveWorkers = func() map[int]bool { return nil }
