package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

const outboxFixtures = "fixtures/publication-outbox"

// outboxRefusal joins every diagnostic message of a refused descriptor.
func outboxRefusal(diags []diag.Diagnostic) string {
	messages := make([]string, 0, len(diags))
	for _, d := range diags {
		messages = append(messages, d.Field+": "+d.Message)
	}
	return strings.Join(messages, "; ")
}

func readOutboxFixture(t *testing.T, kind, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outboxFixtures, kind, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOutboxDescriptorRefusesUnknownDuplicateAndNullMembers(t *testing.T) {
	for name, want := range map[string]string{
		"unknown-root-member.json":     `unknown member "registry"`,
		"unknown-artifact-member.json": `unknown member "url"`,
		"case-folded-member.json":      `unknown member "Coordinate"`,
		"duplicate-member.json":        `duplicate object member "protocolVersion"`,
		"duplicate-nested-member.json": `duplicate object member "digest"`,
		"null-member.json":             "null is not permitted",
		"null-nested-member.json":      "null is not permitted",
		"missing-members.json":         "members is required",
		"trailing-data.json":           "trailing data",
		"not-an-object.json":           "not a JSON object",
		"future-version.json":          "protocolVersion 2 is not 1",
	} {
		t.Run(name, func(t *testing.T) {
			outbox, diags := ParsePublicationOutbox(readOutboxFixture(t, "invalid", name))
			if outbox != nil || len(diags) == 0 {
				t.Fatalf("descriptor accepted, want a refusal naming %q", want)
			}
			if got := outboxRefusal(diags); !strings.Contains(got, want) {
				t.Fatalf("refusal = %q, want it to name %q", got, want)
			}
		})
	}
}

func TestOutboxDescriptorRefusesEscapingPathsAndSymlinks(t *testing.T) {
	for name, want := range map[string]string{
		"parent-path.json":        "escapes the outbox root",
		"nested-parent-path.json": "is not clean",
		"absolute-path.json":      "is absolute",
		"backslash-path.json":     "backslash",
		"drive-path.json":         "colon",
		"dot-path.json":           "is not clean",
		"unclean-path.json":       "is not clean",
		"descriptor-path.json":    "names the descriptor",
		"shared-path.json":        "overlaps",
		"nested-path.json":        "overlaps",
	} {
		t.Run(name, func(t *testing.T) {
			outbox, diags := ParsePublicationOutbox(readOutboxFixture(t, "invalid", name))
			if outbox != nil {
				t.Fatalf("descriptor accepted, want a refusal naming %q", want)
			}
			if got := outboxRefusal(diags); !strings.Contains(got, want) {
				t.Fatalf("refusal = %q, want it to name %q", got, want)
			}
		})
	}

	root := t.TempDir()
	outside := t.TempDir()
	mustWriteOutboxFile(t, filepath.Join(outside, "secret"), "outside the outbox")
	mustWriteOutboxFile(t, filepath.Join(root, "npm", "web.tgz"), "tarball")
	if err := os.MkdirAll(filepath.Join(root, "oci", "app", "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveOutboxPath(root, "npm/web.tgz"); err != nil || got != filepath.Join(root, "npm", "web.tgz") {
		t.Fatalf("ResolveOutboxPath(regular file) = %q, %v", got, err)
	}
	if got, err := ResolveOutboxPath(root, "oci/app"); err != nil || got != filepath.Join(root, "oci", "app") {
		t.Fatalf("ResolveOutboxPath(directory) = %q, %v", got, err)
	}
	for rel, want := range map[string]string{
		"../secret":     "escapes the outbox root",
		"npm/web.tgz/x": "is not a directory",
	} {
		if _, err := ResolveOutboxPath(root, rel); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("ResolveOutboxPath(%q) error = %v, want %q", rel, err, want)
		}
	}
	if _, err := ResolveOutboxPath(root, "npm/missing.tgz"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ResolveOutboxPath(missing) error = %v, want not-exist", err)
	}
	if _, err := ResolveOutboxPath(filepath.Join(root, "npm", "web.tgz"), "x"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("ResolveOutboxPath(file root) error = %v, want a refusal", err)
	}

	if runtime.GOOS == "windows" {
		t.Skip("creating a symbolic link needs a privilege a Windows test host lacks")
	}
	for link, target := range map[string]string{
		"leaf.tgz":       filepath.Join(outside, "secret"),
		"linked-dir":     outside,
		"oci/app/blobs2": filepath.Join(root, "npm"),
	} {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(link))); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{"leaf.tgz", "linked-dir/secret", "linked-dir", "oci/app/blobs2/web.tgz"} {
		if _, err := ResolveOutboxPath(root, rel); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("ResolveOutboxPath(%q) error = %v, want a symbolic-link refusal", rel, err)
		}
	}
	linkedRoot := filepath.Join(t.TempDir(), "outbox")
	if err := os.Symlink(root, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveOutboxPath(linkedRoot, "npm/web.tgz"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("ResolveOutboxPath(linked root) error = %v, want a refusal", err)
	}
}

