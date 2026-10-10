package workspaceclient

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

type bindingImportMove struct {
	language        clientcontract.GeneratedLanguage
	serviceID       string
	providerProject string
	oldImportPath   string
	newImportPath   string
	bindings        []AdaptationBinding
}

// bindingSymbolMove is one public construction the emitter renamed while the
// contract, the operations and the import path stayed identical. The product
// contract promises exactly two of them per service — the client constructor
// and the registration call — so both are named, never inferred from shape.
type bindingSymbolMove struct {
	language        clientcontract.GeneratedLanguage
	serviceID       string
	providerProject string
	importPath      string
	packageName     string
	construction    string
	oldSymbol       string
	newSymbol       string
	bindings        []AdaptationBinding
}

type sourceRewrite struct {
	path        string
	language    string
	content     []byte
	importMoves []bindingImportMove
	symbolMoves []bindingSymbolMove
}

type sourceReplacement struct {
	start      int
	end        int
	value      string
	importMove *bindingImportMove
	symbolMove *bindingSymbolMove
}

// ApplySafeBindingAdoptions rewrites authored sources for the two moves a
// generated manifest can prove on its own: the package the binding lives in,
// and the public constructions the product contract promises for it — the
// client constructor and the registration call. Both require the pre-sync and
// post-sync manifests to agree on contract, service and operations. Argument,
// credential and call-shape changes are never inferred; they stay in the
// adaptation queue.
//
// Every file is planned and staged before any file is renamed into place, so a
// workspace is never left with one consumer migrated and the next one not.
func ApplySafeBindingAdoptions(workspaceRoot, baselineRoot string) ([]Adaptation, error) {
	importMoves, symbolMoves, generated, err := discoverBindingMoves(workspaceRoot, baselineRoot)
	if err != nil || len(importMoves)+len(symbolMoves) == 0 {
		return nil, err
	}
	records, err := discoverSourceRecords(indexedView(workspaceRoot), generated)
	if err != nil {
		return nil, err
	}
	rewrites := make([]sourceRewrite, 0, len(records))
	for _, record := range records {
		content := []byte(record.content)
		var replacements []sourceReplacement
		switch record.language {
		case string(clientcontract.GeneratedLanguageGo):
			replacements = append(goBindingImportReplacements(record.path, content, importMoves),
				goBindingSymbolReplacements(record.path, content, symbolMoves)...)
		case string(clientcontract.GeneratedLanguageTypeScript):
			replacements = append(tsBindingImportReplacements(record.content, importMoves),
				tsBindingSymbolReplacements(record.content, symbolMoves)...)
		}
		if len(replacements) == 0 {
			continue
		}
		updated, usedImports, usedSymbols, replaceErr := applySourceReplacements(content, replacements)
		if replaceErr != nil {
			return nil, fmt.Errorf("plan generated binding adoption for %s: %w", record.path, replaceErr)
		}
		rewrites = append(rewrites, sourceRewrite{path: record.path, language: record.language,
			content: updated, importMoves: usedImports, symbolMoves: usedSymbols})
	}
	if err := commitSourceRewrites(workspaceRoot, rewrites); err != nil {
		return nil, err
	}
	var applied []Adaptation
	for _, rewrite := range rewrites {
		for _, move := range rewrite.importMoves {
			applied = append(applied, Adaptation{
				Path: rewrite.path, Language: rewrite.language, ServiceID: move.serviceID,
				Evidence: []string{
					"contract:" + move.serviceID,
					"import:" + move.oldImportPath + "->" + move.newImportPath,
					"manifest-lineage:contract+operations+binding-symbols",
				},
				Bindings: move.bindings, Disposition: "applied",
				Reason: "the generated package import moved while its validated contract, operations and public binding symbols remained identical",
			})
		}
		for _, move := range rewrite.symbolMoves {
			applied = append(applied, Adaptation{
				Path: rewrite.path, Language: rewrite.language, ServiceID: move.serviceID,
				Evidence: []string{
					"construction:" + move.construction,
					"contract:" + move.serviceID,
					"manifest-lineage:contract+operations+import-path",
					"symbol:" + move.oldSymbol + "->" + move.newSymbol,
				},
				Bindings: move.bindings, Disposition: "applied",
				Reason: "the generated " + move.construction + " was renamed while its validated contract, operations and import path remained identical",
			})
		}
	}
	sort.Slice(applied, func(i, j int) bool {
		return applied[i].Path+"\x00"+applied[i].ServiceID+"\x00"+strings.Join(applied[i].Evidence, "\x00") <
			applied[j].Path+"\x00"+applied[j].ServiceID+"\x00"+strings.Join(applied[j].Evidence, "\x00")
	})
	return applied, nil
}

