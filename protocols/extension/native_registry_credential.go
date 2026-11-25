package extension

import (
	"encoding/json"
	"fmt"
	"time"
)

// NativeRegistryCredentialsEnv names the variable that tells a job which
// native registry credentials the engine materialized for it, and until when
// each answer holds (CLI ADR 0101). Its value is NativeRegistryCredentials as
// JSON. It names hosts, instants and outcomes, never a credential: the token
// stays in the file the cloud wrote. The engine sets it only for a job whose
// task declares TaskDefinition.NativeRegistryCredential, and never on a
// hosted run.
const NativeRegistryCredentialsEnv = "PUTNAMI_NATIVE_REGISTRY_CREDENTIALS"

// The outcomes a NativeRegistryCredential reports, one per class of answer
// the registry-token seam gives to `--materialize`.
const (
	NativeCredentialMaterialized = "materialized"
	NativeCredentialCloudAbsent  = "cloud-absent"
	NativeCredentialNotSignedIn  = "not-signed-in"
	NativeCredentialUnsupported  = "unsupported"
	NativeCredentialFailed       = "failed"
)

// NativeRegistryCredential is the engine's answer for one host.
type NativeRegistryCredential struct {
	// ValidUntil is the RFC 3339 UTC instant the answer stops holding.
	ValidUntil string `json:"validUntil"`
	// Outcome is one of the NativeCredential* outcomes.
	Outcome string `json:"outcome"`
	// Message is the line a job logs for the outcome, or holds as a warning;
	// empty when there is nothing to say.
	Message string `json:"message,omitempty"`
}

// NativeRegistryCredentials is the value of NativeRegistryCredentialsEnv,
// keyed by lowercase host.
type NativeRegistryCredentials map[string]NativeRegistryCredential

// Encode renders the value of NativeRegistryCredentialsEnv.
func (c NativeRegistryCredentials) Encode() string {
	data, err := json.Marshal(c)
	if err != nil {
		// Strings only: Marshal cannot fail.
		panic(fmt.Sprintf("extension: native registry credentials are not serializable: %v", err))
	}
	return string(data)
}

// ParseNativeRegistryCredentials reads the value of
// NativeRegistryCredentialsEnv. An empty value names no host.
func ParseNativeRegistryCredentials(value string) (NativeRegistryCredentials, error) {
	if value == "" {
		return nil, nil
	}
	var credentials NativeRegistryCredentials
	if err := json.Unmarshal([]byte(value), &credentials); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object of hosts: %w", NativeRegistryCredentialsEnv, err)
	}
	return credentials, nil
}

// Holds reports whether the answer still holds at now. An unreadable instant
// holds nothing.
func (c NativeRegistryCredential) Holds(now time.Time) bool {
	until, err := time.Parse(time.RFC3339, c.ValidUntil)
	return err == nil && now.Before(until)
}
