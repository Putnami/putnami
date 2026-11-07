package swift

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-swift"

// writeTree writes files, keyed by slash path, under a new directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const pricingManifest = `// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "Pricing",
    platforms: [.iOS(.v16)],
    products: [
        .library(name: "Pricing", targets: ["Pricing"]),
    ],
    dependencies: [
        // .package(path: "../Legacy"),
        .package(path: "../Networking"),
        .package(url: "https://github.com/apple/swift-collections.git", from: "1.1.0"),
    ],
    targets: [
        .target(name: "Pricing", dependencies: ["Networking", .product(name: "Collections", package: "swift-collections")]),
        .testTarget(name: "PricingTests", dependencies: ["Pricing"]),
    ]
)
`

const networkingManifest = `// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "Networking",
    products: [.library(name: "Networking", targets: ["Networking"])],
    targets: [
        .target(name: "Networking"),
        .testTarget(name: "NetworkingTests", dependencies: ["Networking"]),
    ]
)
`

// pricingExport is swift package describe --type json for Pricing, as Swift
// 5.10 writes it on a macOS runner.
const pricingExport = `{
  "dependencies" : [
    {
      "identity" : "networking",
      "path" : "/Users/runner/work/shop/shop/Packages/Networking",
      "type" : "fileSystem"
    },
    {
      "identity" : "swift-collections",
      "requirement" : { "range" : [ { "lower_bound" : "1.1.0", "upper_bound" : "2.0.0" } ] },
      "type" : "sourceControl",
      "url" : "https://github.com/apple/swift-collections.git"
    }
  ],
  "manifest_display_name" : "Pricing",
  "name" : "Pricing",
  "path" : "/Users/runner/work/shop/shop/Packages/Pricing",
  "platforms" : [ { "name" : "ios", "version" : "16.0" } ],
  "products" : [
    { "name" : "Pricing", "targets" : [ "Pricing" ], "type" : { "library" : [ "automatic" ] } }
  ],
  "targets" : [
    {
      "c99name" : "PricingTests",
      "module_type" : "SwiftTarget",
      "name" : "PricingTests",
      "path" : "Tests/PricingTests",
      "sources" : [ "PriceCalculatorTests.swift" ],
      "target_dependencies" : [ "Pricing" ],
      "type" : "test"
    },
    {
      "c99name" : "Pricing",
      "module_type" : "SwiftTarget",
      "name" : "Pricing",
      "path" : "Sources/Pricing",
      "product_dependencies" : [ "Networking", "Collections" ],
      "product_memberships" : [ "Pricing" ],
      "sources" : [ "PriceCalculator.swift" ],
      "type" : "library"
    },
    {
      "c99name" : "PricingFixtures",
      "module_type" : "SwiftTarget",
      "name" : "PricingFixtures",
      "path" : "Fixtures",
      "sources" : [ "Quotes.swift" ],
      "type" : "library"
    }
  ],
  "tools_version" : "5.9"
}
`

// shopProject is the project.pbxproj of an app that references the local
// package Packages/Pricing and a remote package, in the shape Xcode 16
// writes it, cut to the sections the detector reads and their neighbors.
const shopProject = `// !$*UTF8*$!
{
	archiveVersion = 1;
	classes = {
	};
	objectVersion = 77;
	objects = {

/* Begin PBXBuildFile section */
		A10000000000000000000001 /* Pricing in Frameworks */ = {isa = PBXBuildFile; productRef = A10000000000000000000020 /* Pricing */; };
/* End PBXBuildFile section */

/* Begin PBXFileReference section */
		A10000000000000000000002 /* Shop.app */ = {isa = PBXFileReference; explicitFileType = wrapper.application; includeInIndex = 0; path = Shop.app; sourceTree = BUILT_PRODUCTS_DIR; };
		A10000000000000000000003 /* ShopTests.xctest */ = {isa = PBXFileReference; explicitFileType = wrapper.cfbundle; includeInIndex = 0; path = ShopTests.xctest; sourceTree = BUILT_PRODUCTS_DIR; };
/* End PBXFileReference section */

/* Begin PBXFileSystemSynchronizedRootGroup section */
		A10000000000000000000004 /* Shop */ = {isa = PBXFileSystemSynchronizedRootGroup; path = Shop; sourceTree = "<group>"; };
		A10000000000000000000005 /* ShopTests */ = {isa = PBXFileSystemSynchronizedRootGroup; path = ShopTests; sourceTree = "<group>"; };
/* End PBXFileSystemSynchronizedRootGroup section */

/* Begin PBXNativeTarget section */
		A10000000000000000000010 /* Shop */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = A10000000000000000000030 /* Build configuration list for PBXNativeTarget "Shop" */;
			buildPhases = (
			);
			dependencies = (
			);
			fileSystemSynchronizedGroups = (
				A10000000000000000000004 /* Shop */,
			);
			name = Shop;
			packageProductDependencies = (
				A10000000000000000000020 /* Pricing */,
			);
			productName = Shop;
			productReference = A10000000000000000000002 /* Shop.app */;
			productType = "com.apple.product-type.application";
		};
		A10000000000000000000011 /* ShopTests */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = A10000000000000000000031 /* Build configuration list for PBXNativeTarget "ShopTests" */;
			buildPhases = (
			);
			fileSystemSynchronizedGroups = (
				A10000000000000000000005 /* ShopTests */,
			);
			name = ShopTests;
			productName = ShopTests;
			productReference = A10000000000000000000003 /* ShopTests.xctest */;
			productType = "com.apple.product-type.bundle.unit-test";
		};