// commitSourceRewrites stages every planned file next to its target, then
// renames them all. Staging is where a failure is expected — an unwritable
// directory, a missing file, a full disk — and no rename happens until every
// stage has succeeded.
func commitSourceRewrites(workspaceRoot string, rewrites []sourceRewrite) error {
	staged := make([]stagedSource, 0, len(rewrites))
	defer func() {
		for _, item := range staged {
			_ = os.Remove(item.temporary)
		}
	}()
	for _, rewrite := range rewrites {
		target := filepath.Join(workspaceRoot, filepath.FromSlash(rewrite.path))
		temporary, err := stageSource(target, rewrite.content)
		if err != nil {
			return fmt.Errorf("stage generated binding adoption for %s: %w", rewrite.path, err)
		}
		staged = append(staged, stagedSource{target: target, temporary: temporary, path: rewrite.path})
	}
	for _, item := range staged {
		if err := os.Rename(item.temporary, item.target); err != nil {
			return fmt.Errorf("write generated binding adoption to %s: %w", item.path, err)
		}
	}
	staged = nil
	return nil
}

type stagedSource struct {
	target    string
	temporary string
	path      string
}

func discoverBindingMoves(workspaceRoot, baselineRoot string) ([]bindingImportMove, []bindingSymbolMove, map[string]bool, error) {
	generated, current, err := validGeneratedManifests(workspaceRoot)
	if err != nil {
		return nil, nil, nil, err
	}
	_, before, err := validGeneratedManifests(baselineRoot)
	if err != nil {
		return nil, nil, nil, err
	}
	paths := make([]string, 0, len(before))
	for path := range before {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	moves := make([]bindingImportMove, 0, len(paths))
	symbolMoves := make([]bindingSymbolMove, 0, len(paths))
	oldDestinations := map[string]string{}
	for _, path := range paths {
		oldManifest := before[path]
		newManifest, exists := current[path]
		if !exists || !sameGeneratedContractLineage(oldManifest, newManifest) {
			continue
		}
		manifestDir := filepath.ToSlash(filepath.Dir(path))
		providerProject := manifestDir
		if index := strings.Index(manifestDir, "/clients/"); index >= 0 {
			providerProject = manifestDir[:index]
		}
		bindings := make([]AdaptationBinding, 0, len(newManifest.Binding.Clients))
		for _, binding := range newManifest.Binding.Clients {
			bindings = append(bindings, AdaptationBinding{
				ProviderProject: providerProject, ImportPath: newManifest.Binding.ImportPath,
				Service: binding.Service, ClientSymbol: binding.ClientSymbol, BindingSymbol: binding.BindingSymbol,
			})
		}
		movedImport := oldManifest.Binding.ImportPath != newManifest.Binding.ImportPath
		if movedImport && reflect.DeepEqual(oldManifest.Binding.Clients, newManifest.Binding.Clients) {
			key := string(newManifest.Language) + "\x00" + oldManifest.Binding.ImportPath
			if existing, conflict := oldDestinations[key]; conflict && existing != newManifest.Binding.ImportPath {
				return nil, nil, nil, fmt.Errorf("generated import %q has conflicting migration destinations", oldManifest.Binding.ImportPath)
			}
			oldDestinations[key] = newManifest.Binding.ImportPath
			moves = append(moves, bindingImportMove{
				language: newManifest.Language, serviceID: newManifest.Service.ID, providerProject: providerProject,
				oldImportPath: oldManifest.Binding.ImportPath, newImportPath: newManifest.Binding.ImportPath,
				bindings: bindings,
			})
			continue
		}
		if movedImport {
			// The package moved AND a public construction was renamed. Two
			// simultaneous proofs are one guess: leave it in the queue.
			continue
		}
		packageName := generatedGoPackageName(workspaceRoot, path, newManifest)
		for _, renamed := range renamedBindingConstructions(oldManifest, newManifest) {
			renamed.language = newManifest.Language
			renamed.serviceID = newManifest.Service.ID
			renamed.providerProject = providerProject
			renamed.importPath = newManifest.Binding.ImportPath
			renamed.packageName = packageName
			renamed.bindings = bindings
			symbolMoves = append(symbolMoves, renamed)
		}
	}
	return moves, symbolMoves, generated, nil
}

// renamedBindingConstructions pairs the before and after manifests by service
// and names each public construction whose symbol changed. A service that
// appears or disappears is a contract change, not a rename, and produces
// nothing.
func renamedBindingConstructions(before, after *clientcontract.GeneratedClientManifestV1) []bindingSymbolMove {
	if len(before.Binding.Clients) != len(after.Binding.Clients) {
		return nil
	}
	previous := make(map[string]clientcontract.GeneratedBindingClient, len(before.Binding.Clients))
	for _, client := range before.Binding.Clients {
		previous[client.Service] = client
	}
	var moves []bindingSymbolMove
	for _, client := range after.Binding.Clients {
		old, exists := previous[client.Service]
		if !exists {
			return nil
		}
		for _, construction := range []struct{ name, from, to string }{
			{"client constructor", old.ClientSymbol, client.ClientSymbol},
			{"registration call", old.BindingSymbol, client.BindingSymbol},
		} {
			if construction.from == construction.to || construction.from == "" || construction.to == "" {
				continue
			}
			moves = append(moves, bindingSymbolMove{
				construction: construction.name, oldSymbol: construction.from, newSymbol: construction.to,
			})
		}
	}
	sort.Slice(moves, func(i, j int) bool { return moves[i].oldSymbol < moves[j].oldSymbol })
	return moves
}

// generatedGoPackageName reads the package clause the emitter actually wrote.
// Go's package identifier is source-owned, so a rename of a symbol inside an
// unaliased import can only be applied against the real package name, never
// against the last segment of the import path.
func generatedGoPackageName(root, manifestPath string, manifest *clientcontract.GeneratedClientManifestV1) string {
	if manifest.Language != clientcontract.GeneratedLanguageGo {
		return ""
	}
	base := filepath.ToSlash(filepath.Dir(manifestPath))
	for _, file := range manifest.Files {
		if !strings.HasSuffix(file.Path, ".go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(joinRel(base, file.Path)))) //nolint:gosec // validated generated manifest path
		if err != nil {
			continue
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), file.Path, content, parser.PackageClauseOnly)
		if parseErr == nil && parsed.Name != nil {
			return parsed.Name.Name
		}
	}
	return ""
}

func validGeneratedManifests(root string) (map[string]bool, map[string]*clientcontract.GeneratedClientManifestV1, error) {
	generated := map[string]bool{}
	manifests := map[string]*clientcontract.GeneratedClientManifestV1{}
	if root == "" {
		return generated, manifests, nil
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			if path != root && excludedSnapshotDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != clientcontract.GeneratedManifestFile {
			return nil
		}
		data, readErr := os.ReadFile(path) //nolint:gosec // workspace or private snapshot traversal
		if readErr != nil {
			return readErr
		}
		manifest, valid := parseManifestWithoutDiagnostics(data)
		if !valid {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		base := filepath.ToSlash(filepath.Dir(rel))
		if !manifestFilesMatch(workspaceFiles{root: root}, base, manifest.Files) {
			return nil
		}
		manifests[rel] = manifest
		for _, file := range manifest.Files {
			generated[joinRel(base, file.Path)] = true
		}
		return nil
	})
	return generated, manifests, err
}

// sameGeneratedContractLineage is the proof both codemods share: the emitter
// produced the same service, from the same contract bytes, with the same
// operations and method symbols. What may still differ is where the package
// lives and what its public constructions are called.
func sameGeneratedContractLineage(before, after *clientcontract.GeneratedClientManifestV1) bool {
	return before != nil && after != nil &&
		before.ProtocolVersion == after.ProtocolVersion && before.GeneratedBy == after.GeneratedBy &&
		before.Language == after.Language && before.Service == after.Service &&
		before.ContractSHA256 == after.ContractSHA256 &&
		reflect.DeepEqual(before.Operations, after.Operations)
}

func goBindingImportReplacements(path string, content []byte, moves []bindingImportMove) []sourceReplacement {
	moveByImport := map[string]bindingImportMove{}
	for _, move := range moves {
		if move.language == clientcontract.GeneratedLanguageGo {
			moveByImport[move.oldImportPath] = move
		}
	}
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, path, content, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	var replacements []sourceReplacement
	for _, spec := range file.Imports {
		value, unquoteErr := strconv.Unquote(spec.Path.Value)
		move, exists := moveByImport[value]
		// The generated manifest carries the import path and public symbols, but
		// Go's default package identifier is source-owned and can change across a
		// package move. An explicit alias is the proof that selector identity is
		// stable, so unaliased, dot and blank imports stay in the manual queue.
		if unquoteErr != nil || !exists || spec.Name == nil || spec.Name.Name == "." || spec.Name.Name == "_" {
			continue
		}
		start := files.Position(spec.Path.Pos()).Offset
		end := files.Position(spec.Path.End()).Offset
		replacements = append(replacements, sourceReplacement{start: start, end: end,
			value: strconv.Quote(move.newImportPath), importMove: &move})
	}
	return replacements
}

var (
	tsSideEffectImport = regexp.MustCompile(`\bimport\s*(['"])([^'"\r\n]+)(['"])`)
	tsFromImport       = regexp.MustCompile(`(?s)\b(?:import|export)\s+(?:type\s+)?[A-Za-z0-9_$*{},\s]+?\s+from\s*(['"])([^'"\r\n]+)(['"])`)
)

func tsBindingImportReplacements(content string, moves []bindingImportMove) []sourceReplacement {
	moveByImport := map[string]bindingImportMove{}
	for _, move := range moves {
		if move.language == clientcontract.GeneratedLanguageTypeScript {
			moveByImport[move.oldImportPath] = move
		}
	}
	code := tsCodeMask(content)
	seen := map[int]bool{}
	var replacements []sourceReplacement
	for _, pattern := range []*regexp.Regexp{tsSideEffectImport, tsFromImport} {
		for _, indices := range pattern.FindAllStringSubmatchIndex(content, -1) {
			if len(indices) < 8 || indices[0] < 0 || !sourceKeywordAt(code, indices[0]) {
				continue
			}
			quoteStart, quoteEnd := indices[2], indices[3]
			moduleStart, moduleEnd := indices[4], indices[5]
			closingStart, closingEnd := indices[6], indices[7]
			if quoteStart < 0 || quoteEnd-quoteStart != 1 || closingStart < 0 || closingEnd-closingStart != 1 ||
				content[quoteStart] != content[closingStart] || seen[moduleStart] {
				continue
			}
			move, exists := moveByImport[content[moduleStart:moduleEnd]]
			if !exists {
				continue
			}
			seen[moduleStart] = true
			replacements = append(replacements, sourceReplacement{start: moduleStart, end: moduleEnd,
				value: move.newImportPath, importMove: &move})
		}
	}
	return replacements
}

func sourceKeywordAt(code string, offset int) bool {
	return offset >= 0 && offset < len(code) &&
		(strings.HasPrefix(code[offset:], "import") || strings.HasPrefix(code[offset:], "export"))
}

func applySourceReplacements(content []byte, replacements []sourceReplacement) (
	[]byte, []bindingImportMove, []bindingSymbolMove, error,
) {
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start > replacements[j].start })
	updated := append([]byte(nil), content...)
	usedImports := map[string]bindingImportMove{}
	usedSymbols := map[string]bindingSymbolMove{}
	lastStart := len(content) + 1
	for _, replacement := range replacements {
		if replacement.start < 0 || replacement.end < replacement.start || replacement.end > len(content) || replacement.end > lastStart {
			return nil, nil, nil, fmt.Errorf("overlapping or invalid source replacement")
		}
		updated = append(append(append([]byte(nil), updated[:replacement.start]...), replacement.value...), updated[replacement.end:]...)
		lastStart = replacement.start
		if move := replacement.importMove; move != nil {
			usedImports[string(move.language)+"\x00"+move.oldImportPath+"\x00"+move.newImportPath] = *move
		}
		if move := replacement.symbolMove; move != nil {
			usedSymbols[string(move.language)+"\x00"+move.importPath+"\x00"+move.oldSymbol+"\x00"+move.newSymbol] = *move
		}
	}
	imports := make([]bindingImportMove, 0, len(usedImports))
	for _, move := range usedImports {
		imports = append(imports, move)
	}
	sort.Slice(imports, func(i, j int) bool {
		return string(imports[i].language)+"\x00"+imports[i].oldImportPath < string(imports[j].language)+"\x00"+imports[j].oldImportPath
	})
	symbols := make([]bindingSymbolMove, 0, len(usedSymbols))
	for _, move := range usedSymbols {
		symbols = append(symbols, move)
	}
	sort.Slice(symbols, func(i, j int) bool {
		return string(symbols[i].language)+"\x00"+symbols[i].importPath+"\x00"+symbols[i].oldSymbol <
			string(symbols[j].language)+"\x00"+symbols[j].importPath+"\x00"+symbols[j].oldSymbol
	})
	return updated, imports, symbols, nil
}

