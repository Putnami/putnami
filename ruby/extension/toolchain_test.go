package ruby

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/stacks"
)

func resolved(t *testing.T, root, platform, committed string) *wsproto.LockToolchainEntry {
	t.Helper()
	request := wsproto.ToolchainResolveRequest{Identities: []string{LockIdentity}, Platform: platform}
	if committed != "" {
		request.Locked = map[string]wsproto.LockToolchainEntry{LockIdentity: {Version: committed}}
	}
	result := answerToolchainResolve(root, request, stack(t).Profile)
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

func TestTheExtensionPinsARubyReleaseTheRepositoryAccepts(t *testing.T) {
	spectest.Proves(t, feature, "pins-a-ruby-release", "the-extension-pins-a-ruby-release-the-repository-accepts")
	cases := []struct {
		name, rubyVersion, toolVersions, committed, want string
	}{
		{name: "no statement takes the newest", want: "4.0.9"},
		{name: "a minor takes its shipped patch", rubyVersion: "3.4\n", want: "3.4.12"},
		{name: "a ruby- prefix reads the same", rubyVersion: "ruby-3.3\n", want: "3.3.13"},
		{name: "the shipped patch itself", rubyVersion: "3.2.11\n", want: "3.2.11"},
		{name: "a major takes its newest minor", rubyVersion: "3\n", want: "3.4.12"},
		{name: "a tool-versions file", toolVersions: "nodejs 22.1.0\nruby 3.3\n", want: "3.3.13"},
		{name: "an accepted committed pin is kept", committed: "3.3.13", want: "3.3.13"},
		{name: "a committed pin the statement refuses is replaced", rubyVersion: "3.4\n", committed: "3.3.13", want: "3.4.12"},
		{name: "a committed release no longer shipped is replaced", committed: "3.4.7", want: "4.0.9"},
		{name: "an older patch than the shipped one", rubyVersion: "3.4.7\n", want: "no pin"},
		{name: "a minor no release ships", rubyVersion: "3.1\n", want: "no pin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "Gemfile", "source \"https://rubygems.org\"\n")
			if c.rubyVersion != "" {
				writeFile(t, root, ".ruby-version", c.rubyVersion)
			}
			if c.toolVersions != "" {
				writeFile(t, root, ".tool-versions", c.toolVersions)
			}
			if got := version(resolved(t, root, "linux/amd64", c.committed)); got != c.want {
				t.Errorf("pin = %s, want %s", got, c.want)
			}
		})
	}

	nested := t.TempDir()
	writeFile(t, nested, "Gemfile", "source \"https://rubygems.org\"\n")
	writeFile(t, nested, "gems/billing/billing.gemspec", "Gem::Specification.new { |s| s.name = \"billing\" }\n")
	writeFile(t, nested, "gems/billing/.ruby-version", "3.3\n")
	if got := version(resolved(t, nested, "linux/amd64", "")); got != "3.3.13" {
		t.Errorf("a component's statement narrows the pin: %s", got)
	}
	writeFile(t, nested, "gems/ledger/ledger.gemspec", "Gem::Specification.new { |s| s.name = \"ledger\" }\n")
	writeFile(t, nested, "gems/ledger/.ruby-version", "3.4\n")
	if got := version(resolved(t, nested, "linux/amd64", "")); got != "4.0.9" {
		t.Errorf("components with no common release leave the root's choice: %s", got)
	}

	none := t.TempDir()
	writeFile(t, none, "vendor/bundle/ruby/3.4.0/gems/rake/Gemfile", "source \"https://rubygems.org\"\n")
	writeFile(t, none, "README.md", "# No Ruby\n")
	if pin := resolved(t, none, "linux/amd64", ""); pin != nil {
		t.Errorf("a workspace with no Ruby component pins nothing: %v", pin.Version)
	}

	root := t.TempDir()
	writeFile(t, root, "Gemfile", "source \"https://rubygems.org\"\n")
	for _, platform := range []string{"windows/amd64", "windows/arm64", "linux/riscv64"} {
		if pin := resolved(t, root, platform, ""); pin != nil {
			t.Errorf("%s has no archive and pins nothing: %v", platform, pin.Version)
		}
	}
	pin := resolved(t, root, "darwin/arm64", "")
	if len(pin.Archives) != 4 || len(pin.Integrities) != 4 {
		t.Fatalf("each platform has an archive and a digest: %+v", pin)
	}
	want := map[string]string{
		"darwin/amd64": "ruby-4.0.9.ventura.tar.gz",
		"darwin/arm64": "ruby-4.0.9.arm64_sonoma.tar.gz",
		"linux/amd64":  "ruby-4.0.9.x86_64_linux.tar.gz",
		"linux/arm64":  "ruby-4.0.9.arm64_linux.tar.gz",
	}
	for platform, file := range want {
		archive := pin.Archives[platform]
		if archive.URL != "https://github.com/spinel-coop/rv-ruby/releases/download/20261023/"+file {
			t.Errorf("%s: %s, want %s", platform, archive.URL, file)
		}
		if archive.Format != wsproto.LockArchiveFormatTarGz || archive.Executable != "rv-ruby@4.0.9/4.0.9/bin/ruby" {
			t.Errorf("%s archive: %+v", platform, archive)
		}
		if pin.Integrities[platform] != releases[len(releases)-1].digests[platform] {
			t.Errorf("%s integrity: %s", platform, pin.Integrities[platform])
		}
	}
}