/* End PBXNativeTarget section */

/* Begin PBXProject section */
		A10000000000000000000040 /* Project object */ = {
			isa = PBXProject;
			attributes = {
				BuildIndependentTargetsInParallel = 1;
				LastSwiftUpdateCheck = 1600;
			};
			mainGroup = A10000000000000000000050;
			packageReferences = (
				A10000000000000000000060 /* XCLocalSwiftPackageReference "Packages/Pricing" */,
				A10000000000000000000061 /* XCRemoteSwiftPackageReference "swift-log" */,
			);
			targets = (
				A10000000000000000000010 /* Shop */,
				A10000000000000000000011 /* ShopTests */,
			);
		};
/* End PBXProject section */

/* Begin XCLocalSwiftPackageReference section */
		A10000000000000000000060 /* XCLocalSwiftPackageReference "Packages/Pricing" */ = {
			isa = XCLocalSwiftPackageReference;
			relativePath = Packages/Pricing;
		};
/* End XCLocalSwiftPackageReference section */

/* Begin XCRemoteSwiftPackageReference section */
		A10000000000000000000061 /* XCRemoteSwiftPackageReference "swift-log" */ = {
			isa = XCRemoteSwiftPackageReference;
			repositoryURL = "https://github.com/apple/swift-log";
			requirement = {
				kind = upToNextMajorVersion;
				minimumVersion = 1.6.0;
			};
		};
/* End XCRemoteSwiftPackageReference section */

/* Begin XCSwiftPackageProductDependency section */
		A10000000000000000000020 /* Pricing */ = {
			isa = XCSwiftPackageProductDependency;
			productName = Pricing;
		};
