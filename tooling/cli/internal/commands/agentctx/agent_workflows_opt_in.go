package agentctx

import (
	"errors"
	"fmt"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
)

// OptInOutcome reports what OptIntoExtensionAgentContent decided and wrote.
type OptInOutcome struct {
	// ShipsContent reports that the extension declares agent content. When it
	// is false nothing was written.
	ShipsContent bool
	// Declared reports that the call added the extension to `extensions`.
	Declared bool
	// OptedIn reports that the call added `extension:<name>` to `agentArtifacts`.
	OptedIn bool
}

// OptIntoExtensionAgentContent records the request a user makes by naming an
// installed extension: it declares the extension in `extensions` and opts into
// its agent content in `agentArtifacts`, but only when the extension ships
// agent content. An extension without content leaves the workspace file
// untouched, and so does a run whose declarations are already in place.
//
// The extension must already be installed, because the content is read from
// the release the lock pins. It materializes nothing: the caller runs the
// ordinary agent phase over the updated declarations.
//
// Only the extension the user names gets an opt-in. An extension installed
// because another one depends on it stays undeclared and opted out.
func OptIntoExtensionAgentContent(wsRoot string, cfg *wsproto.Config, name string) (OptInOutcome, error) {
	var outcome OptInOutcome
	needsDeclaration := false
	declared, err := extension.FindDeclaredExtension(wsRoot, cfg, name)
	switch {
	case err == nil:
		// A dependency of the workspace's graph is not a declaration: naming
		// it is the request that declares it.
		needsDeclaration = declared.RequiredBy != ""
	case extension.IsUndeclaredExtension(err):
		needsDeclaration = true
	default:
		return outcome, err
	}
	probe := cfg
	if needsDeclaration {
		probe = withDeclaredExtension(cfg, name)
	}
	if _, err := extension.LocateAgentContent(wsRoot, probe, name); err != nil {
		if errors.Is(err, extension.ErrNoAgentContent) {
			return outcome, nil
		}
		return outcome, err
	}
	outcome.ShipsContent = true

	needsOptIn := !optsInto(cfg, name)
	if !needsDeclaration && !needsOptIn {
		return outcome, nil
	}
	path := wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename)
	raw, err := jsonutil.ReadFile(path)
	if err != nil {
		return outcome, protocolcli.Classify(
			fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err), protocolcli.ErrInvalidConfig)
	}
	if needsDeclaration {
		if err := declareExtension(raw, name); err != nil {
			return outcome, err
		}
		outcome.Declared = true
	}
	if needsOptIn {
		if err := appendAgentArtifact(raw, extensionAgentContentPrefix+name); err != nil {
			return outcome, err
		}
		outcome.OptedIn = true
	}
	if err := jsonutil.WriteFile(path, raw); err != nil {
		return OptInOutcome{}, fmt.Errorf("write %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	return outcome, nil
}

// withDeclaredExtension is cfg with name added to its extensions, leaving cfg
// itself unchanged.
func withDeclaredExtension(cfg *wsproto.Config, name string) *wsproto.Config {
	probe := wsproto.Config{}
	if cfg != nil {
		probe = *cfg
	}
	list := make(map[string]string, len(probe.Extensions.List)+1)
	for declared, constraint := range probe.Extensions.List {
		list[declared] = constraint
	}
	list[name] = ""
	probe.Extensions = wsproto.ExtensionsConfig{List: list}
	return &probe
}

// declareExtension adds name to the `extensions` member, in the array form or
// the name-to-constraint object form, whichever the file uses.
func declareExtension(raw *jsonutil.OrderedMap, name string) error {
	current, _ := raw.Get("extensions")
	switch value := current.(type) {
	case nil:
		raw.Set("extensions", []any{name})
	case []any:
		raw.Set("extensions", appendUnique(value, name))
	case *jsonutil.OrderedMap:
		if _, exists := value.Get(name); !exists {
			value.Set(name, "")
		}
	default:
		return protocolcli.Classify(
			fmt.Errorf("%s: extensions must be an array or an object; add %s to it by hand", wsproto.WorkspaceConfigFilename, name),
			protocolcli.ErrInvalidConfig)
	}
	return nil
}

// appendAgentArtifact adds an opt-in entry to the `agentArtifacts` member.
func appendAgentArtifact(raw *jsonutil.OrderedMap, entry string) error {
	current, _ := raw.Get("agentArtifacts")
	switch value := current.(type) {
	case nil:
		raw.Set("agentArtifacts", []any{entry})
	case []any:
		raw.Set("agentArtifacts", appendUnique(value, entry))
	default:
		return protocolcli.Classify(
			fmt.Errorf("%s: agentArtifacts must be an array; add %s to it by hand", wsproto.WorkspaceConfigFilename, entry),
			protocolcli.ErrInvalidConfig)
	}
	return nil
}

func appendUnique(values []any, entry string) []any {
	for _, value := range values {
		if text, ok := value.(string); ok && text == entry {
			return values
		}
	}
	return append(values, entry)
}
