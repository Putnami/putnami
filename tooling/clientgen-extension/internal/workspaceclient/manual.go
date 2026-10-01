package workspaceclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	diag "go.putnami.dev/protocol/diagnostic"
)

type sourceRecord struct {
	path     string
	project  string
	language string
	content  string
	// code is the TypeScript comment/string/JSX mask of content, computed once
	// by the scan. Masking is not cheap and every consumer of a TypeScript
	// record needs the same mask, so the record carries it instead of each
	// consumer recomputing it. It is empty for Go records.
	code string
	// imports is the file's own import set: decoded Go import paths, or
	// TypeScript module specifiers of the named and namespace import forms this
	// package understands. It is the exact evidence that decides whether a file
	// can reference a module at all, so a caller can skip a file no import
	// makes relevant without weakening what the scanner would have concluded.
	imports []string
	calls   []detectedTransport
}

// importsModule reports whether this file imports the exact module path.
//
// Both scanners answer "does this file use symbol S from module M" by parsing
// or masking the whole file. Neither can answer yes for a file that does not
// import M, so this exact, already-collected fact is a sound gate in front of
// that work rather than a heuristic that could hide a usage.
func (r sourceRecord) importsModule(path string) bool {
	for _, value := range r.imports {
		if value == path {
			return true
		}
	}
	return false
}

type detectedTransport struct {
	callsite   TransportCallsite
	evidence   string
	expression string
}

// scanManualClients classifies every transport callsite the workspace scan
// found. It receives the records rather than scanning for them, because the
// consumer-edge scan needs the identical set and the scan is the check's
// dominant cost (see inspect).
func scanManualClients(workspaceRoot string, providers []provider, records []sourceRecord, report Report) ([]Adaptation, []Finding) {
	externalCallsites := map[string]bool{}
	for _, contract := range report.ExternalContracts {
		for _, callsite := range contract.Callsites {
			externalCallsites[externalCallsiteKey(callsite)] = true
		}
	}
	frameworkCallsites := map[string]bool{}
	for _, transport := range report.FrameworkTransports {
		for _, callsite := range transport.Callsites {
			frameworkCallsites[externalCallsiteKey(callsite)] = true
		}
	}
	// Service configuration and transport construction routinely live in
	// separate files. Aggregate references at Putnami project scope before
	// classifying call sites so variable endpoints do not evade the guard.
	projectReferences := map[string]map[string][]string{}
	for _, record := range records {
		for _, provider := range providers {
			if provider.classification != ClassificationFirstParty || provider.document == nil {
				continue
			}
			refs := configuredFirstPartyReferences(record.content, provider)
			if len(refs) == 0 {
				continue
			}
			if projectReferences[record.project] == nil {
				projectReferences[record.project] = map[string][]string{}
			}
			serviceID := provider.document.Service.ID
			projectReferences[record.project][serviceID] = append(projectReferences[record.project][serviceID], refs...)
		}
	}

	var adaptations []Adaptation
	var findings []Finding
	usedExternalCallsites := map[string]bool{}
	usedFrameworkCallsites := map[string]bool{}
	for _, record := range records {
		for _, call := range record.calls {
			matched := false
			for _, provider := range providers {
				if provider.classification != ClassificationFirstParty || provider.document == nil {
					continue
				}
				serviceID := provider.document.Service.ID
				serviceEvidence := literalFirstPartyReferences(call.expression, provider)
				if len(serviceEvidence) == 0 {
					continue
				}
				matched = true
				evidence := append([]string{call.evidence}, serviceEvidence...)
				sort.Strings(evidence)
				bindings := adaptationBindings(report, record.language, serviceID)
				adaptations = append(adaptations, manualAdaptation(record, call, serviceID, uniqueStrings(evidence), bindings,
					"a typed binding must replace this first-party transport; argument and credential flow need source-aware migration"))
				findings = append(findings, manualFinding("clientgen.handwritten-first-party-client", call.callsite, serviceID,
					"handwritten first-party transport to "+serviceID+" must be replaced by the generated binding: "+
						bindingAdvice(report, record.project, record.language, serviceID, bindings)))
			}
			if matched {
				continue
			}
			// An exact-callsite entry outranks the weaker associations below —
			// a service identity that merely appears somewhere in the project,
			// or a dependency edge — but never the literal reference above,
			// which is the transport expression naming the service itself.
			claimKey := externalCallsiteKey(externalIdentity(call.callsite))
			if frameworkCallsites[claimKey] {
				usedFrameworkCallsites[claimKey] = true
				continue
			}
			if externalCallsites[claimKey] {
				usedExternalCallsites[claimKey] = true
				continue
			}
			for _, provider := range providers {
				if provider.classification != ClassificationFirstParty || provider.document == nil {
					continue
				}
				serviceID := provider.document.Service.ID
				serviceEvidence := projectReferences[record.project][serviceID]
				if len(serviceEvidence) == 0 {
					continue
				}
				matched = true
				evidence := append([]string{call.evidence}, serviceEvidence...)
				sort.Strings(evidence)
				bindings := adaptationBindings(report, record.language, serviceID)
				adaptations = append(adaptations, manualAdaptation(record, call, serviceID, uniqueStrings(evidence), bindings,
					"a project-scoped service reference associates this transport with a first-party provider"))
				findings = append(findings, manualFinding("clientgen.handwritten-first-party-client", call.callsite, serviceID,
					"handwritten first-party transport to "+serviceID+" must be replaced by the generated binding: "+
						bindingAdvice(report, record.project, record.language, serviceID, bindings)))
			}
			if matched {
				continue
			}
			candidates := projectFirstPartyCandidates(workspaceRoot, record.project, providers, report)
			if len(candidates) == 1 {
				serviceID := candidates[0]
				bindings := adaptationBindings(report, record.language, serviceID)
				adaptations = append(adaptations, manualAdaptation(record, call, serviceID, []string{call.evidence}, bindings,
					"the project depends on this first-party provider, but the dynamic endpoint needs source-aware migration"))
				findings = append(findings, manualFinding("clientgen.handwritten-first-party-client", call.callsite, serviceID,
					"opaque transport in a consumer of "+serviceID+" must use its generated binding: "+
						bindingAdvice(report, record.project, record.language, serviceID, bindings)))
				continue
			}
			if len(candidates) > 1 {
				adaptations = append(adaptations, manualAdaptation(record, call, "", []string{call.evidence},
					adaptationBindingsForServices(report, record.language, candidates),
					"the endpoint is dynamic and the project has several first-party providers; select the generated binding explicitly"))
				findings = append(findings, manualFinding("clientgen.unclassified-handwritten-transport", call.callsite, "",
					"opaque transport in a multi-provider consumer must be classified as a generated binding or explicit external callsite; "+
						"first-party candidates: "+strings.Join(candidates, ", ")))
				continue
			}
			adaptations = append(adaptations, manualAdaptation(record, call, "", []string{call.evidence}, nil,
				"no generated binding or external authority owns this transport callsite"))
			findings = append(findings, manualFinding("clientgen.unclassified-transport", call.callsite, "",
				"transport callsite must be associated with a generated binding or an explicit external authority"))
		}
	}
	for claimKey := range externalCallsites {
		if !usedExternalCallsites[claimKey] {
			findings = append(findings, Finding{Code: "clientgen.stale-external-callsite", Path: externalCallsitePath(claimKey),
				Message: "external inventory callsite does not match a detected production transport expression"})
		}
	}
	for claimKey := range frameworkCallsites {
		if !usedFrameworkCallsites[claimKey] {
			findings = append(findings, Finding{Code: "clientgen.stale-framework-callsite", Path: externalCallsitePath(claimKey),
				Message: "framework inventory callsite does not match a detected production transport expression"})
		}
	}
	return adaptations, findings
}

