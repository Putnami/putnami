package database

import (
	"context"
	"encoding/json"

	pconfig "go.putnami.dev/config"
	"go.putnami.dev/errors"
	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
)

// ConfigSection is the resolved-config section a deploy target injects the
// managed database binding document into. It is the config-resolution
// counterpart of EnvBinding: the control plane merges the same canonical
// database Binding (go.putnami.dev/protocol/database) into this section of the
// workload's resolved config — operator-authored keys in the section are
// preserved, managed keys win. A document here takes precedence over
// EnvBinding, which remains the fallback transport until every deploy target
// stops injecting it.
const ConfigSection = "database"

// configSourceLabel names the config transport in binding diagnostics, playing
// the role EnvBinding plays in the env transport's messages.
const configSourceLabel = `the "` + ConfigSection + `" config section`

// bindingConfigDoc is a loose view of the managed binding document keys within
// the ConfigSection block. The section can also carry operator-authored keys,
// so only the protocol document keys are extracted; they are kept untyped and
// re-encoded through documentJSON so the config transport reuses the exact
// strict parse and validation diagnostics of the env transport instead of the
// config mapper's reflection rules.
type bindingConfigDoc struct {
	Schema          string `json:"$schema"`
	ProtocolVersion any    `json:"protocolVersion"`
	Databases       any    `json:"databases"`
}

// bindingConfigSection binds the ConfigSection path of the resolved config.
var bindingConfigSection = pconfig.Config[bindingConfigDoc](ConfigSection)

// carriesBindings reports whether the resolved section carries a managed
// binding document. An absent or empty databases key means "no document" so
// the caller can fall back to the env transport; a present but non-map value
// is reported as carried so the strict parse rejects it loudly instead of the
// misconfiguration silently degrading to env or local fallbacks.
func (d bindingConfigDoc) carriesBindings() bool {
	if d.Databases == nil {
		return false
	}
	if m, ok := d.Databases.(map[string]any); ok {
		return len(m) > 0
	}
	return true
}

// managedBindingDiag summarizes why the section carried no managed binding
// (see effectivePoolConfigFor): an empty databases map
// means the managed overlay resolved to zero datasources — a versioned
// config-resolve that skipped the overlay, or an empty resolve — whereas no
// databases map at all means the workload is simply unmanaged (no overlay, no
// env binding). It is meaningful only for a (nil, nil) load outcome, where
// carriesBindings was false, so Databases is nil or an empty map.
func (d bindingConfigDoc) managedBindingDiag() string {
	if m, ok := d.Databases.(map[string]any); ok && len(m) == 0 {
		return `the resolved "database" config section carries an empty "databases" map (the managed overlay resolved to zero datasources)`
	}
	return `the resolved "database" config section carries no "databases" map`
}

// documentJSON re-encodes the extracted binding keys as the canonical JSON
// document the protocol parser accepts.
func (d bindingConfigDoc) documentJSON() ([]byte, error) {
	doc := map[string]any{"databases": d.Databases}
	if d.Schema != "" {
		doc["$schema"] = d.Schema
	}
	if d.ProtocolVersion != nil {
		doc["protocolVersion"] = d.ProtocolVersion
	}
	return json.Marshal(doc)
}

// loadConfigBinding resolves the managed binding document from the
// ConfigSection block of the resolved application config. It returns
// (nil, nil) when the section carries no binding document — local serve/test
// flows and the EnvBinding transport are then untouched — and an error when
// the section carries one that fails the protocol's strict parse and
// validation, so a malformed managed document surfaces at startup exactly like
// a malformed env document does at pool open.
func loadConfigBinding(ctx context.Context) (*pdb.Binding, error) {
	b, _, err := loadConfigBindingWithDiag(ctx)
	return b, err
}

// loadConfigBindingWithDiag is loadConfigBinding plus a human-readable
// diagnostic describing the section's managed-binding state. The diag is
// non-empty only for the (nil, nil) outcome — a resolved binding needs no
// explanation and an error carries its own — so a nil-binding pool-open failure
// can name why no binding was found (see managedBindingDiag /
// effectivePoolConfigFor) rather than emitting a generic message.
func loadConfigBindingWithDiag(ctx context.Context) (*pdb.Binding, string, error) {
	doc, err := pconfig.LoadContext(ctx, bindingConfigSection)
	if err != nil {
		return nil, "", errors.Wrapf(err, CodeBinding, "resolve "+configSourceLabel)
	}
	b, err := bindingFromConfigDoc(doc)
	if err != nil {
		return nil, "", err
	}
	if b == nil {
		return nil, doc.managedBindingDiag(), nil
	}
	return b, "", nil
}

// bindingFromConfigDoc turns the loose section view into a validated protocol
// Binding via the same ParseAndValidateBinding path ParseBinding wraps for the
// env transport, so misconfiguration reads identically in both transports.
func bindingFromConfigDoc(doc bindingConfigDoc) (*pdb.Binding, error) {
	if !doc.carriesBindings() {
		return nil, nil
	}
	data, err := doc.documentJSON()
	if err != nil {
		return nil, errors.Wrapf(err, CodeBinding, "encode the binding document carried by "+configSourceLabel)
	}
	b, diags := pdb.ParseAndValidateBinding(data)
	if diag.HasErrors(diags) {
		return nil, errors.Newf(CodeBinding, "invalid database binding in %s: %s", configSourceLabel, formatBindingDiags(diags))
	}
	return b, nil
}
