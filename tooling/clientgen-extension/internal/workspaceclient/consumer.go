package workspaceclient

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// scanConsumerEdges records which projects register each generated binding. It
// reads the workspace scan's records rather than scanning again: the manual
// classifier needs the identical set, and the scan is what this check spends
// its time on (see inspect).
func scanConsumerEdges(records []sourceRecord, providers []provider, report Report) []ConsumerEdge {
	providerByProject := make(map[string]provider, len(providers))
	for _, item := range providers {
		providerByProject[item.rel] = item
	}

	var edges []ConsumerEdge
	for _, providerReport := range report.Providers {
		item, exists := providerByProject[providerReport.Project]
		if !exists || providerReport.Classification != ClassificationFirstParty || item.document == nil {
			continue
		}
		for _, target := range providerReport.Targets {
			if target.Binding == nil {
				continue
			}
			for _, binding := range target.Binding.Clients {
				consumers := bindingConsumers(records, target.Language, target.Binding.ImportPath, binding.BindingSymbol)
				for _, operation := range target.GeneratedOperations {
					if operation.Service != binding.Service {
						continue
					}
					for _, consumer := range consumers {
						edges = append(edges, ConsumerEdge{
							ProviderProject:   providerReport.Project,
							ServiceID:         item.document.Service.ID,
							OperationID:       operation.OperationID,
							Stream:            operation.Stream,
							Language:          target.Language,
							GeneratedArtifact: target.Manifest,
							ConsumerProject:   consumer,
							BindingImport:     target.Binding.ImportPath,
							ClientSymbol:      binding.ClientSymbol,
							BindingSymbol:     binding.BindingSymbol,
							MethodSymbol:      operation.MethodSymbol,
							AuthProfiles:      operationAuthProfiles(item.security[operation.OperationID]),
							Transports:        append([]clientcontract.Transport(nil), operation.Transports...),
						})
					}
				}
			}
		}
	}
	return edges
}

func bindingConsumers(records []sourceRecord, language clientcontract.GeneratedLanguage, importPath, bindingSymbol string) []string {
	seen := map[string]bool{}
	for _, record := range records {
		// A workspace declares one binding per generated service, and this runs
		// once per binding over every record. Both answers below re-read the
		// whole file, so without this gate the workspace paid one parse or one
		// mask per (file x binding) pair. A file that does not import the
		// generated package cannot register its binding, so the gate removes
		// only work whose answer was already no.
		if record.language != string(language) || !record.importsModule(importPath) {
			continue
		}
		used := false
		if language == clientcontract.GeneratedLanguageGo {
			used = goUsesBinding(record.path, []byte(record.content), importPath, bindingSymbol)
		} else {
			used = tsUsesBinding(record.content, record.code, importPath, bindingSymbol)
		}
		if used {
			seen[record.project] = true
		}
	}
	projects := make([]string, 0, len(seen))
	for project := range seen {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	return projects
}

func goUsesBinding(path string, content []byte, importPath, bindingSymbol string) bool {
	file, err := parser.ParseFile(token.NewFileSet(), path, content, parser.SkipObjectResolution)
	if err != nil {
		return false
	}
	aliases := map[string]bool{}
	dotImport := false
	for _, spec := range file.Imports {
		value, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil || value != importPath {
			continue
		}
		if spec.Name != nil {
			switch spec.Name.Name {
			case ".":
				dotImport = true
			case "_":
				continue
			default:
				aliases[spec.Name.Name] = true
			}
		} else {
			aliases[filepath.Base(importPath)] = true
		}
	}
	used := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			ident, identOK := fn.X.(*ast.Ident)
			if identOK && aliases[ident.Name] && fn.Sel.Name == bindingSymbol {
				used = true
				return false
			}
		case *ast.Ident:
			if dotImport && fn.Name == bindingSymbol {
				used = true
				return false
			}
		}
		return true
	})
	return used
}