func externalIdentity(callsite TransportCallsite) ExternalCallsite {
	return ExternalCallsite{Path: callsite.Path, Transport: callsite.Transport,
		Symbol: callsite.Symbol, Fingerprint: callsite.Fingerprint}
}

func manualAdaptation(record sourceRecord, call detectedTransport, serviceID string, evidence []string,
	bindings []AdaptationBinding, reason string,
) Adaptation {
	return Adaptation{
		Path: record.path, Language: record.language, ServiceID: serviceID, Callsite: &call.callsite,
		Evidence: evidence, Bindings: bindings, Disposition: "manual", Reason: reason,
	}
}

func manualFinding(code string, callsite TransportCallsite, serviceID, message string) Finding {
	return Finding{Code: code, Path: callsite.Path, Line: callsite.Line, Column: callsite.Column,
		ServiceID: serviceID, Message: fmt.Sprintf("%s (transport=%s symbol=%s fingerprint=%s)",
			message, callsite.Transport, callsite.Symbol, callsite.Fingerprint)}
}

func externalCallsitePath(key string) string {
	path, _, _ := strings.Cut(key, "\x00")
	return path
}

func projectFirstPartyCandidates(workspaceRoot, project string, providers []provider, report Report) []string {
	dependencies := projectDependencies(workspaceRoot, project)
	candidates := map[string]bool{}
	for _, item := range providers {
		if item.classification != ClassificationFirstParty || item.document == nil {
			continue
		}
		providerPath := "/" + strings.TrimPrefix(item.rel, "/")
		if project == item.rel || dependencies[providerPath] || dependencies[item.rel] {
			candidates[item.document.Service.ID] = true
		}
	}
	for _, edge := range report.ConsumerEdges {
		if edge.ConsumerProject == project {
			candidates[edge.ServiceID] = true
		}
	}
	result := make([]string, 0, len(candidates))
	for serviceID := range candidates {
		result = append(result, serviceID)
	}
	sort.Strings(result)
	return result
}

func projectDependencies(workspaceRoot, project string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(project), "putnami.json")) //nolint:gosec // indexed project metadata
	if err != nil {
		return nil
	}
	var metadata struct {
		Dependencies []string `json:"dependencies"`
	}
	if json.Unmarshal(data, &metadata) != nil {
		return nil
	}
	result := make(map[string]bool, len(metadata.Dependencies))
	for _, dependency := range metadata.Dependencies {
		result[dependency] = true
	}
	return result
}

func adaptationBindingsForServices(report Report, language string, serviceIDs []string) []AdaptationBinding {
	bindings := make([]AdaptationBinding, 0, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		bindings = append(bindings, adaptationBindings(report, language, serviceID)...)
	}
	return bindings
}

func adaptationBindings(report Report, language, serviceID string) []AdaptationBinding {
	var bindings []AdaptationBinding
	for _, provider := range report.Providers {
		if provider.Classification != ClassificationFirstParty || provider.ServiceID != serviceID {
			continue
		}
		for _, target := range provider.Targets {
			if string(target.Language) != language || target.Binding == nil {
				continue
			}
			for _, binding := range target.Binding.Clients {
				bindings = append(bindings, AdaptationBinding{
					ProviderProject: provider.Project,
					ImportPath:      target.Binding.ImportPath,
					Service:         binding.Service,
					ClientSymbol:    binding.ClientSymbol,
					BindingSymbol:   binding.BindingSymbol,
				})
			}
		}
	}
	sort.Slice(bindings, func(i, j int) bool {
		left, right := bindings[i], bindings[j]
		leftKey := left.ProviderProject + "\x00" + left.ImportPath + "\x00" + left.Service + "\x00" + left.ClientSymbol
		rightKey := right.ProviderProject + "\x00" + right.ImportPath + "\x00" + right.Service + "\x00" + right.ClientSymbol
		return leftKey < rightKey
	})
	return bindings
}

