package storage

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
)

func TestMemoryBackend_ConcurrentPutGet(t *testing.T) {
	backend := NewMemoryBackend()
	ctx := context.Background()
	const numGoroutines = 20
	const opsPerGoroutine = 50

	var wg sync.WaitGroup
	errs := make(chan error, numGoroutines*opsPerGoroutine)

	// Concurrent puts
	for i := range numGoroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range opsPerGoroutine {
				key := fmt.Sprintf("key-%d-%d", i, j)
				data := []byte(fmt.Sprintf("value-%d-%d", i, j))
				_, err := backend.Put(ctx, "test-bucket", key, bytes.NewReader(data), nil)
				if err != nil {
					errs <- fmt.Errorf("put %s: %w", key, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Concurrent gets
	errs2 := make(chan error, numGoroutines*opsPerGoroutine)
	for i := range numGoroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range opsPerGoroutine {
				key := fmt.Sprintf("key-%d-%d", i, j)
				result, err := backend.Get(ctx, "test-bucket", key)
				if err != nil {
					errs2 <- fmt.Errorf("get %s: %w", key, err)
					return
				}
				if result == nil {
					errs2 <- fmt.Errorf("get %s: not found", key)
					return
				}
				result.Body.Close()
			}
		}(i)
	}
	wg.Wait()
	close(errs2)
	for err := range errs2 {
		t.Error(err)
	}
}

func TestMemoryBackend_ConcurrentPutDeleteExists(t *testing.T) {
	backend := NewMemoryBackend()
	ctx := context.Background()
	const numGoroutines = 20
	const opsPerGoroutine = 50

	var wg sync.WaitGroup

	// Half goroutines put, half delete — exercises mutex contention
	for i := range numGoroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range opsPerGoroutine {
				key := fmt.Sprintf("shared-key-%d", j)
				if i%2 == 0 {
					backend.Put(ctx, "bucket", key, bytes.NewReader([]byte("data")), nil)
				} else {
					backend.Delete(ctx, "bucket", key)
				}
			}
		}(i)
	}
	wg.Wait()

	// Concurrent exists checks
	for range numGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range opsPerGoroutine {
				key := fmt.Sprintf("shared-key-%d", j)
				backend.Exists(ctx, "bucket", key)
			}
		}()
	}
	wg.Wait()
}

func TestMemoryBackend_ConcurrentListAndWrite(t *testing.T) {
	backend := NewMemoryBackend()
	ctx := context.Background()
	const numWriters = 10
	const numReaders = 10

	// Pre-populate
	for i := range 20 {
		key := fmt.Sprintf("item-%02d", i)
		backend.Put(ctx, "bucket", key, bytes.NewReader([]byte("data")), nil)
	}

	var wg sync.WaitGroup

	// Writers adding/deleting concurrently with readers listing
	for i := range numWriters {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("new-item-%d", i)
			backend.Put(ctx, "bucket", key, bytes.NewReader([]byte("new")), nil)
			backend.Delete(ctx, "bucket", key)
		}(i)
	}

	errs := make(chan error, numReaders)
	for range numReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := backend.List(ctx, "bucket", nil)
			if err != nil {
				errs <- err
				return
			}
			if result == nil {
				errs <- fmt.Errorf("list returned nil")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestMemoryBackend_ConcurrentCopy(t *testing.T) {
	backend := NewMemoryBackend()
	ctx := context.Background()

	// Pre-populate source
	backend.Put(ctx, "bucket", "source", bytes.NewReader([]byte("original")), nil)

	var wg sync.WaitGroup
	const numGoroutines = 20

	errs := make(chan error, numGoroutines)
	for i := range numGoroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dest := fmt.Sprintf("copy-%d", i)
			if err := backend.Copy(ctx, "bucket", "source", dest); err != nil {
				errs <- fmt.Errorf("copy to %s: %w", dest, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