// stageSource writes the migrated bytes beside their target and returns the
// staged path. Nothing is renamed here: the caller renames every stage only
// once all of them exist.
func stageSource(path string, content []byte) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".putnami-clientgen-adopt-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return "", err
	}
	if _, err := bytes.NewReader(content).WriteTo(temporary); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return "", err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return "", err
	}
	return temporaryPath, nil
}

// goBindingSymbolReplacements renames a qualified selector on the exact
// package identifier the file binds to the generated import. A Go selector is
// unambiguous once the identifier is resolved, so a same-named symbol from
// another package is never touched, and an unaliased import is safe here
// because the package clause the emitter wrote is read, not guessed.
func goBindingSymbolReplacements(path string, content []byte, moves []bindingSymbolMove) []sourceReplacement {
	byImport := map[string][]bindingSymbolMove{}
	for _, move := range moves {
		if move.language == clientcontract.GeneratedLanguageGo {
			byImport[move.importPath] = append(byImport[move.importPath], move)
		}
	}
	if len(byImport) == 0 {
		return nil
	}
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, path, content, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	identifiers := map[string][]bindingSymbolMove{}
	for _, spec := range file.Imports {
		value, unquoteErr := strconv.Unquote(spec.Path.Value)
		candidates, exists := byImport[value]
		if unquoteErr != nil || !exists {
			continue
		}
		identifier := ""
		switch {
		case spec.Name == nil:
			identifier = candidates[0].packageName
		case spec.Name.Name == "." || spec.Name.Name == "_":
			// A dot import erases the qualifier and a blank import binds no
			// symbol: neither can be rewritten by selector.
			continue
		default:
			identifier = spec.Name.Name
		}
		if identifier == "" {
			continue
		}
		identifiers[identifier] = append(identifiers[identifier], candidates...)
	}
	if len(identifiers) == 0 {
		return nil
	}
	var replacements []sourceReplacement
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		for index, move := range identifiers[qualifier.Name] {
			if move.oldSymbol != selector.Sel.Name {
				continue
			}
			replacements = append(replacements, sourceReplacement{
				start: files.Position(selector.Sel.Pos()).Offset,
				end:   files.Position(selector.Sel.End()).Offset,
				value: move.newSymbol, symbolMove: &identifiers[qualifier.Name][index],
			})
			break
		}
		return true
	})
	return replacements
}

