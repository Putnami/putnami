package registrycred

import (
	"os"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
)

// outcomeNames maps each outcome the engine reports to the wire name of
// extensionproto.NativeRegistryCredentialsEnv. KindSkipped has none: the
// engine names no host it skipped.
var outcomeNames = map[outcomeKind]string{
	KindMaterialized: extensionproto.NativeCredentialMaterialized,
	KindCloudAbsent:  extensionproto.NativeCredentialCloudAbsent,
	KindNotSignedIn:  extensionproto.NativeCredentialNotSignedIn,
	KindUnsupported:  extensionproto.NativeCredentialUnsupported,
	KindFailed:       extensionproto.NativeCredentialFailed,
}

// EngineAnswer is the entry the engine writes for host in
// extensionproto.NativeRegistryCredentialsEnv: the outcome of a call made at
// start, which holds until validUntil. ok is false for KindSkipped, which the
// engine never reports.
func EngineAnswer(outcome Outcome, validUntil time.Time) (extensionproto.NativeRegistryCredential, bool) {
	name, ok := outcomeNames[outcome.Kind]
	if !ok {
		return extensionproto.NativeRegistryCredential{}, false
	}
	return extensionproto.NativeRegistryCredential{
		ValidUntil: validUntil.UTC().Format(time.RFC3339),
		Outcome:    name,
		Message:    outcome.Message,
	}, true
}

// engineAnswer returns the engine's outcome for host when the engine named
// it in extensionproto.NativeRegistryCredentialsEnv and the answer still holds
// at now. An unreadable variable, an unknown outcome and an answer that no
// longer holds answer nothing, and the caller calls the seam itself.
func engineAnswer(host string, now time.Time) (Outcome, bool) {
	credentials, err := extensionproto.ParseNativeRegistryCredentials(os.Getenv(extensionproto.NativeRegistryCredentialsEnv))
	if err != nil {
		return Outcome{}, false
	}
	answer, ok := credentials[host]
	if !ok || !answer.Holds(now) {
		return Outcome{}, false
	}
	for kind, name := range outcomeNames {
		if name != answer.Outcome {
			continue
		}
		outcome := Outcome{Kind: kind, Message: answer.Message}
		if answer.Message != "" {
			outcome.Level = levelOf(kind)
		}
		return outcome, true
	}
	return Outcome{}, false
}

// levelOf is the level an outcome's line is logged at: a credential written,
// or no cloud to ask, is a debug line; anything else warns.
func levelOf(kind outcomeKind) string {
	if kind == KindMaterialized || kind == KindCloudAbsent {
		return "debug"
	}
	return "warn"
}