// bindingAdvice is the sentence a failing handwritten-transport diagnostic
// carries so the developer reads what to call instead of the report. The
// report's adaptation entry holds the same bindings, but the SDK drops a job's
// data payload when the job fails — which is exactly when the guard finds a
// handwritten transport — so the diagnostic text is the only channel that
// reaches the developer.
func bindingAdvice(report Report, consumerProject, language, serviceID string, bindings []AdaptationBinding) string {
	if len(bindings) > 0 {
		advice := make([]string, 0, len(bindings))
		for _, binding := range bindings {
			advice = append(advice, fmt.Sprintf("import %s and call %s (%s)", binding.ImportPath, binding.BindingSymbol, binding.ClientSymbol))
		}
		return strings.Join(advice, ", or ")
	}
	for _, provider := range report.Providers {
		if provider.Classification != ClassificationFirstParty || provider.ServiceID != serviceID {
			continue
		}
		if provider.EmptyContract {
			// No target of this provider can ever carry this call: its contract
			// declares no first-party operation, so there is no binding to
			// regenerate and none to declare. The only honest instruction is
			// the one the inventory exists for (ADR 0004 decision 5).
			return fmt.Sprintf("%s declares no first-party operation, so no %s client is generated for it; "+
				"classify this callsite in %s under the authority that owns the wire",
				serviceID, languageName(language), projectInventoryPath(consumerProject, externalInventoryFile))
		}
		for _, target := range provider.Targets {
			if string(target.Language) == language {
				return fmt.Sprintf("the %s target of %s has no committed client manifest at %s; regenerate the client",
					languageName(language), serviceID, target.Manifest)
			}
		}
		return fmt.Sprintf("%s generates no %s client yet; declare the target on the provider %s",
			serviceID, languageName(language), provider.Project)
	}
	return fmt.Sprintf("%s generates no %s client yet; declare the target on the provider", serviceID, languageName(language))
}

// languageName spells a generated-language code the way the diagnostic reads.
func languageName(language string) string {
	switch clientcontract.GeneratedLanguage(language) {
	case clientcontract.GeneratedLanguageGo:
		return "Go"
	case clientcontract.GeneratedLanguageTypeScript:
		return "TypeScript"
	}
	return language
}

func generatedSourceInventory(workspaceRoot string, report Report) map[string]bool {
	generated := map[string]bool{}
	for _, providerReport := range report.Providers {
		for _, target := range providerReport.Targets {
			data, err := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(target.Manifest))) //nolint:gosec
			if err != nil {
				continue
			}
			manifest, valid := parseManifestWithoutDiagnostics(data)
			base := filepath.ToSlash(filepath.Dir(target.Manifest))
			if !valid || !manifestFilesMatch(workspaceRoot, base, manifest.Files) {
				continue
			}
			for _, file := range manifest.Files {
				generated[joinRel(base, file.Path)] = true
			}
		}
	}
	return generated
}

func manifestFilesMatch(workspaceRoot, base string, files []clientcontract.GeneratedFile) bool {
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(joinRel(base, file.Path)))) //nolint:gosec // validated generated manifest path
		if err != nil {
			return false
		}
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != file.SHA256 {
			return false
		}
	}
	return true
}

// workspaceSourceRecords is the one production-source scan a check performs. It
// derives the generated-file exclusion set from the report's providers, which is
// the only report field discoverSourceRecords depends on.
func workspaceSourceRecords(workspaceRoot string, report Report) ([]sourceRecord, error) {
	return discoverSourceRecords(workspaceRoot, generatedSourceInventory(workspaceRoot, report))
}

