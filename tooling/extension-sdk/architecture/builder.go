package architecture

import (
	"fmt"
	"strings"

	archproto "go.putnami.dev/protocol/architecture"
	diag "go.putnami.dev/protocol/diagnostic"
)

// Builder accumulates one domain's architecture manifest. The zero value is not
// usable; call NewDomain. Nothing is validated as members are added — a manifest
// is only meaningful as a whole, since a projection's local model is judged
// against the facts the same import minimizes — so the verdict happens in Build.
type Builder struct {
	manifest archproto.Manifest
}

// NewDomain starts a manifest for one domain and the owner accountable for it.
// The schema reference and the protocol version are the protocol's, so an
// authoring program never states a wire version it does not implement.
func NewDomain(domain, owner string) *Builder {
	return &Builder{manifest: archproto.Manifest{
		Schema:          archproto.ManifestSchemaURL,
		ProtocolVersion: archproto.ProtocolVersion,
		Domain:          domain,
		Owner:           owner,
		// The three collections are required members, not optional ones: the
		// protocol reads an absent `projects` differently from an empty one, so
		// a domain that maps nothing still says so.
		Projects: []string{},
		Exports:  []archproto.Export{},
		Imports:  []archproto.Import{},
	}}
}

// Projects adds Putnami projects to the domain. Calls accumulate, so a domain
// assembled from several lists does not have to be written as one.
func (b *Builder) Projects(ids ...string) *Builder {
	b.manifest.Projects = append(b.manifest.Projects, ids...)
	return b
}

// Owns adds semantic concepts the domain is authoritative for. This is a
// boundary statement, not a source inventory: it names the facts, models,
// schemas, APIs, and events whose meaning the domain decides.
func (b *Builder) Owns(concepts ...archproto.OwnedConcept) *Builder {
	b.manifest.Owns = append(b.manifest.Owns, concepts...)
	return b
}

// Export adds producer-owned contracts other domains may consume. Repeating an
// export ID is a duplicate rather than a replacement — unlike an extension
// manifest's name-keyed maps, an export list is authored once and two entries
// under one ID mean the author wrote the same contract twice.
func (b *Builder) Export(exports ...archproto.Export) *Builder {
	b.manifest.Exports = append(b.manifest.Exports, exports...)
	return b
}

// Import adds consumer-owned access contracts. Use Reference, Query, Snapshot,
// Command, or Projection to build one: those are the five shapes the protocol
// accepts, and each constructor can only set the members its mode allows.
func (b *Builder) Import(imports ...archproto.Import) *Builder {
	b.manifest.Imports = append(b.manifest.Imports, imports...)
	return b
}

// Build validates the accumulated manifest and returns an independent copy of
// it in the protocol's canonical form. The error is a *ValidationError carrying
// every protocol diagnostic, so an author sees all of their mistakes in one pass
// rather than one per run.
//
// Canonical form is identity-sorted, so the order members were added in is not
// observable in the result. That is deliberate: a manifest's meaning is its
// identities, and letting authoring order reach a committed file would make two
// programs that declare the same domain produce two different documents.
//
// The returned manifest never aliases the builder: a caller may keep building
// after a Build without rewriting what it already produced.
func (b *Builder) Build() (*archproto.Manifest, error) {
	built := archproto.CanonicalManifest(&b.manifest)
	if err := Validate(built); err != nil {
		return nil, err
	}
	return built, nil
}

// CanonicalBytes validates the manifest and renders it exactly as the protocol's
// canonical writer does: identity-sorted, two-space indented, one trailing
// newline. That is what makes builder output and a committed file comparable —
// see Pin.
//
// The rendered bytes are then read back through the protocol's STRICT reader,
// the one `architecture validate` uses, and re-validated. A builder cannot
// normally produce a document that reader rejects, and that is the point: the
// round trip is what turns "cannot normally" into a checked property, so a
// future member with a wire shape the strict reader refuses fails here rather
// than in the workspace that committed the file.
func (b *Builder) CanonicalBytes() ([]byte, error) {
	built, err := b.Build()
	if err != nil {
		return nil, err
	}
	data, err := archproto.MarshalManifest(built)
	if err != nil {
		return nil, fmt.Errorf("encode architecture manifest: %w", err)
	}
	reparsed, diagnostics := archproto.ParseAndValidateManifest(data)
	if errs := diag.Errors(diagnostics); len(errs) > 0 {
		return nil, &ValidationError{Diagnostics: errs}
	}
	if reparsed == nil {
		return nil, fmt.Errorf("the canonical rendering of domain %q does not read back", built.Domain)
	}
	return data, nil
}

