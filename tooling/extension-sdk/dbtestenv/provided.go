// The provided-server provider behind Up/Down: an externally managed Postgres
// this run consumes but does not own.
//
// A pipeline starts ONE Postgres for the whole run and names it in
// PUTNAMI_TEST_PG_URL. Every project's own `test~test-env` task then does what
// it already does locally — resolve the policy, merge its dependency closure's
// datasources, write the 0600 binding artifact and the lease — against that
// server. There is no workspace-union binding: a task provisions exactly its own
// closure, inside the DAG where its cost is scheduled, bounded, cached and
// measured, instead of inside a test framework's per-suite hook budget.
//
// The POOL every binding carries is not one of them: both providers hand back a
// test binding, and 10 connections per datasource held idle for 30 minutes is
// wrong for a test binary either way, so the bound is applied once for both in
// SynthesizeBinding (see withTestPoolDefaults). What is specific to this
// provider is that PUTNAMI_TEST_PG_URL can override it: the URL's query becomes
// the connection's params, and a key it states wins over the default.
//
// TWO POLICY CHOICES distinguish this provider's binding from the docker one,
// and both exist because the server is shared by the whole run rather than
// started per configuration:
//
//   - isolation: schema. One engine, one database, N schemas. A per-suite
//     CREATE/DROP DATABASE on a busy shared cluster forces an immediate
//     cluster-wide checkpoint whose cost tracks the whole fleet's concurrent
//     writes, not the dropped database; CREATE/DROP SCHEMA costs neither. Both
//     runtime providers (go.putnami.dev/database/testprovider and
//     @putnami/database) implement schema isolation.
//   - reuse: none. The bundle-template layer amortizes migrations across suites
//     that share a LONG-LIVED server. On a server that lives one run, every
//     template is built cold, once per bundle, serialized on a Postgres advisory
//     lock — pure overhead, paid inside the per-suite hook budget. It is turned
//     off by policy, not retired: the value stays legal in the protocol and a
//     caller that wants it supplies its own DATABASE_TEST_BINDINGS, which always
//     wins.
//
// Nothing here starts, configures or removes a server. The only outward call is
// a bounded TCP dial answering "is something accepting connections at that
// endpoint", so a misconfigured URL fails once, here, with a cause — instead of
// failing every suite with a connection error. It is deliberately not a Postgres
// handshake: this module has no database driver and must not grow one.
//
// SECURITY. The URL carries a password, so it is confined exactly like the
// docker provider's: it reaches the pdb.Connection (and therefore the sensitive
// artifact) and nothing else. The digest is taken over the NON-SECRET identity
// alone — host, port, database, user — so no value derived from a provided
// server can carry the credential, and no error this file returns interpolates
// the raw URL (url.Parse echoes its input, so only a sentinel escapes).
package dbtestenv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	pdb "go.putnami.dev/protocol/database"
)

// EnvProvidedServer names the externally managed Postgres a run's test
// environments are built on. It is the pipeline's whole side of the contract:
// start a server, export this, and run the ordinary command.
//
// It is a URL rather than a full DATABASE_TEST_BINDINGS because a binding is
// per-project — it names datasources, schemas and policy this package derives
// from each project's own committed requirements closure — while a server is one
// fact about the machine.
const EnvProvidedServer = "PUTNAMI_TEST_PG_URL"

// providedProviderID names the resource kind a provided-server lease reclaims.
// It exists so a lease stays self-describing; there is nothing to reclaim,
// because this run created nothing.
const providedProviderID = "postgres-provided"

