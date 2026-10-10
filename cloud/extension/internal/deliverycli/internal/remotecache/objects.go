package remotecache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	cacheserverclient "go.putnami.dev/cloud/clients/cache-server/go"
	cache "go.putnami.dev/protocol/cache"
)

// The object-cache client half: the keyed (namespace, id) index behind the
// provider RPC's object-cache socket, whose first consumer is Go's GOCACHEPROG.
//
// Only the INDEX is new here. Object bytes ride the blob paths this package
// already implements: a lookup answers small objects inline (one round trip for a
// whole batch — the caller is a compiler on its critical path) and hands back a
// presigned GET for the rest, and a store uploads through find-missing plus
// presigned PUTs before it records a single index row. The two routes are
// versioned by the same cache.ProtocolVersion as the rest of the cache
// vocabulary, like the run-marker and repo-bundle surfaces.
//
// The wire types are declared here rather than shared with the cache-server
// because the CLI extension must not link the server's storage library (its DI,
// database, migration, and object-storage dependencies). They are the exact JSON
// shapes cache-server's internal/api/object declares, and TestObjectWireShape on
// each side pins the pairing against the same canonical literal. The calls go
// through the generated operations for /v1/cache/objects/lookup and
// /v1/cache/objects/store, which read these values into the declared input.

// ObjectLookupRequest resolves a batch of object ids inside ONE namespace.
type ObjectLookupRequest struct {
	ProtocolVersion int      `json:"protocolVersion"`
	Namespace       string   `json:"namespace"`
	IDs             []string `json:"ids"`
}

// ObjectRecord is one served object: the stored digest and size, the opaque meta
// stored with it, the SERVER-derived provenance, and one of the two byte
// channels — Blob inline for a small object, Download for a large one.
type ObjectRecord struct {
	ID        string              `json:"id"`
	Digest    string              `json:"digest"`
	SizeBytes int64               `json:"sizeBytes"`
	Meta      string              `json:"meta,omitempty"`
	Producer  string              `json:"producer,omitempty"`
	Channel   string              `json:"channel,omitempty"`
	Blob      *cache.InlineBlob   `json:"blob,omitempty"`
	Download  *cache.BlobTransfer `json:"download,omitempty"`
}

// ObjectLookupResponse returns the servable objects. A requested id with no row,
// or one whose bytes have aged out of the CAS, is omitted — a miss, never a
// phantom hit.
type ObjectLookupResponse struct {
	ProtocolVersion int            `json:"protocolVersion"`
	Objects         []ObjectRecord `json:"objects,omitempty"`
}

