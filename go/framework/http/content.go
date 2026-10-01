package http

import (
	"mime"
	"sort"
	"strconv"
	"strings"
)

// MediaType represents a parsed media type from the Accept header.
type MediaType struct {
	Type    string  // e.g., "application"
	Subtype string  // e.g., "json"
	Quality float64 // 0.0–1.0 (from q= parameter)
	Full    string  // e.g., "application/json"
}

// ConcreteMediaType reports whether value is a single concrete media type
// safe to send as a Content-Type header: it parses, names both a type and a
// subtype, contains no wildcard, and carries no CR or LF. Parameters are
// allowed.
func ConcreteMediaType(value string) bool {
	if strings.ContainsAny(value, "\r\n") {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || strings.Contains(mediaType, "*") {
		return false
	}
	typeName, subtype, ok := strings.Cut(mediaType, "/")
	return ok && typeName != "" && subtype != ""
}

// ParseAccept parses an Accept header into a sorted list of media types.
// Results are sorted by quality (descending), then specificity.
//
//	types := http.ParseAccept("text/html, application/json;q=0.9, */*;q=0.1")
func ParseAccept(header string) []MediaType {
	if header == "" {
		return nil
	}

	parts := strings.Split(header, ",")
	types := make([]MediaType, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		mt := MediaType{Quality: 1.0}

		// Split media type from parameters
		segments := strings.Split(part, ";")
		mt.Full = strings.TrimSpace(segments[0])

		// Parse type/subtype
		slashIdx := strings.IndexByte(mt.Full, '/')
		if slashIdx == -1 {
			mt.Type = mt.Full
			mt.Subtype = "*"
		} else {
			mt.Type = strings.TrimSpace(mt.Full[:slashIdx])
			mt.Subtype = strings.TrimSpace(mt.Full[slashIdx+1:])
		}

		// Parse q parameter
		for _, seg := range segments[1:] {
			seg = strings.TrimSpace(seg)
			if strings.HasPrefix(seg, "q=") {
				if q, err := strconv.ParseFloat(seg[2:], 64); err == nil {
					mt.Quality = q
				}
			}
		}

		types = append(types, mt)
	}

	// Sort: higher quality first, then more specific first
	sort.SliceStable(types, func(i, j int) bool {
		if types[i].Quality != types[j].Quality {
			return types[i].Quality > types[j].Quality
		}
		// More specific wins (no wildcards)
		iSpec := specificity(types[i])
		jSpec := specificity(types[j])
		return iSpec > jSpec
	})

	return types
}

func specificity(mt MediaType) int {
	if mt.Type == "*" {
		return 0
	}
	if mt.Subtype == "*" {
		return 1
	}
	return 2
}

// NegotiateContentType selects the best content type from offered types
// based on the client's Accept header. Returns the matched type or empty string.
//
//	best := http.NegotiateContentType(
//	    ctx.Accept(),
//	    []string{"application/json", "text/html"},
//	)
func NegotiateContentType(acceptHeader string, offered []string) string {
	if acceptHeader == "" || len(offered) == 0 {
		if len(offered) > 0 {
			return offered[0]
		}
		return ""
	}

	accepted := ParseAccept(acceptHeader)
	for _, mt := range accepted {
		for _, o := range offered {
			if matchesMediaType(mt, o) {
				return o
			}
		}
	}

	return ""
}

func matchesMediaType(mt MediaType, offered string) bool {
	if mt.Full == "*/*" {
		return true
	}

	slashIdx := strings.IndexByte(offered, '/')
	if slashIdx == -1 {
		return mt.Full == offered
	}

	oType := offered[:slashIdx]
	oSubtype := offered[slashIdx+1:]

	if mt.Type == oType && mt.Subtype == "*" {
		return true
	}

	return mt.Type == oType && mt.Subtype == oSubtype
}