const (
	// DefaultProvidedReachableTimeout bounds the whole reachability wait for a
	// provided server: one named default, no magic numbers, no unbounded wait.
	// It is shorter than the docker provisioner's readiness budget because
	// nothing is being started — the server is either up or misconfigured.
	DefaultProvidedReachableTimeout = 30 * time.Second

	// providedDialTimeout bounds one connection attempt.
	providedDialTimeout = 3 * time.Second

	// providedProbeInterval is the gap between reachability probes.
	providedProbeInterval = 200 * time.Millisecond

	// defaultProvidedPort is the Postgres port a URL without one resolves to.
	defaultProvidedPort = 5432

	// defaultProvidedDatabase is the database a URL with no path resolves to.
	// The maintenance database exists on every cluster, and under schema
	// isolation it is only ever the server the isolated schemas live in.
	defaultProvidedDatabase = "postgres"
)

// errProvidedURLUnparseable is the credential-free error a malformed URL
// surfaces. net/url echoes its whole input in the error it returns, and that
// input carries the password, so nothing derived from a parse failure is ever
// returned to a caller.
var errProvidedURLUnparseable = errors.New(EnvProvidedServer + " is not a parseable postgres:// URL")

// providedConfig is the digest-affecting, NON-SECRET identity of a provided
// server: the coordinates two runs would have to share for their environments
// to be interchangeable. The password is deliberately not a field — exactly as
// in serverConfig — and neither are the query parameters, which a caller may use
// to pass one.
type providedConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
}

// digest is the stable, canonical identity of a provided server. Hex only, so
// no credential substring can appear in a value derived through it.
func (c providedConfig) digest() string { return digestOf(c) }

// dialFunc reports whether the endpoint accepts connections. It is the single
// seam tests replace, so no case in this package needs a live server.
type dialFunc func(ctx context.Context, host string, port int) error

// ProvidedServer is the provider for a Postgres the pipeline manages. The zero
// value is not usable; construct one with NewProvidedServer.
type ProvidedServer struct {
	// url is the raw PUTNAMI_TEST_PG_URL value. It carries the credential, so
	// it never reaches a digest, a lease, a diagnostic or an error.
	url              string
	dial             dialFunc
	reachableTimeout time.Duration
}

// NewProvidedServer returns a ProvidedServer reading PUTNAMI_TEST_PG_URL and
// probing reachability with a bounded TCP dial.
func NewProvidedServer() *ProvidedServer {
	return &ProvidedServer{
		url:              os.Getenv(EnvProvidedServer),
		dial:             dialTCP,
		reachableTimeout: DefaultProvidedReachableTimeout,
	}
}

// id names the resource kind a provided-server lease reclaims.
func (s *ProvidedServer) id() string { return providedProviderID }

// available reports whether the pipeline named a server. It is an environment
// read and nothing more: whether the value is a USABLE URL is Provision's
// answer, because a run that names a broken server must hear why rather than
// silently fall back to another provider.
func (s *ProvidedServer) available() bool {
	return s != nil && strings.TrimSpace(s.url) != ""
}

// ownsServer reports false: the pipeline started this server and the pipeline
// ends it. That is what lets a CI run — where no provider may ever create
// infrastructure — still have a test environment.
func (s *ProvidedServer) ownsServer() bool { return false }

// bindingDefaults states the two choices this provider makes for a shared
// server: schema isolation (no CREATE/DROP DATABASE at all) and no
// bundle-template reuse (a cold template rebuild is pure overhead on a server
// that lives one run). See the file comment for the reasoning.
func (s *ProvidedServer) bindingDefaults() TestPolicy {
	return TestPolicy{Isolation: pdb.IsolationSchema, Reuse: pdb.ReuseNone}
}

// LabelKey is empty: there is no resource to filter on, because this run
// created none.
func (s *ProvidedServer) LabelKey() string { return "" }

// ContainerName is empty for the same reason: no container, no name.
func (s *ProvidedServer) ContainerName(string) string { return "" }

// TeardownAll does nothing and succeeds. `--infra-down` reaps what this
// workspace started; a server the pipeline provided is not that, and tearing it
// down would break every other task still running against it.
func (s *ProvidedServer) TeardownAll() error { return nil }

