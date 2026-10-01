package extension

import (
	"errors"
	"strings"
	"testing"
)

// An earlier design replaced the cache/publish provider gates — feature
// detection by command name PLUS a product name PLUS an extension-version floor
// — with resolution by the protocol's reserved command name alone. These tests
// pin what survived and what must not come back.

func ext(name, version string, commands ...string) *ExtensionDescription {
	cmds := make(map[string]string, len(commands))
	for _, c := range commands {
		cmds[c] = "desc"
	}
	return &ExtensionDescription{Name: name, Version: version, Commands: cmds}
}

const providerCmd = "cache-provider"

func TestResolveReservedProvider_SoleDeclarerWins(t *testing.T) {
	exts := []*ExtensionDescription{
		ext("@putnami/go", "1.0.0", "build", "test"),
		ext("@putnami/cloud", "1.2.0", "login", providerCmd),
	}
	provider, err := ResolveReservedProvider(exts, providerCmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider == nil {
		t.Fatal("provider = nil, want the declaring extension")
	}
	if provider.ExtensionName != "@putnami/cloud" || provider.Version != "1.2.0" || provider.Command != providerCmd {
		t.Errorf("provider = %+v", provider)
	}
}

// TestResolveReservedProvider_AnyVendorMayServe is the deletion of the product
// name. A third-party extension declaring the reserved command is a provider on
// exactly the same terms as the first-party one — no allowlist, no preference.
func TestResolveReservedProvider_AnyVendorMayServe(t *testing.T) {
	exts := []*ExtensionDescription{ext("@acme/cache", "0.1.0", providerCmd)}
	provider, err := ResolveReservedProvider(exts, providerCmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if provider == nil || provider.ExtensionName != "@acme/cache" {
		t.Fatalf("provider = %+v, want @acme/cache", provider)
	}
}

// TestResolveReservedProvider_UnorderedVersionsAreNotRejected is the deletion
// of the version floor, and the reason it had to go: a provider published as a
// SHA-stamped prerelease (0.0.0-<sha>) has no semver order at all, so any floor
// compared it lexically and rejected NEWER builds as older. Compatibility is
// the extension contract version at discovery plus RPC negotiation at
// initialize; nothing here may re-derive it from a version string.
func TestResolveReservedProvider_UnorderedVersionsAreNotRejected(t *testing.T) {
	for _, version := range []string{"0.0.0-abc1234", "0.0.0-0000000", "", "not-a-version"} {
		provider, err := ResolveReservedProvider([]*ExtensionDescription{ext("@putnami/cloud", version, providerCmd)}, providerCmd)
		if err != nil {
			t.Errorf("version %q: unexpected error: %v", version, err)
			continue
		}
		if provider == nil {
			t.Errorf("version %q: provider rejected; version must not gate resolution", version)
		}
	}
}

// TestResolveReservedProvider_LoadedNonDeclarerIsAbsence pins the behavior
// change the old gate's product name bought: an extension that is loaded but
// declares no provider command used to be an "upgrade it" error. It is now
// plain absence — core has no opinion about which extension SHOULD have
// declared the command.
func TestResolveReservedProvider_LoadedNonDeclarerIsAbsence(t *testing.T) {
	exts := []*ExtensionDescription{ext("@putnami/cloud", "1.2.0", "login", "deploy")}
	provider, err := ResolveReservedProvider(exts, providerCmd)
	if provider != nil || err != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", provider, err)
	}
}

func TestResolveReservedProvider_AmbiguousIsAnError(t *testing.T) {
	exts := []*ExtensionDescription{
		ext("@acme/cache", "1.0.0", providerCmd),
		ext("@putnami/cloud", "1.2.0", providerCmd),
	}
	provider, err := ResolveReservedProvider(exts, providerCmd)
	if provider != nil {
		t.Fatalf("provider = %+v, want nil", provider)
	}
	if !errors.Is(err, ErrProviderAmbiguous) {
		t.Fatalf("error = %v, want ErrProviderAmbiguous", err)
	}
	// Both identities, in canonical order, so the message does not depend on
	// discovery order.
	if !strings.Contains(err.Error(), "[@acme/cache @putnami/cloud]") {
		t.Errorf("error should name both candidates in sorted order: %v", err)
	}
}

func TestResolveReservedProvider_EmptyCommandDisablesResolution(t *testing.T) {
	exts := []*ExtensionDescription{{Name: "@acme/cache", Commands: map[string]string{"": "unnamed"}}}
	provider, err := ResolveReservedProvider(exts, "")
	if provider != nil || err != nil {
		t.Fatalf("got (%v, %v), want (nil, nil) — an empty reserved name must never match", provider, err)
	}
}

func TestResolveReservedProvider_IgnoresNilEntries(t *testing.T) {
	exts := []*ExtensionDescription{nil, ext("@acme/cache", "1.0.0", providerCmd), nil}
	provider, err := ResolveReservedProvider(exts, providerCmd)
	if err != nil || provider == nil || provider.ExtensionName != "@acme/cache" {
		t.Fatalf("got (%v, %v)", provider, err)
	}
}

func TestSkippedProviderCause_IsDeterministic(t *testing.T) {
	skipped := []SkippedExtension{
		{Name: "@z/late", Reason: errors.New("bad json")},
		{Name: "@a/early", Reason: errors.New("contract too new")},
	}
	first := SkippedProviderCause(skipped)
	second := SkippedProviderCause([]SkippedExtension{skipped[1], skipped[0]})
	if first != second {
		t.Fatalf("cause depends on input order:\n  %s\n  %s", first, second)
	}
	if !strings.Contains(first, "2 extension(s)") {
		t.Errorf("cause should count the skips: %s", first)
	}
}

// TestSkippedProviderCause_FallsBackToRef keeps an unloadable extension
// nameable even when its manifest never parsed far enough to yield a name.
func TestSkippedProviderCause_FallsBackToRef(t *testing.T) {
	cause := SkippedProviderCause([]SkippedExtension{{Ref: "./local/ext", Reason: errors.New("unexpected end of JSON input")}})
	if !strings.Contains(cause, "./local/ext") {
		t.Errorf("cause should fall back to the ref: %s", cause)
	}
}