// tsUsesBinding takes the masked source the workspace scan already computed for
// this file; an empty mask is recomputed so a caller outside the scan still
// gets the same answer.
func tsUsesBinding(content, code, importPath, bindingSymbol string) bool {
	if code == "" {
		code = tsCodeMask(content)
	}
	for _, match := range tsImportMatches(tsNamedImport, content, code) {
		if match[2] != importPath {
			continue
		}
		for _, raw := range strings.Split(match[1], ",") {
			parts := strings.Fields(strings.TrimSpace(raw))
			local := ""
			switch {
			case len(parts) == 1 && parts[0] == bindingSymbol:
				local = parts[0]
			case len(parts) == 3 && parts[0] == bindingSymbol && parts[1] == "as":
				local = parts[2]
			}
			if local != "" && cachedPattern(`\b`+regexp.QuoteMeta(local)+`\s*\(`).MatchString(code) {
				return true
			}
		}
	}
	for _, match := range tsImportMatches(tsNamespaceImport, content, code) {
		if match[2] == importPath && cachedPattern(`\b`+regexp.QuoteMeta(match[1])+`\s*\.\s*`+
			regexp.QuoteMeta(bindingSymbol)+`\s*\(`).MatchString(code) {
			return true
		}
	}
	return false
}

func tsCodeMask(content string) string {
	masker := typeScriptMasker{source: content, output: []byte(content)}
	masker.scanCode(0, false)
	return string(masker.output)
}

type typeScriptMasker struct {
	source string
	output []byte
}

func (m *typeScriptMasker) scanCode(start int, stopAtBrace bool) int {
	braceDepth := 0
	for index := start; index < len(m.source); {
		switch {
		case index+1 < len(m.source) && m.source[index:index+2] == "//":
			index = m.maskLineComment(index)
		case index+1 < len(m.source) && m.source[index:index+2] == "/*":
			index = m.maskBlockComment(index)
		case m.source[index] == '\'' || m.source[index] == '"':
			index = m.maskQuoted(index, m.source[index])
		case m.source[index] == '`':
			index = m.maskTemplate(index)
		case m.source[index] == '/' && m.regexCanStart(index):
			index = m.maskRegularExpression(index)
		case m.source[index] == '<':
			if next, ok := m.maskJSXElement(index); ok {
				index = next
			} else {
				index++
			}
		case m.source[index] == '{':
			braceDepth++
			index++
		case m.source[index] == '}':
			if stopAtBrace && braceDepth == 0 {
				return index
			}
			if braceDepth > 0 {
				braceDepth--
			}
			index++
		default:
			index++
		}
	}
	return len(m.source)
}

func (m *typeScriptMasker) maskLineComment(start int) int {
	index := start
	for index < len(m.source) && m.source[index] != '\n' {
		m.output[index] = ' '
		index++
	}
	return index
}

func (m *typeScriptMasker) maskBlockComment(start int) int {
	index := start
	for index < len(m.source) {
		m.maskByte(index)
		if index+1 < len(m.source) && m.source[index:index+2] == "*/" {
			m.maskByte(index + 1)
			return index + 2
		}
		index++
	}
	return index
}

func (m *typeScriptMasker) maskQuoted(start int, quote byte) int {
	escaped := false
	for index := start; index < len(m.source); index++ {
		character := m.source[index]
		m.maskByte(index)
		if index == start {
			continue
		}
		if escaped {
			escaped = false
		} else if character == '\\' {
			escaped = true
		} else if character == quote {
			return index + 1
		}
	}
	return len(m.source)
}

func (m *typeScriptMasker) maskTemplate(start int) int {
	m.maskByte(start)
	for index := start + 1; index < len(m.source); {
		switch {
		case m.source[index] == '\\':
			m.maskByte(index)
			if index+1 < len(m.source) {
				m.maskByte(index + 1)
			}
			index += 2
		case m.source[index] == '`':
			m.maskByte(index)
			return index + 1
		case index+1 < len(m.source) && m.source[index:index+2] == "${":
			m.maskByte(index)
			m.maskByte(index + 1)
			closing := m.scanCode(index+2, true)
			if closing >= len(m.source) {
				return closing
			}
			m.maskByte(closing)
			index = closing + 1
		default:
			m.maskByte(index)
			index++
		}
	}
	return len(m.source)
}

func (m *typeScriptMasker) regexCanStart(index int) bool {
	previous := index - 1
	for previous >= 0 && (m.output[previous] == ' ' || m.output[previous] == '\t' || m.output[previous] == '\r' || m.output[previous] == '\n') {
		previous--
	}
	if previous < 0 || strings.ContainsRune("([{:;,=!?&|+-*%^~<>", rune(m.output[previous])) {
		return true
	}
	if m.output[previous] == '>' && previous > 0 && m.output[previous-1] == '=' {
		return true
	}
	end := previous + 1
	for previous >= 0 && (m.output[previous] == '_' || m.output[previous] == '$' ||
		m.output[previous] >= 'a' && m.output[previous] <= 'z' || m.output[previous] >= 'A' && m.output[previous] <= 'Z') {
		previous--
	}
	switch string(m.output[previous+1 : end]) {
	case "await", "case", "delete", "in", "instanceof", "of", "return", "throw", "typeof", "void", "yield":
		return true
	default:
		return false
	}
}

