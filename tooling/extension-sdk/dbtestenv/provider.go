// The provider seam Up and Down act through, and the one rule that picks a
// provider for a run.
//
// Two providers ship. The docker Provisioner (docker.go) STARTS a Postgres this
// run owns, which is the local default. ProvidedServer (provided.go) consumes a
// Postgres the pipeline already started and named in PUTNAMI_TEST_PG_URL, which
// is the only thing a CI runner can do — and the reason the task, not the
// runner, stays the place the intelligence lives.
//
// The seam is small on purpose: everything Up and Down do — the gates, the
// closure merge, the artifact, the lease — is provider-independent, and a
// provider answers only four questions about itself (what it is, whether it can
// be used, whether it owns what it hands back, and what policy its binding
// carries).
package dbtestenv

import (
	"os"
	"strings"

	pdb "go.putnami.dev/protocol/database"
)

// Provider is the resource half of a test environment: something that hands
// back a ready Postgres connection and a non-secret digest identifying it.
//
// The interface is SEALED — its policy members are unexported, so only the two
// providers in this package can implement it. That is deliberate: `ownsServer`
// steers the fail-closed gate that decides whether a run may create
// infrastructure, and a gate whose answer can be supplied from outside is not a
// gate.
type Provider interface {
	// LabelKey is the resource label a lease records so a later invocation can
	// reclaim by filter. Empty when the provider owns no reclaimable resource.
	LabelKey() string
	// ContainerName is the deterministic resource name for a configuration
	// digest. Empty when the provider owns no resource.
	ContainerName(digest string) string
	// Provision returns a ready-to-use connection and the server's non-secret
	// configuration digest.
	Provision() (pdb.Connection, string, error)
	// TeardownAll releases every resource this provider owns. A provider that
	// owns nothing reports success without acting.
	TeardownAll() error

	// id names the resource kind a lease reclaims. A locator, never a
	// credential.
	id() string
	// available reports whether this provider can be used at all. Cheap by
	// contract: a PATH lookup or an environment read — never a daemon call, a
	// network call or a parse.
	available() bool
	// ownsServer reports whether the provider CREATES the server it hands back.
	// It is what the fail-closed CI gate keys on: a run may never start
	// infrastructure of its own on CI or outside `auto`, while consuming a
	// server the pipeline already provided is exactly what a CI run is for.
	ownsServer() bool
	// bindingDefaults is the isolation/reuse policy this provider's synthesized
	// binding carries to the language runtime's test provider.
	bindingDefaults() TestPolicy
}

// TestPolicy is the provisioning policy the synthesized binding carries: the
// resolved mode plus the isolation and reuse the PROVIDER chose.
//
// An empty Isolation or Reuse is omitted from the binding JSON, which is how
// the docker provider keeps the runtime defaults it has always shipped
// (database isolation, no template reuse) without stating them.
type TestPolicy struct {
	// Mode is the resolved auto/require/skip policy.
	Mode pdb.TestMode
	// Isolation is the per-datasource boundary the runtime provider creates.
	Isolation pdb.Isolation
	// Reuse is the migrated-database caching policy.
	Reuse pdb.Reuse
}

// SelectProvider returns the provider this run must use.
//
// PUTNAMI_TEST_PG_URL wins whenever it is set: naming a server is an explicit
// instruction, it is cheaper than starting a container, and on CI it is the only
// option. A blank value selects docker, the local default. An externally
// supplied DATABASE_TEST_BINDINGS still beats both — Up short-circuits before it
// ever asks a provider for anything.
//
// The selection reads the environment and nothing else: no daemon call, no
// network call, and deliberately no parse. A malformed URL must fail LOUDLY out
// of Provision, with a diagnostic naming it, rather than silently falling back
// to a container the pipeline did not ask for.
func SelectProvider() Provider {
	if strings.TrimSpace(os.Getenv(EnvProvidedServer)) != "" {
		return NewProvidedServer()
	}
	return NewProvisioner()
}

// The docker Provisioner's half of the seam. Its own file stays exactly what it
// was before a second provider existed: the reuse digest, the container
// identity and the reaping rules are unchanged, and these three answers are
// what place it behind the shared gates.

// id names the resource kind a docker lease reclaims.
func (p *Provisioner) id() string { return providerID }

// ownsServer reports true: this provider starts the container it hands back, so
// the auto-only, never-on-CI gate applies to it in full.
func (p *Provisioner) ownsServer() bool { return true }

// bindingDefaults states no policy, so the synthesized binding omits isolation
// and reuse and the runtime provider applies its own defaults — database
// isolation with no template reuse, exactly as before this seam existed.
func (p *Provisioner) bindingDefaults() TestPolicy { return TestPolicy{} }