var tsNamedImportClause = regexp.MustCompile(`(?s)\b(?:import|export)\s+(?:type\s+)?\{([^}]*)\}\s*from\s*['"]([^'"\r\n]+)['"]`)

// tsBindingSymbolReplacements renames an imported construction and, when the
// specifier carries no local alias, its uses in the same module. It refuses a
// file where the name is also bound locally or appears in object-shorthand
// position, because renaming either would change a meaning the manifest does
// not describe. Refused files stay in the adaptation queue.
func tsBindingSymbolReplacements(content string, moves []bindingSymbolMove) []sourceReplacement {
	byImport := map[string][]bindingSymbolMove{}
	for _, move := range moves {
		if move.language == clientcontract.GeneratedLanguageTypeScript {
			byImport[move.importPath] = append(byImport[move.importPath], move)
		}
	}
	if len(byImport) == 0 {
		return nil
	}
	code := tsCodeMask(content)
	clauses := tsNamedImportClause.FindAllStringSubmatchIndex(content, -1)
	// Every import clause in the module, including other modules', is off
	// limits to a use rename: a clause is rewritten only by the specifier loop
	// that owns it, so a homonym imported from elsewhere keeps its own name.
	clauseSpans := make([][2]int, 0, len(clauses))
	for _, indices := range clauses {
		if len(indices) >= 4 && indices[2] >= 0 {
			clauseSpans = append(clauseSpans, [2]int{indices[2], indices[3]})
		}
	}
	var replacements []sourceReplacement
	for _, indices := range clauses {
		if len(indices) < 6 || indices[0] < 0 || !sourceKeywordAt(code, indices[0]) {
			continue
		}
		candidates, exists := byImport[content[indices[4]:indices[5]]]
		if !exists {
			continue
		}
		clauseStart, clauseEnd := indices[2], indices[3]
		for _, specifier := range tsImportSpecifiers(content[clauseStart:clauseEnd], clauseStart) {
			for index, move := range candidates {
				if move.oldSymbol != specifier.imported {
					continue
				}
				replacements = append(replacements, sourceReplacement{start: specifier.start,
					end: specifier.start + len(specifier.imported), value: move.newSymbol,
					symbolMove: &candidates[index]})
				if specifier.aliased {
					break
				}
				uses, safe := tsLocalIdentifierUses(code, specifier.imported, clauseSpans)
				if !safe {
					// A source that cannot be fully adopted is left entirely to
					// the queue. Renaming one construction and refusing its
					// neighbor would leave the module importing a symbol the
					// new package no longer exports.
					return nil
				}
				for _, offset := range uses {
					replacements = append(replacements, sourceReplacement{start: offset,
						end: offset + len(specifier.imported), value: move.newSymbol,
						symbolMove: &candidates[index]})
				}
				break
			}
		}
	}
	return replacements
}

