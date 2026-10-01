package output

import (
	"fmt"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/jobs"
)

// publishedArtifact is one artifact published (or, on a dry run, prepared)
// during the session. It is reconstructed from the "published" artifact
// events that publish jobs emit.
type publishedArtifact struct {
	registry string // npm, docker, go, archives, config
	name     string // registry coordinate without a version
	version  string
	tags     []string
	// contentStatus records, for docker artifacts, whether the content was
	// uploaded this session ("pushed") or already existed in the registry and
	// only received new references ("retagged"). Empty for other channels.
	contentStatus string
	dryRun        bool
}

type labelCount struct {
	label string
	count int
}

type publishedSummary struct {
	total             int
	dryRun            int
	dockerPushed      int
	dockerRetagged    int
	dockerReused      int
	registryCounts    []labelCount
	versionCounts     []labelCount
	exactVersions     []labelCount
	goVersionCounts   []labelCount
	tagCounts         []labelCount
	hasMixedGoVersion bool
}

// collectPublished scans job results for "published" artifact events and
// returns the artifacts de-duplicated and sorted by registry then coordinate,
// giving the end-of-session summary a stable order.
func collectPublished(results map[string]*jobs.JobResult) []publishedArtifact {
	var out []publishedArtifact
	seen := make(map[string]bool)

	for _, res := range results {
		for _, ev := range res.Events {
			if ev.Type != jobs.EventTypeArtifact {
				continue
			}
			if stringField(ev.Data, "kind") != "published" {
				continue
			}
			a := publishedArtifact{
				registry:      stringField(ev.Data, "registry"),
				name:          stringField(ev.Data, "name"),
				version:       stringField(ev.Data, "version"),
				tags:          stringSlice(ev.Data["tags"]),
				contentStatus: stringField(ev.Data, "contentStatus"),
			}
			a.dryRun, _ = ev.Data["dryRun"].(bool)

			key := a.registry + "\x00" + a.name + "\x00" + a.version
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, a)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].registry != out[j].registry {
			return out[i].registry < out[j].registry
		}
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].version < out[j].version
	})
	return out
}

func summarizePublished(published []publishedArtifact) publishedSummary {
	s := publishedSummary{total: len(published)}
	var registries, versions, exactVersions, goVersions, nonGoVersions, tags []string

	for _, a := range published {
		if a.dryRun {
			s.dryRun++
		}
		switch a.contentStatus {
		case "pushed":
			s.dockerPushed++
		case "retagged":
			s.dockerRetagged++
		case "reused":
			s.dockerReused++
		}
		if a.registry != "" {
			registries = append(registries, a.registry)
		}
		if a.version != "" {
			exactVersions = append(exactVersions, a.version)
			versions = append(versions, normalizedPublishedVersion(a))
			if a.registry == "go" {
				goVersions = append(goVersions, a.version)
			} else {
				nonGoVersions = append(nonGoVersions, a.version)
			}
		}
		tags = append(tags, a.tags...)
	}

	s.registryCounts = countLabels(registries)
	s.exactVersions = countLabels(exactVersions)
	if len(nonGoVersions) > 0 {
		s.versionCounts = countLabels(versions)
	} else {
		s.versionCounts = s.exactVersions
	}
	s.goVersionCounts = countLabels(goVersions)
	s.hasMixedGoVersion = len(s.versionCounts) == 1 && len(s.exactVersions) > 1 && len(s.goVersionCounts) > 0
	s.tagCounts = countLabels(tags)
	return s
}

func normalizedPublishedVersion(a publishedArtifact) string {
	if a.registry == "go" && len(a.version) > 1 && a.version[0] == 'v' && isASCIIDigit(a.version[1]) {
		return a.version[1:]
	}
	return a.version
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func countLabels(values []string) []labelCount {
	counts := make(map[string]int)
	order := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if counts[v] == 0 {
			order = append(order, v)
		}
		counts[v]++
	}
	out := make([]labelCount, 0, len(order))
	for _, label := range order {
		out = append(out, labelCount{label: label, count: counts[label]})
	}
	return out
}