// Provision resolves the provided URL into a connection and confirms the
// endpoint is accepting connections, bounded by reachableTimeout.
//
// It creates nothing, so "provisioning" here is exactly two things: turning one
// fact about the machine into the structured connection a binding carries, and
// failing early — once per task, with a cause — when that fact is wrong.
func (s *ProvidedServer) Provision() (pdb.Connection, string, error) {
	conn, cfg, err := parseProvidedURL(s.url)
	if err != nil {
		return pdb.Connection{}, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.reachableTimeout)
	defer cancel()
	if err := s.waitReachable(ctx, cfg.Host, cfg.Port); err != nil {
		return pdb.Connection{}, "", err
	}
	return conn, cfg.digest(), nil
}

// waitReachable blocks until the endpoint accepts a connection or the bounded
// deadline elapses. Each attempt is itself context-bounded. Never unbounded.
func (s *ProvidedServer) waitReachable(ctx context.Context, host string, port int) error {
	endpoint := net.JoinHostPort(host, strconv.Itoa(port))
	for {
		attempt, cancel := context.WithTimeout(ctx, providedDialTimeout)
		err := s.dial(attempt, host, port)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("provided postgres server %s did not accept a connection within %s",
				endpoint, s.reachableTimeout)
		case <-time.After(providedProbeInterval):
		}
	}
}

// parseProvidedURL turns the provided URL into the structured connection the
// binding carries and the non-secret identity its digest is taken over.
//
// Structured rather than a DSN passthrough, for two reasons. The digest can then
// be taken over the identity alone, with the password nowhere near it. And the
// runtime provider can rewrite the database — which is what database isolation
// needs — if a caller ever overrides the schema-isolation default.
func parseProvidedURL(raw string) (pdb.Connection, providedConfig, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return pdb.Connection{}, providedConfig{}, fmt.Errorf("%s is empty", EnvProvidedServer)
	}
	u, err := url.Parse(value)
	if err != nil {
		return pdb.Connection{}, providedConfig{}, errProvidedURLUnparseable
	}
	switch u.Scheme {
	case "postgres", "postgresql":
	default:
		return pdb.Connection{}, providedConfig{}, fmt.Errorf(
			"%s must be a postgres:// URL, not %q", EnvProvidedServer, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return pdb.Connection{}, providedConfig{}, fmt.Errorf("%s names no host", EnvProvidedServer)
	}
	port := defaultProvidedPort
	if text := u.Port(); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed <= 0 || parsed > 65535 {
			return pdb.Connection{}, providedConfig{}, fmt.Errorf(
				"%s port %q is not a TCP port", EnvProvidedServer, text)
		}
		port = parsed
	}
	database := strings.TrimPrefix(u.Path, "/")
	if database == "" {
		database = defaultProvidedDatabase
	}
	password, _ := u.User.Password()

	conn := pdb.Connection{
		Host:     host,
		Port:     port,
		Database: database,
		User:     u.User.Username(),
		Password: password,
		Params:   queryParams(u),
	}
	cfg := providedConfig{Host: host, Port: port, Database: database, User: u.User.Username()}
	return conn, cfg, nil
}

// queryParams projects the URL's query onto the connection's driver options
// (sslmode, application_name, …), first value wins for a repeated key. Returns
// nil for an empty query so the binding JSON omits the member.
//
// This is also how a pipeline overrides the test-pool default: a pool parameter
// in the URL arrives here, and SynthesizeBinding fills only the keys the
// connection left unsaid.
func queryParams(u *url.URL) map[string]string {
	query := u.Query()
	if len(query) == 0 {
		return nil
	}
	params := make(map[string]string, len(query))
	for key, values := range query {
		if len(values) > 0 && strings.TrimSpace(values[0]) != "" {
			params[key] = values[0]
		}
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

// dialTCP is the production reachability probe: one bounded TCP connect. Its
// error names the endpoint, never the URL, so no credential can reach a
// diagnostic through it.
func dialTCP(ctx context.Context, host string, port int) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}
