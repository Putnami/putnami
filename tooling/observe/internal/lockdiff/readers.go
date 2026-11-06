package lockdiff

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.putnami.dev/tooling/observe/internal/toml"
)

// ReadPackageLock reads an npm package-lock.json: the packages map of
// lockfile versions 2 and 3, else the nested dependencies of version 1. A
// package's name is the part of its install path after the last
// node_modules/; links and the workspace's own packages are left out.
func ReadPackageLock(data []byte) (Packages, error) {
	var lock struct {
		Packages map[string]struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Link    bool   `json:"link"`
		} `json:"packages"`
		Dependencies map[string]npmV1Dependency `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("package-lock.json is not JSON: %w", err)
	}
	out := Packages{}
	if lock.Packages != nil {
		for key, entry := range lock.Packages {
			at := strings.LastIndex(key, "node_modules/")
			if at < 0 || entry.Link {
				continue
			}
			name := key[at+len("node_modules/"):]
			if entry.Name != "" {
				name = entry.Name
			}
			out.add(name, entry.Version)
		}
		return out, nil
	}
	var walk func(map[string]npmV1Dependency)
	walk = func(deps map[string]npmV1Dependency) {
		for name, dep := range deps {
			if !strings.HasPrefix(dep.Version, "file:") && !strings.HasPrefix(dep.Version, "link:") {
				out.add(name, dep.Version)
			}
			walk(dep.Dependencies)
		}
	}
	walk(lock.Dependencies)
	return out, nil
}

type npmV1Dependency struct {
	Version      string                     `json:"version"`
	Dependencies map[string]npmV1Dependency `json:"dependencies"`
}

// ReadPNPMLock reads the keys of the top-level packages section of a
// pnpm-lock.yaml: name@version from lockfile version 6, with a leading slash
// in version 6, and /name/version before it. A peer suffix in parentheses is
// dropped. A local package, whose key or version starts with file: or link:,
// is left out.
func ReadPNPMLock(data []byte) (Packages, error) {
	out := Packages{}
	inPackages, oldFormat := false, false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \r")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			if value, ok := strings.CutPrefix(line, "lockfileVersion:"); ok {
				v := strings.Trim(strings.TrimSpace(value), `'"`)
				oldFormat = strings.HasPrefix(v, "5") || strings.HasPrefix(v, "4") || strings.HasPrefix(v, "3")
			}
			inPackages = line == "packages:"
			continue
		}
		if !inPackages || !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		key := strings.TrimSpace(line)
		key = strings.TrimSuffix(strings.TrimSuffix(key, " {}"), ":")
		key = strings.Trim(key, `'"`)
		if cut := strings.IndexByte(key, '('); cut > 0 {
			key = key[:cut]
		}
		key = strings.TrimPrefix(key, "/")
		if localPNPMSpec(key) {
			continue
		}
		var name, version string
		if oldFormat {
			at := strings.LastIndexByte(key, '/')
			if at <= 0 {
				continue
			}
			name, version = key[:at], key[at+1:]
			if cut := strings.IndexByte(version, '_'); cut > 0 {
				version = version[:cut]
			}
		} else {
			at := strings.LastIndexByte(key, '@')
			if at <= 0 {
				continue
			}
			name, version = key[:at], key[at+1:]
		}
		if localPNPMSpec(version) {
			continue
		}
		out.add(name, version)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("pnpm-lock.yaml cannot be read: %w", err)
	}
	return out, nil
}

// localPNPMSpec reports whether a pnpm-lock.yaml key or version names a local
// package: a file: or link: specifier.
func localPNPMSpec(s string) bool {
	return strings.HasPrefix(s, "file:") || strings.HasPrefix(s, "link:")
}