func formatPublishedSummaryLines(s publishedSummary) []string {
	if s.total == 0 {
		return nil
	}

	var lines []string
	if versionLine := publishedVersionLine(s); versionLine != "" {
		lines = append(lines, versionLine)
	}
	if s.hasMixedGoVersion {
		lines = append(lines, "Go module "+countedLine("version", "versions", s.goVersionCounts, 3))
	}

	artifactParts := []string{fmt.Sprintf("%s %s", formatThousands(s.total), artifactNoun(s.total))}
	if s.dryRun == s.total {
		artifactParts[0] += " (dry run)"
	} else if s.dryRun > 0 {
		artifactParts = append(artifactParts, fmt.Sprintf("%s dry run", formatThousands(s.dryRun)))
	}
	if len(s.registryCounts) > 0 {
		artifactParts = append(artifactParts, formatRegistryCounts(s.registryCounts, 8)...)
	}
	lines = append(lines, "Artifacts: "+strings.Join(artifactParts, " · "))

	// Distinguish docker images whose content was uploaded this session from
	// ones that already existed and only received new references — with
	// content-addressed publishing, "retagged" means the digest did not change.
	if s.dockerPushed > 0 || s.dockerRetagged > 0 || s.dockerReused > 0 {
		var parts []string
		if s.dockerPushed > 0 {
			parts = append(parts, fmt.Sprintf("%d pushed", s.dockerPushed))
		}
		if s.dockerRetagged > 0 {
			parts = append(parts, fmt.Sprintf("%d unchanged (retagged)", s.dockerRetagged))
		}
		if s.dockerReused > 0 {
			parts = append(parts, fmt.Sprintf("%d unchanged (reused)", s.dockerReused))
		}
		lines = append(lines, "Docker content: "+strings.Join(parts, " · "))
	}

	if len(s.tagCounts) > 0 {
		lines = append(lines, "Tags: "+strings.Join(formatLabelCounts(s.tagCounts, 8, true), " · "))
	}

	return lines
}

func publishedVersionLine(s publishedSummary) string {
	if len(s.versionCounts) == 0 {
		return ""
	}
	if len(s.exactVersions) == 1 {
		return "Version: " + s.exactVersions[0].label
	}
	return countedLine("Version", "Versions", s.versionCounts, 5)
}

func countedLine(singular, plural string, counts []labelCount, max int) string {
	if len(counts) == 1 {
		return singular + ": " + counts[0].label
	}
	return plural + ": " + strings.Join(formatLabelCounts(counts, max, true), " · ")
}

func artifactNoun(n int) string {
	if n == 1 {
		return "artifact"
	}
	return "artifacts"
}

func formatRegistryCounts(counts []labelCount, max int) []string {
	return formatLabelCounts(counts, max, false)
}

func formatLabelCounts(counts []labelCount, max int, parens bool) []string {
	limit := len(counts)
	if max > 0 && limit > max {
		limit = max
	}
	parts := make([]string, 0, limit+1)
	for _, c := range counts[:limit] {
		if parens {
			parts = append(parts, fmt.Sprintf("%s (%s)", c.label, formatThousands(c.count)))
		} else {
			parts = append(parts, fmt.Sprintf("%s %s", c.label, formatThousands(c.count)))
		}
	}
	if max > 0 && len(counts) > max {
		parts = append(parts, fmt.Sprintf("+%d more", len(counts)-max))
	}
	return parts
}

// stringField reads a string value from event data, returning "" if absent.
func stringField(data map[string]any, key string) string {
	s, _ := data[key].(string)
	return s
}

// stringSlice coerces a JSON-decoded value into a string slice. Tags arrive as
// []any after JSONL parsing, so handle that alongside a native []string.
func stringSlice(v any) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