func (m *typeScriptMasker) maskRegularExpression(start int) int {
	inClass := false
	escaped := false
	for index := start; index < len(m.source); index++ {
		character := m.source[index]
		m.maskByte(index)
		if index == start {
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '[' {
			inClass = true
		} else if character == ']' {
			inClass = false
		} else if character == '/' && !inClass {
			index++
			for index < len(m.source) && ((m.source[index] >= 'a' && m.source[index] <= 'z') || (m.source[index] >= 'A' && m.source[index] <= 'Z')) {
				m.maskByte(index)
				index++
			}
			return index
		} else if character == '\n' || character == '\r' {
			return index
		}
	}
	return len(m.source)
}

func (m *typeScriptMasker) maskJSXElement(start int) (int, bool) {
	nameStart := start + 1
	if nameStart >= len(m.source) || !isJSXNameStart(m.source[nameStart]) || !m.jsxCanStart(start) {
		return start, false
	}
	nameEnd := nameStart + 1
	for nameEnd < len(m.source) && isJSXNamePart(m.source[nameEnd]) {
		nameEnd++
	}
	name := m.source[nameStart:nameEnd]
	index, selfClosing, ok := m.maskJSXTag(start, nameEnd)
	if !ok || selfClosing {
		return index, ok
	}
	for index < len(m.source) {
		switch {
		case strings.HasPrefix(m.source[index:], "</"+name):
			return m.maskJSXClosingTag(index), true
		case m.source[index] == '<':
			if next, nested := m.maskJSXElement(index); nested {
				index = next
			} else {
				m.maskByte(index)
				index++
			}
		case m.source[index] == '{':
			m.maskByte(index)
			closing := m.scanCode(index+1, true)
			if closing >= len(m.source) {
				return closing, true
			}
			m.maskByte(closing)
			index = closing + 1
		default:
			m.maskByte(index)
			index++
		}
	}
	return index, true
}

func (m *typeScriptMasker) jsxCanStart(start int) bool {
	previous := start - 1
	for previous >= 0 && (m.output[previous] == ' ' || m.output[previous] == '\t' || m.output[previous] == '\r' || m.output[previous] == '\n') {
		previous--
	}
	return previous < 0 || strings.ContainsRune("=([{,:;>!&|?", rune(m.output[previous]))
}

func (m *typeScriptMasker) maskJSXTag(start, index int) (int, bool, bool) {
	for position := start; position < index; position++ {
		m.maskByte(position)
	}
	for index < len(m.source) {
		switch {
		case m.source[index] == '\'' || m.source[index] == '"':
			index = m.maskQuoted(index, m.source[index])
		case m.source[index] == '{':
			m.maskByte(index)
			closing := m.scanCode(index+1, true)
			if closing >= len(m.source) {
				return closing, false, false
			}
			m.maskByte(closing)
			index = closing + 1
		case m.source[index] == '>':
			selfClosing := index > start && m.source[index-1] == '/'
			m.maskByte(index)
			return index + 1, selfClosing, true
		default:
			m.maskByte(index)
			index++
		}
	}
	return index, false, false
}

func (m *typeScriptMasker) maskJSXClosingTag(start int) int {
	index := start
	for index < len(m.source) {
		m.maskByte(index)
		if m.source[index] == '>' {
			return index + 1
		}
		index++
	}
	return index
}

func (m *typeScriptMasker) maskByte(index int) {
	if m.output[index] != '\n' && m.output[index] != '\r' {
		m.output[index] = ' '
	}
}

func isJSXNameStart(character byte) bool {
	return character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func isJSXNamePart(character byte) bool {
	return isJSXNameStart(character) || character >= '0' && character <= '9' || character == '-' || character == '.' || character == ':'
}

func operationAuthProfiles(security clientcontract.Security) []string {
	seen := map[string]bool{}
	for _, alternative := range security.Alternatives {
		for _, requirement := range alternative.AllOf {
			if requirement.Profile != "" {
				seen[requirement.Profile] = true
			}
		}
	}
	profiles := make([]string, 0, len(seen))
	for profile := range seen {
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	return profiles
}
