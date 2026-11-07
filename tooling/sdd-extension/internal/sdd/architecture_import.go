package sdd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	archproto "go.putnami.dev/protocol/architecture"
	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/sdd/extension/internal/structurizr"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// `architecture import` turns an authored architecture model into domain
// manifests (ADR 0005).
//
// A Structurizr relationship is a declaration a person wrote and reviewed in
// the model, not an observation, so the command writes exports and imports
// from it. It still never turns a code dependency into a permission: a binding
// is added only for an edge the graph observes, and only to the import the
// model declares for that domain pair. An edge the model does not relate stays
// an `architecture validate` violation.
//
// A rerun only adds. An entry that already exists is never changed or removed,
// so a hand edit survives every rerun.

// ImportFormatStructurizr is the one model format `architecture import` reads.
const ImportFormatStructurizr = "structurizr"

// Default import settings.
const (
	DefaultImportAt           = "architecture"
	DefaultImportPathProperty = "path"
	importDomainProperty      = "putnami.domain"
	importOwnerProperty       = "putnami.owner"
	importTextLimit           = 4096
	importOwnerLimit          = 256
)

var importDomainPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(/[a-z][a-z0-9-]*)*$`)

// ArchitectureImportOptions carries what the caller decided.
type ArchitectureImportOptions struct {
	// Format names the model format; only "structurizr" is read.
	Format string
	// Source is the model file, already resolved against the caller's
	// directory. SourceLabel is how the report names it.
	Source      string
	SourceLabel string
	// At is the workspace-relative directory new manifests go below.
	At string
	// PathProperty is the element property that names a project.
	PathProperty string
	// Apply writes the manifests; without it nothing is written.
	Apply bool
}

// ArchitectureImportUnmapped is one model element or relationship the command
// did not map, with the reason.
type ArchitectureImportUnmapped struct {
	Element string `json:"element"`
	Kind    string `json:"kind"`
	Line    int    `json:"line,omitempty"`
	Reason  string `json:"reason"`
}

// ArchitectureImportManifest is what the import adds to one domain manifest.
type ArchitectureImportManifest struct {
	Path          string                    `json:"path"`
	Domain        string                    `json:"domain"`
	Created       bool                      `json:"created"`
	Changed       bool                      `json:"changed"`
	Written       bool                      `json:"written"`
	AddedProjects []string                  `json:"addedProjects"`
	AddedExports  []string                  `json:"addedExports"`
	AddedImports  []string                  `json:"addedImports"`
	AddedBindings []ArchitectureSyncBinding `json:"addedBindings"`
	// NotInModel names the imports between two model domains that the model no
	// longer relates. The import leaves them for a person to remove.
	NotInModel []string `json:"notInModel"`
	// Diff is the line diff from the file on disk to Contents, when changed.
	Diff     string `json:"diff,omitempty"`
	Contents string `json:"contents,omitempty"`
}

// ArchitectureImportSummary is the compact projection both renderings share.
type ArchitectureImportSummary struct {
	Domains       int `json:"domains"`
	Created       int `json:"created"`
	Changed       int `json:"changed"`
	Written       int `json:"written"`
	AddedProjects int `json:"addedProjects"`
	AddedExports  int `json:"addedExports"`
	AddedImports  int `json:"addedImports"`
	AddedBindings int `json:"addedBindings"`
	NotInModel    int `json:"notInModel"`
	Unmapped      int `json:"unmapped"`
}

// ArchitectureImportReport is what one import run decided.
type ArchitectureImportReport struct {
	Source      string                       `json:"source"`
	Format      string                       `json:"format"`
	Applied     bool                         `json:"applied"`
	Summary     ArchitectureImportSummary    `json:"summary"`
	Manifests   []ArchitectureImportManifest `json:"manifests"`
	Unmapped    []ArchitectureImportUnmapped `json:"unmapped"`
	Diagnostics []diag.Diagnostic            `json:"diagnostics"`
}

// importDomain is one domain the model declares.
type importDomain struct {
	id          string
	owner       string
	description string
	projects    []string
	// consumes maps a producer domain to the relationships that relate them.
	consumes map[string][]structurizr.Relationship
	produces bool
}

// BuildArchitectureImportResult maps a model to manifests and, with apply set,
// writes the ones that change.
func BuildArchitectureImportResult(ws *workspace.Workspace, options ArchitectureImportOptions) (ArchitectureImportReport, error) {
	report := ArchitectureImportReport{
		Source:      options.SourceLabel,
		Format:      options.Format,
		Applied:     options.Apply,
		Manifests:   []ArchitectureImportManifest{},
		Unmapped:    []ArchitectureImportUnmapped{},
		Diagnostics: []diag.Diagnostic{},
	}
	if ws == nil {
		return report, protocolcli.Usagef("architecture import requires a resolved workspace")
	}
	if options.Format != ImportFormatStructurizr {
		return report, protocolcli.Usagef("--from %q is not a model format architecture import reads; use --from %s",
			options.Format, ImportFormatStructurizr)
	}
	if strings.TrimSpace(options.Source) == "" {
		return report, protocolcli.Usagef("architecture import requires the model file: a Structurizr workspace.dsl or workspace.json")
	}
	if ws.HasWarningCode(workspace.WarningCodeProviderViewUnavailable) {
		return report, protocolcli.Usagef(
			"architecture import needs a complete project view to map projects and bindings; this run received a partial one " +
				"(run `putnami projects sync` when a provider probe is the cause)")
	}
	pathProperty := strings.TrimSpace(options.PathProperty)
	if pathProperty == "" {
		pathProperty = DefaultImportPathProperty
	}
	at := strings.TrimSpace(options.At)
	if at == "" {
		at = DefaultImportAt
	}
	at = path.Clean(filepath.ToSlash(at))
	if at != "." && validateArchitectureWritePath(at) != "" {
		return report, protocolcli.Usagef("--at %s is not a contained workspace-relative directory", options.At)
	}
	if !discoverableManifestPath(at + "/" + archproto.ManifestFilename) {
		return report, protocolcli.Usagef("--at %s is a directory architecture discovery skips (hidden, vendor or node_modules)", options.At)
	}

	model, err := structurizr.Read(options.Source)
	if err != nil {
		if os.IsNotExist(err) {
			return report, protocolcli.NotFoundf("model file %s does not exist", options.SourceLabel)
		}
		return report, protocolcli.InvalidConfigf("cannot read the Structurizr model: %v", err)
	}

	discovery := Discover(ws.Root)
	diagnostics := append([]diag.Diagnostic(nil), discovery.Diagnostics...)
	diagnostics = append(diagnostics, archproto.ValidateRepository(discovery.Sources)...)
	sortDiagnostics(diagnostics)
	report.Diagnostics = copyArchitectureDiagnostics(diagnostics)
	if diag.HasErrors(diagnostics) {
		return report, protocolcli.WithResultData(protocolcli.InvalidConfigf(
			"architecture import needs valid declarations; fix the reported errors first"), report)
	}

	mapper := newImportMapper(ws, model, pathProperty, discovery.Sources)
	domains := mapper.mapModel()
	report.Unmapped = append(report.Unmapped, mapper.unmapped...)

	merge, err := newImportMerge(discovery.Sources, at)
	if err != nil {
		return report, protocolcli.InvalidConfigf("cannot read the declared manifests: %v", err)
	}
	byID := make(map[string]*importDomain, len(domains))
	for _, domain := range domains {
		byID[domain.id] = domain
		merge.addDomain(domain)
	}
	for _, domain := range domains {
		merge.addImports(domain, byID)
	}
	merge.reportNotInModel()
	merge.bind(ws)
	report.Unmapped = append(report.Unmapped, merge.unmapped...)
	report.Summary = summarizeArchitectureImport(report, len(domains))

	blocked := merge.checkTargets(ws.Root)
	if repoDiagnostics := archproto.ValidateRepository(merge.sources()); diag.HasErrors(repoDiagnostics) {
		blocked = append(blocked, repoDiagnostics...)
	}
	if diag.HasErrors(blocked) {
		sortDiagnostics(blocked)
		report.Diagnostics = copyArchitectureDiagnostics(blocked)
		return report, protocolcli.WithResultData(protocolcli.InvalidConfigf(
			"the imported manifests cannot be written as they are; nothing was written"), report)
	}

	for _, entry := range merge.ordered() {
		row, err := entry.finish(ws.Root, options.Apply)
		if err != nil {
			report.Summary = summarizeArchitectureImport(report, len(domains))
			return report, protocolcli.WithResultData(err, report)
		}
		report.Manifests = append(report.Manifests, row)
	}
	report.Summary = summarizeArchitectureImport(report, len(domains))
	return report, nil
}

// --- mapping ----------------------------------------------------------------

type importMapper struct {
	ws           *workspace.Workspace
	model        *structurizr.Workspace
	pathProperty string
	unmapped     []ArchitectureImportUnmapped
	// declaredProject maps a project to the domain an existing manifest gives it.
	declaredProject map[string]string
	// rejected holds the elements whose name gives a domain id another element
	// already derived, with the reason.
	rejected map[*structurizr.Element]string
}

func newImportMapper(ws *workspace.Workspace, model *structurizr.Workspace, pathProperty string, sources []archproto.ManifestSource) *importMapper {
	declared := map[string]string{}
	for _, source := range sources {
		if source.Manifest == nil {
			continue
		}
		for _, project := range source.Manifest.Projects {
			declared[project] = source.Manifest.Domain
		}
	}
	return &importMapper{
		ws: ws, model: model, pathProperty: pathProperty,
		declaredProject: declared, rejected: map[*structurizr.Element]string{},
	}
}

func (m *importMapper) skip(element *structurizr.Element, reason string) {
	m.unmapped = append(m.unmapped, ArchitectureImportUnmapped{
		Element: element.Path(), Kind: string(element.Kind), Line: element.Line, Reason: reason,
	})
}

// domainOf returns the domain an element belongs to, or "" with the reason.
func (m *importMapper) domainOf(element *structurizr.Element) (string, string) {
	if reason, rejected := m.rejected[element]; rejected {
		return "", reason
	}
	switch element.Kind {
	case structurizr.KindPerson:
		return "", "a person is an actor outside the code"
	case structurizr.KindOther:
		return "", "a custom element has no C4 level to map"
	case structurizr.KindComponent:
		return m.domainOf(element.Parent)
	case structurizr.KindSoftwareSystem:
		if containers := containersOf(element); len(containers) > 0 {
			return m.systemDomain(element, containers)
		}
		if override := element.Property(importDomainProperty); override != "" {
			return validImportDomain(override, element)
		}
		return validImportDomain(slugImportName(element.Name), element)
	case structurizr.KindContainer:
		if override := element.Property(importDomainProperty); override != "" {
			return validImportDomain(override, element)
		}
		if override := element.Parent.Property(importDomainProperty); override != "" {
			return validImportDomain(override, element)
		}
		return validImportDomain(slugImportName(element.Parent.Name)+"/"+slugImportName(element.Name), element)
	}
	return "", "the element has no C4 level to map"
}

// systemDomain is the domain of a software system that has containers: the
// one domain all its containers share, or none when they spread over several.
func (m *importMapper) systemDomain(system *structurizr.Element, containers []*structurizr.Element) (string, string) {
	shared := ""
	for _, container := range containers {
		domain, reason := m.domainOf(container)
		if domain == "" {
			return "", reason
		}
		if shared != "" && shared != domain {
			return "", fmt.Sprintf("%s spans several domains, one per container; map the relationship at the container level", system.Name)
		}
		shared = domain
	}
	return shared, ""
}

// derivesDomainID reports whether an element's domain id comes from its own
// name rather than from a putnami.domain property or from its container.
func derivesDomainID(element *structurizr.Element) bool {
	switch element.Kind {
	case structurizr.KindContainer:
		return element.Property(importDomainProperty) == "" && element.Parent.Property(importDomainProperty) == ""
	case structurizr.KindSoftwareSystem:
		return len(containersOf(element)) == 0 && element.Property(importDomainProperty) == ""
	}
	return false
}

func containersOf(system *structurizr.Element) []*structurizr.Element {
	var containers []*structurizr.Element
	for _, child := range system.Children {
		if child.Kind == structurizr.KindContainer {
			containers = append(containers, child)
		}
	}
	return containers
}

func validImportDomain(id string, element *structurizr.Element) (string, string) {
	if !importDomainPattern.MatchString(id) {
		return "", fmt.Sprintf("%q is not a valid domain id; set a %s property on %s", id, importDomainProperty, element.Name)
	}
	return id, ""
}

// slugImportName lower-cases a name and turns each run of other characters
// into one hyphen.
func slugImportName(name string) string {
	var builder strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			hyphen = false
			continue
		}
		if !hyphen && builder.Len() > 0 {
			builder.WriteByte('-')
			hyphen = true
		}
	}
	return strings.TrimSuffix(builder.String(), "-")
}

func (m *importMapper) mapModel() []*importDomain {
	byID := map[string]*importDomain{}
	domainFor := func(id string, element *structurizr.Element) *importDomain {
		domain := byID[id]
		if domain == nil {
			domain = &importDomain{id: id, consumes: map[string][]structurizr.Relationship{}}
			byID[id] = domain
		}
		if domain.owner == "" {
			owner := element.Property(importOwnerProperty)
			if owner == "" && element.Parent != nil {
				owner = element.Parent.Property(importOwnerProperty)
			}
			if len(owner) > importOwnerLimit {
				m.skip(element, fmt.Sprintf("its %s is longer than %d bytes, so the owner is the domain id", importOwnerProperty, importOwnerLimit))
				owner = ""
			}
			domain.owner = owner
		}
		if domain.description == "" {
			domain.description = singleLine(element.Description)
		}
		return domain
	}

	claimed := map[string]string{}
	derived := map[string]*structurizr.Element{}
	for _, element := range m.model.Elements {
		domainID, reason := m.domainOf(element)
		if domainID != "" && derivesDomainID(element) {
			if first := derived[domainID]; first != nil && first != element {
				reason = fmt.Sprintf("its name gives domain %s, which %s already gives; set a %s property", domainID, first.Path(), importDomainProperty)
				m.rejected[element] = reason
				domainID = ""
			} else {
				derived[domainID] = element
			}
		}
		if element.Kind == structurizr.KindSoftwareSystem && len(containersOf(element)) > 0 {
			if element.Property(m.pathProperty) != "" {
				m.skip(element, fmt.Sprintf("the %s of a system with containers is not read: its containers are the domains", m.pathProperty))
			}
			continue
		}
		if domainID == "" {
			m.skip(element, reason)
			continue
		}
		owner := element
		if element.Kind == structurizr.KindComponent {
			owner = element.Parent
		}
		domain := domainFor(domainID, owner)
		location := element.Property(m.pathProperty)
		if location == "" {
			if element.Kind == structurizr.KindComponent {
				m.skip(element, fmt.Sprintf("a component maps to a project only through a %q property", m.pathProperty))
			}
			continue
		}
		project := m.resolveProject(location)
		switch {
		case project == "":
			m.skip(element, fmt.Sprintf("%s %q is no project of this workspace", m.pathProperty, location))
		case claimed[project] != "" && claimed[project] != domainID:
			m.skip(element, fmt.Sprintf("project %s is already mapped to domain %s", project, claimed[project]))
		case m.declaredProject[project] != "" && m.declaredProject[project] != domainID:
			m.skip(element, fmt.Sprintf("project %s already belongs to domain %s in its manifest", project, m.declaredProject[project]))
		default:
			if claimed[project] == "" {
				claimed[project] = domainID
				domain.projects = append(domain.projects, project)
			}
		}
	}

	for _, relationship := range m.model.Relationships {
		label := relationship.Source.Path() + " -> " + relationship.Destination.Path()
		consumer, reason := m.domainOf(relationship.Source)
		if consumer == "" {
			m.skipRelationship(label, relationship.Line, "its source is not mapped: "+reason)
			continue
		}
		producer, reason := m.domainOf(relationship.Destination)
		if producer == "" {
			m.skipRelationship(label, relationship.Line, "its destination is not mapped: "+reason)
			continue
		}
		if consumer == producer {
			m.skipRelationship(label, relationship.Line, "it stays inside domain "+consumer)
			continue
		}
		domainFor(consumer, relationship.Source).consumes[producer] = append(
			domainFor(consumer, relationship.Source).consumes[producer], relationship)
		domainFor(producer, relationship.Destination).produces = true
	}

	domains := make([]*importDomain, 0, len(byID))
	for _, domain := range byID {
		if domain.owner == "" {
			domain.owner = domain.id
		}
		sort.Strings(domain.projects)
		domains = append(domains, domain)
	}
	sort.Slice(domains, func(i, j int) bool { return domains[i].id < domains[j].id })
	return domains
}

func (m *importMapper) skipRelationship(label string, line int, reason string) {
	m.unmapped = append(m.unmapped, ArchitectureImportUnmapped{Element: label, Kind: "relationship", Line: line, Reason: reason})
}

// resolveProject reads a path property: a project ID, or the project's
// directory relative to the workspace root.
func (m *importMapper) resolveProject(location string) string {
	if strings.HasPrefix(location, "/") {
		if project := m.ws.ProjectByID(location); project != nil {
			return project.ID
		}
		return ""
	}
	cleaned := path.Clean(filepath.ToSlash(location))
	for _, project := range m.ws.Projects {
		if project != nil && path.Clean(filepath.ToSlash(project.Path)) == cleaned {
			return project.ID
		}
	}
	return ""
}

// --- generated contracts ----------------------------------------------------

func dottedDomain(domain string) string { return strings.ReplaceAll(domain, "/", ".") }

func importExportID(domain string) string { return dottedDomain(domain) + ".interface.v1" }

func importFactName(domain string) string {
	return strings.ReplaceAll(dottedDomain(domain), "-", "_") + ".interface"
}

func importImportID(consumer, producer string, version int) string {
	return fmt.Sprintf("%s.uses.%s.v%d", dottedDomain(consumer), dottedDomain(producer), version)
}

func generatedExport(domain *importDomain) archproto.Export {
	description := domain.description
	if description == "" {
		description = fmt.Sprintf("The interface of %s in the Structurizr model.", domain.id)
	}
	return archproto.Export{
		ID:          importExportID(domain.id),
		Version:     1,
		Status:      archproto.StatusActive,
		Description: truncateText(description, importTextLimit),
		Facts:       []archproto.Fact{{Name: importFactName(domain.id), Authority: domain.id}},
		Modes:       []archproto.AccessMode{archproto.ModeReference},
		Compatibility: archproto.Compatibility{
			Strategy:               archproto.CompatibilityAdditive,
			MinimumConsumerVersion: 1,
		},
	}
}

func importJustification(relationships []structurizr.Relationship) string {
	parts := make([]string, 0, len(relationships))
	for _, relationship := range relationships {
		part := relationship.Source.Path() + " -> " + relationship.Destination.Path()
		if description := singleLine(relationship.Description); description != "" {
			part += ": " + description
		}
		if technology := singleLine(relationship.Technology); technology != "" {
			part += " [" + technology + "]"
		}
		parts = append(parts, part)
	}
	return truncateText("From the Structurizr model: "+strings.Join(parts, "; ")+".", importTextLimit)
}

func singleLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit - len("…")
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut]) + "…"
}

// --- merge ------------------------------------------------------------------

type importEntry struct {
	path     string
	created  bool
	manifest *archproto.Manifest
	row      ArchitectureImportManifest
	touched  bool
}

type importPair struct{ consumer, producer string }

type importMerge struct {
	byDomain map[string]*importEntry
	at       string
	// model holds the domains the model declares, related the domain pairs its
	// relationships relate.
	model    map[string]bool
	related  map[importPair]bool
	unmapped []ArchitectureImportUnmapped
}

// newImportMerge indexes the declared manifests as their authors wrote them:
// no canonical reordering, so an addition is the only change a diff shows.
func newImportMerge(sources []archproto.ManifestSource, at string) (*importMerge, error) {
	merge := &importMerge{
		byDomain: map[string]*importEntry{}, at: at,
		model: map[string]bool{}, related: map[importPair]bool{},
	}
	for _, source := range sources {
		if source.Manifest == nil {
			continue
		}
		manifest, err := cloneManifest(source.Manifest)
		if err != nil {
			return nil, err
		}
		merge.byDomain[manifest.Domain] = &importEntry{
			path:     source.Path,
			manifest: manifest,
			row:      newImportRow(source.Path, manifest.Domain),
		}
	}
	return merge, nil
}

func cloneManifest(manifest *archproto.Manifest) (*archproto.Manifest, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	var clone archproto.Manifest
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func newImportRow(manifestPath, domain string) ArchitectureImportManifest {
	return ArchitectureImportManifest{
		Path:          manifestPath,
		Domain:        domain,
		AddedProjects: []string{},
		AddedExports:  []string{},
		AddedImports:  []string{},
		AddedBindings: []ArchitectureSyncBinding{},
		NotInModel:    []string{},
	}
}

// addDomain creates the manifest of a domain no manifest declares yet, and
// adds the projects the model maps to it.
func (merge *importMerge) addDomain(domain *importDomain) {
	merge.model[domain.id] = true
	entry := merge.byDomain[domain.id]
	if entry == nil {
		manifestPath := domain.id + "/" + archproto.ManifestFilename
		if merge.at != "." {
			manifestPath = merge.at + "/" + manifestPath
		}
		entry = &importEntry{
			path:    manifestPath,
			created: true,
			manifest: &archproto.Manifest{
				Schema:          archproto.ManifestSchemaURL,
				ProtocolVersion: archproto.ProtocolVersion,
				Domain:          domain.id,
				Owner:           domain.owner,
				Projects:        []string{},
				Exports:         []archproto.Export{},
				Imports:         []archproto.Import{},
			},
			row: newImportRow(manifestPath, domain.id),
		}
		merge.byDomain[domain.id] = entry
	}
	entry.touched = true
	for _, project := range domain.projects {
		if !slices.Contains(entry.manifest.Projects, project) {
			entry.manifest.Projects = append(entry.manifest.Projects, project)
			entry.row.AddedProjects = append(entry.row.AddedProjects, project)
		}
	}
}

// addImports adds one import per producer domain the consumer relates to and
// declares no import from yet. A pair that already has an import keeps it as
// a person may have renamed or refined it: the import never adds a second
// contract for the same pair.
func (merge *importMerge) addImports(domain *importDomain, domains map[string]*importDomain) {
	entry := merge.byDomain[domain.id]
	producers := make([]string, 0, len(domain.consumes))
	for producer := range domain.consumes {
		producers = append(producers, producer)
	}
	sort.Strings(producers)
	for _, producer := range producers {
		merge.related[importPair{domain.id, producer}] = true
		if declaresImportFrom(entry.manifest, producer) {
			continue
		}
		export, generate, reason := merge.producerExport(domains[producer])
		label := domain.id + " -> " + producer
		if reason != "" {
			merge.unmapped = append(merge.unmapped, ArchitectureImportUnmapped{Element: label, Kind: "relationship", Reason: reason})
			continue
		}
		id := importImportID(domain.id, producer, export.Version)
		if hasImport(entry.manifest, id) {
			merge.unmapped = append(merge.unmapped, ArchitectureImportUnmapped{
				Element: label, Kind: "relationship",
				Reason: fmt.Sprintf("domain %s already declares an import %s from another domain", domain.id, id),
			})
			continue
		}
		if generate {
			producerEntry := merge.byDomain[producer]
			producerEntry.manifest.Exports = append(producerEntry.manifest.Exports, export)
			producerEntry.row.AddedExports = append(producerEntry.row.AddedExports, export.ID)
		}
		facts := make([]string, 0, len(export.Facts))
		for _, fact := range export.Facts {
			facts = append(facts, fact.Name)
		}
		imported := archproto.Import{
			ID:            id,
			Version:       export.Version,
			From:          archproto.ExportReference{Domain: producer, Export: export.ID},
			As:            dottedDomain(domain.id) + ".uses." + dottedDomain(producer),
			Mode:          archproto.ModeReference,
			Status:        archproto.StatusActive,
			Facts:         facts,
			Justification: importJustification(domain.consumes[producer]),
		}
		entry.manifest.Imports = append(entry.manifest.Imports, imported)
		entry.row.AddedImports = append(entry.row.AddedImports, imported.ID)
	}
}

// producerExport is the export a new import reads: the producer's generated
// one, else the one active export that allows reference access. A producer
// that declares no export gets the generated one, which the caller adds once
// the import is certain (generate). Any other case is a choice the import
// leaves to a person, and reason says why.
func (merge *importMerge) producerExport(domain *importDomain) (archproto.Export, bool, string) {
	entry := merge.byDomain[domain.id]
	var candidates []archproto.Export
	for _, export := range entry.manifest.Exports {
		if export.Status == archproto.StatusActive && slices.Contains(export.Modes, archproto.ModeReference) {
			if export.ID == importExportID(domain.id) {
				return export, false, ""
			}
			candidates = append(candidates, export)
		}
	}
	switch {
	case len(candidates) == 1:
		return candidates[0], false, ""
	case len(candidates) > 1:
		return archproto.Export{}, false, fmt.Sprintf(
			"domain %s declares %d exports that allow reference access; declare this import by hand to choose one",
			domain.id, len(candidates))
	case len(entry.manifest.Exports) > 0:
		return archproto.Export{}, false, fmt.Sprintf(
			"no export of domain %s allows active reference access; declare this import by hand", domain.id)
	}
	return generatedExport(domain), true, ""
}

// reportNotInModel names the imports between two model domains that the model
// no longer relates.
func (merge *importMerge) reportNotInModel() {
	for _, entry := range merge.ordered() {
		for _, imported := range entry.manifest.Imports {
			generated := imported.ID == importImportID(entry.manifest.Domain, imported.From.Domain, imported.Version)
			if generated && merge.model[imported.From.Domain] && !merge.related[importPair{entry.manifest.Domain, imported.From.Domain}] {
				entry.row.NotInModel = append(entry.row.NotInModel, imported.ID)
			}
		}
	}
}

// bind adds a binding for each observed edge between two domains the model
// relates, under the architecture sync rule: to the one import that already
// covers the pair. Every other observed edge from a model domain is listed,
// because architecture validate will report it.
func (merge *importMerge) bind(ws *workspace.Workspace) {
	sources := merge.sources()
	authorized := authorizedBindingKeys(sources)
	for _, edge := range DetectProjectDependencies(ws, sources) {
		key := syncBindingKey(edge.ConsumerProject, edge.ProducerProject)
		if _, granted := authorized[key]; granted {
			continue
		}
		entry := merge.byDomain[edge.ConsumerDomain]
		if entry == nil || !entry.touched {
			continue
		}
		label := edge.ConsumerProject + " -> " + edge.ProducerProject
		if !merge.related[importPair{edge.ConsumerDomain, edge.ProducerDomain}] {
			merge.unmapped = append(merge.unmapped, ArchitectureImportUnmapped{
				Element: label, Kind: "project-edge",
				Reason: fmt.Sprintf("the model does not relate domain %s to domain %s, so architecture validate reports this dependency",
					edge.ConsumerDomain, edge.ProducerDomain),
			})
			continue
		}
		target, refusal := candidateImport(entry.manifest, edge)
		if target == nil {
			reason := fmt.Sprintf("domain %s declares no active import from %s that can carry the binding; declare it by hand",
				edge.ConsumerDomain, edge.ProducerDomain)
			if refusal.Reason == SyncRefusalAmbiguousImport {
				reason = fmt.Sprintf("domain %s declares several imports from %s; bind the edge by hand to the one it implements",
					edge.ConsumerDomain, edge.ProducerDomain)
			}
			merge.unmapped = append(merge.unmapped, ArchitectureImportUnmapped{Element: label, Kind: "project-edge", Reason: reason})
			continue
		}
		target.Bindings = append(target.Bindings, archproto.Binding{
			Kind:            archproto.BindingProjectDependency,
			ConsumerProject: edge.ConsumerProject,
			ProducerProject: edge.ProducerProject,
		})
		entry.row.AddedBindings = append(entry.row.AddedBindings, ArchitectureSyncBinding{
			Import: target.ID, ConsumerProject: edge.ConsumerProject, ProducerProject: edge.ProducerProject,
		})
		authorized[key] = target.ID
	}
}

func (merge *importMerge) sources() []archproto.ManifestSource {
	sources := make([]archproto.ManifestSource, 0, len(merge.byDomain))
	for _, entry := range merge.byDomain {
		sources = append(sources, archproto.ManifestSource{Path: entry.path, Manifest: entry.manifest})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources
}

// ordered lists the entries the model touches, by domain.
func (merge *importMerge) ordered() []*importEntry {
	entries := make([]*importEntry, 0, len(merge.byDomain))
	for _, entry := range merge.byDomain {
		if entry.touched {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].manifest.Domain < entries[j].manifest.Domain })
	return entries
}

// checkTargets refuses, before anything is written, a new manifest whose path
// is taken or that discovery would never read.
func (merge *importMerge) checkTargets(wsRoot string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	for _, entry := range merge.ordered() {
		if !entry.created {
			continue
		}
		if !discoverableManifestPath(entry.path) {
			diagnostics = append(diagnostics, diag.Errorf(archproto.ErrorCodeInvalidDomain, entry.path,
				"domain %s would be written where architecture discovery never reads; set a %s property to rename it",
				entry.manifest.Domain, importDomainProperty))
			continue
		}
		_, err := os.Lstat(filepath.Join(wsRoot, filepath.FromSlash(entry.path)))
		switch {
		case err == nil:
			diagnostics = append(diagnostics, diag.Errorf(archproto.ErrorCodeParseError, entry.path,
				"a file already exists where domain %s would be created; architecture import never overwrites a file", entry.manifest.Domain))
		case !errors.Is(err, fs.ErrNotExist):
			diagnostics = append(diagnostics, diag.Errorf(archproto.ErrorCodeParseError, entry.path,
				"domain %s cannot be created here: %v", entry.manifest.Domain, err))
		}
	}
	return diagnostics
}

// discoverableManifestPath reports whether Discover reads a manifest at this
// workspace-relative path: no segment is hidden or excluded.
func discoverableManifestPath(relative string) bool {
	for _, segment := range strings.Split(path.Dir(relative), "/") {
		if segment == "." {
			continue
		}
		if strings.HasPrefix(segment, ".") || excludedDiscoveryDirectories[segment] {
			return false
		}
	}
	return true
}

func (entry *importEntry) finish(wsRoot string, apply bool) (ArchitectureImportManifest, error) {
	row := entry.row
	row.Created = entry.created
	row.Changed = entry.created || len(row.AddedProjects) > 0 || len(row.AddedExports) > 0 ||
		len(row.AddedImports) > 0 || len(row.AddedBindings) > 0
	if !row.Changed {
		return row, nil
	}
	manifest := entry.manifest
	before := []byte(nil)
	if entry.created {
		manifest = archproto.CanonicalManifest(manifest)
	} else {
		data, err := os.ReadFile(filepath.Join(wsRoot, filepath.FromSlash(entry.path)))
		if err != nil {
			return row, protocolcli.Classify(fmt.Errorf("read %s: %w", entry.path, err), protocolcli.ErrInvalidConfig)
		}
		before = data
	}
	contents, err := importManifestBytes(manifest)
	if err != nil {
		return row, err
	}
	row.Contents = string(contents)
	row.Diff = lineDiff(entry.path, string(before), row.Contents)
	if !apply {
		return row, nil
	}
	if entry.created {
		err = createArchitectureManifest(wsRoot, entry.path, contents)
	} else {
		err = rewriteArchitectureManifest(wsRoot, entry.path, contents)
	}
	if err != nil {
		return row, protocolcli.Classify(fmt.Errorf("write %s: %w", entry.path, err), protocolcli.ErrInvalidConfig)
	}
	row.Written = true
	return row, nil
}

// importManifestBytes encodes a manifest in the order its entries hold, in the
// layout of the protocol's canonical writer: two-space indentation, the same
// escaping and one trailing newline. A manifest any Putnami writer produced
// therefore keeps every existing line; one in another layout is rewritten in
// this layout once.
func importManifestBytes(manifest *archproto.Manifest) ([]byte, error) {
	if diagnostics := archproto.ValidateManifest(manifest); diag.HasErrors(diagnostics) {
		return nil, protocolcli.InvalidConfigf("the imported manifest for domain %s is not valid", manifest.Domain)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func declaresImportFrom(manifest *archproto.Manifest, producer string) bool {
	for _, imported := range manifest.Imports {
		if imported.From.Domain == producer {
			return true
		}
	}
	return false
}

func hasImport(manifest *archproto.Manifest, id string) bool {
	for _, imported := range manifest.Imports {
		if imported.ID == id {
			return true
		}
	}
	return false
}

func summarizeArchitectureImport(report ArchitectureImportReport, domains int) ArchitectureImportSummary {
	summary := ArchitectureImportSummary{Domains: domains, Unmapped: len(report.Unmapped)}
	for _, manifest := range report.Manifests {
		if manifest.Created {
			summary.Created++
		}
		if manifest.Changed {
			summary.Changed++
		}
		if manifest.Written {
			summary.Written++
		}
		summary.AddedProjects += len(manifest.AddedProjects)
		summary.AddedExports += len(manifest.AddedExports)
		summary.AddedImports += len(manifest.AddedImports)
		summary.AddedBindings += len(manifest.AddedBindings)
		summary.NotInModel += len(manifest.NotInModel)
	}
	return summary
}

// lineDiff renders a unified line diff with three lines of context.
func lineDiff(name, before, after string) string {
	a, b := splitDiffLines(before), splitDiffLines(after)
	// lcs[i][j] is the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		kind byte
		text string
		ai   int
		bi   int
	}
	var ops []op
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, op{' ', a[i], i, j})
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			ops = append(ops, op{'+', b[j], i, j})
			j++
		default:
			ops = append(ops, op{'-', a[i], i, j})
			i++
		}
	}
	const context = 3
	var out strings.Builder
	oldName := "a/" + name
	if before == "" {
		oldName = "/dev/null"
	}
	fmt.Fprintf(&out, "--- %s\n+++ b/%s\n", oldName, name)
	for start := 0; start < len(ops); {
		if ops[start].kind == ' ' {
			start++
			continue
		}
		from := max(start-context, 0)
		end := start
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				break
			}
			end = run
		}
		to := min(end+context, len(ops))
		oldCount, newCount := 0, 0
		for _, o := range ops[from:to] {
			if o.kind != '+' {
				oldCount++
			}
			if o.kind != '-' {
				newCount++
			}
		}
		oldStart, newStart := ops[from].ai+1, ops[from].bi+1
		if oldCount == 0 {
			oldStart--
		}
		if newCount == 0 {
			newStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, o := range ops[from:to] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		start = to
	}
	return out.String()
}

func splitDiffLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