type tsImportSpecifier struct {
	imported string
	start    int
	aliased  bool
}

func tsImportSpecifiers(clause string, offset int) []tsImportSpecifier {
	var specifiers []tsImportSpecifier
	position := 0
	for _, raw := range strings.Split(clause, ",") {
		segment := raw
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(segment), "type "))
		if len(fields) == 1 || len(fields) == 3 && fields[1] == "as" {
			imported := fields[0]
			specifiers = append(specifiers, tsImportSpecifier{imported: imported,
				start: offset + position + strings.Index(segment, imported), aliased: len(fields) == 3})
		}
		position += len(raw) + 1
	}
	return specifiers
}

// tsLocalIdentifierUses returns every code occurrence of name outside any
// import clause. It reports safe=false when the name is also declared locally
// or is used in object-shorthand position, where a rename would silently
// change a property key or capture a different binding.
func tsLocalIdentifierUses(code, name string, clauseSpans [][2]int) ([]int, bool) {
	if tsIdentifierShadowed(code, name) {
		return nil, false
	}
	pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	var uses []int
	for _, index := range pattern.FindAllStringIndex(code, -1) {
		if withinSpans(index[0], clauseSpans) {
			continue
		}
		if memberAccessAt(code, index[0]) || tsObjectShorthandAt(code, index[0], index[1]) {
			return nil, false
		}
		uses = append(uses, index[0])
	}
	return uses, true
}

func withinSpans(offset int, spans [][2]int) bool {
	for _, span := range spans {
		if offset >= span[0] && offset < span[1] {
			return true
		}
	}
	return false
}

func tsObjectShorthandAt(code string, start, end int) bool {
	before := byte(0)
	for index := start - 1; index >= 0; index-- {
		if code[index] != ' ' && code[index] != '\t' && code[index] != '\r' && code[index] != '\n' {
			before = code[index]
			break
		}
	}
	after := byte(0)
	for index := end; index < len(code); index++ {
		if code[index] != ' ' && code[index] != '\t' && code[index] != '\r' && code[index] != '\n' {
			after = code[index]
			break
		}
	}
	return (before == '{' || before == ',') && (after == '}' || after == ',')
}
