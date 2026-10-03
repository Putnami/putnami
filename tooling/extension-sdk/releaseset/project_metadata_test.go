package releaseset

import (
	"encoding/json"
	"strings"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
)

func TestProjectReleaseMetadataUnionsEveryProvider(t *testing.T) {
	metadata := map[string]json.RawMessage{
		"@putnami/typescript": json.RawMessage(`{"main":"dist/index.js","releaseSet":{"ecosystems":[{"ecosystem":"npm","coordinate":"@putnami/web","packageStep":"npm","publishStep":"npm","dependencies":["@putnami/config","@putnami/runtime"]}]}}`),
		"@putnami/cloud":      json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"oci","coordinate":"putnami/web","packagePublisher":"@putnami/go","packageStep":"docker","publishStep":"docker"}]}}`),
	}
	projection, found, err := ProjectReleaseMetadata(metadata)
	if err != nil || !found || len(projection.Ecosystems) != 2 {
		t.Fatalf("projection = %+v, %v, %v", projection, found, err)
	}
	// Canonical (ecosystem, coordinate) order, not provider order.
	if projection.Ecosystems[0].Ecosystem != "npm" || projection.Ecosystems[0].Coordinate != "@putnami/web" {
		t.Fatalf("first member = %+v", projection.Ecosystems[0])
	}
	if len(projection.Ecosystems[0].Dependencies) != 2 {
		t.Fatalf("npm dependencies = %v", projection.Ecosystems[0].Dependencies)
	}
	if step, ok := projection.PackageStepFor("npm", "@putnami/web"); !ok || step != "npm" {
		t.Fatalf("npm package step = %q, %v", step, ok)
	}
	if step, ok := projection.PublishStepFor("npm", "@putnami/web"); !ok || step != "npm" {
		t.Fatalf("npm publish step = %q, %v", step, ok)
	}
	if projection.Ecosystems[1].Ecosystem != "oci" || projection.Ecosystems[1].Coordinate != "putnami/web" {
		t.Fatalf("second member = %+v", projection.Ecosystems[1])
	}
	if publisher, ok := projection.PublisherFor("npm", "@putnami/web"); !ok || publisher != "@putnami/typescript" {
		t.Fatalf("npm publisher = %q, %v", publisher, ok)
	}
	if publisher, ok := projection.PublisherFor("oci", "putnami/web"); !ok || publisher != "@putnami/cloud" {
		t.Fatalf("oci publisher = %q, %v", publisher, ok)
	}
	if publisher, ok := projection.PackagePublisherFor("oci", "putnami/web"); !ok || publisher != "@putnami/go" {
		t.Fatalf("oci package publisher = %q, %v", publisher, ok)
	}
	if publisher, ok := projection.PackagePublisherFor("npm", "@putnami/web"); !ok || publisher != "@putnami/typescript" {
		t.Fatalf("default npm package publisher = %q, %v", publisher, ok)
	}
	if publisher, ok := projection.PublisherFor("npm", "@putnami/missing"); ok || publisher != "" {
		t.Fatalf("missing publisher = %q, %v", publisher, ok)
	}
	if publisher, ok := projection.PackagePublisherFor("npm", "@putnami/missing"); ok || publisher != "" {
		t.Fatalf("missing package publisher = %q, %v", publisher, ok)
	}
	if step, ok := projection.PackageStepFor("npm", "@putnami/missing"); ok || step != "" {
		t.Fatalf("missing package step = %q, %v", step, ok)
	}
	if step, ok := projection.PublishStepFor("npm", "@putnami/missing"); ok || step != "" {
		t.Fatalf("missing publish step = %q, %v", step, ok)
	}
}

func TestProjectReleaseMetadataAdmitsSeveralMembersInOneEcosystem(t *testing.T) {
	projection, found, err := ProjectReleaseMetadata(map[string]json.RawMessage{
		"@putnami/typescript": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"npm","coordinate":"@putnami/a","packageStep":"npm-a","publishStep":"npm-a"},{"ecosystem":"npm","coordinate":"@putnami/b","packageStep":"npm-b","publishStep":"npm-b"}]}}`),
	})
	if err != nil || !found || len(projection.Ecosystems) != 2 {
		t.Fatalf("projection = %+v, %v, %v", projection, found, err)
	}
}

