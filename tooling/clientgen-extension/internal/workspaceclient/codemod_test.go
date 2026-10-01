package workspaceclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func TestSafeBindingImportCodemodUsesExactManifestLineageAndPreservesAliases(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "safe-source-adoption", "unchanged-generated-lineage-applies-go-and-typescript-import-moves")
	root := t.TempDir()
	baseline := t.TempDir()
	writeSourceIndex(t, root, "consumers/app")
	writeWorkspaceFile(t, root, "consumers/app/putnami.json", `{"name":"consumer"}`)
	writeWorkspaceFile(t, root, "consumers/app/client.go", `package app
import catalog "example.dev/old/catalog"
func bind(target any) { catalog.RegisterCatalogClient(target) }
`)
	writeWorkspaceFile(t, root, "consumers/app/client.ts", `
import { registerCatalogClient as bind } from "@example/old-catalog";
export { CatalogClient } from '@example/old-catalog';
const fixture = 'import { CatalogClient } from "@example/old-catalog"';
const regexFixture = /import "@example\/old-catalog"/;
const jsxFixture = <div>
  import "@example/old-catalog"
</div>;
bind(app);
`)
	writeWorkspaceFile(t, root, "consumers/app/unaliased.go", `package app
import "example.dev/old/catalog"
`)
	writeCodemodManifest(t, baseline, "providers/catalog/clients/go", clientcontract.GeneratedLanguageGo,
		"example.dev/old/catalog", "client.gen.go", "package catalog\n")
	writeCodemodManifest(t, root, "providers/catalog/clients/go", clientcontract.GeneratedLanguageGo,
		"example.dev/new/catalog", "client.gen.go", "package catalog\n")
	writeCodemodManifest(t, baseline, "providers/catalog/clients/ts", clientcontract.GeneratedLanguageTypeScript,
		"@example/old-catalog", "client.gen.ts", "export class CatalogClient {}\n")
	writeCodemodManifest(t, root, "providers/catalog/clients/ts", clientcontract.GeneratedLanguageTypeScript,
		"@example/new-catalog", "client.gen.ts", "export class CatalogClient {}\n")

	applied, err := ApplySafeBindingAdoptions(root, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || applied[0].Disposition != "applied" || applied[1].Disposition != "applied" {
		t.Fatalf("applied adaptations = %+v, want one per authored source", applied)
	}
	goSource := readCodemodSource(t, root, "consumers/app/client.go")
	if !strings.Contains(goSource, `catalog "example.dev/new/catalog"`) || strings.Contains(goSource, "example.dev/old/catalog") {
		t.Fatalf("Go alias/import migration is unsafe or incomplete:\n%s", goSource)
	}
	tsSource := readCodemodSource(t, root, "consumers/app/client.ts")
	if !strings.Contains(tsSource, `registerCatalogClient as bind } from "@example/new-catalog"`) ||
		!strings.Contains(tsSource, `export { CatalogClient } from '@example/new-catalog'`) ||
		!strings.Contains(tsSource, `const fixture = 'import { CatalogClient } from "@example/old-catalog"'`) ||
		!strings.Contains(tsSource, `/import "@example\/old-catalog"/`) ||
		!strings.Contains(tsSource, "import \"@example/old-catalog\"\n</div>") {
		t.Fatalf("TypeScript import migration changed aliases or string fixtures:\n%s", tsSource)
	}
	if source := readCodemodSource(t, root, "consumers/app/unaliased.go"); !strings.Contains(source, "example.dev/old/catalog") {
		t.Fatalf("unaliased Go import moved without package-name proof:\n%s", source)
	}
}

