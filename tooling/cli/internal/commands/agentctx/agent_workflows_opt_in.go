package agentctx

import (
	"errors"
	"fmt"
	"slices"

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
//
// release is the version a new declaration holds, "" for none: the release the
// lock pins when the user named a channel or a version, so the workspace keeps
// what the user asked for instead of falling back to latest.
func OptIntoExtensionAgentContent(wsRoot string, cfg *wsproto.Config, name, release string) (OptInOutcome, error) {
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
		if err := declareExtension(raw, name, release); err != nil {
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

// PinOutcome reports what PinDeclaredExtension found and wrote.
type PinOutcome struct {
	// Pinned reports that the call wrote the release to the declaration.
	Pinned bool
	// Kept is the constraint a declaration already holds and kept, "" when
	// it held none or the release itself.
	Kept string
}

// PinDeclaredExtension writes release to the declaration of name in the
// workspace file when that declaration holds no constraint: an empty value of
// the object form, or an entry of the list form, which becomes an object as
// declareExtension turns it. A declaration that holds another constraint is
// kept and reported, and a name the workspace file does not declare is left
// to OptIntoExtensionAgentContent. Only that file is read and written.
func PinDeclaredExtension(wsRoot, name, release string) (PinOutcome, error) {
	var outcome PinOutcome
	path := wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename)
	raw, err := jsonutil.ReadFile(path)
	if err != nil {
		return outcome, protocolcli.Classify(
			fmt.Errorf("read %s: %w", wsproto.WorkspaceConfigFilename, err), protocolcli.ErrInvalidConfig)
	}
	current, _ := raw.Get("extensions")
	switch value := current.(type) {
	case []any:
		if !slices.Contains(value, any(name)) {
			return outcome, nil
		}
		if err := declareExtension(raw, name, release); err != nil {
			return outcome, err
		}
		declared := raw.GetMap("extensions")
		declared.Set(name, release)
	case *jsonutil.OrderedMap:
		constraint, exists := value.Get(name)
		if !exists {
			return outcome, nil
		}
		if text, _ := constraint.(string); text != "" {
			if text != release {
				outcome.Kept = text
			}
			return outcome, nil
		}
		value.Set(name, release)
	default:
		return outcome, nil
	}
	if err := jsonutil.WriteFile(path, raw); err != nil {
		return PinOutcome{}, fmt.Errorf("write %s: %w", wsproto.WorkspaceConfigFilename, err)
	}
	outcome.Pinned = true
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
// the name-to-constraint object form, whichever the file uses. A release
// needs the object form, the only one that holds a constraint: an array
// becomes an object whose other members keep their order and declare no
// constraint, as the config writer turns a list with a constraint into one.
func declareExtension(raw *jsonutil.OrderedMap, name, release string) error {
	current, _ := raw.Get("extensions")
	switch value := current.(type) {
	case nil:
		if release == "" {
			raw.Set("extensions", []any{name})
			return nil
		}
		declared := jsonutil.New()
		declared.Set(name, release)
		raw.Set("extensions", declared)
	case []any:
		if release == "" {
			raw.Set("extensions", appendUnique(value, name))
			return nil
		}
		declared := jsonutil.New()
		for _, entry := range value {
			text, ok := entry.(string)
			if !ok {
				return protocolcli.Classify(
					fmt.Errorf("%s: extensions must list names; add %s to it by hand", wsproto.WorkspaceConfigFilename, name),
					protocolcli.ErrInvalidConfig)
			}
			if _, exists := declared.Get(text); !exists {
				declared.Set(text, "")
			}
		}
		if _, exists := declared.Get(name); !exists {
			declared.Set(name, release)
		}
		raw.Set("extensions", declared)
	case *jsonutil.OrderedMap:
		if _, exists := value.Get(name); !exists {
			value.Set(name, release)
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
