// Package maven is the Maven detector of @putnami/observe: it finds the
// modules of a Maven build and the dependencies between them, without
// running Maven.
//
// The read is static. The root pom.xml and the modules it lists, recursively,
// are the build; each POM that packages code, any packaging but pom, is a
// component. A dependency whose groupId and artifactId name another module is
// an edge. Parent POMs outside the repository, BOMs and inherited properties
// are not resolved for the edges.
//
// Each POM read is also a lockfile of declared versions: Detection.ReadDeclared
// lists the versions its dependencies and its parent state, through the
// properties of its parent POMs in the repository.
package maven

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Ecosystem is the evidence ecosystem slug of a Maven component.
const Ecosystem = "maven"

// POM is the file name of a module's POM.
const POM = "pom.xml"

// Diagnostic codes.
const (
	CodeModuleMissing = "observe.maven_module_missing"
	CodePOMInvalid    = "observe.maven_pom_invalid"
	CodeWalkBounded   = "observe.maven_walk_bounded"
)

// Bounds of one detection: the POMs read and the bytes of each.
const (
	MaxPOMs     = 10000
	MaxPOMBytes = 16 << 20
)

// Component is one Maven module.
type Component struct {
	// ID is the module directory relative to the repository root, or ".".
	ID string
	// Name is groupId:artifactId.
	Name string
	// DependsOn lists the ids of the modules it depends on, sorted.
	DependsOn []string
}

// Detection is what the detector found in one repository.
type Detection struct {
	// Components are sorted by ID; empty outside a Maven build.
	Components []Component
	// POMs are the repository paths of the POMs read, aggregators included,
	// sorted.
	POMs []string
	// Modules are the groupId:artifactId of the POMs read, sorted.
	Modules     []string
	Diagnostics []diag.Diagnostic
	// poms are the POMs read, by repository path; byCoordinates the path of
	// the first POM of each groupId:artifactId.
	poms          map[string]pom
	byCoordinates map[string]string
	// root is the repository's top directory.
	root string
}

// IsPOM reports whether a repository path is a POM of the build.
func (d Detection) IsPOM(file string) bool {
	_, found := slices.BinarySearch(d.POMs, file)
	return found
}

// IsModule reports whether groupId:artifactId names a POM of the build.
func (d Detection) IsModule(coordinates string) bool {
	_, found := slices.BinarySearch(d.Modules, coordinates)
	return found
}

type dependency struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
}

type modules struct {
	Modules     []string `xml:"modules>module"`
	Subprojects []string `xml:"subprojects>subproject"`
}

type project struct {
	XMLName    xml.Name
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Packaging  string `xml:"packaging"`
	Parent     struct {
		GroupID    string `xml:"groupId"`
		ArtifactID string `xml:"artifactId"`
		Version    string `xml:"version"`
	} `xml:"parent"`
	Properties properties `xml:"properties"`
	modules
	Dependencies []dependency `xml:"dependencies>dependency"`
	Managed      []dependency `xml:"dependencyManagement>dependencies>dependency"`
	Profiles     []struct {
		modules
		Dependencies []dependency `xml:"dependencies>dependency"`
		Managed      []dependency `xml:"dependencyManagement>dependencies>dependency"`
		Properties   properties   `xml:"properties"`
	} `xml:"profiles>profile"`
}

// properties are the entries of a properties element.
type properties struct {
	Entries []struct {
		XMLName xml.Name
		Value   string `xml:",chardata"`
	} `xml:",any"`
}

// pom is one POM read.
type pom struct {
	file    string
	dir     string
	project project
	groupID string
}

