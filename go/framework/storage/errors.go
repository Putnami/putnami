package storage

import "go.putnami.dev/errors"

// Storage error codes.
const (
	CodeStorageRead     errors.Code = "storage.read"
	CodeStorageWrite    errors.Code = "storage.write"
	CodeStorageDelete   errors.Code = "storage.delete"
	CodeStorageList     errors.Code = "storage.list"
	CodeStorageNotFound errors.Code = "storage.not_found"
	CodeStorageRequest  errors.Code = "storage.request"
	CodeStorageCopy     errors.Code = "storage.copy"
	// CodeStorageUnbound marks a fail-closed resolution error: an operation
	// targeted a logical bucket that has no injected binding. The binding layer
	// returns it instead of silently targeting a literal-named bucket.
	CodeStorageUnbound errors.Code = "storage.unbound"
	// CodeStorageUnsupported marks an operation a resolved backend cannot
	// perform — e.g. a signed-URL request against a backend that is not a
	// URLSigner.
	CodeStorageUnsupported errors.Code = "storage.unsupported"
)
