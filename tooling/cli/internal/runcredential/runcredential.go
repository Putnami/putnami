// Package runcredential holds the run credential: the bearer a hosted runner
// hands the engine on a descriptor named by --credential-fd, so that it never
// sits in an environment, an argument or a file a repository process can read.
//
// Capture reads it before the process can start anything. It denies inspection
// of the process (procguard), reads the descriptor to its end, at most
// MaxBytes, closes it, and keeps the bearer in memory only. A process that
// captured a credential also drops PUTNAMI_CACHE_TOKEN, PUTNAMI_CLOUD_TOKEN,
// PUTNAMI_SESSION_REPORTER_TOKEN and PUTNAMI_LOG_REPORTER_TOKEN from its
// environment, because the run credential replaces them, and sets
// PUTNAMI_OFFLINE_DEPENDENCIES to "1" there, because only the fetch downloads
// dependencies.
//
// The credential leaves the process only for a holder: the cache provider,
// the credential-provider, the session reporter and the log reporter receive
// it over their protocol, a workspace-fetch job
// receives the job credential it yields, and when the process replaces its
// image with another CLI (Exec), the new image receives the bearer on a fresh
// pipe that --credential-fd names again. Every holder starts before the
// process starts repository code (StartHolder, MarkRepositoryCodeStarted).
package runcredential

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/procguard"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/iox"
)

const (
	// Flag names the descriptor that carries the run credential.
	Flag = "--credential-fd"
	// MaxBytes bounds what Capture reads from the descriptor, the trailing
	// line ending included.
	MaxBytes = registry.MaxRunCredentialBytes
	// CacheTokenEnv is the remote-cache credential a CI environment exports.
	// A process that holds the run credential removes it.
	CacheTokenEnv = "PUTNAMI_CACHE_TOKEN"
	// CloudTokenEnv is the cloud control-plane credential a CI environment
	// exports (extensionproto.CloudTokenEnv). A process that holds the run
	// credential removes it, so no job, hook or child receives it.
	CloudTokenEnv = extensionproto.CloudTokenEnv
	// Redacted is how every fmt verb renders a held credential.
	Redacted = "<redacted>"
	// firstDescriptor is the lowest descriptor Flag may name: 0, 1 and 2 are
	// the standard streams.
	firstDescriptor = 3
)

// credential is the captured run credential.
type credential struct {
	bearer string
	// custody records whether repository code started since the process
	// held the credential (see MarkRepositoryCodeStarted).
	custody *custody
}

func newCredential(bearer string) *credential {
	return &credential{bearer: bearer, custody: &custody{}}
}

// Format renders the credential as Redacted for every verb, so a diagnostic
// that formats it shows nothing of the bearer.
func (credential) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, Redacted)
}

var held atomic.Pointer[credential]

// Current returns the run credential this process holds, and whether it
// holds one.
func Current() (bearer string, ok bool) {
	c := held.Load()
	if c == nil {
		return "", false
	}
	return c.bearer, true
}

// Hosted reports whether this process captured a run credential.
func Hosted() bool {
	return held.Load() != nil
}

// SetForTest makes the process hold bearer as its run credential, or none
// when bearer is empty, until restore runs. The credential starts with no
// repository code recorded (MarkRepositoryCodeStarted), and restore brings
// back the previous credential with its own record. A test that calls it
// changes the whole process, so it must not run in parallel.
func SetForTest(bearer string) (restore func()) {
	var next *credential
	if bearer != "" {
		next = newCredential(bearer)
	}
	previous := held.Swap(next)
	return func() { held.Store(previous) }
}

// Capture reads the run credential when args carry Flag, and does nothing
// otherwise or once the process holds a credential. It honors the flag
// wherever it stands in args, a bare "--" included, as the global flag parser
// does: a descriptor left open would pass to every process this one starts.
//
// Every failure is a usage error that names the flag and the descriptor and
// never the bytes read.
func Capture(args []string) error {
	if Hosted() {
		return nil
	}
	c, err := capture(args, runtime.GOOS, procguard.DenyInspection, readDescriptor)
	if err != nil || c == nil {
		return err
	}
	held.Store(c)
	dropCredentialVariables(os.Stderr)
	declareOffline()
	return nil
}

// capture is Capture without the process state: the platform, the inspection
// guard and the descriptor reader are its parameters.
func capture(args []string, goos string, deny func() error, read func(fd int) ([]byte, error)) (*credential, error) {
	values, err := flagValues(args)
	if err == nil && len(values) == 0 {
		return nil, nil
	}
	if goos == "windows" {
		return nil, cmderr.Usagef("%s is not supported on Windows", Flag)
	}
	if err != nil {
		return nil, err
	}
	if len(values) > 1 {
		return nil, cmderr.Usagef("%s is given %d times: pass the run credential on one descriptor", Flag, len(values))
	}
	fd, err := Descriptor(values[0])
	if err != nil {
		return nil, err
	}
	// Inspection is denied before the bytes exist in this process.
	if err := deny(); err != nil {
		return nil, cmderr.Usagef("%s %d: %v", Flag, fd, err)
	}
	data, err := read(fd)
	if err != nil {
		return nil, cmderr.Usagef("%s %d: %v", Flag, fd, err)
	}
	defer clear(data)
	bearer, err := parseBearer(data)
	if err != nil {
		return nil, cmderr.Usagef("%s %d: %v", Flag, fd, err)
	}
	return newCredential(bearer), nil
}