func discoverSourceRecords(workspaceRoot string, generated map[string]bool) ([]sourceRecord, error) {
	projects, err := indexedProjectPaths(workspaceRoot)
	if err != nil {
		return nil, err
	}
	var records []sourceRecord
	seen := map[string]bool{}
	for _, project := range projects {
		projectRoot := filepath.Join(workspaceRoot, filepath.FromSlash(project))
		walkErr := filepath.WalkDir(projectRoot, func(path string, entry fs.DirEntry, pathErr error) error {
			if pathErr != nil {
				return pathErr
			}
			if entry.IsDir() {
				if path != projectRoot && excludedProductionDirectory(entry.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			rel, relErr := filepath.Rel(workspaceRoot, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			if seen[rel] || sourceProjectFromIndex(rel, projects) != project || generated[rel] || excludedManualScanPath(rel) {
				return nil
			}
			seen[rel] = true
			language, supported := sourceLanguage(rel)
			if !supported {
				return nil
			}
			content, readErr := os.ReadFile(path) //nolint:gosec // indexed project production source
			if readErr != nil {
				return readErr
			}
			text := string(content)
			record := sourceRecord{path: rel, project: project, language: language, content: text}
			if language == "ts" {
				record.code = tsCodeMask(text)
				record.calls, record.imports = tsScanSource(rel, text, record.code)
			} else {
				record.imports = goImportPaths(path, content)
				// A file that imports no transport package cannot produce a
				// transport callsite: every branch of goTransportCallsites is
				// gated on an alias or dot-import of one. Deciding that from
				// the import block alone — which the real Go parser reads and
				// then stops — skips the full parse of the ~90 % of Go files
				// that could never have matched, and skips nothing the full
				// parse would have found.
				if goImportsTransport(record.imports) {
					record.calls = goTransportCallsites(rel, path, content)
				}
			}
			records = append(records, record)
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].path < records[j].path })
	return records, nil
}

func excludedProductionDirectory(name string) bool {
	switch strings.ToLower(name) {
	case ".context", ".git", ".gen", ".putnami", ".cache", "build", "coverage", "dist", "node_modules", "out", "tmp", "vendor":
		return true
	default:
		return false
	}
}

func sourceProjectFromIndex(path string, projects []string) string {
	owner := ""
	for _, project := range projects {
		if project == "." || path == project || strings.HasPrefix(path, strings.TrimSuffix(project, "/")+"/") {
			if owner == "" || len(project) > len(owner) {
				owner = project
			}
		}
	}
	return owner
}

func goTransportEvidence(path string, content []byte) []string {
	calls := goTransportCallsites(filepath.ToSlash(path), path, content)
	evidence := make([]string, 0, len(calls))
	for _, call := range calls {
		evidence = append(evidence, call.evidence)
	}
	sort.Strings(evidence)
	return uniqueStrings(evidence)
}

// goImportPaths returns the decoded import paths of a Go file, reading only its
// import block. A header that does not parse yields no paths, which is what a
// full parse of the same file would also conclude: the parser reports the same
// error either way.
func goImportPaths(path string, content []byte) []string {
	file, err := parser.ParseFile(token.NewFileSet(), path, content, parser.ImportsOnly|parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		value, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			continue
		}
		paths = append(paths, value)
	}
	return paths
}

// goImportsTransport reports whether any import path classifies as a transport
// package, using the same classifier the callsite scanner uses.
func goImportsTransport(paths []string) bool {
	for _, path := range paths {
		if goTransportImport(path) != "" {
			return true
		}
	}
	return false
}

func goTransportCallsites(relative, path string, content []byte) []detectedTransport {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, content, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	aliases := map[string]string{}
	dotImports := map[string]bool{}
	for _, spec := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			continue
		}
		kind := goTransportImport(importPath)
		if kind == "" {
			continue
		}
		if spec.Name != nil && spec.Name.Name == "." {
			dotImports[kind] = true
			continue
		}
		alias := filepath.Base(importPath)
		if spec.Name != nil && spec.Name.Name != "_" {
			alias = spec.Name.Name
		}
		aliases[alias] = kind
	}
	var calls []detectedTransport
	callOrdinals := map[string]int{}
	appendCall := func(node ast.Node, kind, evidence string) {
		position := fileSet.Position(node.Pos())
		expression := goCallExpression(fileSet, node)
		ordinalKey := kind + "\x00" + evidence + "\x00" + expression
		ordinal := callOrdinals[ordinalKey]
		callOrdinals[ordinalKey]++
		calls = append(calls, detectedTransport{callsite: TransportCallsite{
			Path: relative, Line: position.Line, Column: position.Column, Transport: kind,
			Symbol: evidence, Fingerprint: sourceCallsiteFingerprint(callsiteBindingContext(string(content), expression), expression, ordinal),
		}, evidence: evidence, expression: expression})
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.CallExpr:
			switch fn := value.Fun.(type) {
			case *ast.SelectorExpr:
				if ident, ok := fn.X.(*ast.Ident); ok && goTransportMethod(aliases[ident.Name], fn.Sel.Name) {
					appendCall(value, aliases[ident.Name], ident.Name+"."+fn.Sel.Name)
				}
				if owner, ok := fn.X.(*ast.SelectorExpr); ok {
					if ident, identOK := owner.X.(*ast.Ident); identOK &&
						((aliases[ident.Name] == "http" && owner.Sel.Name == "DefaultClient" && fn.Sel.Name == "Do") ||
							(aliases[ident.Name] == "websocket" && owner.Sel.Name == "DefaultDialer" && (fn.Sel.Name == "Dial" || fn.Sel.Name == "DialContext"))) {
						appendCall(value, aliases[ident.Name], ident.Name+"."+owner.Sel.Name+"."+fn.Sel.Name)
					}
				}
			case *ast.Ident:
				for kind := range dotImports {
					if goTransportMethod(kind, fn.Name) {
						appendCall(value, kind, fn.Name)
					}
				}
			}
		case *ast.CompositeLit:
			selector, ok := value.Type.(*ast.SelectorExpr)
			if !ok {
				break
			}
			if ident, identOK := selector.X.(*ast.Ident); identOK {
				kind := aliases[ident.Name]
				if kind == "putnami-client" && selector.Sel.Name == "Request" ||
					kind == "http" && (selector.Sel.Name == "Client" || selector.Sel.Name == "Transport" || selector.Sel.Name == "Request") {
					appendCall(value, kind, ident.Name+"."+selector.Sel.Name)
				}
			}
		}
		return true
	})
	sortDetectedTransports(calls)
	return uniqueDetectedTransports(calls)
}

