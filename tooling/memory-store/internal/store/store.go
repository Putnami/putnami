// Package store defines what a memory backend keeps and the interface the
// provider writes through: the store identity document, the record document
// every backend stores byte for byte, the revision of a record, and the
// failures a backend reports.
//
// A store holds one identity document at MetaPath and one record document per
// record at RecordPath(id). The file backend keeps them in a directory; the
// Git backend commits the same paths on a branch. Neither the provider's
// selection policy nor a caller reads these paths: they are how a backend
// finds a record, never what makes a record relevant.
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
)

// FormatVersion is the version of the identity and record documents this
// package reads and writes. A document of another version is refused, never
// reinterpreted.
const FormatVersion = 1

// Store layout, relative to the store root (a directory or a Git tree).
const (
	MetaPath   = "store.json"
	RecordsDir = "records"
)

// MaxDocumentBytes bounds one stored document. It is the contract's document
// bound, which holds every record whose members are within their bounds, so
// every checkpoint the contract accepts is stored and read back. Encode
// refuses a larger document and Decode never reads one.
const MaxDocumentBytes = collab.MaxDocumentBytes

// RecordPath is where the record id lives inside a store.
func RecordPath(id string) string { return RecordsDir + "/" + id + ".json" }

// RecordFileName is the file name of the record id inside RecordsDir.
func RecordFileName(id string) string { return id + ".json" }

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

// ValidID reports whether id can name a record.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// IDFromFileName returns the record id a file inside RecordsDir holds, and
// false for a name that is not a record file (a temporary file, a note to a
// person, an editor backup).
func IDFromFileName(name string) (string, bool) {
	id, found := strings.CutSuffix(name, ".json")
	if !found || !ValidID(id) {
		return "", false
	}
	return id, true
}

// Meta is the store identity document. ID qualifies every reference the
// store issues, so references from two stores never collide.
type Meta struct {
	Format int    `json:"format"`
	ID     string `json:"id"`
}

var storeIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// NewMeta allocates the identity of a new store.
func NewMeta() (Meta, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Meta{}, fmt.Errorf("allocate a store id: %w", err)
	}
	return Meta{Format: FormatVersion, ID: hex.EncodeToString(raw[:])}, nil
}

// EncodeMeta renders an identity document.
func EncodeMeta(meta Meta) []byte {
	return encode(meta)
}

// DecodeMeta reads an identity document.
func DecodeMeta(data []byte) (Meta, error) {
	var meta Meta
	if err := strictUnmarshal(data, &meta); err != nil {
		return Meta{}, fmt.Errorf("%s is not a store identity document: %w", MetaPath, err)
	}
	if meta.Format != FormatVersion {
		return Meta{}, fmt.Errorf("%s has format %d; this provider reads format %d", MetaPath, meta.Format, FormatVersion)
	}
	if !storeIDPattern.MatchString(meta.ID) {
		return Meta{}, fmt.Errorf("%s names no valid store id", MetaPath)
	}
	return meta, nil
}

// Document is one stored record.
type Document struct {
	Format   int                   `json:"format"`
	ID       string                `json:"id"`
	Kind     collab.MemoryKind     `json:"kind"`
	Identity collab.MemoryIdentity `json:"identity"`
	// Sequence counts the writes that produced this record: 1 after the
	// first checkpoint, one more after each following one.
	Sequence   int               `json:"sequence"`
	Title      string            `json:"title,omitempty"`
	Content    string            `json:"content"`
	Sources    []collab.Ref      `json:"sources,omitempty"`
	Evidence   []collab.Evidence `json:"evidence,omitempty"`
	Provenance collab.Provenance `json:"provenance"`
	UpdatedAt  string            `json:"updatedAt"`
	// Write identifies the checkpoint that produced this revision, so a
	// repeat of it is recognized instead of written twice.
	Write *Write `json:"write,omitempty"`
}

// Write is the idempotency record of the write that produced a revision:
// digests of the idempotency key and of what the write asked for. Neither
// digest can be reversed into the request.
type Write struct {
	Key     string `json:"key"`
	Request string `json:"request"`
}

