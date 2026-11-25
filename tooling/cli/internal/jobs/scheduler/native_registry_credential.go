package scheduler

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	cache "go.putnami.dev/protocol/cache"
	extensionproto "go.putnami.dev/protocol/extension"
	hostrpcproto "go.putnami.dev/protocol/extension/hostrpc"
	registryproto "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// nativeCredentialsKey carries the run's nativeCredentials in the context the
// scheduler hands its jobs.
type nativeCredentialsKey struct{}

// nativeCredentialValidity is how long the engine holds an answer: the window
// the seam's `--for-run` shape promises. Only a test changes it
// (HoldNativeCredentialsForTest).
var nativeCredentialValidity = registryproto.NativeCredentialValidity

// HoldNativeCredentialsForTest makes an answer hold for window, so a test that
// counts the seam calls of a run is not moved by how long the run takes. It
// returns the function that restores the window.
func HoldNativeCredentialsForTest(window time.Duration) (restore func()) {
	previous := nativeCredentialValidity
	nativeCredentialValidity = window
	return func() { nativeCredentialValidity = previous }
}

// materializeNativeCredential is the seam call the engine makes for one host,
// a variable so tests count calls without a cloud.
var materializeNativeCredential = registrycred.MaterializeNativeCredential

// nativeCredentials holds the run's answer for each host whose native registry
// credential a job declared (CLI ADR 0101). The engine calls the seam once per
// host and per validity window, before the first job that needs it; the jobs
// that need the host while a call runs wait for it and share its answer.
type nativeCredentials struct {
	root string
	now  func() time.Time
	mu   sync.Mutex
	// hosts holds one entry per host, each with its own lock, so a call for
	// one host never waits for another.
	hosts map[string]*nativeCredentialHost
}

type nativeCredentialHost struct {
	mu     sync.Mutex
	answer extensionproto.NativeRegistryCredential
	held   bool
}

func newNativeCredentials(root string) *nativeCredentials {
	return &nativeCredentials{root: root, now: time.Now, hosts: map[string]*nativeCredentialHost{}}
}

// withNativeCredentials gives ctx the run's credential answers.
func withNativeCredentials(ctx context.Context, credentials *nativeCredentials) context.Context {
	return context.WithValue(ctx, nativeCredentialsKey{}, credentials)
}

// nativeCredentialEnv returns the NativeRegistryCredentialsEnv entry for job:
// the answer that holds for the host its task declares, materialized first
// when the run holds none. It returns "" when the job declares no host, the
// context carries no run answers (a job run outside the scheduler), or the run
// is hosted: a hosted run calls nothing (ADR 0055), and its jobs skip the
// credential themselves.
func nativeCredentialEnv(ctx context.Context, job *ScheduledJob) string {
	credentials, _ := ctx.Value(nativeCredentialsKey{}).(*nativeCredentials)
	if credentials == nil || job == nil || job.JobDef == nil || job.JobDef.NativeRegistryCredential == nil {
		return ""
	}
	if runcredential.Hosted() || os.Getenv(extensionproto.OfflineDependenciesEnv) == "1" {
		return ""
	}
	host := declaredRegistryHost(job, *job.JobDef.NativeRegistryCredential)
	if host == "" {
		return ""
	}
	answer, ok := credentials.ensure(ctx, host)
	if !ok || !namedToJobs(answer) {
		return ""
	}
	return extensionproto.NativeRegistryCredentialsEnv + "=" + extensionproto.NativeRegistryCredentials{host: answer}.Encode()
}

// ensure returns the answer for host that holds now, and calls the seam once
// when there is none: on the first job that needs host, and on the first one
// after the answer stops holding.
func (c *nativeCredentials) ensure(ctx context.Context, host string) (extensionproto.NativeRegistryCredential, bool) {
	c.mu.Lock()
	entry := c.hosts[host]
	if entry == nil {
		entry = &nativeCredentialHost{}
		c.hosts[host] = entry
	}
	c.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.held && entry.answer.Holds(c.now()) {
		return entry.answer, true
	}
	executable, err := os.Executable()
	if err != nil || executable == "" {
		return extensionproto.NativeRegistryCredential{}, false
	}
	start := c.now()
	// The child is a CLI that loads the workspace's extensions, as
	// `putnami cloud release-set` is (ADR 0055 §4).
	runcredential.MarkRepositoryCodeStarted("putnami " + registryproto.SeamParentCommand + " " + registryproto.SeamSubcommand)
	// A job canceled while the call runs does not cut it short for the jobs
	// waiting on the same host: the seam bounds the call itself, at its 10 s
	// timeout and 1 s wait for the child's pipes, so a canceled run waits at
	// most that long for a call in flight.
	outcome := materializeNativeCredential(context.WithoutCancel(ctx), executable, c.root, host, nativeCredentialChildEnv(os.Environ()))
	answer, ok := registrycred.EngineAnswer(outcome, start.Add(nativeCredentialValidity))
	entry.answer, entry.held = answer, ok
	return answer, ok
}

// namedToJobs reports whether the engine hands its answer to the jobs. A
// cloud that refused --for-run (unsupported) wrote nothing that holds for the
// window, and a failed call (a timeout, an unreachable cloud) may succeed for
// the next job: the engine names neither, and each job calls the seam itself,
// as it does outside a run. The engine still asks again only after the window.
func namedToJobs(answer extensionproto.NativeRegistryCredential) bool {
	return answer.Outcome != extensionproto.NativeCredentialUnsupported && answer.Outcome != extensionproto.NativeCredentialFailed
}

// nativeCredentialChildEnv is the engine's environment without the variables
// that state facts of this run's spawns, which buildJobInvocation strips from
// a job for the same reasons: a bound request would hijack the child into a
// portable execution, and the provider choice, the caller and invocation
// directories, the cache socket, the host RPC channel, the job credential
// descriptor and the native credential answers name this process's launch,
// not the child's.
func nativeCredentialChildEnv(env []string) []string {
	for _, key := range []string{
		runner.BoundRequestEnv, ProvidersEnv, CallerDirEnv, InvocationDirEnv,
		cache.ObjectCacheSocketEnv, cache.CacheTrustEnv,
		hostrpcproto.SocketEnv, hostrpcproto.TokenEnv,
		extensionproto.JobCredentialFDEnv, extensionproto.NativeRegistryCredentialsEnv,
	} {
		env = removeEnvKey(env, key)
	}
	return env
}

// declaredRegistryHost is the lowercase host of the endpoint the task
// declares: member Endpoint of the project's effective registries entry
// Registry, a URL or a bare host. It is "" when the entry, the member or its
// value is absent or unreadable.
func declaredRegistryHost(job *ScheduledJob, declared extensionproto.TaskRegistryCredential) string {
	if job.Project == nil {
		return ""
	}
	raw, ok := job.Project.Registries[declared.Registry]
	if !ok || len(raw) == 0 {
		return ""
	}
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil {
		return ""
	}
	var endpoint string
	if json.Unmarshal(entry[declared.Endpoint], &endpoint) != nil {
		return ""
	}
	return hostOfEndpoint(endpoint)
}

// hostOfEndpoint is the lowercase host name of a declared endpoint. A bare
// host is the shape a person writes; url.Parse reads it as a path, so a scheme
// is supplied first.
func hostOfEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}
