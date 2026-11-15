package screenshots

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Stable diagnostic codes of the version 1 decoder.
const (
	ErrorCodeParseError             = "screenshots.parse_error"
	ErrorCodeTooLarge               = "screenshots.too_large"
	ErrorCodeUnknownField           = "screenshots.unknown_field"
	ErrorCodeDuplicateField         = "screenshots.duplicate_field"
	ErrorCodeMissingField           = "screenshots.missing_field"
	ErrorCodeTextTooLong            = "screenshots.text_too_long"
	ErrorCodeInvalidProtocolVersion = "screenshots.invalid_protocol_version"
	ErrorCodeInvalidProject         = "screenshots.invalid_project"
	ErrorCodeInvalidSHA             = "screenshots.invalid_sha"
	ErrorCodeTooManyPages           = "screenshots.too_many_pages"
	ErrorCodeInvalidPageFile        = "screenshots.invalid_page_file"
	ErrorCodeInvalidRoute           = "screenshots.invalid_route"
	ErrorCodeInvalidPath            = "screenshots.invalid_path"
	ErrorCodeInvalidPageSides       = "screenshots.invalid_page_sides"
	ErrorCodeDuplicateRoute         = "screenshots.duplicate_route"
	ErrorCodeTooManyCaptures        = "screenshots.too_many_captures"
	ErrorCodeInvalidCaptureFile     = "screenshots.invalid_capture_file"
	ErrorCodeDuplicateCaptureFile   = "screenshots.duplicate_capture_file"
	ErrorCodeUnknownRoute           = "screenshots.unknown_route"
	ErrorCodeInvalidSide            = "screenshots.invalid_side"
	ErrorCodeInvalidViewport        = "screenshots.invalid_viewport"
	ErrorCodeInvalidScheme          = "screenshots.invalid_scheme"
	ErrorCodeInvalidSHA256          = "screenshots.invalid_sha256"
	ErrorCodeInvalidStatus          = "screenshots.invalid_status"
	ErrorCodeInvalidURL             = "screenshots.invalid_url"
	ErrorCodeUnexpectedSide         = "screenshots.unexpected_side"
	ErrorCodeDuplicateCapture       = "screenshots.duplicate_capture"
	ErrorCodeMissingCapture         = "screenshots.missing_capture"
)

// ValidErrorCodes is the closed set of codes the decoder emits.
var ValidErrorCodes = map[string]bool{
	ErrorCodeParseError: true, ErrorCodeTooLarge: true, ErrorCodeUnknownField: true,
	ErrorCodeDuplicateField: true, ErrorCodeMissingField: true, ErrorCodeTextTooLong: true,
	ErrorCodeInvalidProtocolVersion: true, ErrorCodeInvalidProject: true, ErrorCodeInvalidSHA: true,
	ErrorCodeTooManyPages: true, ErrorCodeInvalidPageFile: true, ErrorCodeInvalidRoute: true,
	ErrorCodeInvalidPath: true, ErrorCodeInvalidPageSides: true, ErrorCodeDuplicateRoute: true,
	ErrorCodeTooManyCaptures: true, ErrorCodeInvalidCaptureFile: true, ErrorCodeDuplicateCaptureFile: true,
	ErrorCodeUnknownRoute: true, ErrorCodeInvalidSide: true, ErrorCodeInvalidViewport: true,
	ErrorCodeInvalidScheme: true, ErrorCodeInvalidSHA256: true, ErrorCodeInvalidStatus: true,
	ErrorCodeInvalidURL: true, ErrorCodeUnexpectedSide: true, ErrorCodeDuplicateCapture: true,
	ErrorCodeMissingCapture: true,
}

