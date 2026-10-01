package infra

import (
	"math/rand/v2"
	"time"
)

// The manifests this package writes replace their destination through a
// temporary file and a rename, so a reader observes the previous file or the
// complete new one. On Windows each side can fail for a moment instead: a
// rename or a removal fails while another process holds the file open without
// sharing delete access, which is how every Go reader opens a file, and an open
// fails while a rename replaces the file. renameFile, removeFile and readFile
// retry those failures for a bounded time, as cmd/go's internal robustio
// package does. On Unix they call os.Rename, os.Remove and os.ReadFile once.
// go.putnami.dev/sdk/extension/robustio is the reference implementation.

// retry calls op until it succeeds, fails with an error transient rejects, or
// budget would run out before the next attempt, sleeping a randomized,
// doubling backoff between attempts. It returns op's last error.
func retry(op func() error, transient func(error) bool, budget time.Duration) error {
	start := time.Now()
	sleep := time.Millisecond
	for {
		err := op()
		if err == nil || !transient(err) {
			return err
		}
		if time.Since(start)+sleep >= budget {
			return err
		}
		time.Sleep(sleep)
		sleep += rand.N(sleep)
	}
}
