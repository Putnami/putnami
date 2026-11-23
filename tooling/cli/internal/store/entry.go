package store

import (
	"encoding/json"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// EntryResult mirrors the job result stored as result.json inside a blob.
type EntryResult struct {
	Status string              `json:"status"`
	Data   map[string]any      `json:"data,omitempty"`
	Error  *EntryError         `json:"error,omitempty"`
	Events []cache.ActionEvent `json:"events,omitempty"`
}

// EntryError is the error component of a result.
type EntryError struct {
	Message string `json:"message,omitempty"`
	Code    string `json:"code,omitempty"`
}

// EntryMetadata is stored as meta.json inside each blob directory.
type EntryMetadata struct {
	// Hash is the content-addressed key for this entry.
	Hash string `json:"hash"`

	// CreatedAt is when the entry was first stored.
	CreatedAt time.Time `json:"createdAt"`

	// Extension is the extension that produced this entry.
	Extension string `json:"extension"`

	// Task is the task name (e.g., "build~transpile").
	Task string `json:"task"`

	// Project is the project name.
	Project string `json:"project"`

	// Duration is how long the job took (in milliseconds).
	DurationMs int64 `json:"durationMs"`

	// Size is the total size of the blob directory (files + metadata) in bytes.
	Size int64 `json:"size"`

	// OutputFiles lists the relative paths of files in the files/ directory.
	OutputFiles []string `json:"outputFiles,omitempty"`
}

// UnmarshalResult deserializes a result from JSON.
func UnmarshalResult(data []byte) (*EntryResult, error) {
	var r EntryResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// UnmarshalMetadata deserializes metadata from JSON.
func UnmarshalMetadata(data []byte) (*EntryMetadata, error) {
	var m EntryMetadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