func TestOutboxDescriptorRefusesInvalidArtifactsAndIdentities(t *testing.T) {
	for name, want := range map[string]string{
		"two-blocks.json":         "2 ecosystem blocks",
		"wrong-block.json":        "a go member requires the go block",
		"unknown-ecosystem.json":  `ecosystem "pypi"`,
		"uppercase-digest.json":   "must be sha256:",
		"empty-artifact.json":     "size 0 is outside",
		"oversized-zip.json":      fmt.Sprintf("outside 1..%d", MaxOutboxGoZipBytes),
		"oversized-tarball.json":  fmt.Sprintf("outside 1..%d", MaxOutboxNPMTarballBytes),
		"empty-version.json":      "version is required",
		"spaced-coordinate.json":  "space, control or invalid character",
		"repeated-member.json":    "repeats members[0]",
		"foreign-repository.json": `followed by the coordinate "acme/app"`,
		"repeated-tag.json":       "is repeated",
		"invalid-tag.json":        "is not an OCI tag",
	} {
		t.Run(name, func(t *testing.T) {
			outbox, diags := ParsePublicationOutbox(readOutboxFixture(t, "invalid", name))
			if outbox != nil {
				t.Fatalf("descriptor accepted, want a refusal naming %q", want)
			}
			if got := outboxRefusal(diags); !strings.Contains(got, want) {
				t.Fatalf("refusal = %q, want it to name %q", got, want)
			}
		})
	}
}

// Every fixture in the corpus is asserted by a test above: a new
// counter-example cannot be added without a stated reason for refusal.
func TestOutboxFixtureCorpusIsCovered(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(outboxFixtures, "invalid", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no invalid outbox fixtures: %v", err)
	}
	for _, file := range files {
		outbox, diags := ParsePublicationOutbox(mustReadOutbox(t, file))
		if outbox != nil || len(diags) == 0 {
			t.Errorf("%s was accepted", file)
		}
	}
	valid, err := filepath.Glob(filepath.Join(outboxFixtures, "valid", "*.json"))
	if err != nil || len(valid) == 0 {
		t.Fatalf("no valid outbox fixtures: %v", err)
	}
	for _, file := range valid {
		outbox, diags := ParsePublicationOutbox(mustReadOutbox(t, file))
		if outbox == nil || len(diags) != 0 {
			t.Errorf("%s was refused: %s", file, outboxRefusal(diags))
		}
	}
}

func TestOutboxDescriptorAcceptsEveryEcosystem(t *testing.T) {
	outbox, diags := ParsePublicationOutbox(readOutboxFixture(t, "valid", "every-ecosystem.json"))
	if outbox == nil {
		t.Fatalf("valid descriptor refused: %s", outboxRefusal(diags))
	}
	if len(outbox.Members) != 3 {
		t.Fatalf("members = %d, want 3", len(outbox.Members))
	}
	npm, goModule, image := outbox.Members[0], outbox.Members[1], outbox.Members[2]
	if npm.NPM == nil || npm.NPM.Tarball.Path != "npm/web/web-1.2.3-r42.tgz" || npm.NPM.Manifest.Size != 312 {
		t.Fatalf("npm member = %+v", npm)
	}
	if goModule.Go == nil || goModule.Go.Info.Path != "go/api/v1.4.0.info" || goModule.Project != "/go/api" {
		t.Fatalf("go member = %+v", goModule)
	}
	if image.OCI == nil || image.OCI.Repository != "oci.acme.dev/acme/app" || len(image.OCI.Tags) != 1 {
		t.Fatalf("oci member = %+v", image)
	}

	empty, diags := ParsePublicationOutbox(readOutboxFixture(t, "valid", "no-member.json"))
	if empty == nil || len(empty.Members) != 0 || empty.Members == nil {
		t.Fatalf("an empty member list is a statement, not an absence: %+v %s", empty, outboxRefusal(diags))
	}
}

