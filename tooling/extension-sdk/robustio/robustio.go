// Package robustio retries the renames and removals Windows fails for a
// moment, and writes a file through a temporary file and such a rename, so a
// reader sees the previous file or the new one, never a part.
//
// Windows fails a rename or a removal while another process, such as a reader
// of the file, an antivirus scanner or the search indexer, holds a handle on
// the file without sharing delete access: the call fails with
// ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION until that handle closes.
// Rename and Remove follow cmd/go's internal robustio package: a bounded retry
// with a randomized, doubling backoff. Unix renames and removes once, as
// os.Rename and os.Remove do.
package robustio

import (
	"math/rand/v2"
	"time"
)

// Rename is os.Rename. On Windows it retries for up to two seconds while the
// rename fails with ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION, and then
// returns the last error.
func Rename(oldpath, newpath string) error {
	return rename(oldpath, newpath)
}

// Remove is os.Remove. On Windows it retries for up to two seconds while the
// removal fails with ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION, and then
// returns the last error.
func Remove(path string) error {
	return remove(path)
}

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
