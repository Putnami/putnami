package config

import "time"

// ConfigEntry represents a config value stored in the config server.
// Used for PUT /api/configs writes.
//
//nolint:revive // ConfigEntry is the documented protocol wire type name.
type ConfigEntry struct {
	// AppName is the application the entry belongs to.
	AppName string `json:"appName"`
	// Environment is the environment the entry applies to.
	Environment string `json:"environment"`
	// Version scopes the entry to an app version; empty applies to all versions.
	Version string `json:"version,omitempty"`
	// Path is the config path the values are written under.
	Path string `json:"path"`
	// Values is the config value tree stored at Path.
	Values map[string]any `json:"values"`
	// UpdatedAt is the server-assigned last-write time; nil on write requests.
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// DeleteRequest is the input for deleting a config or secret entry.
// Used for DELETE /api/configs and DELETE /api/secrets.
type DeleteRequest struct {
	// AppName is the application the entry to delete belongs to.
	AppName string `json:"appName"`
	// Environment is the environment the entry to delete applies to.
	Environment string `json:"environment"`
	// Version scopes the deletion to an app version; empty targets all versions.
	Version string `json:"version,omitempty"`
	// Path is the config path of the entry to delete.
	Path string `json:"path"`
}
