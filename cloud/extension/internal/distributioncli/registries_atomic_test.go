package distributioncli

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

// errEmptyState marks a read that returned no access entries — treated as a
// truncation-adjacent failure in the concurrency test below.
var errEmptyState = errors.New("read a state with no access entries")

// TestWriteRegistriesStateConcurrentReadNeverTruncated reproduces the publish
// stampede: `cloud token` refreshes rewrite registries.json while concurrent
// publish jobs (e.g. cloud-publish-archives) read it. With a truncating write a
// reader observes an empty file and fails with
// "registries.json: unexpected end of JSON input". The atomic temp+rename write
// closes that window, so every read must return a complete state, never an error.
func TestWriteRegistriesStateConcurrentReadNeverTruncated(t *testing.T) {
	t.Parallel()
	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}

	// Seed a valid file so readers always have a complete state to observe.
	if err := WriteRegistriesState(env, sampleState("seed")); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	const writers, readers, iterations = 4, 8, 200

	var writersWg, readersWg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, writers+readers)

	for id := range writers {
		writersWg.Go(func() {
			for range iterations {
				if err := WriteRegistriesState(env, sampleState(string(rune('a'+id)))); err != nil {
					errs <- err
					return
				}
			}
		})
	}

	for range readers {
		readersWg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				// A truncated read surfaces as a non-nil error here; a valid
				// read returns a populated state (never nil, since a complete
				// file always exists once seeded).
				state, err := readRegistriesState(env)
				if err != nil {
					errs <- err
					return
				}
				if state == nil || len(state.Access) == 0 {
					errs <- errEmptyState
					return
				}
			}
		})
	}

	writersWg.Wait()
	close(stop)
	readersWg.Wait()
	close(errs)

	for err := range errs {
		if strings.Contains(err.Error(), "unexpected end of JSON input") {
			t.Fatalf("reader observed a truncated write: %v", err)
		}
		t.Fatalf("unexpected error during concurrent access: %v", err)
	}
}

// TestReadRegistriesStateToleratesEmptyFile guards the defense-in-depth path: a
// residual zero-byte file (from an older, non-atomic CLI) must read as "no
// state" rather than crashing the reader.
func TestReadRegistriesStateToleratesEmptyFile(t *testing.T) {
	t.Parallel()
	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}

	if err := os.WriteFile(registriesStatePath(env), nil, 0o600); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}

	state, err := readRegistriesState(env)
	if err != nil {
		t.Fatalf("empty file should not error, got: %v", err)
	}
	if state != nil {
		t.Fatalf("empty file should read as nil state, got: %+v", state)
	}
}

// TestWriteRegistriesStatePermissions confirms the atomic write keeps the file
// 0600 (the temp+rename path must not widen permissions).
func TestWriteRegistriesStatePermissions(t *testing.T) {
	t.Parallel()
	env := map[string]string{"PUTNAMI_HOME": t.TempDir()}

	if err := WriteRegistriesState(env, sampleState("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(registriesStatePath(env))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("registries.json perm = %o, want 600", perm)
	}
}

func sampleState(marker string) *RegistriesState {
	return &RegistriesState{
		Version: 1,
		Keys: []KeyRef{
			{Registry: RegistryGomod, ID: "id-" + marker, Host: "go.putnami.dev"},
			{Registry: RegistryOCI, ID: "id-" + marker, Host: "oci.putnami.dev"},
		},
		Access: map[string]registryAccessToken{
			"go.putnami.dev":  {AccessToken: "tok-go-" + marker, TokenType: "Bearer"},
			"oci.putnami.dev": {AccessToken: "tok-oci-" + marker, TokenType: "Bearer"},
		},
	}
}