// Validate applies the protocol's local-invariant validation to a manifest built
// by any means. It is exported for authors who assemble an archproto.Manifest
// directly or load one from disk and want the same authoring-time verdict a
// Builder gives.
//
// It is deliberately the LOCAL verdict only. Whether a producer domain exists,
// whether it publishes the imported export, and whether a bound project belongs
// to the domain that claims it are cross-document questions
// (archproto.ValidateRepository), and one manifest cannot answer them. The gate
// asks them over the whole workspace; an authoring program that tried would be
// guessing at documents it cannot see.
func Validate(manifest *archproto.Manifest) error {
	if errs := diag.Errors(archproto.ValidateManifest(manifest)); len(errs) > 0 {
		return &ValidationError{Diagnostics: errs}
	}
	return nil
}

// ValidationError reports every protocol violation in an authored manifest.
type ValidationError struct {
	Diagnostics []diag.Diagnostic
}

// Error renders one line per violation, each naming the manifest field, so the
// message points at what to edit.
func (e *ValidationError) Error() string {
	lines := make([]string, 0, len(e.Diagnostics)+1)
	lines = append(lines, fmt.Sprintf("architecture manifest has %d protocol violation(s):", len(e.Diagnostics)))
	for _, d := range e.Diagnostics {
		lines = append(lines, "  - "+d.String())
	}
	return strings.Join(lines, "\n")
}

// Codes returns the diagnostic codes in order, for a caller that branches on
// which rule was broken instead of printing the message.
func (e *ValidationError) Codes() []string {
	codes := make([]string, 0, len(e.Diagnostics))
	for _, d := range e.Diagnostics {
		codes = append(codes, d.Code)
	}
	return codes
}

// --- exports ----------------------------------------------------------------

// Fact declares one field an export exposes, with its authority and its data
// class stated rather than assumed. Authority is a domain ID and need not be the
// exporting domain: an export may carry a fact another domain owns, which is
// exactly the provenance the protocol keeps visible.
func Fact(name, authority string, classification archproto.Classification, personal archproto.PersonalData) archproto.Fact {
	return archproto.Fact{
		Name:           name,
		Authority:      authority,
		Classification: classification,
		PersonalData:   personal,
	}
}

// ReferenceFact declares one member of an export whose sole allowed mode is
// reference. A code reference crosses an authority boundary without carrying
// data, so the protocol permits both data-classification fields to be omitted.
// Build refuses this shorter shape if the export allows any other mode.
func ReferenceFact(name, authority string) archproto.Fact {
	return archproto.Fact{Name: name, Authority: authority}
}

// Concept declares one semantic authority boundary for Owns.
func Concept(id string, kind archproto.OwnershipKind, description string) archproto.OwnedConcept {
	return archproto.OwnedConcept{ID: id, Kind: kind, Description: description}
}

// Compatible states how an export evolves across versions and the oldest
// consumer version it still serves.
func Compatible(strategy archproto.CompatibilityStrategy, minimumConsumerVersion int) archproto.Compatibility {
	return archproto.Compatibility{Strategy: strategy, MinimumConsumerVersion: minimumConsumerVersion}
}

// --- transports -------------------------------------------------------------

// Carried names a concrete carrier for a contract and states whether that
// carrier is usable yet. Availability is the carrier's own lifecycle, separate
// from the import's: an active import may not depend on a planned carrier, which
// is what keeps a design from reading as as-built.
func Carried(kind archproto.TransportKind, contract string, availability archproto.LifecycleStatus) archproto.Transport {
	return archproto.Transport{Kind: kind, Contract: contract, Availability: availability}
}

// NoCarrier declares the absence of a carrier — the shape a contract fulfilled
// without any transport takes. It cannot name a contract, which is the one thing
// a Transport literal gets wrong.
func NoCarrier(availability archproto.LifecycleStatus) archproto.Transport {
	return archproto.Transport{Kind: archproto.TransportNone, Availability: availability}
}

// From identifies the producer-owned export an import consumes.
func From(domain, export string) archproto.ExportReference {
	return archproto.ExportReference{Domain: domain, Export: export}
}

// --- imports ----------------------------------------------------------------

// ImportOption configures one import.
type ImportOption func(*archproto.Import)

// As names the imported capability in the consumer's own domain language. It is
// how a consumer talks about a fact it does not own without adopting the
// producer's vocabulary.
func As(name string) ImportOption {
	return func(i *archproto.Import) { i.As = name }
}

// Status states whether the import is planned, active, legacy, or deprecated.
// Imports default to planned: an authoring that forgets to say otherwise
// describes an intention, never as-built reality.
func Status(status archproto.LifecycleStatus) ImportOption {
	return func(i *archproto.Import) { i.Status = status }
}

// Facts lists the exact producer-owned facts the import consumes. Minimization
// is the point — an import that names a fact it does not use is a permission
// nobody needed.
func Facts(names ...string) ImportOption {
	return func(i *archproto.Import) { i.Facts = append(i.Facts, names...) }
}

// Justification explains the domain need for the dependency, in the consumer's
// own words. The protocol requires it because an unexplained cross-domain
// dependency is the one a reviewer cannot judge.
func Justification(text string) ImportOption {
	return func(i *archproto.Import) { i.Justification = text }
}

