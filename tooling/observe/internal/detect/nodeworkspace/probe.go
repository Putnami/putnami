package nodeworkspace

import (
	"encoding/json"
	"path"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// Extension names the provider of the probe result while the detector lives
// in @putnami/observe.
const Extension = "@putnami/observe"

// Ecosystem is the component ecosystem of a Node workspace member.
const Ecosystem = "node"

// ReportPatterns are where a member writes its reports by default: Jest and
// Vitest JUnit reporters, Istanbul, c8 and V8 coverage. Each pattern is
// relative to the member directory; a report's kind is read from its content.
var ReportPatterns = Reports{
	JUnit: []string{
		"junit.xml",
		"junit/*.xml",
		"reports/junit.xml",
		"reports/junit/*.xml",
		"test-results/*.xml",
	},
	Coverage: []string{
		"coverage/lcov.info",
		"coverage/coverage-final.json",
		"coverage/cobertura-coverage.xml",
		"coverage/clover.xml",
		"reports/coverage/lcov.info",
		"reports/coverage/coverage-final.json",
		"reports/coverage/cobertura-coverage.xml",
		"reports/coverage/clover.xml",
	},
}

// Reports lists report path patterns, relative to a member directory.
type Reports struct {
	JUnit    []string `json:"junit"`
	Coverage []string `json:"coverage"`
}

// Metadata is the provider-owned metadata of each member in a probe result.
type Metadata struct {
	// Ecosystem is always node.
	Ecosystem string `json:"ecosystem"`
	// Manager is npm, pnpm or yarn.
	Manager string `json:"manager"`
	// Lockfile is the repo-relative root lockfile whose diff is the member's
	// dependency change. Absent when the workspace has none.
	Lockfile string `json:"lockfile,omitempty"`
	// Reports are where the member writes its test and coverage reports.
	Reports Reports `json:"reports"`
}

// DecodeMetadata reads the metadata of a probe project this detector answered.
func DecodeMetadata(raw json.RawMessage) (Metadata, error) {
	var m Metadata
	err := json.Unmarshal(raw, &m)
	return m, err
}

// Detect reads the workspace rooted at root and answers for all its members.
// ok is false when root declares no Node workspace.
func Detect(root string) (result wsproto.ProbeResult, ok bool) {
	ws, ok, diags := Read(root)
	if !ok {
		return wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: Extension, Diagnostics: diags}, false
	}
	result = answer(root, ws, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: Extension, Paths: ws.Members, DependencySources: true,
	})
	result.Diagnostics = append(diags, result.Diagnostics...)
	return result, true
}

// Probe answers a workspace probe request against the tree rooted at root;
// bound to a root, it is a wsproto.ProbeAnswerFunc. It reports the members
// among the request's paths, and a workspace it cannot read as diagnostics,
// never as an error.
func Probe(root string, request wsproto.ProbeRequest) (wsproto.ProbeResult, error) {
	ws, ok, diags := Read(root)
	if !ok {
		return wsproto.ProbeResult{Version: wsproto.ProbeProtocolVersion, Extension: Extension, Diagnostics: diags}, nil
	}
	result := answer(root, ws, request)
	result.Diagnostics = append(diags, result.Diagnostics...)
	return result, nil
}

