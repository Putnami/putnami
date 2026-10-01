package versioncmd

import "time"

const (
	// renameAttempts and renameRetryDelay bound how long a switch waits for a
	// transient refusal to clear: 20 attempts 100 ms apart, under 2 s in all.
	renameAttempts   = 20
	renameRetryDelay = 100 * time.Millisecond
)

// retryingRename returns a rename that calls rename again, after sleep(delay),
// while transient reports its error as transient, up to attempts calls in all.
// It returns the last error. An antivirus or indexing service that holds a
// freshly written binary open makes Windows refuse a rename for a moment; a
// retry outlasts it, as the go command does for the same errors.
func retryingRename(
	rename func(oldpath, newpath string) error,
	transient func(error) bool,
	attempts int,
	delay time.Duration,
	sleep func(time.Duration),
) func(oldpath, newpath string) error {
	return func(oldpath, newpath string) error {
		for attempt := 1; ; attempt++ {
			err := rename(oldpath, newpath)
			if err == nil || attempt >= attempts || !transient(err) {
				return err
			}
			sleep(delay)
		}
	}
}