// Guarantees declares the freshness, ordering, idempotence, and failure behavior
// of copied or queried facts.
func Guarantees(consistency archproto.Consistency) ImportOption {
	return func(i *archproto.Import) {
		copied := consistency
		i.Consistency = &copied
	}
}

// Deletes declares how a producer's deletion reaches the local copy, for every
// strategy except tombstones. It cannot name a tombstone field, because only a
// tombstone strategy has one.
func Deletes(strategy archproto.DeletionStrategy) ImportOption {
	return func(i *archproto.Import) {
		i.Deletion = &archproto.Deletion{Strategy: strategy}
	}
}

// Tombstones declares tombstone deletion and the local field that marks it. The
// field is required by the strategy, which is why it is a separate constructor
// rather than an option a Deletes call might forget.
func Tombstones(field string) ImportOption {
	return func(i *archproto.Import) {
		i.Deletion = &archproto.Deletion{Strategy: archproto.DeletionTombstone, TombstoneField: field}
	}
}

// Model describes the consumer-owned representation of copied facts: what is
// projected, what stays locally authoritative, where provenance and freshness
// are recorded, who writes it, and how it is rebuilt.
func Model(model archproto.LocalModel) ImportOption {
	return func(i *archproto.Import) {
		copied := model
		i.LocalModel = &copied
	}
}

// Bind authorizes one exact project dependency as an implementation of this
// import.
//
// THIS IS THE PERMISSION HALF OF THE MANIFEST. A binding says "this consumer
// project may depend on that producer project, and the reason is this declared
// contract". It exists so the grant is read in a reviewed diff. No generator in
// Putnami may call it from an observed edge: turning drift into authorization is
// the anti-pattern ADR 0001 exists to forbid, and `architecture sync` refuses it
// by design.
//
// The protocol allows a binding only on a non-planned import, so a target
// design can never quietly authorize a current dependency.
func Bind(consumerProject, producerProject string) ImportOption {
	return func(i *archproto.Import) {
		i.Bindings = append(i.Bindings, archproto.Binding{
			Kind:            archproto.BindingProjectDependency,
			ConsumerProject: consumerProject,
			ProducerProject: producerProject,
		})
	}
}

// Reference imports a stable identifier: the consumer keeps a handle to
// something another domain owns and resolves it there. Nothing is copied, so a
// reference declares neither a projection's bootstrap and updates nor a local
// model.
func Reference(id string, version int, from archproto.ExportReference, opts ...ImportOption) archproto.Import {
	return newImport(id, version, archproto.ModeReference, from, opts)
}

// Query imports a read the producer answers on demand. One transport carries it,
// and the consistency block states what the consumer does when the answer is
// missing or older than its bound.
func Query(id string, version int, from archproto.ExportReference, transport archproto.Transport, opts ...ImportOption) archproto.Import {
	imported := newImport(id, version, archproto.ModeQuery, from, opts)
	imported.Transport = &transport
	return imported
}

// Snapshot imports an immutable version-addressed read: the consumer attaches a
// copy that is never updated in place, only replaced.
func Snapshot(id string, version int, from archproto.ExportReference, transport archproto.Transport, opts ...ImportOption) archproto.Import {
	imported := newImport(id, version, archproto.ModeSnapshot, from, opts)
	imported.Transport = &transport
	return imported
}

// Command imports the right to ask another domain to do something. The producer
// stays the authority over whether it happens; the consumer declares only how
// the request is carried.
func Command(id string, version int, from archproto.ExportReference, transport archproto.Transport, opts ...ImportOption) archproto.Import {
	imported := newImport(id, version, archproto.ModeCommand, from, opts)
	imported.Transport = &transport
	return imported
}

// Projection imports a rebuildable local copy. It takes bootstrap and updates as
// two separate carriers, each with its own availability, and cannot take the
// single ambiguous transport the other modes use — that split is the whole
// reason a projection is a distinct constructor rather than a mode option.
//
// The protocol requires the rest of the projection contract too: Guarantees,
// Deletes or Tombstones, and Model. They stay options because they are
// independent decisions with independent reviewers, and Build names whichever
// one is missing.
func Projection(id string, version int, from archproto.ExportReference, bootstrap, updates archproto.Transport, opts ...ImportOption) archproto.Import {
	imported := newImport(id, version, archproto.ModeProjection, from, opts)
	imported.Bootstrap = &bootstrap
	imported.Updates = &updates
	return imported
}

func newImport(id string, version int, mode archproto.AccessMode, from archproto.ExportReference, opts []ImportOption) archproto.Import {
	imported := archproto.Import{
		ID:      id,
		Version: version,
		From:    from,
		Mode:    mode,
		Status:  archproto.StatusPlanned,
	}
	for _, opt := range opts {
		opt(&imported)
	}
	return imported
}