func TestSafeBindingImportCodemodQueuesNothingWhenLineageChanges(t *testing.T) {
	root := t.TempDir()
	baseline := t.TempDir()
	writeSourceIndex(t, root, "consumer")
	writeWorkspaceFile(t, root, "consumer/putnami.json", `{"name":"consumer"}`)
	writeWorkspaceFile(t, root, "consumer/client.go", `package consumer
import catalog "example.dev/old/catalog"
`)
	writeCodemodManifest(t, baseline, "provider/clients/go", clientcontract.GeneratedLanguageGo,
		"example.dev/old/catalog", "client.gen.go", "package catalog\n")
	writeCodemodManifest(t, root, "provider/clients/go", clientcontract.GeneratedLanguageGo,
		"example.dev/new/catalog", "client.gen.go", "package catalog\n")
	manifestPath := filepath.Join(root, "provider", "clients", "go", clientcontract.GeneratedManifestFile)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest clientcontract.GeneratedClientManifestV1
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Operations[0].MethodSymbol = "GetCatalogV2"
	writeJSONFile(t, manifestPath, manifest)

	applied, err := ApplySafeBindingAdoptions(root, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("changed public lineage produced unsafe edits: %+v", applied)
	}
	if source := readCodemodSource(t, root, "consumer/client.go"); !strings.Contains(source, "example.dev/old/catalog") {
		t.Fatalf("source changed despite operation-symbol drift:\n%s", source)
	}
}

