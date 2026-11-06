package lockdiff

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-analyses"

func read(t *testing.T, name, content string) Packages {
	t.Helper()
	kind, ok := KindOf("sub/" + name)
	if !ok {
		t.Fatalf("%s is not a lockfile", name)
	}
	packages, err := kind.Read([]byte(content))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return packages
}

func expect(t *testing.T, label string, got Packages, want map[string][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %v, want %v", label, got, want)
	}
	for name, versions := range want {
		have := slices.Clone(got[name])
		slices.Sort(have)
		if !slices.Equal(have, versions) {
			t.Errorf("%s: %s = %v, want %v", label, name, got[name], versions)
		}
	}
}

func TestEachLockfileFormatGivesItsPackageChanges(t *testing.T) {
	spectest.Proves(t, feature, "dependency-changes", "each-lockfile-format-gives-its-package-changes")
	expect(t, "package-lock v3", read(t, PackageLock, `{"lockfileVersion":3,"packages":{
		"":{"name":"root"},
		"packages/pricing":{"name":"@demo/pricing","version":"1.0.0"},
		"node_modules/@demo/pricing":{"resolved":"packages/pricing","link":true},
		"node_modules/decimal.js":{"version":"10.5.0"},
		"node_modules/@scope/a":{"version":"1.0.0"},
		"node_modules/b/node_modules/@scope/a":{"version":"2.0.0"},
		"node_modules/alias":{"name":"real","version":"3.0.0"}}}`),
		map[string][]string{"decimal.js": {"10.5.0"}, "@scope/a": {"1.0.0", "2.0.0"}, "real": {"3.0.0"}})
	expect(t, "package-lock v1", read(t, PackageLock, `{"lockfileVersion":1,"dependencies":{
		"a":{"version":"1.0.0","dependencies":{"b":{"version":"2.0.0"}}},
		"local":{"version":"file:../local"}}}`),
		map[string][]string{"a": {"1.0.0"}, "b": {"2.0.0"}})
	expect(t, "pnpm v9", read(t, PNPMLock, `lockfileVersion: '9.0'

importers:

  packages/pricing:
    dependencies:
      zod:
        specifier: 3.24.1
        version: 3.24.1

packages:

  '@vitest/coverage-v8@3.2.7':
    resolution: {integrity: sha512-x}

  zod@3.24.1:
    resolution: {integrity: sha512-y}

snapshots:

  zod@3.24.1: {}
  other@1.0.0: {}
`), map[string][]string{"@vitest/coverage-v8": {"3.2.7"}, "zod": {"3.24.1"}})
	expect(t, "pnpm v6", read(t, PNPMLock, "lockfileVersion: '6.0'\n\npackages:\n\n  /react-dom@18.2.0(react@18.2.0):\n    resolution: {integrity: x}\n"),
		map[string][]string{"react-dom": {"18.2.0"}})
	expect(t, "pnpm v5", read(t, PNPMLock, "lockfileVersion: 5.4\n\npackages:\n\n  /@babel/core/7.22.0_abc:\n    resolution: {integrity: x}\n"),
		map[string][]string{"@babel/core": {"7.22.0"}})
	expect(t, "pnpm local packages", read(t, PNPMLock, "lockfileVersion: 5.4\n\npackages:\n\n  file:projects/api.tgz:\n    name: '@rush-temp/api'\n  /zod/3.24.1:\n    resolution: {integrity: x}\n"),
		map[string][]string{"zod": {"3.24.1"}})
	expect(t, "pnpm v9 local packages", read(t, PNPMLock, "lockfileVersion: '9.0'\n\npackages:\n\n  '@rush-temp/api@file:projects/api.tgz':\n    resolution: {tarball: file:projects/api.tgz}\n  zod@3.24.1:\n    resolution: {integrity: x}\n"),
		map[string][]string{"zod": {"3.24.1"}})
	expect(t, "yarn classic", read(t, YarnLock, `# yarn lockfile v1

"@babel/core@^7.0.0", "@babel/core@^7.1.0":
  version "7.2.0"
  dependencies:
    debug "^4.1.0"

debug@^4.1.0:
  version "4.3.4"
`), map[string][]string{"@babel/core": {"7.2.0"}, "debug": {"4.3.4"}})
	expect(t, "yarn berry", read(t, YarnLock, `__metadata:
  version: 8

"@demo/pricing@workspace:packages/pricing":
  version: 0.0.0-use.local

"zod@npm:3.24.1":
  version: 3.24.1
  resolution: "zod@npm:3.24.1"
`), map[string][]string{"zod": {"3.24.1"}})
	expect(t, "composer", read(t, ComposerLock, `{"packages":[
		{"name":"brick/math","version":"0.13.1","dist":{"type":"zip"}},
		{"name":"demo/pricing","version":"dev-main","dist":{"type":"path"}}],
		"packages-dev":[{"name":"phpunit/phpunit","version":"12.0.0"}]}`),
		map[string][]string{"brick/math": {"0.13.1"}, "phpunit/phpunit": {"12.0.0"}})
	expect(t, "uv", read(t, UVLock, `version = 1
requires-python = ">=3.12"

[manifest]
members = ["api", "shared"]

[[package]]
name = "api"
version = "0.1.0"
source = { editable = "api" }
dependencies = [
    { name = "fastapi" },
    { name = "shared" },
]

[[package]]
name = "demo"
source = { virtual = "." }

[[package]]
name = "fastapi"
version = "0.115.6"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/fastapi-0.115.6.tar.gz", hash = "sha256:00", size = 1 }
wheels = [
    { url = "https://files.pythonhosted.org/fastapi-0.115.6-py3-none-any.whl", hash = "sha256:01", size = 1 },
]

[[package]]
name = "Typing_Extensions"
version = "4.12.2"
source = { registry = "https://pypi.org/simple" }

[[package]]
name = "vendored"
version = "1.0.0"
source = { path = "wheels/vendored-1.0.0.whl" }

[[package]]
name = "tool"
version = "2.0.0"
source = { git = "https://github.com/example/tool?rev=v2#abc" }
`), map[string][]string{"fastapi": {"0.115.6"}, "typing-extensions": {"4.12.2"}, "tool": {"2.0.0"}})
	expect(t, "poetry", read(t, PoetryLock, `# This file is automatically @generated by Poetry 1.8.3 and should not be changed by hand.

[[package]]
name = "pydantic"
version = "2.10.4"
description = "Data validation using Python type hints"
optional = false
python-versions = ">=3.8"
files = [
    {file = "pydantic-2.10.4-py3-none-any.whl", hash = "sha256:00"},
]

[package.dependencies]
typing-extensions = ">=4.12.2"

[package.extras]
email = ["email-validator (>=2.0.0)"]

[[package]]
name = "shared"
version = "0.1.0"
description = ""
optional = false
python-versions = "^3.12"
files = []
develop = true

[package.source]
type = "directory"
url = "../shared"

[[package]]
name = "zope.interface"
version = "7.2"
files = []

[metadata]
lock-version = "2.0"
python-versions = "^3.12"
content-hash = "00"
`), map[string][]string{"pydantic": {"2.10.4"}, "zope-interface": {"7.2"}})
	if got := PyPIName(" Flask__Login.ext "); got != "flask-login-ext" {
		t.Errorf("PyPIName = %q", got)
	}
	if _, ok := KindOf("go.sum"); ok {
		t.Error("go.sum is read as a lockfile")
	}
	for name, malformed := range map[string]string{PackageLock: "{", ComposerLock: "{", UVLock: "[[package]\n", PoetryLock: "name = \n"} {
		kind, _ := KindOf(name)
		if _, err := kind.Read([]byte(malformed)); err == nil {
			t.Errorf("a malformed %s reads", name)
		}
	}

	changes := Diff(
		Packages{"same": {"1.0.0"}, "up": {"1.2.3"}, "down": {"2.0.0"}, "gone": {"1.0.0"}, "git": {"abc"}, "multi": {"1.0.0"}, "pre": {"1.0.0-beta.2"}},
		Packages{"same": {"1.0.0"}, "up": {"1.10.0"}, "down": {"1.9.9"}, "new": {"0.1.0"}, "git": {"def"}, "multi": {"1.0.0", "2.0.0"}, "pre": {"1.0.0"}},
	)
	want := []Change{
		{Name: "down", Change: evidence.DependencyDowngraded, From: "2.0.0", To: "1.9.9"},
		{Name: "git", Change: evidence.DependencyChanged, From: "abc", To: "def"},
		{Name: "gone", Change: evidence.DependencyRemoved, From: "1.0.0"},
		{Name: "multi", Change: evidence.DependencyChanged, From: "1.0.0", To: "1.0.0, 2.0.0"},
		{Name: "new", Change: evidence.DependencyAdded, To: "0.1.0"},
		{Name: "pre", Change: evidence.DependencyUpgraded, From: "1.0.0-beta.2", To: "1.0.0"},
		{Name: "up", Change: evidence.DependencyUpgraded, From: "1.2.3", To: "1.10.0"},
	}
	if !slices.Equal(changes, want) {
		t.Errorf("diff\n got %+v\nwant %+v", changes, want)
	}
	for _, c := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.20.0", "v0.21.0", -1, true},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1, true},
		{"1.0.0-alpha.10", "1.0.0-alpha.9", 1, true},
		{"1.0.0-alpha.beta", "1.0.0-alpha.1", 1, true},
		{"1.0.0-rc.1", "1.0.0-beta", 1, true},
		{"1.2", "1.2.0+build", 0, true},
		{"dev-main", "1.0.0", 0, false},
	} {
		if got, ok := Compare(c.a, c.b); ok != c.ok || (ok && (got < 0) != (c.want < 0)) || (ok && (got > 0) != (c.want > 0)) {
			t.Errorf("Compare(%s, %s) = %d %v, want %d %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

func TestAPEXLockfileListsItsLockedRequirements(t *testing.T) {
	spectest.Proves(t, "tooling/observe-pants", "pants-resolve-lockfiles", "a-pex-lockfile-lists-its-locked-requirements")
	lock := `// This lockfile was autogenerated by Pants. To regenerate, run:
//
//    pants generate-lockfiles --resolve=python-default
//
// --- BEGIN PANTS LOCKFILE METADATA: DO NOT EDIT OR REMOVE ---
// {
//   "version": 3,
//   "generated_with_requirements": ["Flask_Login==0.6.3"]
// }
// --- END PANTS LOCKFILE METADATA ---

{
  "allow_builds": true,
  "locked_resolves": [
    {
      "locked_requirements": [
        {"project_name": "Flask_Login", "version": "0.6.3", "requires_dists": []},
        {"project_name": "werkzeug", "version": "3.1.3"}
      ],
      "platform_tag": null
    },
    {
      "locked_requirements": [
        {"project_name": "werkzeug", "version": "3.0.6"}
      ],
      "platform_tag": ["cp312", "cp312", "manylinux_2_17_x86_64"]
    }
  ],
  "pex_version": "2.27.1"
}
`
	got, err := ReadPEXLock([]byte(lock))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, "pex lock", got, map[string][]string{"flask-login": {"0.6.3"}, "werkzeug": {"3.0.6", "3.1.3"}})
	for name, content := range map[string]string{
		"not JSON":           "// header\nlocked_resolves = []\n",
		"no locked resolves": `{"pex_version": "2.27.1"}`,
	} {
		if _, err := ReadPEXLock([]byte(content)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

const jvmFeature = "tooling/observe-jvm"

func TestAGradleLockfileListsItsDependencies(t *testing.T) {
	spectest.Proves(t, jvmFeature, "gradle-dependency-changes", "a-gradle-lockfile-lists-its-dependencies")
	lock := `# This is a Gradle generated file for dependency locking.
# Manual edits can break the build and are not advised.
# This file is expected to be part of source control.
com.google.guava:guava:33.3.1-jre=compileClasspath,runtimeClasspath
org.jetbrains.kotlin:kotlin-stdlib:2.0.21=compileClasspath,runtimeClasspath,testCompileClasspath
org.jetbrains:annotations:13.0=compileClasspath
empty=annotationProcessor,testAnnotationProcessor
`
	for _, name := range []string{GradleLock, SettingsLock} {
		kind, ok := KindOf("pricing/" + name)
		if !ok || kind.Ecosystem != EcosystemMaven {
			t.Fatalf("%s: kind %+v %v", name, kind, ok)
		}
		got, err := kind.Read([]byte(lock))
		if err != nil {
			t.Fatal(err)
		}
		expect(t, name, got, map[string][]string{
			"com.google.guava:guava":             {"33.3.1-jre"},
			"org.jetbrains.kotlin:kotlin-stdlib": {"2.0.21"},
			"org.jetbrains:annotations":          {"13.0"},
		})
	}
	if _, err := ReadGradleLockfile([]byte("com.google.guava:guava=compileClasspath\n")); err == nil {
		t.Error("a line without a version reads")
	}
	changes := Diff(Packages{"com.google.guava:guava": {"33.3.1-jre"}}, Packages{"com.google.guava:guava": {"33.4.0-jre"}})
	if len(changes) != 1 || changes[0].Change != evidence.DependencyUpgraded {
		t.Errorf("changes %+v, want one upgrade", changes)
	}
}

func TestAVersionCatalogListsItsLibrariesAndPlugins(t *testing.T) {
	spectest.Proves(t, jvmFeature, "gradle-dependency-changes", "a-version-catalog-lists-its-libraries-and-plugins")
	catalog := `[versions]
kotlin = "2.0.21"
guava = { strictly = "[33.0, 34.0[", prefer = "33.3.1-jre" }
junit = { require = "5.11.4" }

[libraries]
commons-lang3 = "org.apache.commons:commons-lang3:3.17.0"
guava = { module = "com.google.guava:guava", version.ref = "guava" }
junit-jupiter = { group = "org.junit.jupiter", name = "junit-jupiter", version.ref = "junit" }
okhttp = { module = "com.squareup.okhttp3:okhttp", version = "4.12.0" }
coroutines = { module = "org.jetbrains.kotlinx:kotlinx-coroutines-core", version = { prefer = "1.9.0" } }
junit-bom = { module = "org.junit:junit-bom", version.ref = "junit" }
managed = { module = "org.slf4j:slf4j-api" }
loose = "org.slf4j:slf4j-simple"

[bundles]
testing = ["junit-jupiter"]

[plugins]
kotlin-jvm = { id = "org.jetbrains.kotlin.jvm", version.ref = "kotlin" }
android-library = "com.android.library:8.7.3"
versions = { id = "com.github.ben-manes.versions" }
`
	kind, ok := KindOf("gradle/libs.versions.toml")
	if !ok || kind.Ecosystem != EcosystemMaven {
		t.Fatalf("kind %+v %v", kind, ok)
	}
	if _, ok := KindOf("libs.versions.toml"); ok {
		t.Error("a catalog outside a gradle directory is read")
	}
	if _, ok := KindOf("gradle/tools.versions.toml"); !ok {
		t.Error("a second catalog of the gradle directory is not read")
	}
	got, err := kind.Read([]byte(catalog))
	if err != nil {
		t.Fatal(err)
	}
	// The strictly range wins over prefer: it is what the catalog states.
	expect(t, "catalog", got, map[string][]string{
		"org.apache.commons:commons-lang3":              {"3.17.0"},
		"com.google.guava:guava":                        {"[33.0, 34.0["},
		"org.junit.jupiter:junit-jupiter":               {"5.11.4"},
		"com.squareup.okhttp3:okhttp":                   {"4.12.0"},
		"org.jetbrains.kotlinx:kotlinx-coroutines-core": {"1.9.0"},
		"org.junit:junit-bom":                           {"5.11.4"},
		"org.jetbrains.kotlin.jvm":                      {"2.0.21"},
		"com.android.library":                           {"8.7.3"},
	})
	if _, err := ReadVersionCatalog([]byte("[libraries\n")); err == nil {
		t.Error("a malformed catalog reads")
	}
}

func TestPackageResolvedListsItsPins(t *testing.T) {
	spectest.Proves(t, "tooling/observe-swift", "swift-package-resolved", "package-resolved-lists-its-pins")
	v3 := `{
  "originHash" : "4f6d3c1e",
  "pins" : [
    {
      "identity" : "swift-collections",
      "kind" : "remoteSourceControl",
      "location" : "https://github.com/apple/swift-collections.git",
      "state" : { "revision" : "671108c96644956dddcd89dd59c203dcdb36cec7", "version" : "1.1.4" }
    },
    {
      "identity" : "swift-log",
      "kind" : "remoteSourceControl",
      "location" : "https://github.com/apple/swift-log",
      "state" : { "branch" : "main", "revision" : "96a2f8a0fa41e9e09af4585e2724c4e825410b91" }
    }
  ],
  "version" : 3
}`
	expect(t, "version 3", read(t, PackageResolved, v3), map[string][]string{
		"swift-collections": {"1.1.4"},
		"swift-log":         {"96a2f8a0fa41e9e09af4585e2724c4e825410b91"},
	})
	v1 := `{
  "object": {
    "pins": [
      {
        "package": "Alamofire",
        "repositoryURL": "https://github.com/Alamofire/Alamofire.git",
        "state": { "branch": null, "revision": "f455c2975872ccd2d9c81594c658af65716e9b9a", "version": "5.9.1" }
      }
    ]
  },
  "version": 1
}`
	expect(t, "version 1", read(t, PackageResolved, v1), map[string][]string{"Alamofire": {"5.9.1"}})
	kind, _ := KindOf("App.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved")
	if kind.Ecosystem != EcosystemSwift {
		t.Errorf("ecosystem %q", kind.Ecosystem)
	}
	for _, bad := range []string{"{", `{"version": 2}`} {
		if _, err := ReadPackageResolved([]byte(bad)); err == nil {
			t.Errorf("%q read without an error", bad)
		}
	}
}

func TestPodfileLockListsItsPods(t *testing.T) {
	spectest.Proves(t, "tooling/observe-swift", "swift-podfile-lock", "podfile-lock-lists-its-pods")
	lock := `PODS:
  - Alamofire (5.9.1)
  - Firebase/Core (11.4.0):
    - Firebase/CoreOnly
    - FirebaseAnalytics (= 11.4.0)
  - Firebase/CoreOnly (11.4.0):
    - FirebaseCore (= 11.4.0)
  - "GoogleUtilities/Environment (8.0.2)":
    - GoogleUtilities/Privacy
  - Pricing (1.0.0)

DEPENDENCIES:
  - Alamofire (~> 5.9)
  - Firebase/Core
  - Pricing (from ` + "`../Packages/Pricing`" + `)

SPEC REPOS:
  trunk:
    - Alamofire
    - Firebase

EXTERNAL SOURCES:
  Pricing:
    :path: "../Packages/Pricing"

SPEC CHECKSUMS:
  Alamofire: 7193b3b92c74a07f85569e1a6c4f4237291e7496

PODFILE CHECKSUM: 2f3c4d

COCOAPODS: 1.16.2
`
	kind, ok := KindOf("ios/Podfile.lock")
	if !ok || kind.Ecosystem != EcosystemCocoaPods {
		t.Fatalf("kind %+v, %v", kind, ok)
	}
	expect(t, "Podfile.lock", read(t, PodfileLock, lock), map[string][]string{
		"Alamofire":       {"5.9.1"},
		"Firebase":        {"11.4.0"},
		"GoogleUtilities": {"8.0.2"},
	})
	if _, err := ReadPodfileLock([]byte("COCOAPODS: 1.16.2\n")); err == nil {
		t.Error("a Podfile.lock without PODS read without an error")
	}
}

func TestAPubspecLockListsItsHostedPackages(t *testing.T) {
	spectest.Proves(t, "tooling/observe-dart", "pub-lockfile", "a-pubspec-lock-lists-its-hosted-packages")
	lock := `# Generated by pub
# See https://dart.dev/tools/pub/glossary#lockfile
packages:
  async:
    dependency: transitive
    description:
      name: async
      sha256: "758e6d74e971c3e5aceb4110bfd6698efc7f501675bcfe0c775459a8140750eb"
      url: "https://pub.dev"
    source: hosted
    version: "2.13.0"
  flutter:
    dependency: "direct main"
    description: flutter
    source: sdk
    version: "0.0.0"
  pricing:
    dependency: "direct main"
    description:
      path: "../../packages/pricing"
      relative: true
    source: path
    version: "1.0.0"
  vendored_lib:
    dependency: "direct main"
    description:
      path: "vendor/lib"
      ref: main
      resolved-ref: "0f6c2a6f86c35f53b4f04c3af11f1dd1e7d07ebf"
      url: "https://github.com/acme/vendored_lib.git"
    source: git
    version: "0.4.1"
sdks:
  dart: ">=3.6.0 <4.0.0"
  flutter: ">=3.27.0"
`
	expect(t, "pubspec.lock", read(t, PubspecLock, lock), map[string][]string{"async": {"2.13.0"}, "vendored_lib": {"0.4.1"}})
	if kind, _ := KindOf("apps/mobile/pubspec.lock"); kind.Ecosystem != EcosystemPub {
		t.Errorf("ecosystem %q", kind.Ecosystem)
	}
	expect(t, "an empty pubspec.lock", read(t, PubspecLock, "# Generated by pub\npackages: {}\nsdks:\n  dart: \">=3.6.0 <4.0.0\"\n"), map[string][]string{})
	for name, content := range map[string]string{
		"not YAML":           "packages:\n  a: [1\n",
		"packages not a map": "packages:\n  - a\n",
	} {
		if _, err := ReadPubspecLock([]byte(content)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
