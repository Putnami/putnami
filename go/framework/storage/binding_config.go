package storage

import (
	"context"
	"encoding/json"

	pconfig "go.putnami.dev/config"
	"go.putnami.dev/errors"
)

// ConfigSection is the resolved-config section a deploy target injects the
// managed storage bindings document into. It is the config-resolution
// counterpart of EnvBindings: the control plane merges the same
// BindingsDocument into this section of the workload's resolved config —
// operator-authored keys in the section are preserved, managed keys win. A
// document here takes precedence over EnvBindings, which remains the fallback
// transport until every deploy target stops injecting it.
const ConfigSection = "storage"

// bindingsConfigDoc is a loose view of the managed bindings document keys
// within the ConfigSection block. The section can also carry operator-authored
// keys, so only the document keys are extracted; they are kept untyped and
// re-encoded through documentJSON so the config transport reuses the exact
// strict decode and validation the env transport has always had.
type bindingsConfigDoc struct {
	Schema          string `json:"$schema"`
	ProtocolVersion any    `json:"protocolVersion"`
	Bindings        any    `json:"bindings"`
	Providers       any    `json:"providers"`
}

// bindingsConfigSection binds the ConfigSection path of the resolved config.
var bindingsConfigSection = pconfig.Config[bindingsConfigDoc](ConfigSection)

// carriesBindings reports whether the resolved section carries a managed
// bindings document. An absent or empty bindings key means "no document" so
// the caller can fall back to the env transport; a present but non-list value
// is reported as carried so the strict decode rejects it loudly instead of the
// misconfiguration silently degrading to the env transport.
func (d bindingsConfigDoc) carriesBindings() bool {
	if d.Bindings == nil {
		return false
	}
	if l, ok := d.Bindings.([]any); ok {
		return len(l) > 0
	}
	return true
}

// documentJSON re-encodes the extracted document keys as the canonical JSON
// document ParseBindings accepts.
func (d bindingsConfigDoc) documentJSON() ([]byte, error) {
	doc := map[string]any{"bindings": d.Bindings}
	if d.Schema != "" {
		doc["$schema"] = d.Schema
	}
	if d.ProtocolVersion != nil {
		doc["protocolVersion"] = d.ProtocolVersion
	}
	if d.Providers != nil {
		doc["providers"] = d.Providers
	}
	return json.Marshal(doc)
}

// loadConfigBindings resolves the managed bindings document from the
// ConfigSection block of the resolved application config. It returns
// (nil, nil) when the section carries no document — local serve/test flows and
// the EnvBindings transport are then untouched — and an error when the section
// carries one that fails the same strict decode + validation the env transport
// uses, so misconfiguration reads identically over both transports.
func loadConfigBindings(ctx context.Context) (*BindingsDocument, error) {
	doc, err := pconfig.LoadContext(ctx, bindingsConfigSection)
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest,
			errors.String("op", "resolve_config_bindings"), errors.String("section", ConfigSection))
	}
	return bindingsFromConfigDoc(doc)
}

// bindingsFromConfigDoc turns the loose section view into a validated
// BindingsDocument via the same decode path ParseBindings uses for the env
// transport.
func bindingsFromConfigDoc(doc bindingsConfigDoc) (*BindingsDocument, error) {
	if !doc.carriesBindings() {
		return nil, nil
	}
	data, err := doc.documentJSON()
	if err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest,
			errors.String("op", "encode_config_bindings"), errors.String("section", ConfigSection))
	}
	return parseBindingsDocument(data, errors.String("section", ConfigSection))
}
