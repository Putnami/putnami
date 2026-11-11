package php

import (
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

func resolved(t *testing.T, root, platform, committed string) *wsproto.LockToolchainEntry {
	t.Helper()
	request := wsproto.ToolchainResolveRequest{Identities: []string{LockIdentity}, Platform: platform}
	if committed != "" {
		request.Locked = map[string]wsproto.LockToolchainEntry{LockIdentity: {Version: committed}}
	}
	result := stacks.AnswerToolchainResolve(root, request, ExtensionName, LockIdentity, pinResolver(stack(t).Profile))
	if resolution, ok := result.Toolchains[LockIdentity]; ok {
		return resolution.Pin
	}
	return nil
}

func version(pin *wsproto.LockToolchainEntry) string {
	if pin == nil {
		return "no pin"
	}
	return pin.Version
}

func TestTheExtensionPinsAPHPReleaseTheRepositoryAccepts(t *testing.T) {
	spectest.Proves(t, feature, "pins-a-php-release", "the-extension-pins-a-php-release-the-repository-accepts")
	cases := []struct {
		name, composer, phpVersion, committed, want string
	}{
		{name: "no statement takes the default", composer: `{}`, want: "8.4.23"},
		{name: "a range that accepts the default", composer: `{"require": {"php": ">=8.3"}}`, want: "8.4.23"},
		{name: "a range above the default", composer: `{"require": {"php": ">=8.5"}}`, want: "8.5.8"},
		{name: "a range below the default takes the highest", composer: `{"require": {"php": "~8.3.0"}}`, want: "8.3.32"},
		{name: "a php-version file wins", composer: `{"require": {"php": ">=8.3"}}`, phpVersion: "8.5\n", want: "8.5.8"},
		{name: "an accepted committed pin is kept", composer: `{"require": {"php": ">=8.3"}}`, committed: "8.3.32", want: "8.3.32"},
		{name: "a committed pin the root refuses is replaced", composer: `{"require": {"php": ">=8.4"}}`, committed: "8.3.32", want: "8.4.23"},
		{name: "a committed release no longer shipped is replaced", composer: `{}`, committed: "8.2.1", want: "8.4.23"},
		{name: "no accepted release", composer: `{"require": {"php": ">=9"}}`, want: "no pin"},
		{name: "an unreadable manifest", composer: `{`, want: "no pin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			stackstest.WriteFile(t, root, "composer.json", c.composer)
			if c.phpVersion != "" {
				stackstest.WriteFile(t, root, ".php-version", c.phpVersion)
			}
			if got := version(resolved(t, root, "linux/amd64", c.committed)); got != c.want {
				t.Errorf("pin = %s, want %s", got, c.want)
			}
		})
	}

	nested := t.TempDir()
	stackstest.WriteFile(t, nested, "services/api/composer.json", `{"require": {"php": "~8.3.0"}}`)
	if got := version(resolved(t, nested, "linux/amd64", "")); got != "8.3.32" {
		t.Errorf("a component's statement narrows the pin: %s", got)
	}
	stackstest.WriteFile(t, nested, "services/web/composer.json", `{"require": {"php": ">=8.5"}}`)
	if got := version(resolved(t, nested, "linux/amd64", "")); got != "8.4.23" {
		t.Errorf("components with no common release leave the root's choice: %s", got)
	}

	root := t.TempDir()
	stackstest.WriteFile(t, root, "vendor/acme/composer.json", `{}`)
	if pin := resolved(t, root, "linux/amd64", ""); pin != nil {
		t.Errorf("a composer.json under vendor/ pins nothing: %v", pin.Version)
	}

	stackstest.WriteFile(t, root, "composer.json", `{}`)
	if pin := resolved(t, root, "windows/arm64", ""); pin != nil {
		t.Errorf("a platform with no archive pins nothing: %v", pin.Version)
	}
	pin := resolved(t, root, "linux/amd64", "")
	request := wsproto.ToolchainResolveRequest{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  ExtensionName,
		Identities: []string{LockIdentity},
		Locked:     map[string]wsproto.LockToolchainEntry{LockIdentity: *pin},
		Platform:   "windows/arm64",
	}
	result := stacks.AnswerToolchainResolve(root, request, ExtensionName, LockIdentity, pinResolver(stack(t).Profile))
	if kept := result.Toolchains[LockIdentity].Pin; kept == nil || !reflect.DeepEqual(*kept, *pin) {
		t.Errorf("a platform with no archive keeps the committed pin as committed: %+v", kept)
	}
	if diags := wsproto.ValidateToolchainResolveAnswer(&request, &result); len(diags) != 0 {
		t.Errorf("the kept pin answers the request: %v", diags)
	}
	if kept := resolved(t, root, "windows/arm64", "8.2.1"); kept != nil {
		t.Errorf("a committed release the extension does not choose is not kept: %v", kept.Version)
	}
	if len(pin.Archives) != 5 || len(pin.Integrities) != 5 {
		t.Fatalf("each platform has an archive and a digest: %+v", pin)
	}
	want := map[string]string{
		"darwin/amd64":  "php-8.4.23-cli-macos-x86_64.tar.gz",
		"darwin/arm64":  "php-8.4.23-cli-macos-aarch64.tar.gz",
		"linux/amd64":   "php-8.4.23-cli-linux-x86_64.tar.gz",
		"linux/arm64":   "php-8.4.23-cli-linux-aarch64.tar.gz",
		"windows/amd64": "php-8.4.23-nts-Win32-vs17-x64.zip",
	}
	for platform, file := range want {
		if archive := pin.Archives[platform]; !strings.HasSuffix(archive.URL, "/"+file) {
			t.Errorf("%s: %s, want %s", platform, archive.URL, file)
		}
	}
	if archive := pin.Archives["windows/amd64"]; archive.Format != wsproto.LockArchiveFormatZip || archive.Executable != "php.exe" {
		t.Errorf("windows archive: %+v", archive)
	}
	if archive := pin.Archives["linux/arm64"]; archive.Format != wsproto.LockArchiveFormatTarGz || archive.Executable != "php" {
		t.Errorf("linux archive: %+v", archive)
	}
}

func TestToolchainResolveRefusesAnIdentityItDoesNotPin(t *testing.T) {
	result := stacks.AnswerToolchainResolve(t.TempDir(), wsproto.ToolchainResolveRequest{Identities: []string{"go"}, Platform: "linux/amd64"}, ExtensionName, LockIdentity, pinResolver(stack(t).Profile))
	if result.Toolchains["go"].Problem == nil {
		t.Errorf("another identity is a problem: %+v", result)
	}
}