/* End XCSwiftPackageProductDependency section */
	};
	rootObject = A10000000000000000000040 /* Project object */;
}
`

func shopTree(t *testing.T, extra map[string]string) string {
	t.Helper()
	files := map[string]string{
		"Shop.xcodeproj/project.pbxproj":                                    shopProject,
		"Shop/ShopApp.swift":                                                "import SwiftUI\n",
		"ShopTests/CartTests.swift":                                         "import XCTest\n\nfinal class CheckoutTests: XCTestCase {}\n",
		"Packages/Pricing/Package.swift":                                    pricingManifest,
		"Packages/Pricing/Sources/Pricing/PriceCalculator.swift":            "import Networking\n",
		"Packages/Pricing/Tests/PricingTests/PriceCalculatorTests.swift":    "import XCTest\n\nfinal class PriceCalculatorTests: XCTestCase {}\n",
		"Packages/Pricing/Tests/PricingTests/Helpers/QuoteAssertions.swift": "@MainActor\nstruct QuoteSuite {}\n",
		"Packages/Networking/Package.swift":                                 networkingManifest,
		"Packages/Networking/Sources/Networking/Client.swift":               "public struct Client {}\n",
		"Packages/Networking/Tests/NetworkingTests/ClientTests.swift":       "import XCTest\n",
	}
	for name, content := range extra {
		files[name] = content
	}
	return writeTree(t, files)
}

func byID(components []Component) map[string]Component {
	out := map[string]Component{}
	for _, c := range components {
		out[c.ID] = c
	}
	return out
}

func expectComponent(t *testing.T, components []Component, want Component) {
	t.Helper()
	got, ok := byID(components)[want.ID]
	if !ok {
		t.Errorf("no component %s in %+v", want.ID, components)
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("component %s = %+v, want %+v", want.ID, got, want)
	}
}

func codes(diags []diag.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, string(d.Severity)+" "+d.Code)
	}
	return out
}

func TestExportTargetsAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "swiftpm-export-targets", "export-targets-are-components")
	spectest.Proves(t, feature, "swiftpm-export-edges", "a-target-dependency-is-an-edge")
	spectest.Proves(t, feature, "swiftpm-export-edges", "a-local-product-dependency-is-an-edge")
	root := shopTree(t, map[string]string{"Packages/Pricing/swiftpm.json": pricingExport})
	d := Detect(root)
	components := d.PackageComponents()
	ws, swift := evidence.SourceWorkspace, Ecosystem
	expectComponent(t, components, Component{ID: "Packages/Pricing", Name: "Pricing", Ecosystem: swift, Source: ws,
		DependsOn: []string{"Packages/Pricing/Fixtures", "Packages/Pricing/Sources/Pricing"}})
	expectComponent(t, components, Component{ID: "Packages/Pricing/Sources/Pricing", Name: "Pricing", Ecosystem: swift, Source: ws,
		DependsOn: []string{"Packages/Networking"}})
	expectComponent(t, components, Component{ID: "Packages/Pricing/Tests/PricingTests", Name: "PricingTests", Ecosystem: swift, Source: ws,
		DependsOn: []string{"Packages/Pricing/Sources/Pricing"}})
	expectComponent(t, components, Component{ID: "Packages/Pricing/Fixtures", Name: "PricingFixtures", Ecosystem: swift, Source: ws})
	if len(components) != 7 {
		t.Errorf("components %+v", components)
	}
	for _, d := range d.Diagnostics {
		if d.Code == CodeStaticRead && strings.Contains(d.Message, "Packages/Pricing/Package.swift") {
			t.Errorf("the exported package is reported as read statically: %s", d.Message)
		}
	}
}

func TestAProductDependencyFindsItsPackageByThePathSuffix(t *testing.T) {
	manifest := strings.Replace(pricingManifest, `.package(path: "../Networking"),`, `.package(name: "Networking", url: "https://example.com/networking.git", from: "1.0.0"),`, 1)
	root := shopTree(t, map[string]string{"Packages/Pricing/Package.swift": manifest, "Packages/Pricing/swiftpm.json": pricingExport})
	expectComponent(t, Detect(root).PackageComponents(), Component{ID: "Packages/Pricing/Sources/Pricing", Name: "Pricing", Ecosystem: Ecosystem,
		Source: evidence.SourceWorkspace, DependsOn: []string{"Packages/Networking"}})
}

func TestAMalformedExportFallsBackToTheStaticRead(t *testing.T) {
	spectest.Proves(t, feature, "swiftpm-export-unreadable", "a-malformed-export-falls-back-to-the-static-read")
	for name, content := range map[string]string{
		"not JSON":  "error: manifest parse failed\n",
		"no target": `{"name": "Pricing", "targets": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := Detect(shopTree(t, map[string]string{"Packages/Pricing/swiftpm.json": content}))
			if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{"warning " + CodeExportUnreadable, "info " + CodeStaticRead}) {
				t.Errorf("diagnostics %v", got)
			}
			if d.Diagnostics[0].Field != "Packages/Pricing/swiftpm.json" {
				t.Errorf("field %q", d.Diagnostics[0].Field)
			}
			expectComponent(t, d.PackageComponents(), Component{ID: "Packages/Pricing/Sources/Pricing", Name: "Pricing", Ecosystem: Ecosystem,
				Source: evidence.SourceWorkspace, DependsOn: []string{"Packages/Networking"}})
		})
	}
}