func TestProjectReleaseMetadataRejectsAmbiguousOrMalformedProjection(t *testing.T) {
	cases := []struct {
		name     string
		metadata map[string]json.RawMessage
		want     string
	}{
		{"duplicate member across providers", map[string]json.RawMessage{
			"a": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"go","coordinate":"go.putnami.dev/x","packageStep":"go","publishStep":"go"}]}}`),
			"b": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"go","coordinate":"go.putnami.dev/x","packageStep":"go","publishStep":"go"}]}}`),
		}, "twice"},
		{"unknown field", map[string]json.RawMessage{"a": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"go","coordinate":"x","packageStep":"go","publishStep":"go","extra":true}]}}`)}, "unknown field"},
		{"unsorted", map[string]json.RawMessage{"a": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"go","coordinate":"x","packageStep":"go","publishStep":"go","dependencies":["z","a"]}]}}`)}, "sorted and unique"},
		{"duplicate dependency", map[string]json.RawMessage{"a": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"go","coordinate":"x","packageStep":"go","publishStep":"go","dependencies":["a","a"]}]}}`)}, "sorted and unique"},
		{"bad ecosystem id", map[string]json.RawMessage{"a": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"NPM","coordinate":"x","packageStep":"go","publishStep":"go"}]}}`)}, "invalid ecosystem"},
		{"empty coordinate", map[string]json.RawMessage{"a": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"go","coordinate":"","packageStep":"go","publishStep":"go"}]}}`)}, "invalid coordinate"},
		{"invalid package publisher", map[string]json.RawMessage{"a": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"go","coordinate":"x","packagePublisher":" @putnami/go","packageStep":"go","publishStep":"go"}]}}`)}, "invalid package publisher"},
		{"empty package step", map[string]json.RawMessage{"a": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"go","coordinate":"x","publishStep":"go"}]}}`)}, "projects sync"},
		{"empty publish step", map[string]json.RawMessage{"a": json.RawMessage(`{"releaseSet":{"ecosystems":[{"ecosystem":"go","coordinate":"x","packageStep":"go"}]}}`)}, "projects sync"},
		{"trailing", map[string]json.RawMessage{"a": json.RawMessage(`{"releaseSet":{"ecosystems":[]} {}}`)}, "decode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ProjectReleaseMetadata(tc.metadata)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// The old shape carried one ecosystem at the top level of the releaseSet block.
// A workspace index written before this change decodes into an empty member
// list, so the release would silently publish nothing for that project; the
// error has to name the command that re-probes the workspace.
func TestProjectReleaseMetadataNamesProjectsSyncForTheOldShape(t *testing.T) {
	_, _, err := ProjectReleaseMetadata(map[string]json.RawMessage{
		"@putnami/go": json.RawMessage(`{"releaseSet":{"ecosystem":"go","dependencies":["go.putnami.dev/config"]}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "putnami projects sync") {
		t.Fatalf("error = %v, want the projects sync remedy", err)
	}
}

func TestProjectReleaseMetadataDistinguishesAuthoritativeEmptyFromAbsent(t *testing.T) {
	projection, found, err := ProjectReleaseMetadata(map[string]json.RawMessage{
		"go": json.RawMessage(`{"releaseSet":{"ecosystems":[]}}`),
	})
	if err != nil || !found || len(projection.Ecosystems) != 0 {
		t.Fatalf("empty projection = %+v, %v, %v", projection, found, err)
	}
	projection, found, err = ProjectReleaseMetadata(map[string]json.RawMessage{
		"ts": json.RawMessage(`{"main":"index.js"}`),
	})
	if err != nil || found || len(projection.Ecosystems) != 0 {
		t.Fatalf("absent projection = %+v, %v, %v", projection, found, err)
	}
}

// TestKindForClassifiesDeclaredStepsAndFallsBackToTheEcosystem pins the one
// step-to-role table. A consumer selects members by kind, so a step the table
// does not know must classify NOTHING rather than guess a role that would make
// that consumer bind the wrong artifact.
func TestKindForClassifiesDeclaredStepsAndFallsBackToTheEcosystem(t *testing.T) {
	cases := []struct {
		name        string
		ecosystem   distribution.Ecosystem
		packageStep string
		publishStep string
		want        distribution.MemberKind
	}{
		{"npm package", "npm", "npm", "npm", distribution.KindLibrary},
		{"go module", "go", "go", "go", distribution.KindLibrary},
		{"image packaged by docker", "oci", "docker", "docker", distribution.KindImage},
		{"image packaged by a renamed step", "oci", "service-image", "docker", distribution.KindImage},
		{"image with an unknown step falls back to its ecosystem", "oci", "bake", "ship", distribution.KindImage},
		{"cloud config", "put", "cloud-config-member", "cloud-publish-config", distribution.KindConfig},
		{"cloud migration", "put", "migrations", "cloud-publish-migration", distribution.KindMigration},
		{"cloud archives", "archive", "archives", "cloud-publish-archives", distribution.KindArchive},
		{"template archive", "archive", "template", "cloud-publish-archives", distribution.KindArchive},
		{"site content", "put", "site-content", "site-content", distribution.KindDoc},
		{"cloud site content", "put", "cloud-site-content", "cloud-publish-site-content", distribution.KindDoc},
		{"cloud site content with an unknown publish step", "put", "cloud-site-content", "bespoke", distribution.KindDoc},
		{"deployment declaration", "put", "deployment", "publish-deployment", distribution.KindDeployment},
		{"deployment declaration with an unknown publish step", "put", "deployment", "bespoke", distribution.KindDeployment},
		{"unknown step in a generic ecosystem", "put", "bespoke", "bespoke", ""},
		{"nothing declared", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := KindFor(tc.ecosystem, tc.packageStep, tc.publishStep)
			if got != tc.want {
				t.Fatalf("KindFor(%q, %q, %q) = %q, want %q", tc.ecosystem, tc.packageStep, tc.publishStep, got, tc.want)
			}
			if got != "" && !got.Valid() {
				t.Fatalf("KindFor produced %q, which is outside the protocol vocabulary", got)
			}
		})
	}

	// The publish step decides: it is the step that writes the artifact to its
	// registry, so a package step borrowed from another role never wins.
	if got := KindFor("put", "docker", "cloud-publish-config"); got != distribution.KindConfig {
		t.Fatalf("publish step lost to package step: %q", got)
	}

	// A deployment declaration is emitted only through a member whose declared
	// step maps to it: no ecosystem implies the kind on its own.
	for ecosystem, kind := range kindByEcosystem {
		if kind == distribution.KindDeployment {
			t.Fatalf("ecosystem %q implies the deployment kind without a declared step", ecosystem)
		}
	}
	if got := KindFor("put", "", ""); got != "" {
		t.Fatalf("a put member with no declared step classified as %q", got)
	}
}