func writeCodemodManifest(
	t *testing.T,
	root string,
	dir string,
	language clientcontract.GeneratedLanguage,
	importPath string,
	generatedFile string,
	generatedContent string,
) {
	t.Helper()
	writeWorkspaceFile(t, root, filepath.ToSlash(filepath.Join(dir, generatedFile)), generatedContent)
	digest := sha256.Sum256([]byte(generatedContent))
	manifest := clientcontract.GeneratedClientManifestV1{
		ProtocolVersion: clientcontract.ProtocolVersion,
		GeneratedBy:     clientcontract.GeneratedBy,
		Language:        language,
		Service:         clientcontract.Service{ID: "catalog", Audience: "catalog"},
		Binding: clientcontract.GeneratedBinding{ImportPath: importPath, Clients: []clientcontract.GeneratedBindingClient{{
			Service: "Catalog", ClientSymbol: "CatalogClient", BindingSymbol: "RegisterCatalogClient",
		}},
		},
		ContractSHA256: strings.Repeat("0", 64),
		Operations: []clientcontract.GeneratedOperation{{
			OperationID: "catalog.get", Service: "Catalog", MethodSymbol: "GetCatalog", Stream: clientcontract.StreamUnary,
			Transports: []clientcontract.Transport{{Protocol: clientcontract.TransportRESTJSON, Path: "/catalog", Encoding: clientcontract.EncodingJSON}},
		}},
		Files: []clientcontract.GeneratedFile{{Path: generatedFile, SHA256: hex.EncodeToString(digest[:])}},
	}
	writeJSONFile(t, filepath.Join(root, filepath.FromSlash(dir), clientcontract.GeneratedManifestFile), manifest)
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCodemodSource(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSafeBindingCodemodExchangesTheConstructorAndTheRegistrationCall(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "safe-source-adoption",
		"a-renamed-construction-is-exchanged-at-every-qualified-use")
	root := t.TempDir()
	baseline := t.TempDir()
	writeSourceIndex(t, root, "consumers/app")
	writeWorkspaceFile(t, root, "consumers/app/putnami.json", `{"name":"consumer"}`)
	writeWorkspaceFile(t, root, "consumers/app/wiring.go", `package app
import (
	catalog "example.dev/catalog"
	legacy "example.dev/legacy"
)

func bind(target any) *catalog.CatalogClient {
	catalog.RegisterCatalogClient(target)
	legacy.RegisterCatalogClient(target)
	var homonym legacy.CatalogClient
	_ = homonym
	return nil
}
`)
	writeWorkspaceFile(t, root, "consumers/app/wiring.ts", `
import { CatalogClient, registerCatalogClient } from '@example/catalog';
import { registerCatalogClient as legacyRegister } from '@example/legacy';
export function bind(target: unknown): CatalogClient {
  registerCatalogClient(target);
  legacyRegister(target);
  return new CatalogClient();
}
`)
	writeRenamedCodemodPair(t, root, baseline, "providers/catalog/clients/go", clientcontract.GeneratedLanguageGo,
		"example.dev/catalog", "client.gen.go", "package catalog\n")
	writeRenamedCodemodPair(t, root, baseline, "providers/catalog/clients/ts", clientcontract.GeneratedLanguageTypeScript,
		"@example/catalog", "client.gen.ts", "export class ItemCatalogClient {}\n")

	applied, err := ApplySafeBindingAdoptions(root, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 4 {
		t.Fatalf("applied adaptations = %d, want one per construction per source: %+v", len(applied), applied)
	}
	goSource := readCodemodSource(t, root, "consumers/app/wiring.go")
	for _, want := range []string{
		"catalog.RegisterItemCatalogClient(target)", "*catalog.ItemCatalogClient",
		"legacy.RegisterCatalogClient(target)", "var homonym legacy.CatalogClient",
	} {
		if !strings.Contains(goSource, want) {
			t.Fatalf("Go construction exchange is wrong, want %q in:\n%s", want, goSource)
		}
	}
	tsSource := readCodemodSource(t, root, "consumers/app/wiring.ts")
	for _, want := range []string{
		"import { ItemCatalogClient, registerItemCatalogClient } from '@example/catalog';",
		"registerItemCatalogClient(target);", "return new ItemCatalogClient();",
		"): ItemCatalogClient {",
		"import { registerCatalogClient as legacyRegister } from '@example/legacy';",
	} {
		if !strings.Contains(tsSource, want) {
			t.Fatalf("TypeScript construction exchange is wrong, want %q in:\n%s", want, tsSource)
		}
	}
}

func TestSafeBindingCodemodLeavesAnAlreadyMigratedSourceByteIdentical(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "safe-source-adoption", "an-already-migrated-source-is-left-untouched")
	root := t.TempDir()
	baseline := t.TempDir()
	writeSourceIndex(t, root, "consumers/app")
	writeWorkspaceFile(t, root, "consumers/app/putnami.json", `{"name":"consumer"}`)
	migrated := `package app
import catalog "example.dev/new/catalog"
func bind(target any) { catalog.RegisterCatalogClient(target) }
`
	writeWorkspaceFile(t, root, "consumers/app/client.go", migrated)
	writeCodemodManifest(t, baseline, "providers/catalog/clients/go", clientcontract.GeneratedLanguageGo,
		"example.dev/old/catalog", "client.gen.go", "package catalog\n")
	writeCodemodManifest(t, root, "providers/catalog/clients/go", clientcontract.GeneratedLanguageGo,
		"example.dev/new/catalog", "client.gen.go", "package catalog\n")
	before, err := os.Stat(filepath.Join(root, "consumers", "app", "client.go"))
	if err != nil {
		t.Fatal(err)
	}

	applied, err := ApplySafeBindingAdoptions(root, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("an already migrated source was reported as adapted: %+v", applied)
	}
	if source := readCodemodSource(t, root, "consumers/app/client.go"); source != migrated {
		t.Fatalf("an already migrated source was rewritten:\n%s", source)
	}
	after, err := os.Stat(filepath.Join(root, "consumers", "app", "client.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an already migrated source was rewritten with identical bytes; a no-op must not touch the file")
	}
}

func TestSafeBindingCodemodWritesNoFileWhenOneOfSeveralCannotBeStaged(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "safe-source-adoption", "a-multi-file-adoption-that-cannot-finish-writes-nothing")
	root := t.TempDir()
	baseline := t.TempDir()
	writeSourceIndex(t, root, "consumers/first", "consumers/second")
	original := `package app
import catalog "example.dev/old/catalog"
func bind(target any) { catalog.RegisterCatalogClient(target) }
`
	for _, project := range []string{"consumers/first", "consumers/second"} {
		writeWorkspaceFile(t, root, project+"/putnami.json", `{"name":"`+project+`"}`)
		writeWorkspaceFile(t, root, project+"/client.go", original)
	}
	writeCodemodManifest(t, baseline, "providers/catalog/clients/go", clientcontract.GeneratedLanguageGo,
		"example.dev/old/catalog", "client.gen.go", "package catalog\n")
	writeCodemodManifest(t, root, "providers/catalog/clients/go", clientcontract.GeneratedLanguageGo,
		"example.dev/new/catalog", "client.gen.go", "package catalog\n")
	// The second consumer's directory refuses a new file, so its stage fails
	// after the first one already succeeded.
	blocked := filepath.Join(root, "consumers", "second")
	if err := os.Chmod(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o750) })

	applied, err := ApplySafeBindingAdoptions(root, baseline)
	if err == nil {
		t.Fatal("a partially stageable multi-file adoption reported success")
	}
	if len(applied) != 0 {
		t.Fatalf("a failed adoption reported applied edits: %+v", applied)
	}
	for _, project := range []string{"consumers/first", "consumers/second"} {
		if source := readCodemodSource(t, root, project+"/client.go"); source != original {
			t.Fatalf("%s was migrated even though the adoption could not finish:\n%s", project, source)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "consumers", "first"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".putnami-clientgen-adopt-") {
			t.Fatalf("a staged file survived the failed adoption: %s", entry.Name())
		}
	}
}