// Detect reads the Maven build at root. A root without pom.xml is no Maven
// build and gets an empty detection.
func Detect(root string) Detection {
	out := Detection{root: root}
	if info, err := os.Stat(filepath.Join(root, POM)); err != nil || !info.Mode().IsRegular() {
		return out
	}
	var poms []pom
	visited := map[string]bool{POM: true}
	queue := []string{POM}
	for len(queue) > 0 {
		file := queue[0]
		queue = queue[1:]
		if len(poms) == MaxPOMs {
			out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodeWalkBounded, file, "the Maven detector stopped after %d POMs", MaxPOMs))
			break
		}
		p, err := readPOM(root, file)
		if err != nil {
			out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodePOMInvalid, file, "the POM %s cannot be read, so it is no component and its modules are not read: %v", file, err))
			continue
		}
		poms = append(poms, p)
		out.POMs = append(out.POMs, file)
		if out.poms == nil {
			out.poms, out.byCoordinates = map[string]pom{}, map[string]string{}
		}
		out.poms[file] = p
		if coordinates := p.coordinates(); !slices.Contains(out.Modules, coordinates) {
			out.Modules = append(out.Modules, coordinates)
			out.byCoordinates[coordinates] = file
		}
		listed := append(append([]string(nil), p.project.Modules...), p.project.Subprojects...)
		for _, profile := range p.project.Profiles {
			listed = append(append(listed, profile.Modules...), profile.Subprojects...)
		}
		for _, module := range listed {
			next, ok := modulePOM(p.dir, module)
			if !ok {
				out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodeModuleMissing, file, "the module %s of %s lies outside the repository and is not read", strings.TrimSpace(module), file))
				continue
			}
			if visited[next] {
				continue
			}
			visited[next] = true
			if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(next))); err != nil || !info.Mode().IsRegular() {
				out.Diagnostics = append(out.Diagnostics, diag.Warningf(CodeModuleMissing, file, "the module %s of %s has no POM at %s", strings.TrimSpace(module), file, next))
				continue
			}
			queue = append(queue, next)
		}
	}
	// The coordinates of the modules that package code: a dependency on an
	// aggregator or a BOM, of packaging pom, is no edge.
	byCoordinates := map[string]string{}
	components := map[string]*Component{}
	for _, p := range poms {
		if p.packaging() == "pom" {
			continue
		}
		coordinates := p.groupID + ":" + strings.TrimSpace(p.project.ArtifactID)
		if _, taken := components[p.dir]; taken {
			continue
		}
		components[p.dir] = &Component{ID: p.dir, Name: coordinates}
		if _, taken := byCoordinates[coordinates]; !taken {
			byCoordinates[coordinates] = p.dir
		}
	}
	for _, p := range poms {
		c := components[p.dir]
		if c == nil || c.Name != p.groupID+":"+strings.TrimSpace(p.project.ArtifactID) {
			continue
		}
		deps := append([]dependency(nil), p.project.Dependencies...)
		for _, profile := range p.project.Profiles {
			deps = append(deps, profile.Dependencies...)
		}
		edges := map[string]bool{}
		for _, d := range deps {
			coordinates := p.resolve(d.GroupID) + ":" + p.resolve(d.ArtifactID)
			if target, ok := byCoordinates[coordinates]; ok && target != c.ID {
				edges[target] = true
			}
		}
		for target := range edges {
			c.DependsOn = append(c.DependsOn, target)
		}
		sort.Strings(c.DependsOn)
	}
	for _, c := range components {
		out.Components = append(out.Components, *c)
	}
	sort.Slice(out.Components, func(i, j int) bool { return out.Components[i].ID < out.Components[j].ID })
	sort.Strings(out.POMs)
	sort.Strings(out.Modules)
	return out
}

// modulePOM returns the repository path of the POM a module entry names: the
// pom.xml of its directory, or the .xml file it names.
func modulePOM(dir, module string) (string, bool) {
	module = strings.TrimSpace(strings.ReplaceAll(module, `\`, "/"))
	if module == "" {
		return "", false
	}
	joined := path.Join(dir, module)
	if !strings.HasSuffix(strings.ToLower(joined), ".xml") {
		joined = path.Join(joined, POM)
	}
	if joined == ".." || strings.HasPrefix(joined, "../") || path.IsAbs(module) {
		return "", false
	}
	return joined, true
}

func readPOM(root, file string) (pom, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return pom{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxPOMBytes+1))
	if err != nil {
		return pom{}, err
	}
	if len(data) > MaxPOMBytes {
		return pom{}, errors.New("the POM is larger than 16 MiB")
	}
	return parsePOM(file, data)
}

// parsePOM reads the content of the POM at a repository path.
func parsePOM(file string, data []byte) (pom, error) {
	var p project
	if err := xml.NewDecoder(bytes.NewReader(data)).Decode(&p); err != nil {
		return pom{}, err
	}
	if p.XMLName.Local != "project" {
		return pom{}, errors.New("its root is " + p.XMLName.Local + ", not project")
	}
	if strings.TrimSpace(p.ArtifactID) == "" {
		return pom{}, errors.New("it states no artifactId")
	}
	dir, _ := wsproto.NormalizeProbePath(path.Dir(file))
	out := pom{file: file, dir: dir, project: p}
	out.groupID = strings.TrimSpace(p.GroupID)
	if out.groupID == "" {
		out.groupID = strings.TrimSpace(p.Parent.GroupID)
	}
	out.groupID = out.resolve(out.groupID)
	return out, nil
}

// coordinates is the POM's groupId:artifactId.
func (p pom) coordinates() string {
	return p.groupID + ":" + strings.TrimSpace(p.project.ArtifactID)
}

func (p pom) packaging() string {
	if packaging := strings.TrimSpace(p.project.Packaging); packaging != "" {
		return packaging
	}
	return "jar"
}

// property finds a ${name} reference.
var property = regexp.MustCompile(`\$\{([^}]+)\}`)

// resolve replaces the ${...} references a static read can: the project's
// and its parent's groupId and artifactId, and the POM's own properties.
func (p pom) resolve(value string) string {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "${") {
		return value
	}
	return property.ReplaceAllStringFunc(value, func(ref string) string {
		name := ref[2 : len(ref)-1]
		switch name {
		case "project.groupId", "pom.groupId", "groupId":
			if p.groupID != "" {
				return p.groupID
			}
			return strings.TrimSpace(p.project.Parent.GroupID)
		case "project.parent.groupId", "parent.groupId":
			return strings.TrimSpace(p.project.Parent.GroupID)
		case "project.artifactId", "pom.artifactId", "artifactId":
			return strings.TrimSpace(p.project.ArtifactID)
		}
		for _, entry := range p.project.Properties.Entries {
			if entry.XMLName.Local == name && !strings.Contains(entry.Value, "${") {
				return strings.TrimSpace(entry.Value)
			}
		}
		return ref
	})
}
