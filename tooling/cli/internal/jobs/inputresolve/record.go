package inputresolve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/store"
)

// An answer the resolver keeps is also recorded between runs, in the artifact
// store's store.InputResolveDir. A record answers one request
// (answerKey) of one workspace root, and states the input-resolve protocol
// version, the implementation identity of the extension that answered
// (implementationIdentity) and the workspace's probe digest
// (workspace.Workspace.ProbeDigest) it was recorded under. A later run reads
// it as it reads a kept answer, while all three are the run's, the digest of
// its basis holds and every file it lists is there. The probe digest moves
// when a project or an edge moves, as when a directory becomes a project of
// its own: an answer can follow the projects around its own, which its basis
// does not select. A newer answer to the same request replaces the record. An
// extension with no implementation identity is never recorded, and neither is
// an error answer.

// recordSchema versions an answer record; a record of another schema is not
// read.
const recordSchema = 1

// maxRecord bounds the bytes read from one record.
const maxRecord = 4 << 20

// answerRecord is one recorded answer: the full key it answers, so a hash
// collision or a renamed file is never read as another key's answer, the
// answer and the digest of its basis when it was kept.
type answerRecord struct {
	Schema int                `json:"schema"`
	Key    string             `json:"key"`
	Files  []string           `json:"files"`
	Basis  wsproto.InputBasis `json:"basis"`
	Digest string             `json:"digest"`
}

// records reads and writes the answer records of one workspace. A nil
// records reads and writes none.
type records struct {
	dir  string
	root string
	// identity returns the implementation identity of an extension by name,
	// or "" when it has none.
	identity func(name string) string
	// probeDigest returns the workspace's probe digest.
	probeDigest func() string
}

// newRecords returns the records of the workspace at root, kept under
// storeRoot, the artifact store root, under the probe digest probeDigest
// returns.
func newRecords(storeRoot, root string, byName map[string]*extension.ExtensionDescription, probeDigest func() string) *records {
	if storeRoot == "" {
		return nil
	}
	artifacts := artifactstore.New(storeRoot)
	var mu sync.Mutex
	identities := map[string]string{}
	return &records{
		dir:         filepath.Join(storeRoot, store.InputResolveDir),
		root:        root,
		probeDigest: sync.OnceValue(probeDigest),
		// An identity is computed once per extension: the implementation does
		// not change during a run, and computing one may hash its sources.
		identity: func(name string) string {
			mu.Lock()
			defer mu.Unlock()
			identity, ok := identities[name]
			if !ok {
				identity = implementationIdentity(root, artifacts, byName[name])
				identities[name] = identity
			}
			return identity
		},
	}
}

// implementationIdentity names the implementation of ext that answers its
// input-resolve calls: the digest of the runtime this run prepared, the digest
// of the artifact-store entry it is installed from, or the identity the
// workspace snapshot keys a probe answer on
// (scheduler.ExtensionImplementationIdentity). It is "" for an extension with
// none of them, such as a local extension that prepares no runtime.
func implementationIdentity(root string, artifacts *artifactstore.Store, ext *extension.ExtensionDescription) string {
	if ext == nil {
		return ""
	}
	if ext.RuntimeDigest != "" {
		return ext.RuntimeDigest
	}
	if !ext.LocalSource && ext.Path != "" {
		if digest, ok := artifacts.Entry(ext.Path); ok {
			return digest
		}
	}
	return scheduler.ExtensionImplementationIdentity(root, ext)
}

// file returns the record path of key in this workspace, and the full key a
// record there must state: the request, the protocol version, the
// implementation identity of extensionName and the probe digest. It reports
// false when the extension has no implementation identity.
func (r *records) file(extensionName, key string) (path, recordKey string, ok bool) {
	if r == nil {
		return "", "", false
	}
	identity := r.identity(extensionName)
	if identity == "" {
		return "", "", false
	}
	name, err := json.Marshal([]any{recordSchema, r.root, key})
	if err != nil {
		return "", "", false
	}
	full, err := json.Marshal([]any{recordSchema, r.root, key, wsproto.InputResolveProtocolVersion, identity, r.probeDigest()})
	if err != nil {
		return "", "", false
	}
	sum := sha256.Sum256(name)
	digest := hex.EncodeToString(sum[:])
	return filepath.Join(r.dir, digest[:2], digest), string(full), true
}

// read returns the expansion recorded for key, with the digest of its basis
// when it was recorded, when the record parses whole under the current schema
// and answers exactly this key.
func (r *records) read(extensionName, key string) (kept, bool) {
	path, recordKey, ok := r.file(extensionName, key)
	if !ok {
		return kept{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return kept{}, false
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxRecord+1))
	if err != nil || len(data) > maxRecord {
		return kept{}, false
	}
	var record answerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return kept{}, false
	}
	if record.Schema != recordSchema || record.Key != recordKey || record.Digest == "" ||
		!slices.IsSorted(record.Files) {
		return kept{}, false
	}
	store.KeepRecord(file)
	return kept{
		expansion: store.InputExpansion{Files: record.Files, Basis: record.Basis},
		digest:    record.Digest,
	}, true
}

// write records answer, kept at key, by an atomic rename. A failed write is
// ignored: the next run asks again.
func (r *records) write(extensionName, key string, answer kept) {
	path, recordKey, ok := r.file(extensionName, key)
	if !ok {
		return
	}
	data, err := json.Marshal(answerRecord{
		Schema: recordSchema,
		Key:    recordKey,
		Files:  answer.expansion.Files,
		Basis:  answer.expansion.Basis,
		Digest: answer.digest,
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
	}
}
