// Package store provides a content-addressed cache store for build artifacts.
// Blobs are stored under .putnami/store/blobs/{hash[0:2]}/{hash}/ with
// per-blob meta.json, result.json, and files/ directory.
package store