func TestOutboxDescriptorIsBounded(t *testing.T) {
	oversized := `{"protocolVersion":1,"members":[],"pad":"` + strings.Repeat("x", MaxPublicationOutboxBytes) + `"}`
	if _, diags := ParsePublicationOutbox([]byte(oversized)); !strings.Contains(outboxRefusal(diags), "exceeds") {
		t.Fatalf("oversized descriptor refusal = %q", outboxRefusal(diags))
	}
	if _, diags := ParsePublicationOutbox(nil); len(diags) == 0 {
		t.Fatal("an empty descriptor was accepted")
	}
	if _, diags := ParsePublicationOutbox([]byte("{\"protocolVersion\":1,\"members\":[],\"x\":\"\xff\"}")); !strings.Contains(outboxRefusal(diags), "UTF-8") {
		t.Fatalf("invalid UTF-8 refusal = %q", outboxRefusal(diags))
	}
	deep := `{"protocolVersion":1,"members":[{"ecosystem":"oci","coordinate":"a/b","version":"1","project":"p","oci":{"tags":[["x"]]}}]}`
	if _, diags := ParsePublicationOutbox([]byte(deep)); !strings.Contains(outboxRefusal(diags), "nesting exceeds") {
		t.Fatalf("deep descriptor refusal = %q", outboxRefusal(diags))
	}

	outbox := PublicationOutbox{ProtocolVersion: PublicationOutboxVersion}
	for index := 0; index <= MaxPublicationOutboxMembers; index++ {
		outbox.Members = append(outbox.Members, OutboxMember{
			Ecosystem: OutboxEcosystemGo, Coordinate: fmt.Sprintf("go.acme.dev/m%d", index), Version: "v1.0.0", Project: "/go/m",
			Go: &OutboxGo{
				Zip:  OutboxFile{Path: fmt.Sprintf("m%d/v1.0.0.zip", index), Digest: "sha256:" + strings.Repeat("a", 64), Size: 1},
				Mod:  OutboxFile{Path: fmt.Sprintf("m%d/v1.0.0.mod", index), Digest: "sha256:" + strings.Repeat("b", 64), Size: 1},
				Info: OutboxFile{Path: fmt.Sprintf("m%d/v1.0.0.info", index), Digest: "sha256:" + strings.Repeat("c", 64), Size: 1},
			},
		})
	}
	data, err := json.Marshal(outbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxPublicationOutboxBytes {
		t.Fatalf("the member-bound probe is %d bytes, above the document bound", len(data))
	}
	if _, diags := ParsePublicationOutbox(data); !strings.Contains(outboxRefusal(diags), fmt.Sprintf("exceed the bound of %d", MaxPublicationOutboxMembers)) {
		t.Fatalf("too many members refusal = %q", outboxRefusal(diags))
	}
	outbox.Members = outbox.Members[:MaxPublicationOutboxMembers]
	data, err = json.Marshal(outbox)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, diags := ParsePublicationOutbox(data); parsed == nil {
		t.Fatalf("a descriptor at the member bound was refused: %s", outboxRefusal(diags))
	}
}

// The schema states the same member set as the Go types, at every level.
func TestOutboxSchemaTracksTheGoTypes(t *testing.T) {
	data, err := os.ReadFile("schemas/publication-outbox.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties  map[string]json.RawMessage `json:"properties"`
		Definitions map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	names := func(properties map[string]json.RawMessage) []string {
		out := make([]string, 0, len(properties))
		for name := range properties {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}
	assertFieldParity(t, "PublicationOutbox", names(schema.Properties), goTypeJSONFields(t, PublicationOutbox{}))
	for definition, value := range map[string]any{
		"member": OutboxMember{}, "file": OutboxFile{}, "npm": OutboxNPM{}, "go": OutboxGo{}, "oci": OutboxOCI{},
	} {
		def, ok := schema.Definitions[definition]
		if !ok {
			t.Fatalf("schema has no definitions/%s", definition)
		}
		assertFieldParity(t, definition, names(def.Properties), goTypeJSONFields(t, value))
	}
}

func mustReadOutbox(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustWriteOutboxFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
