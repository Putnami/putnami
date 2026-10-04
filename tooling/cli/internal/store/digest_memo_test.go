package store

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// Concurrent callers for one key share one computation, and two keys never
// share an answer.
func TestCacheManagerDigestComputesOncePerKey(t *testing.T) {
	cm := NewCacheManager(nil)
	var computed atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]string, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value, err := cm.Digest("tree:/ext", func() (string, error) {
				computed.Add(1)
				<-release
				return "digest-a", nil
			})
			if err != nil {
				t.Error(err)
			}
			results[i] = value
		}(i)
	}
	close(release)
	wg.Wait()
	if n := computed.Load(); n != 1 {
		t.Errorf("one key computed %d times, want 1", n)
	}
	for _, got := range results {
		if got != "digest-a" {
			t.Errorf("a caller got %q, want the one computed answer", got)
		}
	}
	other, err := cm.Digest("tree:/other", func() (string, error) { return "digest-b", nil })
	if err != nil || other != "digest-b" {
		t.Errorf("another key answered %q, %v; want its own value", other, err)
	}
}

// An error is the key's answer for the manager's life, so one run never keys
// one input two ways; a nil manager computes on every call.
func TestCacheManagerDigestKeepsAnErrorAndANilManagerComputesEachTime(t *testing.T) {
	cm := NewCacheManager(nil)
	boom := errors.New("boom")
	calls := 0
	compute := func() (string, error) {
		calls++
		if calls == 1 {
			return "", boom
		}
		return "late", nil
	}
	for i := 0; i < 2; i++ {
		if _, err := cm.Digest("k", compute); !errors.Is(err, boom) {
			t.Fatalf("call %d: error = %v, want the first answer", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("computed %d times, want 1", calls)
	}

	var nilManager *CacheManager
	calls = 0
	for i := 0; i < 2; i++ {
		_, _ = nilManager.Digest("k", func() (string, error) { calls++; return "", nil })
	}
	if calls != 2 {
		t.Errorf("a nil manager computed %d times, want 2", calls)
	}
}
