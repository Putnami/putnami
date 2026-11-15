package finalize

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	wire "go.putnami.dev/protocol/extension/hostrpc"

	"go.putnami.dev/tooling/contributor/internal/screens"
)

// TouchedProject is one web project whose pages a change touches, with
// those pages.
type TouchedProject struct {
	// Project is the project's workspace-relative directory.
	Project string
	Pages   []screens.Page
}

// TouchedScreens names the web projects whose pages the paths changed
// between base and head touch, and the pages, under the rule putnami change
// screenshots selects them with. paths are relative to the repository's top
// level, as the Git seam answers them.
type TouchedScreens func(ctx context.Context, base, head string, paths []string) ([]TouchedProject, error)

// HostScreens resolves TouchedScreens with the projects that own each path
// taken from owners, and their route inventories read from git in root.
func HostScreens(root string, owners WorkspaceOwners) TouchedScreens {
	return func(ctx context.Context, base, head string, paths []string) ([]TouchedProject, error) {
		prefix, err := workspacePrefix(root)
		if err != nil {
			return nil, fmt.Errorf("resolve the web projects a change touches: %w", err)
		}
		var relative []string
		for _, candidate := range paths {
			if rel, inside := strings.CutPrefix(candidate, prefix); inside {
				relative = append(relative, rel)
			}
		}
		if len(relative) == 0 {
			return nil, nil
		}
		answer, err := owners(ctx, relative)
		if err != nil {
			return nil, fmt.Errorf("resolve the web projects a change touches: %w", err)
		}
		projects := map[string]bool{}
		for _, entry := range answer {
			for _, project := range entry.Projects {
				projects[path.Clean(project.Path)] = true
			}
		}
		names := make([]string, 0, len(projects))
		for project := range projects {
			names = append(names, project)
		}
		sort.Strings(names)
		var touched []TouchedProject
		for _, project := range names {
			inventories := make([]*screens.Inventory, 2)
			for i, revision := range []string{base, head} {
				data, found, err := fileAt(root, revision, path.Join(project, screens.InventoryPath))
				if err != nil {
					return nil, fmt.Errorf("read the route inventory of %s: %w", project, err)
				}
				if !found {
					continue
				}
				if inventories[i], err = screens.ParseInventory(data); err != nil {
					return nil, fmt.Errorf("%s/%s: %w", project, screens.InventoryPath, err)
				}
			}
			pages, err := screens.TouchedRoutes(screens.Selection{Project: project, Changed: relative,
				Base: inventories[0], Head: inventories[1]})
			if err != nil {
				return nil, err
			}
			if needsCaptures(pages) {
				touched = append(touched, TouchedProject{Project: project, Pages: pages})
			}
		}
		return touched, nil
	}
}

// needsCaptures reports whether a touched page exists at the head: a change
// that only removes pages has nothing to show after.
func needsCaptures(pages []screens.Page) bool {
	for _, page := range pages {
		if page.After {
			return true
		}
	}
	return false
}

// onceOwners asks owners once for each list of paths in a run: the
// requirement ownership and the web project check ask about the same
// changed paths.
func onceOwners(owners WorkspaceOwners) WorkspaceOwners {
	type answer struct {
		owners []wire.PathOwners
		err    error
	}
	var mu sync.Mutex
	answers := map[string]answer{}
	return func(ctx context.Context, paths []string) ([]wire.PathOwners, error) {
		key := strings.Join(paths, "\x00")
		mu.Lock()
		defer mu.Unlock()
		if known, ok := answers[key]; ok {
			return known.owners, known.err
		}
		got, err := owners(ctx, paths)
		if err == nil {
			answers[key] = answer{owners: got}
		}
		return got, err
	}
}

// maxWaiverBytes bounds the reason of --waive-screenshots.
const maxWaiverBytes = 300

// checkScreenshots admits a ready publication of a change that touches a page
// of a web project only with a manifest of the head for each such project,
// or with a waiver. It records what it admitted for the verification line.
func (f Finalizer) checkScreenshots(ctx context.Context, c *publicationCandidate) error {
	if c.opts.Draft {
		return nil
	}
	if f.Screens == nil {
		return errors.New("web project ownership unavailable")
	}
	paths, err := f.Git.ChangedPaths(c.base)
	if err != nil {
		return err
	}
	touched, err := f.Screens(ctx, c.baseSHA, c.head, paths)
	if err != nil {
		return err
	}
	if len(touched) == 0 {
		c.screenshots = "no page touched"
		if c.opts.WaiveScreenshots != "" || len(c.opts.Screenshots) > 0 {
			// A waiver or a manifest the change does not need is named, not
			// dropped, so the reader sees what the run ignored.
			c.screenshots += ", so the screenshots given are unused"
		}
		return nil
	}
	if c.opts.WaiveScreenshots != "" {
		c.screenshots = "waived: " + c.opts.WaiveScreenshots
		return nil
	}
	type read struct {
		file     string
		manifest screens.Manifest
	}
	manifests := map[string]read{}
	for _, file := range c.opts.Screenshots {
		full := file
		if !filepath.IsAbs(full) {
			full = filepath.Join(f.Root, file)
		}
		manifest, err := screens.ReadManifest(full)
		if err != nil {
			return fmt.Errorf("--screenshots %s: %w", file, err)
		}
		project := path.Clean(manifest.Project)
		if previous, twice := manifests[project]; twice {
			return fmt.Errorf("--screenshots %s and %s both capture %s", previous.file, file, project)
		}
		manifests[project] = read{file: full, manifest: manifest}
	}
	var problems, accepted []string
	for _, project := range touched {
		entry, ok := manifests[project.Project]
		if !ok {
			var routes []string
			for _, page := range project.Pages {
				if page.After {
					routes = append(routes, page.Route)
				}
			}
			problems = append(problems, fmt.Sprintf("%s has no --screenshots manifest for its touched pages %s", project.Project, boundedList(routes)))
			continue
		}
		if found := entry.manifest.Check(filepath.Dir(entry.file), c.head, project.Project, project.Pages); len(found) > 0 {
			problems = append(problems, fmt.Sprintf("the manifest of %s does not prove the head: %s", project.Project, boundedList(found)))
			continue
		}
		accepted = append(accepted, fmt.Sprintf("%s, %s of %s", project.Project, screens.Plural(len(entry.manifest.Captures), "capture"),
			screens.Plural(len(entry.manifest.Pages), "page")))
	}
	if len(problems) > 0 {
		return fmt.Errorf("a ready change that touches web pages needs their screenshots: %s; run `putnami change screenshots --project <dir>` on the committed head and pass --screenshots <manifest>, or pass --waive-screenshots <reason>",
			strings.Join(problems, "; "))
	}
	c.screenshots = strings.Join(accepted, "; ")
	return nil
}

// validWaiver checks the reason of --waive-screenshots: one line, at most
// maxWaiverBytes.
func validWaiver(reason string) error {
	if strings.TrimSpace(reason) == "" || strings.ContainsAny(reason, "\r\n") || len(reason) > maxWaiverBytes {
		return fmt.Errorf("--waive-screenshots needs a one-line reason of at most %d bytes", maxWaiverBytes)
	}
	return nil
}