// answer renders one project per member the request names.
func answer(root string, ws Workspace, request wsproto.ProbeRequest) wsproto.ProbeResult {
	result := wsproto.ProbeResult{
		Version:      wsproto.ProbeProtocolVersion,
		Extension:    Extension,
		WatchedFiles: ws.WatchedFiles,
	}
	asked := map[string]bool{}
	for _, p := range request.Paths {
		if normalized, ok := wsproto.NormalizeProbePath(p); ok {
			asked[normalized] = true
		}
	}

	manifests := make(map[string]*Manifest, len(ws.Members))
	byName := map[string]string{}
	isMember := map[string]bool{}
	for _, dir := range ws.Members {
		manifest, err := ReadManifest(root, dir)
		if err != nil || manifest == nil {
			if err != nil {
				result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeManifestInvalid, path.Join(dir, PackageJSON),
					"the package.json of %s cannot be read and the package is left out: %v", dir, err))
			}
			continue
		}
		manifests[dir] = manifest
		isMember[dir] = true
		// Members are sorted, so the first of two packages with one name wins
		// whatever the walk order.
		if first, taken := byName[manifest.Name]; manifest.Name != "" && taken {
			result.Diagnostics = append(result.Diagnostics, diag.Warningf(CodeManifestInvalid, path.Join(dir, PackageJSON),
				"%s and %s are both named %s; a dependency on that name resolves to %s", first, dir, manifest.Name, first))
		} else if manifest.Name != "" {
			byName[manifest.Name] = dir
		}
	}

	metadata, _ := json.Marshal(Metadata{Ecosystem: Ecosystem, Manager: ws.Manager, Lockfile: ws.Lockfile, Reports: ReportPatterns})
	for _, dir := range ws.Members {
		manifest, ok := manifests[dir]
		if !ok || !asked[dir] {
			continue
		}
		project := wsproto.ProbeProject{
			Path:         dir,
			SourceName:   manifest.Name,
			SourceFile:   path.Join(dir, PackageJSON),
			Version:      manifest.Version,
			Dependencies: edges(dir, manifest, ws.LinkRanges, byName, manifests, isMember),
			WatchedFiles: []string{path.Join(dir, PackageJSON)},
			Metadata:     metadata,
		}
		if request.DependencySources {
			project.DependencySources = make(map[string]wsproto.DependencySource, len(project.Dependencies))
			for _, dep := range project.Dependencies {
				project.DependencySources[dep] = wsproto.DependencySourcePackageJSON
			}
		}
		result.Projects = append(result.Projects, project)
	}
	wsproto.NormalizeProbeResult(&result)
	return result
}

// edges returns the member directories a member depends on, sorted.
func edges(dir string, m *Manifest, linkRanges bool, byName map[string]string, manifests map[string]*Manifest, isMember map[string]bool) []string {
	set := map[string]bool{}
	for _, deps := range []map[string]string{m.Dependencies, m.DevDependencies, m.OptionalDependencies, m.PeerDependencies} {
		for name, spec := range deps {
			if target := edge(dir, name, spec, linkRanges, byName, manifests, isMember); target != "" && target != dir {
				set[target] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for target := range set {
		out = append(out, target)
	}
	sort.Strings(out)
	return out
}

// edge returns the member a dependency resolves to, or "" for an external one.
func edge(dir, name, spec string, linkRanges bool, byName map[string]string, manifests map[string]*Manifest, isMember map[string]bool) string {
	spec = strings.TrimSpace(spec)
	if rest, ok := strings.CutPrefix(spec, "workspace:"); ok {
		if strings.HasPrefix(rest, "./") || strings.HasPrefix(rest, "../") {
			return relativeMember(dir, rest, isMember)
		}
		// An alias names another member: "workspace:other@*".
		if at := strings.LastIndex(rest, "@"); at > 0 {
			name = rest[:at]
		}
		return byName[name]
	}
	for _, protocol := range []string{"file:", "link:", "portal:"} {
		if rest, ok := strings.CutPrefix(spec, protocol); ok {
			return relativeMember(dir, rest, isMember)
		}
	}
	target, ok := byName[name]
	if !ok || !linkRanges {
		return ""
	}
	if satisfies(manifests[target].Version, spec) {
		return target
	}
	return ""
}

// relativeMember resolves a path written in a member's manifest to a member.
func relativeMember(dir, rel string, isMember map[string]bool) string {
	target, ok := wsproto.NormalizeProbePath(path.Join(dir, strings.TrimSuffix(rel, "/")))
	if !ok || !isMember[target] {
		return ""
	}
	return target
}