func goCallExpression(fileSet *token.FileSet, node ast.Node) string {
	var canonical bytes.Buffer
	if err := format.Node(&canonical, fileSet, node); err != nil {
		return fmt.Sprintf("%T", node)
	}
	return canonical.String()
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// sourceCallsiteFingerprint identifies one transport expression by what the
// expression means, not by the file it sits in. It hashes the normalized
// expression, its ordinal among identical expressions, and the declarations
// inside the file of every identifier the expression uses.
//
// The declarations are why an authority cannot be repointed: rewriting
// `const url = process.env.VENDOR_URL` to a first-party host changes the
// fingerprint of `fetch(url)` and revokes the entry. Hashing the whole file
// did that too, but it also revoked the entry on any unrelated edit anywhere
// in the file, which made every inventory entry churn.
func sourceCallsiteFingerprint(bindings, expression string, ordinal int) string {
	return sha256Hex([]byte(bindings + "\x00" + expression + "\x00" + strconv.Itoa(ordinal)))
}

// compiledPatterns memoizes the regular expressions this scanner derives from
// identifier and module names. The scanner asks the same question of every
// source file in the workspace, so without the memo a workspace paid one
// compilation per (file x pattern) pair. A compiled expression is immutable and
// a pattern string determines it, so reuse changes nothing a caller can observe.
var compiledPatterns sync.Map

func cachedPattern(expression string) *regexp.Regexp {
	if cached, found := compiledPatterns.Load(expression); found {
		if compiled, ok := cached.(*regexp.Regexp); ok {
			return compiled
		}
	}
	compiled := regexp.MustCompile(expression)
	compiledPatterns.Store(expression, compiled)
	return compiled
}

var (
	sourceIdentifier    = regexp.MustCompile(`[A-Za-z_$][A-Za-z0-9_$]*`)
	sourceBindingForms  = []string{`\b(?:const|let|var|func|function|class|type)\s+%s\b`, `\b%s\s*(?::=|=[^=])`}
	nonBindingIdentifer = map[string]bool{
		// The blank identifier binds nothing. Taking it as a name made every
		// `x, _ :=` statement of the file part of the fingerprint of a callsite
		// such as `func(_ *http.Request)`, so an unrelated edit revoked it.
		"_":     true,
		"await": true, "new": true, "return": true, "this": true, "typeof": true,
		"globalThis": true, "window": true, "http": true, "nil": true, "null": true, "undefined": true,
	}
)

// callsiteBindingContext returns the normalized declaration statements, inside
// this file, of every identifier the transport expression names. An identifier
// declared elsewhere contributes nothing: cross-file service identity is what
// the project-scoped association already covers.
func callsiteBindingContext(source, expression string) string {
	clean := commentFreeSource(source)
	lines := strings.Split(clean, "\n")
	seen := map[string]bool{}
	var statements []string
	var candidates []string
	for _, name := range sourceIdentifier.FindAllString(expression, -1) {
		if seen[name] || nonBindingIdentifer[name] {
			continue
		}
		seen[name] = true
		// Every declaration form embeds the identifier, so a line that does
		// not contain it cannot match any of them. Selecting those lines once
		// replaces one regular-expression match per (form x line) with one
		// substring scan per line, and reaches the same statements: this runs
		// for every transport callsite in the workspace, and a large file's
		// line count multiplied by its callsite count is what made the scan
		// the dominant cost of the workspace guard.
		candidates = candidates[:0]
		for _, line := range lines {
			if strings.Contains(line, name) {
				candidates = append(candidates, line)
			}
		}
		if len(candidates) == 0 {
			continue
		}
		for _, form := range sourceBindingForms {
			pattern := cachedPattern(fmt.Sprintf(form, regexp.QuoteMeta(name)))
			for _, line := range candidates {
				if pattern.MatchString(line) {
					statements = append(statements, normalizeSourceExpression(line))
				}
			}
		}
	}
	sort.Strings(statements)
	return strings.Join(uniqueStrings(statements), "\x00")
}

func goTransportImport(path string) string {
	switch {
	case path == "net/http":
		return "http"
	case path == "go.putnami.dev/client":
		return "putnami-client"
	case strings.Contains(path, "connectrpc.com/connect") || strings.HasSuffix(path, "/connect"):
		return "connect"
	case strings.Contains(path, "websocket"):
		return "websocket"
	default:
		return ""
	}
}

func goTransportMethod(kind, method string) bool {
	switch kind {
	case "http":
		return method == "Get" || method == "Head" || method == "Post" || method == "PostForm" ||
			method == "NewRequest" || method == "NewRequestWithContext"
	case "putnami-client", "connect":
		return method == "New" || method == "NewClient" || method == "NewBuilder"
	case "websocket":
		return method == "Dial" || method == "DialContext"
	default:
		return false
	}
}

var (
	tsNamedImport     = regexp.MustCompile(`(?s)import\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]`)
	tsNamespaceImport = regexp.MustCompile(`import\s*\*\s*as\s*([A-Za-z_$][\w$]*)\s*from\s*['"]([^'"]+)['"]`)
)

func tsTransportEvidence(content string) []string {
	calls := tsTransportCallsites("source.ts", content)
	evidence := make([]string, 0, len(calls))
	for _, call := range calls {
		evidence = append(evidence, call.evidence)
	}
	sort.Strings(evidence)
	return uniqueStrings(evidence)
}

type tsImportedTransport struct {
	imported string
	kind     string
}

func tsTransportCallsites(relative, content string) []detectedTransport {
	calls, _ := tsScanSource(relative, content, tsCodeMask(content))
	return calls
}

// tsScanSource answers both TypeScript questions the workspace scan asks of a
// file, in one pass over one masked source: which transport callsites the file
// contains, and which modules it imports. Both answers read the same mask and
// the same import matches, so deriving them separately scanned every source in
// the workspace twice for nothing.
func tsScanSource(relative, content, code string) ([]detectedTransport, []string) {
	named := tsImportMatches(tsNamedImport, content, code)
	namespace := tsImportMatches(tsNamespaceImport, content, code)
	modules := make([]string, 0, len(named)+len(namespace))
	for _, match := range named {
		modules = append(modules, match[2])
	}
	for _, match := range namespace {
		modules = append(modules, match[2])
	}
	locals := map[string]tsImportedTransport{}
	for _, match := range named {
		kind := tsTransportModuleKind(match[2])
		if kind == "" {
			continue
		}
		for _, raw := range strings.Split(match[1], ",") {
			parts := strings.Fields(strings.TrimSpace(raw))
			if len(parts) == 1 {
				locals[parts[0]] = tsImportedTransport{imported: parts[0], kind: kind}
			} else if len(parts) == 3 && parts[1] == "as" {
				locals[parts[2]] = tsImportedTransport{imported: parts[0], kind: kind}
			}
		}
	}
	for _, match := range namespace {
		if kind := tsTransportModuleKind(match[2]); kind != "" {
			locals[match[1]] = tsImportedTransport{imported: "namespace", kind: kind}
		}
	}
	calls := tsGlobalTransportCallsites(relative, content, code)
	calls = append(calls, tsGlobalTransportReferences(relative, content, code)...)
	for local, imported := range locals {
		if imported.imported == "namespace" {
			for _, method := range []string{"createClient", "createPromiseClient", "createConnectTransport"} {
				calls = append(calls, tsPatternCallsites(relative, content, code, tsCallPattern(local+"."+method, false),
					local, imported.kind, local+"."+method)...)
			}
			continue
		}
		if imported.kind == "putnami-client" && imported.imported == "BaseClient" {
			calls = append(calls, tsPatternCallsites(relative, content, code, tsCallPattern(local, true), local, imported.kind, local)...)
		}
		if imported.kind == "putnami-client" && imported.imported == "ClientBuilder" {
			calls = append(calls, tsPatternCallsites(relative, content, code, tsCallPattern(local+".for", false), local, imported.kind, local)...)
		}
		if imported.kind == "connect" &&
			(imported.imported == "createClient" || imported.imported == "createPromiseClient" || imported.imported == "createConnectTransport") {
			calls = append(calls, tsPatternCallsites(relative, content, code, tsCallPattern(local, false), local, imported.kind, local)...)
		}
		if imported.kind == "websocket" && imported.imported == "WebSocket" {
			calls = append(calls, tsPatternCallsites(relative, content, code, tsCallPattern(local, true), local, imported.kind, local)...)
		}
		if imported.kind == "sse" && imported.imported == "EventSource" {
			calls = append(calls, tsPatternCallsites(relative, content, code, tsCallPattern(local, true), local, imported.kind, local)...)
		}
	}
	sortDetectedTransports(calls)
	return uniqueDetectedTransports(calls), modules
}

func tsGlobalTransportCallsites(relative, content, code string) []detectedTransport {
	types := []struct {
		name        string
		kind        string
		constructor bool
	}{
		{name: "fetch", kind: "http"},
		{name: "WebSocket", kind: "websocket", constructor: true},
		{name: "EventSource", kind: "sse", constructor: true},
	}
	var calls []detectedTransport
	for _, transport := range types {
		// Every pattern below — the property access, the shadowing forms and
		// the bare call — embeds the transport's own name, so a masked source
		// that does not contain it matches none of them. One substring scan
		// replaces six expression scans of the whole file, for the large
		// majority of a workspace's sources that name no transport at all.
		if !strings.Contains(code, transport.name) {
			continue
		}
		calls = append(calls, tsPatternCallsites(relative, content, code,
			tsCallPattern(`(?:globalThis|window)\s*\.\s*`+transport.name, transport.constructor),
			transport.name, transport.kind, transport.name)...)
		if !tsIdentifierShadowed(code, transport.name) {
			calls = append(calls, tsPatternCallsites(relative, content, code, tsCallPattern(transport.name, transport.constructor),
				transport.name, transport.kind, transport.name)...)
		}
	}
	if !tsMentionsGlobalScope(code) {
		return calls
	}

	for _, match := range tsGlobalTransportAlias.FindAllStringSubmatch(code, -1) {
		calls = append(calls, tsPatternCallsites(relative, content, code, tsCallPattern(match[1], match[2] != "fetch"),
			match[1], tsGlobalTransportKind(match[2]), match[1])...)
	}
	for _, match := range tsGlobalTransportDestructure.FindAllStringSubmatch(code, -1) {
		for _, raw := range strings.Split(match[1], ",") {
			parts := strings.Split(strings.TrimSpace(raw), ":")
			imported := strings.TrimSpace(parts[0])
			if imported != "fetch" && imported != "WebSocket" && imported != "EventSource" {
				continue
			}
			local := imported
			if len(parts) == 2 {
				local = strings.TrimSpace(parts[1])
			}
			if tsIdentifierName.MatchString(local) {
				calls = append(calls, tsPatternCallsites(relative, content, code, tsCallPattern(local, imported != "fetch"),
					local, tsGlobalTransportKind(imported), local)...)
			}
		}
	}
	return calls
}

func tsPatternCallsites(relative, content, code string, pattern *regexp.Regexp, identifier, kind, evidence string) []detectedTransport {
	matches := make([][]int, 0, 4)
	for _, index := range pattern.FindAllStringIndex(code, -1) {
		if memberAccessAt(code, index[0]) || declarationKeywordAt(code, index[0]) {
			continue
		}
		matches = append(matches, index)
	}
	calls := make([]detectedTransport, 0, len(matches))
	for ordinal, index := range matches {
		segment := code[index[0]:index[1]]
		identifierOffset := strings.LastIndex(segment, identifier)
		if identifierOffset < 0 {
			identifierOffset = 0
		}
		identifierStart := index[0] + identifierOffset
		line, column := sourceLineColumn(code, identifierStart)
		end := tsCallExpressionEnd(code, index[0], index[1])
		expression := normalizeSourceExpression(content[identifierStart:end])
		fingerprint := sourceCallsiteFingerprint(callsiteBindingContext(content, expression), expression, ordinal)
		calls = append(calls, detectedTransport{callsite: TransportCallsite{
			Path: relative, Line: line, Column: column, Transport: kind, Symbol: evidence, Fingerprint: fingerprint,
		}, evidence: evidence, expression: expression})
	}
	return calls
}

// memberAccessAt reports whether the matched expression is reached through a
// property access. The global transports are only reachable as a bare binding
// or through `globalThis`/`window`, so `this.fetch(sql)` on a repository and
// `cache.fetch(loader)` on a memoizer are not transports. Detection of the
// real global binding is not weakened: globalThis and window keep their own
// patterns, and a value taken from them is recorded by
// tsGlobalTransportReferences.
func memberAccessAt(code string, start int) bool {
	for index := start - 1; index >= 0; index-- {
		switch code[index] {
		case ' ', '\t', '\r', '\n':
			continue
		case '.':
			return true
		default:
			return false
		}
	}
	return false
}

// declarationKeywordAt reports whether the match is a method or function
// declaration rather than a call. `private async fetch(sql, only)` declares a
// repository helper; it opens no transport.
func declarationKeywordAt(code string, start int) bool {
	end := start
	for end > 0 && (code[end-1] == ' ' || code[end-1] == '\t') {
		end--
	}
	wordStart := end
	for wordStart > 0 && (isIdentifierByte(code[wordStart-1])) {
		wordStart--
	}
	if wordStart == end {
		return false
	}
	switch code[wordStart:end] {
	case "function", "async", "get", "set", "static", "public", "private", "protected", "readonly", "declare", "abstract", "override":
		return true
	default:
		return false
	}
}

func isIdentifierByte(character byte) bool {
	return character == '_' || character == '$' ||
		character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}

var (
	tsGlobalTransportReference   = regexp.MustCompile(`\b(?:globalThis|window)\s*\.\s*(fetch|WebSocket|EventSource)\b`)
	tsGlobalTransportAlias       = regexp.MustCompile(`\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:globalThis|window)\s*\.\s*(fetch|WebSocket|EventSource)\b`)
	tsGlobalTransportDestructure = regexp.MustCompile(`(?s)\b(?:const|let|var)\s*\{([^}]*)\}\s*=\s*(?:globalThis|window)\b`)
	tsIdentifierName             = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)
)