// flagValues returns the value of every occurrence of Flag in args, spelled
// "--credential-fd <n>" or "--credential-fd=<n>".
func flagValues(args []string) ([]string, error) {
	var values []string
	for i := 0; i < len(args); i++ {
		if value, ok := strings.CutPrefix(args[i], Flag+"="); ok {
			values = append(values, value)
			continue
		}
		if args[i] != Flag {
			continue
		}
		if i+1 >= len(args) {
			return values, cmderr.Usagef("flag %s requires a value: %s <n>", Flag, Flag)
		}
		i++
		values = append(values, args[i])
	}
	return values, nil
}

// WithoutFlag returns args without any occurrence of Flag and its value. Capture
// has already read the descriptor, so a caller that accepts no other argument
// (the executing side of a bound portable request) checks what remains.
func WithoutFlag(args []string) []string {
	var rest []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], Flag+"=") {
			continue
		}
		if args[i] == Flag {
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return rest
}

// Descriptor parses a value of Flag: a descriptor number of 3 or more.
func Descriptor(value string) (int, error) {
	fd, err := strconv.Atoi(value)
	if err != nil || fd < 0 {
		return 0, cmderr.Usagef("invalid %s value %q: expected a descriptor number", Flag, value)
	}
	if fd < firstDescriptor {
		return 0, cmderr.Usagef("invalid %s value %d: descriptors 0, 1 and 2 are the standard streams", Flag, fd)
	}
	return fd, nil
}

// parseBearer returns the bearer data holds: at most MaxBytes, less one
// trailing "\n" or "\r\n", non-empty and without whitespace.
func parseBearer(data []byte) (string, error) {
	if len(data) > MaxBytes {
		return "", fmt.Errorf("the credential exceeds %d bytes", MaxBytes)
	}
	text := string(data)
	if trimmed, ok := strings.CutSuffix(text, "\r\n"); ok {
		text = trimmed
	} else {
		text = strings.TrimSuffix(text, "\n")
	}
	if !registry.ValidRunCredential(text) {
		return "", errors.New("the credential is empty, holds whitespace, or is not UTF-8")
	}
	return text, nil
}

// credentialVariables are the framework credentials a CI environment
// exports. A hosted run hands its providers and its reporters the run
// credential instead.
var credentialVariables = []string{
	CacheTokenEnv, CloudTokenEnv, protocolcli.SessionReporterTokenEnv, protocolcli.LogReporterTokenEnv,
}

// dropCredentialVariables removes credentialVariables from the environment of
// a process that holds the run credential, and names on stderr each one that
// held a value.
func dropCredentialVariables(stderr io.Writer) {
	for _, name := range credentialVariables {
		value, present := os.LookupEnv(name)
		if !present {
			continue
		}
		_ = os.Unsetenv(name)
		if value != "" {
			iox.Fprintf(stderr, "putnami: %s: removed %s from the environment; the run credential replaces it\n", Flag, name)
		}
	}
}

// declareOffline sets extensionproto.OfflineDependenciesEnv to "1" in the
// environment of a process that holds the run credential. A cache key that
// declares the variable as an input reads it there, so a hosted run keys apart
// from a run that may download. ChildEnv removes it from the fetch.
func declareOffline() {
	_ = os.Setenv(extensionproto.OfflineDependenciesEnv, "1")
}

// Guard denies inspection of this process when it holds credentials: the run
// credential, or the ones the credential provider serves when
// providersEnabled. It has an effect on Linux only; see procguard.
func Guard(providersEnabled bool) error {
	return guard(Hosted(), providersEnabled, procguard.DenyInspection)
}

func guard(hosted, providersEnabled bool, deny func() error) error {
	if !hosted && !providersEnabled {
		return nil
	}
	if err := deny(); err != nil {
		return fmt.Errorf("protect the credentials of this process: %w", err)
	}
	return nil
}

// withDescriptor returns a copy of argv, a program path and its arguments,
// whose Flag names fd: the value of the first occurrence is replaced, or the
// flag is appended.
func withDescriptor(argv []string, fd int) []string {
	value := strconv.Itoa(fd)
	next := append([]string(nil), argv...)
	for i := 1; i < len(next); i++ {
		if strings.HasPrefix(next[i], Flag+"=") || (next[i] == Flag && i+1 == len(next)) {
			next[i] = Flag + "=" + value
			return next
		}
		if next[i] == Flag {
			next[i+1] = value
			return next
		}
	}
	return append(next, Flag+"="+value)
}
