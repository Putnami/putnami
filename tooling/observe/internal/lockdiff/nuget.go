package lockdiff

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// EcosystemNuGet is the ecosystem of the .NET lockfiles.
const EcosystemNuGet = "nuget"

// NuGet files: the lock file NuGet writes beside a project when lock files
// are on, and the central package versions of a repository.
const (
	NuGetLock       = "packages.lock.json"
	CentralPackages = "Directory.Packages.props"
)

// ReadNuGetLock reads a NuGet packages.lock.json: each package each target
// framework of its dependencies resolves, direct, transitive or central, at
// its resolved version. A Project entry is the repository's own project and
// is left out.
func ReadNuGetLock(data []byte) (Packages, error) {
	var lock struct {
		Dependencies map[string]map[string]struct {
			Type     string `json:"type"`
			Resolved string `json:"resolved"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &lock); err != nil {
		return nil, fmt.Errorf("packages.lock.json is not JSON: %w", err)
	}
	out := Packages{}
	for _, framework := range lock.Dependencies {
		for name, entry := range framework {
			if entry.Type != "Project" {
				out.add(name, entry.Resolved)
			}
		}
	}
	return out, nil
}

// ReadCentralPackages reads the PackageVersion items of a
// Directory.Packages.props, central package management's versions: each item
// whose Include and Version are literal, without an MSBuild property.
func ReadCentralPackages(data []byte) (Packages, error) {
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, utf8BOM)))
	out := Packages{}
	root := true
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if root {
				return nil, errors.New("Directory.Packages.props holds no Project element")
			}
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("Directory.Packages.props is not XML: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if root {
			if start.Name.Local != "Project" {
				return nil, fmt.Errorf("Directory.Packages.props has the root %s, want Project", start.Name.Local)
			}
			root = false
			continue
		}
		if start.Name.Local != "PackageVersion" {
			continue
		}
		var include, version string
		for _, attr := range start.Attr {
			switch attr.Name.Local {
			case "Include":
				include = strings.TrimSpace(attr.Value)
			case "Version":
				version = strings.TrimSpace(attr.Value)
			}
		}
		if !strings.Contains(include, "$(") && !strings.Contains(version, "$(") {
			out.add(include, version)
		}
	}
}

var utf8BOM = []byte("\xef\xbb\xbf")
