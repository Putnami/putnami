// Package screenshots is the versioned contract of the manifest that
// putnami change screenshots writes beside its PNG captures: the Go types, a
// strict decoder and the bounds a consumer may apply. The JSON Schema is
// schemas/screenshots.json, and fixtures/ is the shared corpus.
package screenshots

import _ "embed"

// ProtocolVersion is the version every manifest of this contract carries. A
// change to a member, a closed set or a rule needs a new version.
const ProtocolVersion = 1

// SchemaURL is where the JSON Schema of a version 1 manifest is published.
const SchemaURL = "https://putnami.dev/schemas/putnami-screenshots-v1.json"

// ManifestFile is the name of the manifest in the directory that holds the
// captures.
const ManifestFile = "screenshots.json"

// The two sides of a change a page is captured on.
const (
	SideBefore = "before"
	SideAfter  = "after"
)

// The two viewports every page is captured at.
const (
	ViewportDesktop = "desktop"
	ViewportMobile  = "mobile"
)

// The two color schemes every page is captured in.
const (
	SchemeLight = "light"
	SchemeDark  = "dark"
)

// The bounds of a manifest. The decoder refuses a manifest past one of them
// and Marshal refuses to write one, so a consumer may apply them as they are.
const (
	// MaxPages is the most pages a manifest holds.
	MaxPages = 512
	// MaxCaptures is the most captures a manifest holds: eight per page.
	MaxCaptures = 4096
	// MaxManifestBytes is the largest manifest file.
	MaxManifestBytes = 4 << 20
	// MaxCaptureBytes is the largest capture file. The manifest cannot prove
	// it: a reader applies it to each PNG file before it hashes the bytes.
	MaxCaptureBytes = 64 << 20
	// MaxTextBytes is the longest text member, in bytes of UTF-8.
	MaxTextBytes = 2048
	// MaxFileNameBytes is the longest capture file name.
	MaxFileNameBytes = 255
)

// Viewport is one of the sizes a page is captured at: a name of the closed
// set, a size in CSS pixels and a device scale factor.
type Viewport struct {
	// Name is the value a capture carries as viewport.
	Name string
	// Width and Height are the size of the viewport, in CSS pixels.
	Width  int
	Height int
	// Scale is the device scale factor.
	Scale int
}

// Sides returns the closed set of sides, before first.
func Sides() []string { return []string{SideBefore, SideAfter} }

// Viewports returns the closed set of viewports, desktop first.
func Viewports() []Viewport {
	return []Viewport{
		{Name: ViewportDesktop, Width: 1440, Height: 900, Scale: 1},
		{Name: ViewportMobile, Width: 390, Height: 844, Scale: 2},
	}
}

// Schemes returns the closed set of color schemes, light first.
func Schemes() []string { return []string{SchemeLight, SchemeDark} }

// Manifest is the record of one run of putnami change screenshots: the
// project, the two commits it compares, the pages the change touches and one
// capture per page, side, viewport and scheme.
type Manifest struct {
	// ProtocolVersion is 1.
	ProtocolVersion int `json:"protocolVersion"`
	// Project is the project directory, relative to the workspace root.
	Project string `json:"project"`
	// BaseSHA is the full lowercase sha of the merge base of the change.
	BaseSHA string `json:"baseSHA"`
	// HeadSHA is the full lowercase sha of the head the captures were taken at.
	HeadSHA string `json:"headSHA"`
	// Pages are the pages the change touches, no route twice.
	Pages []Page `json:"pages"`
	// Captures holds one capture per page, side, viewport and scheme.
	Captures []Capture `json:"captures"`
}

// Page is one touched page: its page file, relative to the project, the
// route it is captured at, the path visited for that route, and the sides
// that hold the page.
type Page struct {
	// File is the page file, a normalized path relative to the project.
	File string `json:"file"`
	// Route is the route the page is captured at. It starts with a slash.
	Route string `json:"route"`
	// Path is the path visited for the route: the route, or a template with
	// its parameters filled.
	Path string `json:"path"`
	// Before tells whether the base holds the page.
	Before bool `json:"before"`
	// After tells whether the head holds the page.
	After bool `json:"after"`
}

// Capture is one PNG file in the manifest's directory.
type Capture struct {
	// File is a .png file name, with no directory.
	File string `json:"file"`
	// Route is the route of one of the manifest's pages.
	Route string `json:"route"`
	// Side is before or after, a side that holds the page.
	Side string `json:"side"`
	// Viewport is desktop or mobile.
	Viewport string `json:"viewport"`
	// Scheme is light or dark, the preferred color scheme.
	Scheme string `json:"scheme"`
	// SHA256 is the lowercase hexadecimal sha256 of the file.
	SHA256 string `json:"sha256"`
	// Status is the HTTP status of the captured document.
	Status int `json:"status"`
	// URL is where the page ended, after any redirect.
	URL string `json:"url"`
}

//go:embed schemas/screenshots.json
var schema []byte

// Schema returns the JSON Schema of a manifest, the document published at
// SchemaURL.
func Schema() []byte { return append([]byte(nil), schema...) }