func TestStaticReadListsSourceAndTestTargets(t *testing.T) {
	spectest.Proves(t, feature, "swiftpm-static-read", "static-read-lists-source-and-test-targets")
	spectest.Proves(t, feature, "swiftpm-static-read", "a-local-package-path-is-an-edge")
	d := Detect(shopTree(t, nil))
	components := d.PackageComponents()
	ws, swift := evidence.SourceWorkspace, Ecosystem
	for _, want := range []Component{
		{ID: "Packages/Networking", Name: "Networking", Ecosystem: swift, Source: ws, DependsOn: []string{"Packages/Networking/Sources/Networking"}},
		{ID: "Packages/Networking/Sources/Networking", Name: "Networking", Ecosystem: swift, Source: ws},
		{ID: "Packages/Networking/Tests/NetworkingTests", Name: "NetworkingTests", Ecosystem: swift, Source: ws, DependsOn: []string{"Packages/Networking/Sources/Networking"}},
		{ID: "Packages/Pricing", Name: "Pricing", Ecosystem: swift, Source: ws, DependsOn: []string{"Packages/Pricing/Sources/Pricing"}},
		// The commented .package(path: "../Legacy") is no edge.
		{ID: "Packages/Pricing/Sources/Pricing", Name: "Pricing", Ecosystem: swift, Source: ws, DependsOn: []string{"Packages/Networking"}},
		{ID: "Packages/Pricing/Tests/PricingTests", Name: "PricingTests", Ecosystem: swift, Source: ws, DependsOn: []string{"Packages/Pricing/Sources/Pricing"}},
	} {
		expectComponent(t, components, want)
	}
	if len(components) != 6 {
		t.Errorf("components %+v", components)
	}
}

func TestAStaticReadIsAnInfoDiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "swiftpm-static-read-diagnostic", "a-static-read-is-an-info-diagnostic")
	d := Detect(shopTree(t, nil))
	if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{"info " + CodeStaticRead}) {
		t.Fatalf("diagnostics %v", got)
	}
	message := d.Diagnostics[0].Message
	for _, want := range []string{"2 Package.swift files", "Packages/Networking/Package.swift, Packages/Pricing/Package.swift", ExportCommand, "target dependency"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q lacks %q", message, want)
		}
	}
	if d := Detect(writeTree(t, map[string]string{"go.mod": "module example.com/x\n"})); len(d.Diagnostics) != 0 || len(d.PackageComponents()) != 0 {
		t.Errorf("a repository without Swift: %+v", d)
	}
}

func TestCheckoutsAndPodsAreSkipped(t *testing.T) {
	spectest.Proves(t, feature, "swift-walk", "checkouts-and-pods-are-skipped")
	root := shopTree(t, map[string]string{
		"Packages/Pricing/.build/checkouts/swift-collections/Package.swift": networkingManifest,
		"Pods/Alamofire/Package.swift":                                      networkingManifest,
		"Carthage/Checkouts/Kingfisher/Package.swift":                       networkingManifest,
		"DerivedData/SourcePackages/checkouts/swift-log/Package.swift":      networkingManifest,
		"build/SourcePackages/checkouts/swift-log/Package.swift":            networkingManifest,
		"node_modules/react-native/Package.swift":                           networkingManifest,
		"Shop.xcodeproj/project.xcworkspace/Package.swift":                  networkingManifest,
		"Shop.xcworkspace/xcshareddata/Package.swift":                       networkingManifest,
	})
	d := Detect(root)
	var dirs []string
	for _, p := range d.Packages {
		dirs = append(dirs, p.Dir)
	}
	if !reflect.DeepEqual(dirs, []string{"Packages/Networking", "Packages/Pricing"}) {
		t.Errorf("packages %v", dirs)
	}
	if len(d.Projects) != 1 || d.Projects[0].Dir != "." {
		t.Errorf("projects %+v", d.Projects)
	}
}

func TestAnXcodeProjectIsAComponent(t *testing.T) {
	spectest.Proves(t, feature, "xcode-projects", "an-xcode-project-is-a-component")
	spectest.Proves(t, feature, "xcode-projects", "a-local-package-reference-is-an-edge")
	root := shopTree(t, map[string]string{
		"apps/Admin/Admin.xcodeproj/project.pbxproj": strings.Replace(shopProject, "relativePath = Packages/Pricing;", `relativePath = "../../Packages/Networking";`, 1),
	})
	d := Detect(root)
	components := d.ProjectComponents()
	want := []Component{
		{ID: ".", Name: "Shop", Ecosystem: XcodeEcosystem, Source: evidence.SourceManifest, DependsOn: []string{"Packages/Pricing"}},
		{ID: "apps/Admin", Name: "Admin", Ecosystem: XcodeEcosystem, Source: evidence.SourceManifest, DependsOn: []string{"Packages/Networking"}},
	}
	if !reflect.DeepEqual(components, want) {
		t.Errorf("components %+v, want %+v", components, want)
	}
	if !reflect.DeepEqual(d.Projects[0].TestTargets, []string{"ShopTests"}) {
		t.Errorf("test targets %v", d.Projects[0].TestTargets)
	}
}

func TestAnUnreadableProjectIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "xcode-projects", "an-unreadable-project-is-a-diagnostic")
	for name, content := range map[string]string{
		"truncated":  shopProject[:len(shopProject)/2],
		"no objects": "// !$*UTF8*$!\n{\n\tarchiveVersion = 1;\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			d := Detect(writeTree(t, map[string]string{"ios/Shop.xcodeproj/project.pbxproj": content}))
			if got := codes(d.Diagnostics); !reflect.DeepEqual(got, []string{"warning " + CodeProjectUnreadable}) {
				t.Errorf("diagnostics %v", got)
			}
			want := []Component{{ID: "ios", Name: "Shop", Ecosystem: XcodeEcosystem, Source: evidence.SourceManifest}}
			if got := d.ProjectComponents(); !reflect.DeepEqual(got, want) {
				t.Errorf("components %+v", got)
			}
		})
	}
}

func TestAClassNamePlacesOnASwiftPMTestTarget(t *testing.T) {
	spectest.Proves(t, feature, "swift-test-placement", "a-class-name-places-on-a-swiftpm-test-target")
	spectest.Proves(t, feature, "swift-test-file", "a-placed-case-names-its-test-file")
	d := Detect(shopTree(t, nil))
	for name, want := range map[string][2]string{
		"PricingTests.PriceCalculatorTests":       {"Packages/Pricing/Tests/PricingTests/PriceCalculatorTests.swift", "Packages/Pricing/Tests/PricingTests"},
		"PricingTests.QuoteSuite":                 {"Packages/Pricing/Tests/PricingTests/Helpers/QuoteAssertions.swift", "Packages/Pricing/Tests/PricingTests"},
		"PricingTests.PriceCalculatorTests.Inner": {"Packages/Pricing/Tests/PricingTests/PriceCalculatorTests.swift", "Packages/Pricing/Tests/PricingTests"},
		"NetworkingTests.ClientTests":             {"Packages/Networking/Tests/NetworkingTests/ClientTests.swift", "Packages/Networking/Tests/NetworkingTests"},
		"NetworkingTests.RetryTests":              {"Packages/Networking/Tests/NetworkingTests", "Packages/Networking/Tests/NetworkingTests"},
		"PriceCalculatorTests":                    {"Packages/Pricing/Tests/PricingTests/PriceCalculatorTests.swift", "Packages/Pricing/Tests/PricingTests"},
	} {
		file, component, ok := d.Locate(name)
		if !ok || file != want[0] || component != want[1] {
			t.Errorf("Locate(%q) = %q, %q, %v, want %q, %q", name, file, component, ok, want[0], want[1])
		}
	}
	for _, name := range []string{"Unknown.PriceCalculatorTests", "RetryTests", "tests/test_api.py", "example.com/shop/api", ""} {
		if file, component, ok := d.Locate(name); ok {
			t.Errorf("Locate(%q) = %q, %q", name, file, component)
		}
	}
}

func TestAClassNamePlacesOnAnXcodeTestTarget(t *testing.T) {
	spectest.Proves(t, feature, "swift-test-placement", "a-class-name-places-on-an-xcode-test-target")
	d := Detect(shopTree(t, nil))
	file, component, ok := d.Locate("ShopTests.CheckoutTests")
	if !ok || file != "ShopTests/CartTests.swift" || component != "." {
		t.Errorf("Locate = %q, %q, %v", file, component, ok)
	}
	// A test target without a directory of its name searches the project's.
	root := writeTree(t, map[string]string{
		"ios/Shop.xcodeproj/project.pbxproj": shopProject,
		"ios/Tests/CheckoutTests.swift":      "final class CheckoutTests {}\n",
	})
	if file, component, ok := Detect(root).Locate("ShopTests.CheckoutTests"); !ok || file != "ios/Tests/CheckoutTests.swift" || component != "ios" {
		t.Errorf("Locate = %q, %q, %v", file, component, ok)
	}
}

