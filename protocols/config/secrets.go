package config

import "time"

// SealedEnvelope represents an envelope-encrypted secret. All binary
// fields are base64-encoded strings.
type SealedEnvelope struct {
	// KeyURI identifies the KMS key that wrapped the data-encryption key.
	KeyURI string `json:"keyUri"`
	// WrappedDEK is the KMS-wrapped data-encryption key (base64).
	WrappedDEK string `json:"wrappedDek"`
	// Nonce is the AEAD nonce used to encrypt the ciphertext (base64).
	Nonce string `json:"nonce"`
	// Ciphertext is the DEK-encrypted secret payload (base64).
	Ciphertext string `json:"ciphertext"`
}

// ResolveSecretsRequest is the input for remote secret resolution.
// Clients send this to POST /api/secrets/resolve to fetch resolved secrets.
type ResolveSecretsRequest struct {
	// AppName is the application whose secrets to resolve.
	AppName string `json:"appName"`
	// Version narrows resolution to a specific app version; empty resolves the
	// version-agnostic layers.
	Version string `json:"version,omitempty"`
	// Environment is the target environment that selects the per-environment
	// layers.
	Environment string `json:"environment"`
	// SchemaHash, when set, asks the server to confirm the resolved secrets match
	// the client's schema.
	SchemaHash string `json:"schemaHash,omitempty"`
}

// ResolveSecretsResponse is the resolved secrets output from the server.
// Shape mirrors ResolveResponse (config resolve) except for the Secrets field
// name, so clients can share merge and warning-handling logic.
type ResolveSecretsResponse struct {
	// Secrets is the merged, decrypted secret tree resolved across layers.
	Secrets map[string]any `json:"secrets"`
	// Resolved reports whether any secrets matched (false means none found).
	Resolved bool `json:"resolved"`
	// SchemaMatch reports whether the resolved secrets matched the request's
	// SchemaHash.
	SchemaMatch bool `json:"schemaMatch"`
	// Layers lists which dimension layers contributed, in application order.
	Layers []LayerInfo `json:"layers,omitempty"`
	// Warnings carries non-blocking validation messages.
	Warnings []string `json:"warnings,omitempty"`
}

// SecretEntry represents an encrypted secret value stored in the config server.
type SecretEntry struct {
	// AppName is the application the secret belongs to.
	AppName string `json:"appName"`
	// Environment is the environment the secret applies to.
	Environment string `json:"environment"`
	// Version scopes the secret to an app version; empty applies to all versions.
	Version string `json:"version,omitempty"`
	// Path is the config path the secret is stored under.
	Path string `json:"path"`
	// Envelope is the envelope-encrypted secret value.
	Envelope SealedEnvelope `json:"envelope"`
	// UpdatedAt is the server-assigned last-write time; nil on write requests.
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}
