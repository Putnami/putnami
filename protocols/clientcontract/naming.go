package clientcontract

import "strings"

// RPCName is the base name every first-party emitter derives for a route from
// its HTTP method and path: a verb from the method, then the UpperCamel path
// segments with path parameters dropped, or "Resource" when no segment is left.
//
//	GET    /users        → ListUsers
//	GET    /users/{id}   → GetUsers
//	POST   /users        → CreateUsers
//	PUT    /users/{id}   → UpdateUsers
//	DELETE /users/{id}   → DeleteUsers
//
// Distinct routes can share a base name; each emitter applies its own collision
// policy on top of it.
func RPCName(method, path string) string {
	subject := subjectFromPath(path)
	if subject == "" {
		subject = "Resource"
	}
	return verbFromMethod(method, path) + subject
}

func verbFromMethod(method, path string) string {
	switch strings.ToUpper(method) {
	case "GET":
		if strings.Contains(path, "{") {
			return "Get"
		}
		return "List"
	case "POST":
		return "Create"
	case "PUT", "PATCH":
		return "Update"
	case "DELETE":
		return "Delete"
	default:
		return UpperCamel(strings.ToLower(method))
	}
}

func subjectFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || strings.HasPrefix(part, "{") {
			continue
		}
		segments = append(segments, UpperCamel(part))
	}
	return strings.Join(segments, "")
}

// UpperCamel splits s on '-', '_', '.' and ' ' and joins the words with their
// first byte upper-cased; the rest of each word is kept as is.
func UpperCamel(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ' '
	})
	var out strings.Builder
	for _, word := range words {
		out.WriteString(strings.ToUpper(word[:1]))
		out.WriteString(word[1:])
	}
	return out.String()
}

// ScreamingSnake renders a protobuf type name as its conventional enum value
// prefix: WidgetState becomes WIDGET_STATE.
func ScreamingSnake(name string) string {
	var out strings.Builder
	for index, char := range name {
		if index > 0 && char >= 'A' && char <= 'Z' {
			out.WriteByte('_')
		}
		out.WriteRune(char)
	}
	return strings.ToUpper(out.String())
}

// EnumMember maps a protobuf enum value name onto the JSON schema member it
// encodes: the ScreamingSnake prefix of the enum name is dropped and the rest is
// lower-cased, so WIDGET_STATE_ACTIVE and "active" are one member.
func EnumMember(enumName, valueName string) string {
	return strings.ToLower(strings.TrimPrefix(valueName, ScreamingSnake(enumName)+"_"))
}