// ReadYarnLock reads a yarn.lock, classic or Berry: each unindented entry
// names one or more descriptors, and its indented version line the version
// they resolve to. Workspace and link entries are left out.
func ReadYarnLock(data []byte) (Packages, error) {
	out := Packages{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var names []string
	skip := false
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			names, skip = nil, false
			header := strings.TrimSuffix(line, ":")
			if strings.HasPrefix(header, "__metadata") {
				continue
			}
			for _, descriptor := range strings.Split(header, ",") {
				descriptor = strings.Trim(strings.TrimSpace(descriptor), `"`)
				at := strings.LastIndexByte(descriptor, '@')
				if at <= 0 {
					continue
				}
				spec := descriptor[at+1:]
				if strings.HasPrefix(spec, "workspace:") || strings.HasPrefix(spec, "link:") || strings.HasPrefix(spec, "portal:") || strings.HasPrefix(spec, "file:") {
					skip = true
				}
				name := descriptor[:at]
				if !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
			continue
		}
		value, ok := strings.CutPrefix(trimmed, "version ")
		if !ok {
			value, ok = strings.CutPrefix(trimmed, "version: ")
		}
		if !ok || skip || strings.HasPrefix(line, "    ") {
			continue
		}
		version := strings.Trim(strings.TrimSpace(value), `"`)
		for _, name := range names {
			out.add(name, version)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("yarn.lock cannot be read: %w", err)
	}
	return out, nil
}

// ReadComposerLock reads the packages and packages-dev of a composer.lock.
// A package installed from a path repository is the repository's own code
// and is left out.
func ReadComposerLock(data []byte) (Packages, error) {
	type pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Dist    struct {
			Type string `json:"type"`
		} `json:"dist"`
	}
	var lock struct {
		Packages    []pkg `json:"packages"`
		PackagesDev []pkg `json:"packages-dev"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("composer.lock is not JSON: %w", err)
	}
	out := Packages{}
	for _, p := range append(lock.Packages, lock.PackagesDev...) {
		if p.Dist.Type == "path" {
			continue
		}
		out.add(p.Name, p.Version)
	}
	return out, nil
}

// ReadUVLock reads the [[package]] entries of a uv.lock. A package whose
// source is editable, virtual, a directory or a path, such as a workspace
// member, is the repository's own code and is left out.
func ReadUVLock(data []byte) (Packages, error) {
	return readPythonLock(UVLock, data, func(p toml.Table) bool {
		source, _ := p["source"].(toml.Table)
		for _, local := range []string{"editable", "virtual", "directory", "path"} {
			if _, ok := source[local]; ok {
				return true
			}
		}
		return false
	})
}

// ReadPoetryLock reads the [[package]] entries of a poetry.lock. A package
// whose [package.source] is a directory or a local file, or that is installed
// in develop mode, is the repository's own code and is left out.
func ReadPoetryLock(data []byte) (Packages, error) {
	return readPythonLock(PoetryLock, data, func(p toml.Table) bool {
		typ := toml.String(p, "source", "type")
		return typ == "directory" || typ == "file" || p["develop"] == true
	})
}

func readPythonLock(file string, data []byte, local func(toml.Table) bool) (Packages, error) {
	doc, err := toml.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s is not TOML: %w", file, err)
	}
	out := Packages{}
	for _, p := range toml.Tables(doc["package"]) {
		if local(p) {
			continue
		}
		out.add(PyPIName(toml.String(p, "name")), toml.String(p, "version"))
	}
	return out, nil
}

var pypiSeparators = regexp.MustCompile(`[-_.]+`)

// PyPIName is a Python package name in its normalized form (PEP 503): lower
// case, each run of -, _ and . replaced by one -. Two spellings of one
// package, such as Flask_Login and flask-login, compare equal.
func PyPIName(name string) string {
	return pypiSeparators.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
}

// ReadPEXLock reads a PEX JSON lock, the lockfile Pants writes for a Python
// resolve: each locked requirement of each locked resolve, by its project
// name in its normalized form. The lines that start with //, the header Pants
// writes before the JSON document, are no content.
func ReadPEXLock(data []byte) (Packages, error) {
	var body bytes.Buffer
	for line := range bytes.Lines(data) {
		if !bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte("//")) {
			body.Write(line)
		}
	}
	var lock struct {
		LockedResolves *[]struct {
			LockedRequirements []struct {
				ProjectName string `json:"project_name"`
				Version     string `json:"version"`
			} `json:"locked_requirements"`
		} `json:"locked_resolves"`
	}
	if err := json.Unmarshal(body.Bytes(), &lock); err != nil {
		return nil, fmt.Errorf("the PEX lock is not JSON after its // header: %w", err)
	}
	if lock.LockedResolves == nil {
		return nil, fmt.Errorf("the PEX lock has no locked_resolves")
	}
	out := Packages{}
	for _, resolve := range *lock.LockedResolves {
		for _, r := range resolve.LockedRequirements {
			out.add(PyPIName(r.ProjectName), r.Version)
		}
	}
	return out, nil
}