// ObjectOffer is one object offered for indexing. It carries NO provenance: the
// server derives producer and channel from the bearer it verifies, so a caller
// cannot turn a developer object into a trusted one by asserting a channel —
// the same rule the task-cache commit path enforces.
type ObjectOffer struct {
	ID        string `json:"id"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	Meta      string `json:"meta,omitempty"`
}

// ObjectStoreRequest records index rows for a batch of objects inside ONE
// namespace.
type ObjectStoreRequest struct {
	ProtocolVersion int           `json:"protocolVersion"`
	Namespace       string        `json:"namespace"`
	Objects         []ObjectOffer `json:"objects"`
}

// ObjectStoreResult reports one offered object's outcome. Stored=true means the
// offer became an index row, so the number of true results is the number of rows
// written — never more than the number of distinct ids offered.
//
// Stored=false means either that the bytes were not in CAS (upload them and
// re-store) or that the same id was offered again later in the batch, since one
// id is one row and the last offer owns it.
type ObjectStoreResult struct {
	ID     string `json:"id"`
	Stored bool   `json:"stored"`
}

// ObjectStoreResponse returns one result per offered object.
type ObjectStoreResponse struct {
	ProtocolVersion int                 `json:"protocolVersion"`
	Results         []ObjectStoreResult `json:"results,omitempty"`
}

// LookupObjects resolves a batch of object ids in one round trip. A server that
// has not implemented the endpoint is treated as a cold cache (no hits), not an
// error: the object cache is an accelerator whose every failure mode must be a
// local build.
//
// Records outside the requested id set, or repeated, are rejected — the same
// response-fidelity check download-batch and commit-batch apply, because a
// server that answers an id the caller did not ask about would have the caller
// write bytes into its compiler cache under a key it never computed.
func (c *Client) LookupObjects(ctx context.Context, namespace string, ids []string) ([]ObjectRecord, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	req := &ObjectLookupRequest{ProtocolVersion: cache.ProtocolVersion, Namespace: namespace, IDs: ids}
	data, err := postControl(ctx, c, "objects lookup", req, (*cacheserverclient.CacheClient).CreateV1CacheObjectsLookup)
	if IsStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	out, err := parseStrictJSON[ObjectLookupResponse](data, "object lookup response")
	if err != nil {
		return nil, err
	}
	if out.ProtocolVersion != cache.ProtocolVersion {
		return nil, fmt.Errorf("object lookup response protocol version %d, want %d",
			out.ProtocolVersion, cache.ProtocolVersion)
	}
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	seen := make(map[string]bool, len(out.Objects))
	for _, obj := range out.Objects {
		if !requested[obj.ID] {
			return nil, fmt.Errorf("object lookup response included id %q outside the request", obj.ID)
		}
		if seen[obj.ID] {
			return nil, fmt.Errorf("object lookup response contains duplicate object %q", obj.ID)
		}
		seen[obj.ID] = true
		if !cache.ValidDigest(obj.Digest) {
			return nil, fmt.Errorf("object %q carries malformed digest %q", obj.ID, obj.Digest)
		}
	}
	return out.Objects, nil
}

// StoreObjects uploads the offered objects' bytes and then records their index
// rows, returning the ids the server confirmed stored.
//
// Order is load-bearing: bytes first, index row second. The server refuses to
// index a digest it does not hold, so an index row can never name absent bytes —
// the object-cache form of the "rows never advertise missing content" rule the
// task-cache commit path and the retention sweep both keep.
//
// src reads the bytes the caller already staged (for the provider, the
// blob-exchange directory). wp picks the upload mechanism from the server's
// advertised capabilities, exactly as the batched task-cache write path does.
func (c *Client) StoreObjects(ctx context.Context, namespace string, objects []ObjectOffer, src BlobSource, wp WritePath) ([]string, error) {
	if len(objects) == 0 {
		return nil, nil
	}

	sizeByDigest := make(map[string]int64, len(objects))
	for _, obj := range objects {
		if _, ok := sizeByDigest[obj.Digest]; !ok {
			sizeByDigest[obj.Digest] = obj.SizeBytes
		}
	}
	blobs := make([]cache.BlobRef, 0, len(sizeByDigest))
	for digest, size := range sizeByDigest {
		blobs = append(blobs, cache.BlobRef{Digest: digest, SizeBytes: size})
	}

	// Reuse the batched blob write path verbatim: small blobs coalesced inline
	// when the server advertises upload-batch, the rest through the direct grant
	// or one find-missing plus presigned PUTs. The outcome's byte counts are not
	// reported in the run summary (object bytes are not task-entry bytes), so a
	// throwaway accumulator is enough.
	out := &BatchOutcome{Committed: map[string]bool{}}
	perBlob := blobs
	if wp.CoalesceSmall {
		var small, large []cache.BlobRef
		for _, b := range blobs {
			if b.SizeBytes <= cache.MaxInlineBlobBytes {
				small = append(small, b)
			} else {
				large = append(large, b)
			}
		}
		if err := c.uploadInlineBatches(ctx, small, src, out); err != nil {
			return nil, err
		}
		perBlob = large
	}
	if wp.Grant != nil {
		if err := c.directUploadBlobs(ctx, wp.Grant, perBlob, src, out); err != nil {
			return nil, err
		}
	} else if err := c.findMissingUpload(ctx, perBlob, src, sizeByDigest, out); err != nil {
		return nil, err
	}

	req := &ObjectStoreRequest{ProtocolVersion: cache.ProtocolVersion, Namespace: namespace, Objects: objects}
	data, err := postControl(ctx, c, "objects store", req, (*cacheserverclient.CacheClient).CreateV1CacheObjectsStore)
	if IsStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	resp, err := parseStrictJSON[ObjectStoreResponse](data, "object store response")
	if err != nil {
		return nil, err
	}
	stored := make([]string, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.Stored {
			stored = append(stored, r.ID)
		}
	}
	return stored, nil
}

// ObjectWritePath derives the upload mechanism for objects from the server's
// advertised capabilities, and reports whether the server serves the object
// cache at all. A server that does not advertise object-cache has no index to
// write to, so the caller must not open an object-cache socket — a job that
// found one would pay a round trip per compilation to be told 404.
func ObjectWritePath(caps *cache.CapabilitiesResponse) (wp WritePath, objectCache bool) {
	if caps == nil {
		return WritePath{}, false
	}
	for _, capability := range caps.Capabilities {
		switch capability {
		case cache.CapabilityObjectCache:
			objectCache = true
		case cache.CapabilityUploadBatch:
			wp.CoalesceSmall = true
		}
	}
	return wp, objectCache
}

// parseStrictJSON decodes a response body, rejecting unknown fields. Strictness
// matches the shared cache contract's own parsers: these routes are versioned by
// cache.ProtocolVersion, so a server that adds a field bumps the version rather
// than silently reshaping a response a client is already reading.
func parseStrictJSON[T any](data []byte, what string) (*T, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var v T
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", what, err)
	}
	return &v, nil
}
