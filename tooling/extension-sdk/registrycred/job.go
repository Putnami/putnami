package registrycred

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	extensionproto "go.putnami.dev/protocol/extension"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/procguard"
)

// OfflineDependencies reports whether the engine runs this job on a hosted run
// whose dependencies were already downloaded (extensionproto
// OfflineDependenciesEnv). A package manager must then not reach the network
// for dependencies, and no installer may write a credential into the user's
// home: EnsureNativeCredential does nothing.
func OfflineDependencies() bool {
	return os.Getenv(extensionproto.OfflineDependenciesEnv) == "1"
}

// jobCredentialHanded records that the engine named a job credential
// descriptor to this process (extensionproto.JobCredentialFDEnv), whatever the
// descriptor held. ReadJobCredential removes the variable, so the record
// outlives it.
var jobCredentialHanded atomic.Bool

// hostedJob reports whether the engine runs this process as a job of a hosted
// run: the job runs offline (OfflineDependencies), or the engine handed it a
// job credential descriptor, which only a hosted run's workspace-fetch
// receives. A handed descriptor counts even when it carried no credential
// ("{}"): that means the run's provider holds none, not that the run is local.
//
// Such a process starts no `putnami cloud registry-token` child
// (EnsureNativeCredential, ResolveToken). The child is a CLI without the run
// credential that loads the workspace's extensions, and the credential file it
// writes in the user's home is one a repository process can read.
func hostedJob() bool {
	if OfflineDependencies() || jobCredentialHanded.Load() {
		return true
	}
	_, handed := os.LookupEnv(extensionproto.JobCredentialFDEnv)
	return handed
}

// jobCredential is the read of this process's credential descriptor. The
// descriptor is read at most once: a second read would find it closed, and a
// descriptor number the process later reuses for something else must never be
// read as a credential.
var jobCredential struct {
	once       sync.Once
	credential *registry.Credential
	err        error
}

// ReadJobCredential returns the read credential the engine handed this job on
// the descriptor extensionproto.JobCredentialFDEnv names, and removes that
// variable from the process environment. It reads the descriptor once, closes
// it, and answers the same result on every later call.
//
// A nil credential with a nil error means the engine handed none (the variable
// is absent: not a hosted `workspace-fetch`) or the provider holds no read
// credential. The caller then fetches with the credentials the machine
// already has. When the engine handed a descriptor, even one without a
// credential, EnsureNativeCredential and ResolveToken start no process.
//
// The bearer is the caller's to keep out of every environment and every file a
// later process can read: write it only where the one package manager process
// that needs it reads it, and delete that file when the process exits. The
// descriptor is closed before this function returns, so no process the caller
// starts afterwards inherits it. On Linux the process is made non-dumpable
// (procguard.DenyInspection) before the descriptor is read.
func ReadJobCredential() (*registry.Credential, error) {
	jobCredential.once.Do(func() {
		jobCredential.credential, jobCredential.err = readJobCredential()
	})
	return jobCredential.credential, jobCredential.err
}

func readJobCredential() (*registry.Credential, error) {
	value, present := os.LookupEnv(extensionproto.JobCredentialFDEnv)
	_ = os.Unsetenv(extensionproto.JobCredentialFDEnv)
	if !present {
		return nil, nil
	}
	jobCredentialHanded.Store(true)
	fd, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("%s=%q is not a descriptor number above 2", extensionproto.JobCredentialFDEnv, value)
	}
	// Once the bearer is in this process's memory, a process of the same user
	// must not read it through /proc or attach to this one.
	if err := procguard.DenyInspection(); err != nil {
		return nil, fmt.Errorf("deny inspection before reading the job credential: %w", err)
	}
	file := os.NewFile(uintptr(fd), extensionproto.JobCredentialFDEnv)
	if file == nil {
		return nil, fmt.Errorf("%s names descriptor %d, which is not open", extensionproto.JobCredentialFDEnv, fd)
	}
	defer func() { _ = file.Close() }()
	line, err := io.ReadAll(io.LimitReader(file, registry.MaxCredentialLineBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the job credential from descriptor %d: %w", fd, err)
	}
	if len(line) > registry.MaxCredentialLineBytes {
		return nil, fmt.Errorf("the job credential on descriptor %d exceeds %d bytes", fd, registry.MaxCredentialLineBytes)
	}
	line = []byte(strings.TrimSuffix(string(line), "\n"))
	if len(line) == 0 {
		return nil, errors.New("the job credential descriptor was empty")
	}
	result, err := registry.ParseCredentialResult(line)
	if err != nil {
		return nil, fmt.Errorf("the job credential on descriptor %d is invalid: %w", fd, err)
	}
	return result.Credential, nil
}

// resetJobCredentialForTest forgets the read, so a test can hand a new
// descriptor.
func resetJobCredentialForTest() {
	jobCredential.once = sync.Once{}
	jobCredential.credential, jobCredential.err = nil, nil
	jobCredentialHanded.Store(false)
}