// Revision is the record's revision token: the sequence, and a digest of the
// whole document. It is derived from the content, never stored, so two
// backends holding the same document report the same revision, and every
// write produces a new one because the sequence grows.
func (d *Document) Revision() string {
	canonical, err := json.Marshal(d)
	if err != nil {
		return strconv.Itoa(d.Sequence)
	}
	sum := sha256.Sum256(canonical)
	return strconv.Itoa(d.Sequence) + "." + hex.EncodeToString(sum[:8])
}

// Encode renders a record document as stored: indented JSON and a final
// newline. The encoding is deterministic. A document above MaxDocumentBytes
// is refused as Invalid, reason record.too_large, so a backend never writes a
// record Decode would refuse.
func Encode(doc *Document) ([]byte, error) {
	data := encode(doc)
	if len(data) > MaxDocumentBytes {
		return nil, Failf(Invalid, "record.too_large", false,
			"record %s encodes to %d bytes, above the %d-byte bound of a stored record; nothing was written", doc.ID, len(data), MaxDocumentBytes)
	}
	return data, nil
}

// Decode reads the record document stored under id. A document of another
// format, of another id, or with unknown members is refused.
func Decode(id string, data []byte) (*Document, error) {
	if len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("record %s exceeds %d bytes", id, MaxDocumentBytes)
	}
	var doc Document
	if err := strictUnmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("record %s is not a record document: %w", id, err)
	}
	switch {
	case doc.Format != FormatVersion:
		return nil, fmt.Errorf("record %s has format %d; this provider reads format %d", id, doc.Format, FormatVersion)
	case doc.ID != id:
		return nil, fmt.Errorf("record %s is stored under %s", doc.ID, id)
	case doc.Sequence < 0:
		return nil, fmt.Errorf("record %s has a negative sequence", id)
	}
	return &doc, nil
}

func encode(value any) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		panic(fmt.Sprintf("encode a store document: %v", err))
	}
	return buffer.Bytes()
}

func strictUnmarshal(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("more than one JSON value")
	}
	return nil
}

// Backend is where one store's records live.
type Backend interface {
	// Kind names the backend in reference sources: "<kind>:<store id>".
	Kind() string
	// Read returns a view of the store as it is now. A store that was never
	// written is empty and has no identity.
	Read(ctx context.Context) (View, error)
	// Update runs change against the current document id (nil when absent)
	// and writes the document it returns, so that no other Update of the same
	// store lands between the read change saw and the write: when two
	// updates start from one revision, the second sees the first's result. A
	// change that returns an error writes nothing, and Update returns that
	// error unchanged. Update may run change more than once when the store
	// moved under it before anything was written; change must therefore
	// decide from its arguments alone. It returns the written document and
	// the store id.
	Update(ctx context.Context, id string, change Change) (*Document, string, error)
}

// Change decides a write from the store id and the current document.
type Change func(storeID string, current *Document) (*Document, error)

// View is a read of one store.
type View interface {
	// StoreID is "" for a store that was never written.
	StoreID() string
	// Get returns the document id, or nil when there is none.
	Get(ctx context.Context, id string) (*Document, error)
	// List returns every record document.
	List(ctx context.Context) ([]*Document, error)
}

// Outcome classifies a backend failure.
type Outcome string

// Backend failure outcomes.
const (
	// Unavailable: the backend could not be read or written, and nothing was
	// written.
	Unavailable Outcome = "unavailable"
	// Unresolved: a write started and whether it landed is unknown.
	Unresolved Outcome = "unresolved"
	// Denied: the backend refused the write, and nothing was written.
	Denied Outcome = "denied"
	// Invalid: the configuration or the stored content cannot be used.
	Invalid Outcome = "invalid"
)

// Error is a backend failure with its outcome.
type Error struct {
	Outcome   Outcome
	Reason    string
	Retryable bool
	Err       error
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

// Failf builds a backend failure.
func Failf(outcome Outcome, reason string, retryable bool, format string, args ...any) *Error {
	return &Error{Outcome: outcome, Reason: reason, Retryable: retryable, Err: fmt.Errorf(format, args...)}
}
