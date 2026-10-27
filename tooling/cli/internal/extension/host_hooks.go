package extension

import (
	"errors"
	"fmt"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
)

// ErrNoHostHooks is wrapped by the error LocateHostHooks returns for an
// extension whose manifest declares no host hooks.
var ErrNoHostHooks = errors.New("declares no host hooks")

// HostHooksSource is one declared extension's host hooks, read from the
// release the workspace resolved.
type HostHooksSource struct {
	// Name is the extension's resolved name.
	Name string
	// Version is the lock pin of an installed extension, or the manifest's own
	// version for a local one.
	Version string
	// Local reports a development source declared by path.
	Local bool
	// Declaration is the validated hostHooks member.
	Declaration extproto.HostHooksDeclaration
}

// LocateHostHooks reads the host hooks of the declared extension name from the
// manifest discovery loads it from, by the rules LocateAgentContent applies:
// an installed extension only through its lock pin and manifest digest, a
// local one from its declared directory. It writes nothing.
//
// It returns an error wrapping ErrNoHostHooks for an extension that declares
// none, and an error naming each rule a declaration breaks, so a CLI never
// writes hooks its own protocol refuses.
func LocateHostHooks(wsRoot string, cfg *wsproto.Config, name string) (*HostHooksSource, error) {
	declared, err := FindDeclaredExtension(wsRoot, cfg, name)
	if err != nil {
		return nil, err
	}
	var verified *verifiedManifest
	if declared.Local {
		verified, err = readLocalManifest(declared)
	} else {
		verified, err = readPinnedManifest(wsRoot, declared.Name, "host hooks")
	}
	if err != nil {
		return nil, err
	}
	m := verified.Manifest
	if !extproto.DeclaresHostHooks(m) {
		return nil, fmt.Errorf("extension %s %w", declared.Name, ErrNoHostHooks)
	}
	if errs := diag.Errors(extproto.ValidateHostHooks(m)); len(errs) > 0 {
		messages := make([]string, 0, len(errs))
		for _, d := range errs {
			messages = append(messages, d.String())
		}
		return nil, fmt.Errorf("extension %s declares invalid host hooks: %s", declared.Name, strings.Join(messages, "; "))
	}
	return &HostHooksSource{
		Name:        declared.Name,
		Version:     verified.Version,
		Local:       declared.Local,
		Declaration: *m.HostHooks,
	}, nil
}