// tsMentionsGlobalScope reports whether the masked source names the global
// object at all. Every global-transport form that is not a bare call reads it,
// so an absent substring is an exact answer rather than a heuristic.
func tsMentionsGlobalScope(code string) bool {
	return strings.Contains(code, "globalThis") || strings.Contains(code, "window")
}

// tsGlobalTransportReferences records the point where a global transport
// binding is taken as a value — `config.fetch ?? globalThis.fetch`,
// `this.config.webSocket ?? globalThis.WebSocket`. That expression, not every
// later call through the injected field, is the handwritten transport. A
// `typeof globalThis.WebSocket` capability probe binds nothing and is excluded.
func tsGlobalTransportReferences(relative, content, code string) []detectedTransport {
	if !tsMentionsGlobalScope(code) {
		return nil
	}
	var calls []detectedTransport
	ordinals := map[string]int{}
	for _, index := range tsGlobalTransportReference.FindAllStringSubmatchIndex(code, -1) {
		if memberAccessAt(code, index[0]) || typeofOperatorAt(code, index[0]) || callParenthesisAt(code, index[1]) {
			continue
		}
		name := code[index[2]:index[3]]
		line, column := sourceLineColumn(code, index[2])
		expression := normalizeSourceExpression(content[index[0]:index[1]])
		ordinal := ordinals[expression]
		ordinals[expression]++
		calls = append(calls, detectedTransport{callsite: TransportCallsite{
			Path: relative, Line: line, Column: column, Transport: tsGlobalTransportKind(name),
			Symbol: expression, Fingerprint: sourceCallsiteFingerprint(callsiteBindingContext(content, expression), expression, ordinal),
		}, evidence: expression, expression: expression})
	}
	return calls
}