func TestParsePlistReadsQuotedAndBareStrings(t *testing.T) {
	doc, err := parsePlist([]byte(`{ a = "x \"y\" z"; b = (1, two, "3",); c = { d = e/f.g; }; }`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": `x "y" z`, "b": []any{"1", "two", "3"}, "c": map[string]any{"d": "e/f.g"}}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("doc %#v", doc)
	}
	for _, bad := range []string{"{ a = b }", "{ a b; }", `{ a = "b; }`, "( a, b", "{ a = b; } c"} {
		if _, err := parsePlist([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestARootPackageAndItsModuleNames(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Package.swift":                             "// swift-tools-version:5.9\nimport PackageDescription\n\nlet name = \"Tool\"\nlet package = Package(name: name, targets: [])\n",
		"Sources/tool-core/Core.swift":              "struct Core {}\n",
		"Tests/tool-core-tests/CoreTests.swift":     "final class CoreTests {}\n",
		"Tests/tool-core-tests/Fixtures/Data.swift": "enum Data {}\n",
	})
	d := Detect(root)
	ws := evidence.SourceWorkspace
	want := []Component{
		{ID: ".", Ecosystem: Ecosystem, Source: ws, DependsOn: []string{"Sources/tool-core"}},
		{ID: "Sources/tool-core", Name: "tool-core", Ecosystem: Ecosystem, Source: ws},
		{ID: "Tests/tool-core-tests", Name: "tool-core-tests", Ecosystem: Ecosystem, Source: ws, DependsOn: []string{"Sources/tool-core"}},
	}
	if got := d.PackageComponents(); !reflect.DeepEqual(got, want) {
		t.Errorf("components %+v, want %+v", got, want)
	}
	if len(d.Diagnostics) != 1 || d.Diagnostics[0].Field != "Package.swift" {
		t.Errorf("diagnostics %+v", d.Diagnostics)
	}
	// Swift spells the module of tool-core-tests tool_core_tests.
	for name, file := range map[string]string{
		"tool_core_tests.CoreTests": "Tests/tool-core-tests/CoreTests.swift",
		"tool_core_tests.Data":      "Tests/tool-core-tests/Fixtures/Data.swift",
	} {
		if got, component, ok := d.Locate(name); !ok || got != file || component != "Tests/tool-core-tests" {
			t.Errorf("Locate(%q) = %q, %q, %v", name, got, component, ok)
		}
	}
	if _, _, ok := d.Locate("tool-core-tests.CoreTests"); ok {
		t.Error("a name with - is no Swift class name")
	}
}

func TestProjectsOfOneDirectoryAreOneComponent(t *testing.T) {
	ui := strings.NewReplacer(
		"com.apple.product-type.bundle.unit-test", "com.apple.product-type.bundle.ui-testing",
		"name = ShopTests;", "name = ShopUITests;",
		"relativePath = Packages/Pricing;", "relativePath = Packages/Networking;",
	).Replace(shopProject)
	legacy := strings.NewReplacer(
		"com.apple.product-type.bundle.unit-test", "com.apple.product-type.bundle.ocunit-test",
		"name = ShopTests;", "name = LegacyTests;",
	).Replace(shopProject)
	d := Detect(writeTree(t, map[string]string{
		"ios/Shop.xcodeproj/project.pbxproj":   shopProject,
		"ios/ShopUI.xcodeproj/project.pbxproj": ui,
		"ios/Legacy.xcodeproj/project.pbxproj": legacy,
	}))
	if len(d.Projects) != 1 {
		t.Fatalf("projects %+v", d.Projects)
	}
	p := d.Projects[0]
	if p.Name != "Legacy" || !reflect.DeepEqual(p.TestTargets, []string{"LegacyTests", "ShopTests", "ShopUITests"}) ||
		!reflect.DeepEqual(p.LocalPackages, []string{"ios/Packages/Networking", "ios/Packages/Pricing"}) {
		t.Errorf("project %+v", p)
	}
}

func TestOneScanIndexesATargetOnce(t *testing.T) {
	root := shopTree(t, nil)
	d := Detect(root)
	if _, _, ok := d.Locate("PricingTests.PriceCalculatorTests"); !ok {
		t.Fatal("not located")
	}
	// A file added after the first lookup is not seen: the target's
	// directory was indexed once.
	if err := os.WriteFile(filepath.Join(root, "Packages/Pricing/Tests/PricingTests/LateTests.swift"), []byte("final class LateTests {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if file, _, _ := d.Locate("PricingTests.LateTests"); file != "Packages/Pricing/Tests/PricingTests" {
		t.Errorf("Locate = %q, want the target's directory", file)
	}
}