func TestSafeBindingCodemodRefusesATypeScriptRenameItCannotProveLocally(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "safe-source-adoption", "a-local-binding-or-shorthand-keeps-the-rename-in-the-queue")
	root := t.TempDir()
	baseline := t.TempDir()
	writeSourceIndex(t, root, "consumers/shorthand", "consumers/shadowed")
	shorthand := `
import { CatalogClient, registerCatalogClient } from '@example/catalog';
export const registry = { registerCatalogClient };
export type Handle = CatalogClient;
`
	shadowed := `
import { registerCatalogClient } from '@example/catalog';
function wrap() {
  const registerCatalogClient = () => undefined;
  return registerCatalogClient;
}
`
	writeWorkspaceFile(t, root, "consumers/shorthand/putnami.json", `{"name":"shorthand"}`)
	writeWorkspaceFile(t, root, "consumers/shorthand/client.ts", shorthand)
	writeWorkspaceFile(t, root, "consumers/shadowed/putnami.json", `{"name":"shadowed"}`)
	writeWorkspaceFile(t, root, "consumers/shadowed/client.ts", shadowed)
	writeRenamedCodemodPair(t, root, baseline, "providers/catalog/clients/ts", clientcontract.GeneratedLanguageTypeScript,
		"@example/catalog", "client.gen.ts", "export class ItemCatalogClient {}\n")

	applied, err := ApplySafeBindingAdoptions(root, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("an unprovable TypeScript rename was applied: %+v", applied)
	}
	if source := readCodemodSource(t, root, "consumers/shorthand/client.ts"); source != shorthand {
		t.Fatalf("object shorthand was renamed:\n%s", source)
	}
	if source := readCodemodSource(t, root, "consumers/shadowed/client.ts"); source != shadowed {
		t.Fatalf("a locally shadowed name was renamed:\n%s", source)
	}
}

func renameCodemodBindingSymbol(t *testing.T, dir, bindingSymbol, clientSymbol string) {
	t.Helper()
	manifestPath := filepath.Join(dir, clientcontract.GeneratedManifestFile)
	data, err := os.ReadFile(manifestPath) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	var manifest clientcontract.GeneratedClientManifestV1
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if bindingSymbol != "" {
		manifest.Binding.Clients[0].BindingSymbol = bindingSymbol
	}
	if clientSymbol != "" {
		manifest.Binding.Clients[0].ClientSymbol = clientSymbol
	}
	writeJSONFile(t, manifestPath, manifest)
}

// writeRenamedCodemodPair emits the same contract twice at the same import
// path, with the public constructions renamed in the workspace copy.
func writeRenamedCodemodPair(
	t *testing.T,
	root, baseline, dir string,
	language clientcontract.GeneratedLanguage,
	importPath, generatedFile, generatedContent string,
) {
	t.Helper()
	writeCodemodManifest(t, baseline, dir, language, importPath, generatedFile, generatedContent)
	writeCodemodManifest(t, root, dir, language, importPath, generatedFile, generatedContent)
	if language == clientcontract.GeneratedLanguageTypeScript {
		// TypeScript registration is a function, so its emitted name is
		// lowerCamelCase on both sides of the rename.
		renameCodemodBindingSymbol(t, filepath.Join(baseline, filepath.FromSlash(dir)), "registerCatalogClient", "")
	}
	manifestPath := filepath.Join(root, filepath.FromSlash(dir), clientcontract.GeneratedManifestFile)
	data, err := os.ReadFile(manifestPath) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	var manifest clientcontract.GeneratedClientManifestV1
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Binding.Clients[0].ClientSymbol = "ItemCatalogClient"
	manifest.Binding.Clients[0].BindingSymbol = "RegisterItemCatalogClient"
	if language == clientcontract.GeneratedLanguageTypeScript {
		manifest.Binding.Clients[0].BindingSymbol = "registerItemCatalogClient"
	}
	writeJSONFile(t, manifestPath, manifest)
}