func TestEveryShippedArchiveHasAURLAndASHA256(t *testing.T) {
	spectest.Proves(t, feature, "pins-a-ruby-release", "every-shipped-archive-has-a-url-and-a-sha-256")
	var versions []string
	seen := map[string]bool{}
	for _, r := range releases {
		versions = append(versions, r.version)
		if len(r.digests) != len(rvRubyPlatforms) {
			t.Errorf("%s ships %d archives, want one per platform: %v", r.version, len(r.digests), r.digests)
		}
		for platform, digest := range r.digests {
			raw, err := hex.DecodeString(digest)
			if err != nil || len(raw) != 32 {
				t.Errorf("%s %s: %q is no SHA-256", r.version, platform, digest)
			}
			if seen[digest] {
				t.Errorf("%s %s: digest %s is used twice", r.version, platform, digest)
			}
			seen[digest] = true
			archive := r.archive(platform)
			if !strings.HasPrefix(archive.URL, "https://github.com/spinel-coop/rv-ruby/releases/download/"+rvRubyRelease+"/ruby-"+r.version+".") ||
				!strings.HasSuffix(archive.URL, "."+rvRubyPlatforms[platform]+".tar.gz") {
				t.Errorf("%s %s: url %s", r.version, platform, archive.URL)
			}
			if strings.Contains(archive.URL, "latest") || archive.Executable == "" {
				t.Errorf("%s %s: archive %+v", r.version, platform, archive)
			}
		}
	}
	if want := []string{"3.2.11", "3.3.13", "3.4.12", "4.0.9"}; !slices.Equal(versions, want) {
		t.Errorf("shipped releases = %v, want %v, oldest first", versions, want)
	}
}

func TestToolchainResolveRefusesAnIdentityItDoesNotPin(t *testing.T) {
	result := answerToolchainResolve(t.TempDir(), wsproto.ToolchainResolveRequest{Identities: []string{"go"}, Platform: "linux/amd64"}, stack(t).Profile)
	if result.Toolchains["go"].Problem == nil {
		t.Errorf("another identity is a problem: %+v", result)
	}
}

func TestATaskUsesTheProvisionedRubyOnlyWhenTheMachineRubyIsUnusable(t *testing.T) {
	spectest.Proves(t, feature, "machine-ruby-comes-first", "a-task-uses-the-provisioned-ruby-only-when-the-machine-ruby-is-unusable")
	root := fixture(t)
	pinnedDir := t.TempDir()
	used := filepath.Join(pinnedDir, "used")
	writeFile(t, pinnedDir, "ruby", "#!/bin/sh\necho 'ruby 3.3.6 (2024-11-05 revision 75015d4c1f) [x86_64-linux]'\n")
	writeFile(t, pinnedDir, "bundle", `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'Bundler version 2.5.22'; exit 0; fi
echo "$PWD: bundle $*" >> "`+used+`"
`)
	for _, f := range []string{"ruby", "bundle"} {
		if err := os.Chmod(filepath.Join(pinnedDir, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pricing := filepath.Join(root, "packs/pricing")

	// The fixture states 3.3.6: 3.2.4 is outside it.
	machine := stubTools(t, "3.2.4", "2.5.22", 0)
	if r := runJob(t, root, "packs/pricing", stacks.TaskLint); r.status != "FAILED" || r.err == nil || r.err.Error() != stacks.CodeToolchainMismatch {
		t.Fatalf("with no pinned Ruby the check fails as before: %+v", r)
	}
	t.Setenv("PUTNAMI_RUBY_EXECUTABLE", filepath.Join(pinnedDir, "ruby"))
	if r := runJob(t, root, "packs/pricing", stacks.TaskLint); r.status != "OK" || r.err != nil {
		t.Fatalf("a machine Ruby outside the statement gives way: %+v", r)
	}
	if got := calls(t, used); len(got) != 1 || got[0] != pricing+": bundle exec rubocop" {
		t.Errorf("the pinned Ruby's bundle ran %q", got)
	}
	if got := calls(t, machine); len(got) != 1 || got[0] != "" {
		t.Errorf("the machine's bundle ran %q", got)
	}

	if err := os.Remove(used); err != nil {
		t.Fatal(err)
	}
	machine = stubTools(t, "3.3.6", "2.5.22", 0)
	if r := runJob(t, root, "packs/pricing", stacks.TaskLint); r.status != "OK" || r.err != nil {
		t.Fatalf("a machine Ruby in the statement runs: %+v", r)
	}
	if got := calls(t, machine); len(got) != 1 || got[0] != pricing+": bundle exec rubocop" {
		t.Errorf("the machine's bundle ran %q", got)
	}
	if ran, err := os.ReadFile(used); err == nil {
		t.Errorf("a machine Ruby in the statement keeps the pinned Ruby out: %q", ran)
	}
}
