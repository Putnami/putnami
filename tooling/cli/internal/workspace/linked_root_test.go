package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	model "go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/dirlink"
)

// cli-model cannot import dirlink (spec cli/workspace-model#model-purity), so
// it keeps its own standard-library resolver. Both must answer the same for
// every path the CLI resolves: a directory, a directory link (a junction on
// Windows), a link inside a linked directory, a link to a link, and a path
// whose tail does not exist.
func TestModelResolveLinksAgreesWithDirlink(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "workspace-root", "the-discovered-root-is-canonicalized")
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	nested := filepath.Join(real, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(parent, "outer")
	if err := dirlink.Create(real, outer); err != nil {
		t.Fatal(err)
	}
	if err := dirlink.Create(nested, filepath.Join(real, "inner")); err != nil {
		t.Fatal(err)
	}
	chain := filepath.Join(parent, "chain")
	if err := dirlink.Create(outer, chain); err != nil {
		t.Fatal(err)
	}
	physicalNested, err := dirlink.Resolve(nested)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "a plain directory", path: nested, want: physicalNested},
		{name: "a directory link", path: filepath.Join(outer, "a", "b"), want: physicalNested},
		{name: "a nested link", path: filepath.Join(outer, "inner"), want: physicalNested},
		{name: "a link to a link", path: filepath.Join(chain, "inner"), want: physicalNested},
		{name: "a missing tail", path: filepath.Join(outer, "inner", "missing", "tail")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := dirlink.Resolve(tc.path)
			got, gotErr := model.ResolveLinks(tc.path)
			if tc.want == "" {
				if !errors.Is(wantErr, fs.ErrNotExist) || !errors.Is(gotErr, fs.ErrNotExist) {
					t.Fatalf("dirlink.Resolve = %q, %v; model.ResolveLinks = %q, %v; want fs.ErrNotExist from both", want, wantErr, got, gotErr)
				}
				return
			}
			if wantErr != nil || gotErr != nil {
				t.Fatalf("dirlink.Resolve = %v; model.ResolveLinks = %v", wantErr, gotErr)
			}
			if got != want || want != tc.want {
				t.Fatalf("model.ResolveLinks = %q, dirlink.Resolve = %q, want both %q", got, want, tc.want)
			}
		})
	}
}

// A workspace entered through a directory link (a junction on Windows) takes
// the directory's own path as its root, the one Git reports. A changed file
// then finds its owner under either spelling, --impacted selects its owner and
// dependents, and the load cache keeps one entry for both spellings.
func TestALinkedWorkspaceRootResolvesOwnersAndImpact(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "workspace-root", "the-discovered-root-is-canonicalized")
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	writeFile(t, filepath.Join(realRoot, wsproto.WorkspaceConfigFilename), `{"name":"ws","includes":["packages/app","packages/lib","packages/utils"]}`)
	writeFile(t, filepath.Join(realRoot, "packages", "app", wsproto.ConfigFilename), `{"name":"app","dependencies":["lib"]}`)
	writeFile(t, filepath.Join(realRoot, "packages", "lib", wsproto.ConfigFilename), `{"name":"lib"}`)
	writeFile(t, filepath.Join(realRoot, "packages", "utils", wsproto.ConfigFilename), `{"name":"utils"}`)
	gitInitWithBaseline(t, realRoot)
	link := filepath.Join(parent, "link")
	if err := dirlink.Create(realRoot, link); err != nil {
		t.Fatal(err)
	}
	physical, err := dirlink.Resolve(realRoot)
	if err != nil {
		t.Fatal(err)
	}

	if root, err := FindRoot(filepath.Join(link, "packages", "lib")); err != nil || root != physical {
		t.Errorf("FindRoot through the link = %q, %v, want %q", root, err, physical)
	}
	if got, want := loadKey(link), loadKey(realRoot); got != physical || want != physical {
		t.Errorf("loadKey(link) = %q, loadKey(real) = %q, want both %q", got, want, physical)
	}
	InvalidateLoadCache(link)
	ws, err := Load(link)
	if err != nil {
		t.Fatalf("Load through the link: %v", err)
	}
	if ws.Root != physical {
		t.Errorf("Load through the link: Root = %q, want %q", ws.Root, physical)
	}
	if again, err := Load(realRoot); err != nil || again != ws {
		t.Errorf("Load of the real root = %p, %v, want the cached %p", again, err, ws)
	}

	libFile := filepath.Join("packages", "lib", "lib.go")
	writeFile(t, filepath.Join(realRoot, libFile), "package lib\n")
	for _, spelling := range []string{physical, link} {
		owners := ProjectOwnersForPath(ws, filepath.Join(spelling, libFile))
		if len(owners) != 1 || owners[0].Name != "lib" {
			t.Errorf("ProjectOwnersForPath(%s) = %v, want lib", filepath.Join(spelling, libFile), impactNameSet(owners))
		}
	}

	impacted, err := ImpactedProjects(ws, "main")
	if err != nil {
		t.Fatalf("ImpactedProjects: %v", err)
	}
	names := impactNameSet(impacted)
	if len(names) != 2 || !names["lib"] || !names["app"] {
		t.Fatalf("ImpactedProjects through the link = %v, want lib and app", names)
	}
}
