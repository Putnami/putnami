package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
)

const workspaceContextURI = "workspace://context"

func (s *Server) resourceDefs() []Resource {
	resources := []Resource{{
		URI:         workspaceContextURI,
		Name:        "Putnami workspace context",
		Description: "Live workspace graph, layout, publish channels, and pinned versions derived from the same workspace load path as the CLI.",
		MimeType:    "application/json",
	}}
	for _, entry := range s.guidanceResources() {
		resources = append(resources, entry.resource)
	}
	return resources
}

func (s *Server) handleResourcesRead(ctx context.Context, msg *rpcMessage) *rpcResponse {
	var p readResourceParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return s.fail(msg.ID, codeInvalidParams, "invalid params: "+err.Error())
		}
	}
	if p.URI != workspaceContextURI {
		for _, entry := range s.guidanceResources() {
			if entry.resource.URI != p.URI {
				continue
			}
			return s.ok(msg.ID, readResourceResult{Contents: []resourceContent{{
				URI: p.URI, MimeType: entry.resource.MimeType, Text: entry.content,
			}}})
		}
		return s.fail(msg.ID, codeInvalidParams, "unknown resource: "+p.URI)
	}
	data, err := s.workspaceContextResource(ctx)
	if err != nil {
		return s.fail(msg.ID, codeInternalError, fmt.Sprintf("read %s: %v", p.URI, err))
	}
	text, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return s.fail(msg.ID, codeInternalError, "encode resource: "+err.Error())
	}
	return s.ok(msg.ID, readResourceResult{
		Contents: []resourceContent{{
			URI:      p.URI,
			MimeType: "application/json",
			Text:     string(text),
		}},
	})
}

type guidanceResource struct {
	resource Resource
	content  string
}

func (s *Server) guidanceResources() []guidanceResource {
	entries := make([]guidanceResource, 0, len(s.opts.ExtensionGuidance)+len(s.opts.GuidanceIssues))
	available := make(map[string]bool, len(s.opts.ExtensionGuidance))
	for _, guide := range s.opts.ExtensionGuidance {
		if guide.Name == "" || guide.Version == "" || guide.Content == "" {
			continue
		}
		available[guide.Name+"\x00"+guide.Version] = true
		entries = append(entries, guidanceResource{
			resource: Resource{
				URI:         extensionGuidanceURI(guide.Name, guide.Version),
				Name:        guide.Name + "@" + guide.Version + " AI guidance",
				Description: "Exact AI.md bytes from the lock-pinned extension artifact.",
				MimeType:    "text/markdown",
			},
			content: guide.Content,
		})
	}
	for _, issue := range s.opts.GuidanceIssues {
		if issue.Name == "" || available[issue.Name+"\x00"+issue.Version] {
			continue
		}
		content, _ := json.Marshal(map[string]string{
			"status":    "unavailable",
			"extension": issue.Name,
			"version":   issue.Version,
			"reason":    issue.Reason,
			"fallback":  "continue with Putnami local workspace tools; do not substitute guidance from latest",
		})
		entries = append(entries, guidanceResource{
			resource: Resource{
				URI:         extensionGuidanceURI(issue.Name, issue.Version),
				Name:        guidanceStatusResourceName(issue),
				Description: "The exact lock-pinned AI.md is unavailable; read for the bounded failure reason.",
				MimeType:    "application/json",
			},
			content: string(content),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].resource.URI < entries[j].resource.URI })
	return entries
}

// guidanceStatusResourceName avoids rendering a dangling "@" for an extension
// whose lock pin is absent: there is no version to report, and naming one that
// does not exist is exactly the substitution this resource exists to refuse.
func guidanceStatusResourceName(issue GuidanceIssue) string {
	if issue.Version == "" {
		return issue.Name + " AI guidance status (no lock pin)"
	}
	return issue.Name + "@" + issue.Version + " AI guidance status"
}

func extensionGuidanceURI(name, version string) string {
	values := url.Values{}
	values.Set("name", name)
	values.Set("version", version)
	return "putnami://extension-guidance/AI.md?" + values.Encode()
}
