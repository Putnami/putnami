package sdd

import (
	"fmt"
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
	importJustificationLimit  = 4096
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
	report.Unmapped = mapper.unmapped

	merge := newImportMerge(discovery.Sources, at)
	for _, domain := range domains {
		merge.add(domain)
	}
	merge.bind(ws)

	sources := merge.sources()
	if repoDiagnostics := archproto.ValidateRepository(sources); diag.HasErrors(repoDiagnostics) {
		sortDiagnostics(repoDiagnostics)
		report.Diagnostics = copyArchitectureDiagnostics(repoDiagnostics)
		return report, protocolcli.WithResultData(protocolcli.InvalidConfigf(
			"the imported manifests would not be valid; nothing was written"), report)
	}

	for _, entry := range merge.ordered() {
		row, err := entry.finish(ws.Root, options.Apply)
		if err != nil {
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
	return &importMapper{ws: ws, model: model, pathProperty: pathProperty, declaredProject: declared}
}

func (m *importMapper) skip(element *structurizr.Element, reason string) {
	m.unmapped = append(m.unmapped, ArchitectureImportUnmapped{
		Element: element.Path(), Kind: string(element.Kind), Line: element.Line, Reason: reason,
	})
}

// domainOf returns the domain an element belongs to, or "" with the reason.
func (m *importMapper) domainOf(element *structurizr.Element) (string, string) {
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
			domain.owner = element.Property(importOwnerProperty)
			if domain.owner == "" && element.Parent != nil {
				domain.owner = element.Parent.Property(importOwnerProperty)
			}
		}
		if domain.description == "" {
			domain.description = singleLine(element.Description)
		}
		return domain
	}

	claimed := map[string]string{}
	for _, element := range m.model.Elements {
		domainID, reason := m.domainOf(element)
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

func importImportID(consumer, producer string) string {
	return dottedDomain(consumer) + ".uses." + dottedDomain(producer) + ".v1"
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
		Description: description,
		Facts:       []archproto.Fact{{Name: importFactName(domain.id), Authority: domain.id}},
		Modes:       []archproto.AccessMode{archproto.ModeReference},
		Compatibility: archproto.Compatibility{
			Strategy:               archproto.CompatibilityAdditive,
			MinimumConsumerVersion: 1,
		},
	}
}

func generatedImport(consumer, producer string, relationships []structurizr.Relationship) archproto.Import {
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
	justification := truncateText("From the Structurizr model: "+strings.Join(parts, "; ")+".", importJustificationLimit)
	return archproto.Import{
		ID:            importImportID(consumer, producer),
		Version:       1,
		From:          archproto.ExportReference{Domain: producer, Export: importExportID(producer)},
		As:            dottedDomain(consumer) + ".uses." + dottedDomain(producer),
		Mode:          archproto.ModeReference,
		Status:        archproto.StatusActive,
		Facts:         []string{importFactName(producer)},
		Justification: justification,
	}
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
	before   []byte
	created  bool
	manifest *archproto.Manifest
	row      ArchitectureImportManifest
	touched  bool
}

type importMerge struct {
	byDomain map[string]*importEntry
	at       string
	// generated maps each generated import ID to its consumer domain.
	generated map[string]bool
}

func newImportMerge(sources []archproto.ManifestSource, at string) *importMerge {
	merge := &importMerge{byDomain: map[string]*importEntry{}, at: at, generated: map[string]bool{}}
	for _, source := range sources {
		if source.Manifest == nil {
			continue
		}
		manifest := archproto.CanonicalManifest(source.Manifest)
		merge.byDomain[manifest.Domain] = &importEntry{
			path:     source.Path,
			manifest: manifest,
			row:      newImportRow(source.Path, manifest.Domain),
		}
	}
	return merge
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

func (merge *importMerge) add(domain *importDomain) {
	entry := merge.byDomain[domain.id]
	if entry == nil {
		manifestPath := joinArchitecturePath(merge.at, domain.id+"/"+archproto.ManifestFilename)
		if merge.at == "." {
			manifestPath = domain.id + "/" + archproto.ManifestFilename
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
	manifest := entry.manifest

	for _, project := range domain.projects {
		if !slices.Contains(manifest.Projects, project) {
			manifest.Projects = append(manifest.Projects, project)
			entry.row.AddedProjects = append(entry.row.AddedProjects, project)
		}
	}
	if domain.produces {
		export := generatedExport(domain)
		if !hasExport(manifest, export.ID) {
			manifest.Exports = append(manifest.Exports, export)
			entry.row.AddedExports = append(entry.row.AddedExports, export.ID)
		}
	}
	producers := make([]string, 0, len(domain.consumes))
	for producer := range domain.consumes {
		producers = append(producers, producer)
	}
	sort.Strings(producers)
	wanted := map[string]bool{}
	for _, producer := range producers {
		imported := generatedImport(domain.id, producer, domain.consumes[producer])
		wanted[producer] = true
		merge.generated[imported.ID] = true
		if !hasImport(manifest, imported.ID) {
			manifest.Imports = append(manifest.Imports, imported)
			entry.row.AddedImports = append(entry.row.AddedImports, imported.ID)
		}
	}
	for _, imported := range manifest.Imports {
		if imported.ID == importImportID(domain.id, imported.From.Domain) && !wanted[imported.From.Domain] {
			entry.row.NotInModel = append(entry.row.NotInModel, imported.ID)
		}
	}
}

// bind adds a binding for each observed edge between two domains the model
// relates, to the import the model declares for that pair. It is the
// architecture sync rule restricted to the generated imports.
func (merge *importMerge) bind(ws *workspace.Workspace) {
	sources := merge.sources()
	authorized := authorizedBindingKeys(sources)
	for _, edge := range DetectProjectDependencies(ws, sources) {
		if _, granted := authorized[syncBindingKey(edge.ConsumerProject, edge.ProducerProject)]; granted {
			continue
		}
		entry := merge.byDomain[edge.ConsumerDomain]
		if entry == nil || !entry.touched {
			continue
		}
		id := importImportID(edge.ConsumerDomain, edge.ProducerDomain)
		if !merge.generated[id] {
			continue
		}
		for index := range entry.manifest.Imports {
			imported := &entry.manifest.Imports[index]
			if imported.ID != id || imported.Status == archproto.StatusPlanned {
				continue
			}
			imported.Bindings = append(imported.Bindings, archproto.Binding{
				Kind:            archproto.BindingProjectDependency,
				ConsumerProject: edge.ConsumerProject,
				ProducerProject: edge.ProducerProject,
			})
			entry.row.AddedBindings = append(entry.row.AddedBindings, ArchitectureSyncBinding{
				Import: id, ConsumerProject: edge.ConsumerProject, ProducerProject: edge.ProducerProject,
			})
			authorized[syncBindingKey(edge.ConsumerProject, edge.ProducerProject)] = id
			break
		}
	}
}

func (merge *importMerge) sources() []archproto.ManifestSource {
	sources := make([]archproto.ManifestSource, 0, len(merge.byDomain))
	for _, entry := range merge.ordered() {
		sources = append(sources, archproto.ManifestSource{Path: entry.path, Manifest: entry.manifest})
	}
	for _, entry := range merge.byDomain {
		if !entry.touched {
			sources = append(sources, archproto.ManifestSource{Path: entry.path, Manifest: entry.manifest})
		}
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

func (entry *importEntry) finish(wsRoot string, apply bool) (ArchitectureImportManifest, error) {
	row := entry.row
	row.Created = entry.created
	row.Changed = entry.created || len(row.AddedProjects) > 0 || len(row.AddedExports) > 0 ||
		len(row.AddedImports) > 0 || len(row.AddedBindings) > 0
	if !row.Changed {
		return row, nil
	}
	contents, err := architectureManifestBytes(entry.manifest)
	if err != nil {
		return row, err
	}
	if !entry.created {
		before, readErr := os.ReadFile(filepath.Join(wsRoot, filepath.FromSlash(entry.path)))
		if readErr != nil {
			return row, protocolcli.Classify(fmt.Errorf("read %s: %w", entry.path, readErr), protocolcli.ErrInvalidConfig)
		}
		entry.before = before
	}
	row.Contents = string(contents)
	row.Diff = lineDiff(entry.path, string(entry.before), row.Contents)
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

func hasExport(manifest *archproto.Manifest, id string) bool {
	for _, export := range manifest.Exports {
		if export.ID == id {
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