func typeofOperatorAt(code string, start int) bool {
	end := start
	for end > 0 && (code[end-1] == ' ' || code[end-1] == '\t') {
		end--
	}
	return end != start && end >= len("typeof") && code[end-len("typeof"):end] == "typeof"
}

func callParenthesisAt(code string, end int) bool {
	for index := end; index < len(code); index++ {
		switch code[index] {
		case ' ', '\t', '\r', '\n':
			continue
		case '(':
			return true
		default:
			return false
		}
	}
	return false
}

func tsCallExpressionEnd(code string, start, matchedEnd int) int {
	open := strings.LastIndex(code[start:matchedEnd], "(")
	if open < 0 {
		return matchedEnd
	}
	depth := 0
	for index := start + open; index < len(code); index++ {
		switch code[index] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return index + 1
			}
		}
	}
	return matchedEnd
}

func normalizeSourceExpression(expression string) string {
	withoutComments := commentFreeSource(expression)
	var normalized strings.Builder
	quote := byte(0)
	escaped := false
	for index := 0; index < len(withoutComments); index++ {
		character := withoutComments[index]
		if quote == 0 && (character == ' ' || character == '\t' || character == '\r' || character == '\n') {
			continue
		}
		normalized.WriteByte(character)
		if quote != 0 {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == quote {
				quote = 0
			}
		} else if character == '\'' || character == '"' || character == '`' {
			quote = character
		}
	}
	return normalized.String()
}

func sourceLineColumn(content string, offset int) (int, int) {
	line, column := 1, 1
	for i := 0; i < offset && i < len(content); i++ {
		if content[i] == '\n' {
			line, column = line+1, 1
		} else {
			column++
		}
	}
	return line, column
}

func tsGlobalTransportKind(name string) string {
	switch name {
	case "fetch":
		return "http"
	case "WebSocket":
		return "websocket"
	case "EventSource":
		return "sse"
	default:
		return ""
	}
}

func tsCallPattern(expression string, constructor bool) *regexp.Regexp {
	prefix := `\b`
	if constructor {
		prefix = `\bnew\s+`
	}
	return cachedPattern(prefix + expression + `\s*\(`)
}

func tsIdentifierShadowed(code, name string) bool {
	quoted := regexp.QuoteMeta(name)
	patterns := []string{
		`\b(?:const|let|var|function|class)\s+` + quoted + `\b`,
		`\bfunction\s*[A-Za-z_$]*\s*\([^)]*\b` + quoted + `\b[^)]*\)`,
		`\([^)]*\b` + quoted + `\b[^)]*\)\s*=>`,
		`\b` + quoted + `\s*=>`,
	}
	for _, pattern := range patterns {
		if cachedPattern(pattern).MatchString(code) {
			return true
		}
	}
	return false
}

