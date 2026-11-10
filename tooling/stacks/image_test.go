package stacks

import (
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/releaseset"
)

// ociPathComponent is the grammar of one path component of an OCI repository
// name, from the OCI distribution specification. It has no uppercase letter.
var ociPathComponent = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$`)

func TestImageNameIsOneLowercasePathSegment(t *testing.T) {
	for project, want := range map[string]string{
		"shop":                  "shop",
		"userService":           "userservice",
		"UserService":           "userservice",
		"@acme/shop":            "acme-shop",
		"@Acme/Billing-API":     "acme-billing-api",
		"surfaces/admin/Portal": "surfaces-admin-portal",
	} {
		got := ImageName(project)
		if got != want {
			t.Errorf("ImageName(%q) = %q, want %q", project, got, want)
		}
		if !ociPathComponent.MatchString(got) {
			t.Errorf("ImageName(%q) = %q is not an OCI repository path component", project, got)
		}
	}
}

func TestAProjectNamedWithUppercaseLettersPublishesALowercaseImage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "registry.test/acme"}}}`)
	writeFile(t, root, "services/user/putnami.json", `{"options": {"publish": {"docker": true}}}`)

	got := ImageMember(root, "services/user", "userService")
	want := []releaseset.MemberDeclaration{{Ecosystem: "oci", Coordinate: "acme/userservice", PackageStep: "docker", PublishStep: "docker"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("member = %+v, want %+v", got, want)
	}
	// The shared publisher finds the member of a packaged image by the suffix
	// "/" + the image name the package step wrote.
	if image := ImageName("userService"); got[0].Coordinate != "acme/"+image {
		t.Errorf("coordinate %q does not end with the packaged image %q", got[0].Coordinate, image)
	}
}

func TestImageMemberNamesTheProjectAsTheWorkspaceDoes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "putnami.workspace.json", `{"registries": {"oci": {"publish": "registry.test/acme"}}}`)
	writeFile(t, root, "services/putnami.json", `{"publishConfig": {"docker": {"namePattern": "@Acme/{name}"}}}`)
	writeFile(t, root, "services/Billing/putnami.json", `{"publish": ["docker"]}`)
	writeFile(t, root, "services/Named/putnami.json", `{"name": "Shop-API", "publish": ["docker"]}`)
	writeFile(t, root, "apps/Portal/putnami.json", `{"publish": ["docker"], "registries": {"oci": {"publish": "ghcr.io/team/web/"}}}`)

	for _, tc := range []struct {
		dir, sourceName, want string
	}{
		{"services/Billing", "", "acme/acme-billing"},
		{"services/Billing", "billingService", "acme/billingservice"},
		{"services/Named", "ignored", "acme/shop-api"},
		{"apps/Portal", "", "team/web/portal"},
	} {
		got := ImageMember(root, tc.dir, tc.sourceName)
		if len(got) != 1 || got[0].Coordinate != tc.want {
			t.Errorf("ImageMember(%q, %q) = %+v, want coordinate %q", tc.dir, tc.sourceName, got, tc.want)
		}
	}
}

func TestImageMemberIsNoneWithoutAnImageToPublish(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "lib/putnami.json", `{"publish": ["npm"]}`)
	writeFile(t, root, "broken/putnami.json", `{"publish": `)
	writeFile(t, root, "bare/putnami.json", `{"options": {"publish": {"docker": true}}}`)
	for _, dir := range []string{"lib", "broken", "absent"} {
		if got := ImageMember(root, dir, "Lib"); got != nil {
			t.Errorf("ImageMember(%q) = %+v, want none", dir, got)
		}
	}
	// No registry names a namespace: the coordinate is the image name alone.
	if got := ImageMember(root, "bare", "Bare"); len(got) != 1 || got[0].Coordinate != "bare" {
		t.Errorf("ImageMember without a registry = %+v", got)
	}
}

func TestLoadImageNeedsATagAndALayout(t *testing.T) {
	if err := LoadImage(t.TempDir(), nil, nil); err == nil {
		t.Error("no tag is an error")
	}
	if err := LoadImage(filepath.Join(t.TempDir(), "missing"), []string{"demo:c-1"}, nil); err == nil || !strings.Contains(err.Error(), "docker tarball") {
		t.Errorf("a missing layout fails before docker runs: %v", err)
	}
}