var (
	commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Parse strictly decodes a manifest: one JSON object within MaxManifestBytes,
// with every member present, no unknown member and no member twice. It does
// not judge the values; Validate does.
func Parse(data []byte) (*Manifest, []diag.Diagnostic) {
	if len(data) > MaxManifestBytes {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeTooLarge, "",
			"the manifest is %d bytes, more than the %d a manifest may be", len(data), MaxManifestBytes)}
	}
	if finding := scan(data); finding != nil {
		return nil, []diag.Diagnostic{*finding}
	}
	type pageWire struct {
		File   *string `json:"file"`
		Route  *string `json:"route"`
		Path   *string `json:"path"`
		Before *bool   `json:"before"`
		After  *bool   `json:"after"`
	}
	type captureWire struct {
		File     *string `json:"file"`
		Route    *string `json:"route"`
		Side     *string `json:"side"`
		Viewport *string `json:"viewport"`
		Scheme   *string `json:"scheme"`
		SHA256   *string `json:"sha256"`
		Status   *int    `json:"status"`
		URL      *string `json:"url"`
	}
	type manifestWire struct {
		ProtocolVersion *int           `json:"protocolVersion"`
		Project         *string        `json:"project"`
		BaseSHA         *string        `json:"baseSHA"`
		HeadSHA         *string        `json:"headSHA"`
		Pages           *[]pageWire    `json:"pages"`
		Captures        *[]captureWire `json:"captures"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire manifestWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, []diag.Diagnostic{decodeDiagnostic(err)}
	}

	var diags []diag.Diagnostic
	missing := func(field string) {
		diags = append(diags, diag.Errorf(ErrorCodeMissingField, field, "%s is required", field))
	}
	manifest := &Manifest{}
	manifest.ProtocolVersion = required(wire.ProtocolVersion, "protocolVersion", missing)
	manifest.Project = required(wire.Project, "project", missing)
	manifest.BaseSHA = required(wire.BaseSHA, "baseSHA", missing)
	manifest.HeadSHA = required(wire.HeadSHA, "headSHA", missing)
	manifest.Pages = []Page{}
	for i, page := range required(wire.Pages, "pages", missing) {
		field := fmt.Sprintf("pages[%d].", i)
		manifest.Pages = append(manifest.Pages, Page{
			File:   required(page.File, field+"file", missing),
			Route:  required(page.Route, field+"route", missing),
			Path:   required(page.Path, field+"path", missing),
			Before: required(page.Before, field+"before", missing),
			After:  required(page.After, field+"after", missing),
		})
	}
	manifest.Captures = []Capture{}
	for i, capture := range required(wire.Captures, "captures", missing) {
		field := fmt.Sprintf("captures[%d].", i)
		manifest.Captures = append(manifest.Captures, Capture{
			File:     required(capture.File, field+"file", missing),
			Route:    required(capture.Route, field+"route", missing),
			Side:     required(capture.Side, field+"side", missing),
			Viewport: required(capture.Viewport, field+"viewport", missing),
			Scheme:   required(capture.Scheme, field+"scheme", missing),
			SHA256:   required(capture.SHA256, field+"sha256", missing),
			Status:   required(capture.Status, field+"status", missing),
			URL:      required(capture.URL, field+"url", missing),
		})
	}
	if len(diags) > 0 {
		return nil, diags
	}
	return manifest, nil
}

// required returns the value of a member, and reports a member that is
// absent or null.
func required[T any](value *T, field string, missing func(string)) T {
	if value == nil {
		missing(field)
		var zero T
		return zero
	}
	return *value
}

// Decode strictly parses a manifest and validates it. It returns the
// manifest only when no rule of the contract refuses it.
func Decode(data []byte) (*Manifest, []diag.Diagnostic) {
	manifest, diags := Parse(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	if diags = Validate(manifest); diag.HasErrors(diags) {
		return nil, diags
	}
	return manifest, nil
}

// Marshal validates a manifest and serializes it as indented JSON with one
// trailing newline. It refuses a manifest Decode would refuse.
func Marshal(manifest Manifest) ([]byte, error) {
	if manifest.Pages == nil {
		manifest.Pages = []Page{}
	}
	if manifest.Captures == nil {
		manifest.Captures = []Capture{}
	}
	if diags := Validate(&manifest); diag.HasErrors(diags) {
		return nil, errors.New("the screenshots manifest is not valid:\n" + diag.ErrorText(diags))
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("the screenshots manifest is %d bytes, more than the %d a manifest may be", len(data), MaxManifestBytes)
	}
	return data, nil
}

// Validate applies every rule of the contract to a parsed manifest and
// returns each finding, in document order.
func Validate(manifest *Manifest) []diag.Diagnostic {
	if manifest == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "the manifest is nil")}
	}
	var diags []diag.Diagnostic
	report := func(code, field, format string, args ...any) {
		diags = append(diags, diag.Errorf(code, field, format, args...))
	}
	// text reports a text member past its bound, and tells whether the
	// member's own rule is worth applying.
	text := func(field, value string) bool {
		if len(value) > MaxTextBytes {
			report(ErrorCodeTextTooLong, field, "%s is %d bytes, more than %d", field, len(value), MaxTextBytes)
			return false
		}
		return true
	}

	if manifest.ProtocolVersion != ProtocolVersion {
		report(ErrorCodeInvalidProtocolVersion, "protocolVersion",
			"protocolVersion %d is not supported (want %d)", manifest.ProtocolVersion, ProtocolVersion)
	}
	if text("project", manifest.Project) && manifest.Project != "." && !relativePath(manifest.Project) {
		report(ErrorCodeInvalidProject, "project",
			"project %q is no normalized directory relative to the workspace root", manifest.Project)
	}
	for _, commit := range []struct{ field, value string }{{"baseSHA", manifest.BaseSHA}, {"headSHA", manifest.HeadSHA}} {
		if !commitPattern.MatchString(commit.value) {
			report(ErrorCodeInvalidSHA, commit.field,
				"%s %q is no full lowercase commit sha of 40 or 64 hexadecimal characters", commit.field, commit.value)
		}
	}

	if len(manifest.Pages) > MaxPages {
		report(ErrorCodeTooManyPages, "pages", "%d pages, more than %d", len(manifest.Pages), MaxPages)
		return diags
	}
	if len(manifest.Captures) > MaxCaptures {
		report(ErrorCodeTooManyCaptures, "captures", "%d captures, more than %d", len(manifest.Captures), MaxCaptures)
		return diags
	}

	pages := map[string]Page{}
	for i, page := range manifest.Pages {
		field := fmt.Sprintf("pages[%d]", i)
		if text(field+".file", page.File) && !relativePath(page.File) {
			report(ErrorCodeInvalidPageFile, field+".file",
				"file %q is no normalized path relative to the project", page.File)
		}
		if text(field+".route", page.Route) && !strings.HasPrefix(page.Route, "/") {
			report(ErrorCodeInvalidRoute, field+".route", "route %q does not start with a slash", page.Route)
		}
		if text(field+".path", page.Path) && !strings.HasPrefix(page.Path, "/") {
			report(ErrorCodeInvalidPath, field+".path", "path %q does not start with a slash", page.Path)
		}
		if !page.Before && !page.After {
			report(ErrorCodeInvalidPageSides, field, "neither side holds the page %s", page.Route)
		}
		if _, listed := pages[page.Route]; listed {
			report(ErrorCodeDuplicateRoute, field+".route", "the route %s is listed twice", page.Route)
			continue
		}
		pages[page.Route] = page
	}

	type slot struct{ route, side, viewport, scheme string }
	taken := map[slot]bool{}
	files := map[string]bool{}
	for i, capture := range manifest.Captures {
		field := fmt.Sprintf("captures[%d]", i)
		if !captureFile(capture.File) {
			report(ErrorCodeInvalidCaptureFile, field+".file",
				"file %q is no .png file name of at most %d bytes with no directory", capture.File, MaxFileNameBytes)
		} else if files[capture.File] {
			report(ErrorCodeDuplicateCaptureFile, field+".file", "the file %s is listed twice", capture.File)
		}
		files[capture.File] = true
		sideKnown := capture.Side == SideBefore || capture.Side == SideAfter
		if !sideKnown {
			report(ErrorCodeInvalidSide, field+".side", "side %q is not in the set: before, after", capture.Side)
		}
		if capture.Viewport != ViewportDesktop && capture.Viewport != ViewportMobile {
			report(ErrorCodeInvalidViewport, field+".viewport",
				"viewport %q is not in the set: desktop, mobile", capture.Viewport)
		}
		if capture.Scheme != SchemeLight && capture.Scheme != SchemeDark {
			report(ErrorCodeInvalidScheme, field+".scheme", "scheme %q is not in the set: light, dark", capture.Scheme)
		}
		if !sha256Pattern.MatchString(capture.SHA256) {
			report(ErrorCodeInvalidSHA256, field+".sha256", "sha256 is not 64 lowercase hexadecimal characters")
		}
		if capture.Status < 100 || capture.Status > 599 {
			report(ErrorCodeInvalidStatus, field+".status", "status %d is no HTTP status from 100 to 599", capture.Status)
		}
		if text(field+".url", capture.URL) && !absoluteURL(capture.URL) {
			report(ErrorCodeInvalidURL, field+".url", "url %q is no absolute http or https URL", capture.URL)
		}
		if !text(field+".route", capture.Route) {
			continue
		}
		page, listed := pages[capture.Route]
		if !listed {
			report(ErrorCodeUnknownRoute, field+".route", "the route %s is none of the pages", capture.Route)
			continue
		}
		if !sideKnown {
			continue
		}
		if capture.Side == SideBefore && !page.Before || capture.Side == SideAfter && !page.After {
			report(ErrorCodeUnexpectedSide, field+".side",
				"the %s side does not hold the page %s", capture.Side, capture.Route)
			continue
		}
		key := slot{capture.Route, capture.Side, capture.Viewport, capture.Scheme}
		if taken[key] {
			report(ErrorCodeDuplicateCapture, field,
				"%s already has a %s %s %s capture", capture.Route, capture.Side, capture.Viewport, capture.Scheme)
		}
		taken[key] = true
	}

	seen := map[string]bool{}
	for i, page := range manifest.Pages {
		if seen[page.Route] {
			continue
		}
		seen[page.Route] = true
		for _, side := range Sides() {
			if side == SideBefore && !page.Before || side == SideAfter && !page.After {
				continue
			}
			for _, viewport := range Viewports() {
				for _, scheme := range Schemes() {
					if !taken[slot{page.Route, side, viewport.Name, scheme}] {
						report(ErrorCodeMissingCapture, fmt.Sprintf("pages[%d]", i),
							"%s has no %s %s %s capture", page.Route, side, viewport.Name, scheme)
					}
				}
			}
		}
	}
	return diags
}

// relativePath reports whether name is a normalized slash-separated path that
// stays under the directory it is relative to.
func relativePath(name string) bool {
	return name != "" && name != "." && !strings.Contains(name, "\\") && !strings.HasPrefix(name, "/") &&
		path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}

// captureFile reports whether name is a .png file name with no directory.
func captureFile(name string) bool {
	return len(name) > len(".png") && len(name) <= MaxFileNameBytes && strings.HasSuffix(name, ".png") &&
		!strings.ContainsAny(name, "/\\")
}

// absoluteURL reports whether text is an http or https URL with a host.
func absoluteURL(text string) bool {
	parsed, err := url.Parse(text)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// scan walks the token stream: encoding/json keeps the last occurrence of a
// member listed twice, and reads one value where a file holds two.
func scan(data []byte) *diag.Diagnostic {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if finding := scanValue(decoder, ""); finding != nil {
		return finding
	}
	if _, err := decoder.Token(); err != io.EOF {
		finding := diag.Errorf(ErrorCodeParseError, "", "the manifest holds more than one JSON value")
		return &finding
	}
	return nil
}

func scanValue(decoder *json.Decoder, field string) *diag.Diagnostic {
	syntax := func(err error) *diag.Diagnostic {
		finding := diag.Errorf(ErrorCodeParseError, "", "the manifest is not JSON: %v", err)
		return &finding
	}
	token, err := decoder.Token()
	if err != nil {
		return syntax(err)
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return syntax(err)
			}
			key, _ := keyToken.(string)
			member := key
			if field != "" {
				member = field + "." + key
			}
			if seen[key] {
				finding := diag.Errorf(ErrorCodeDuplicateField, member, "the member %s is listed twice", member)
				return &finding
			}
			seen[key] = true
			if finding := scanValue(decoder, member); finding != nil {
				return finding
			}
		}
	case json.Delim('['):
		for index := 0; decoder.More(); index++ {
			if finding := scanValue(decoder, fmt.Sprintf("%s[%d]", field, index)); finding != nil {
				return finding
			}
		}
	default:
		return nil
	}
	if _, err := decoder.Token(); err != nil {
		return syntax(err)
	}
	return nil
}

func decodeDiagnostic(err error) diag.Diagnostic {
	message := err.Error()
	if name, ok := strings.CutPrefix(message, "json: unknown field "); ok {
		return diag.Errorf(ErrorCodeUnknownField, strings.Trim(name, `"`), "%s", message)
	}
	return diag.Errorf(ErrorCodeParseError, "", "not a screenshots manifest: %s", message)
}