func tsImportMatches(pattern *regexp.Regexp, content, code string) [][]string {
	indices := pattern.FindAllStringSubmatchIndex(content, -1)
	matches := make([][]string, 0, len(indices))
	for _, index := range indices {
		if len(index) < 2 || index[0] < 0 || index[1] > len(code) ||
			!strings.HasPrefix(code[index[0]:index[1]], "import") {
			continue
		}
		match := make([]string, len(index)/2)
		for group := 0; group < len(index)/2; group++ {
			start, end := index[group*2], index[group*2+1]
			if start >= 0 && end >= start {
				match[group] = content[start:end]
			}
		}
		matches = append(matches, match)
	}
	return matches
}

func tsTransportModuleKind(module string) string {
	switch {
	case module == "@putnami/client":
		return "putnami-client"
	case strings.Contains(module, "@connectrpc/connect"):
		return "connect"
	case strings.Contains(strings.ToLower(module), "websocket"):
		return "websocket"
	case strings.Contains(strings.ToLower(module), "eventsource"):
		return "sse"
	default:
		return ""
	}
}

func sortDetectedTransports(calls []detectedTransport) {
	sort.Slice(calls, func(i, j int) bool {
		left, right := calls[i], calls[j]
		return externalCallsiteKey(externalIdentity(left.callsite))+"\x00"+left.evidence <
			externalCallsiteKey(externalIdentity(right.callsite))+"\x00"+right.evidence
	})
}

func uniqueDetectedTransports(calls []detectedTransport) []detectedTransport {
	if len(calls) < 2 {
		return calls
	}
	result := calls[:1]
	for _, call := range calls[1:] {
		last := result[len(result)-1]
		if last.callsite.Path == call.callsite.Path && last.callsite.Line == call.callsite.Line &&
			last.callsite.Column == call.callsite.Column && last.callsite.Transport == call.callsite.Transport {
			continue
		}
		result = append(result, call)
	}
	return result
}

func parseManifestWithoutDiagnostics(data []byte) (*clientcontract.GeneratedClientManifestV1, bool) {
	manifest, diags := clientcontract.ParseAndValidateGeneratedManifest(data)
	return manifest, manifest != nil && !diag.HasErrors(diags)
}

func excludedManualScanPath(path string) bool {
	lower := strings.ToLower(path)
	base := filepath.Base(lower)
	return strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.tsx") || strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, ".spec.tsx") || strings.Contains(lower, "/bench/") ||
		strings.Contains(lower, "/test/") || strings.Contains(lower, "/tests/") ||
		strings.Contains(lower, "/fixtures/")
}

func sourceLanguage(path string) (string, bool) {
	switch filepath.Ext(path) {
	case ".go":
		return "go", true
	case ".ts", ".tsx":
		return "ts", true
	default:
		return "", false
	}
}

func providerReferenceValues(provider provider) []string {
	return uniqueStrings([]string{provider.document.Service.ID, provider.document.Service.Audience})
}

func literalFirstPartyReferences(content string, provider provider) []string {
	literals := sourceStringLiterals(commentFreeSource(content))
	var matches []string
	for _, ref := range providerReferenceValues(provider) {
		if ref == "" {
			continue
		}
		for _, literal := range literals {
			if literal.value == ref {
				matches = append(matches, ref)
				break
			}
		}
	}
	return matches
}

var configurationReferenceSignal = regexp.MustCompile(`(?i)\b(?:audience|base_?url|endpoint|host|origin|service|upstream|url|uri)\b`)

func configuredFirstPartyReferences(content string, provider provider) []string {
	clean := commentFreeSource(content)
	literals := sourceStringLiterals(clean)
	var matches []string
	for _, ref := range providerReferenceValues(provider) {
		if ref == "" {
			continue
		}
		for _, literal := range literals {
			if literal.value != ref {
				continue
			}
			contextStart := max(0, literal.start-128)
			if configurationReferenceSignal.MatchString(clean[contextStart:literal.start]) {
				matches = append(matches, ref)
				break
			}
		}
	}
	return matches
}

type sourceStringLiteral struct {
	start int
	value string
}

func sourceStringLiterals(content string) []sourceStringLiteral {
	var literals []sourceStringLiteral
	for index := 0; index < len(content); {
		quote := content[index]
		if quote != '\'' && quote != '"' && quote != '`' {
			index++
			continue
		}
		start := index
		index++
		var value strings.Builder
		escaped := false
		for index < len(content) {
			character := content[index]
			index++
			if escaped {
				value.WriteByte(character)
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				break
			}
			value.WriteByte(character)
		}
		literals = append(literals, sourceStringLiteral{start: start, value: value.String()})
	}
	return literals
}

// commentFreeSource removes comment text while preserving literals. Service
// identity and route references commonly live in string configuration, so the
// transport scanner needs those bytes, but a comment must never associate an
// otherwise unrelated transport call with a first-party provider.
func commentFreeSource(content string) string {
	var out strings.Builder
	out.Grow(len(content))
	quote := byte(0)
	escaped := false
	for i := 0; i < len(content); {
		ch := content[i]
		if quote != 0 {
			out.WriteByte(ch)
			i++
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			out.WriteByte(ch)
			i++
			continue
		}
		if i+1 < len(content) && ch == '/' && content[i+1] == '/' {
			for i < len(content) && content[i] != '\n' {
				out.WriteByte(' ')
				i++
			}
			continue
		}
		if i+1 < len(content) && ch == '/' && content[i+1] == '*' {
			out.WriteString("  ")
			i += 2
			for i < len(content) {
				if i+1 < len(content) && content[i] == '*' && content[i+1] == '/' {
					out.WriteString("  ")
					i += 2
					break
				}
				if content[i] == '\n' {
					out.WriteByte('\n')
				} else {
					out.WriteByte(' ')
				}
				i++
			}
			continue
		}
		out.WriteByte(ch)
		i++
	}
	return out.String()
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
