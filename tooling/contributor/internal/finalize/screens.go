package finalize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	collab "go.putnami.dev/protocol/collaboration"
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
	c.manifests = nil
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
		if found := screens.Check(entry.manifest, filepath.Dir(entry.file), c.head, project.Project, project.Pages); len(found) > 0 {
			problems = append(problems, fmt.Sprintf("the manifest of %s does not prove the head: %s", project.Project, boundedList(found)))
			continue
		}
		c.manifests = append(c.manifests, acceptedManifest{project: project.Project, file: entry.file})
		accepted = append(accepted, fmt.Sprintf("%s, %s of %s", project.Project, screens.Plural(len(entry.manifest.Captures), "capture"),
			screens.Plural(len(entry.manifest.Pages), "page")))
	}
	if len(problems) > 0 {
		c.manifests = nil
		return fmt.Errorf("a ready change that touches web pages needs their screenshots: %s; run `putnami change screenshots --project <dir>` on the committed head and pass --screenshots <manifest>, or pass --waive-screenshots <reason>",
			strings.Join(problems, "; "))
	}
	c.screenshots = strings.Join(accepted, "; ")
	return nil
}

// acceptedManifest is one --screenshots manifest a ready publication
// accepted for a touched web project.
type acceptedManifest struct {
	// project is the web project the manifest captures.
	project string
	// file is the absolute path of the manifest.
	file string
}

// uploadScreenshots sends each accepted manifest, once, to the proposals
// provider, after the proposal is published and before the verification
// comment. A provider that does not offer proposals.screenshots gets no
// call, and one that answers unsupported changes nothing: the report names
// why, and the run goes on. Any other failure is an error, since the
// screenshots are part of what the verification comment says was verified.
// It returns nil when the publication accepted no manifest.
func (f Finalizer) uploadScreenshots(ctx context.Context, c *publicationCandidate, proposal collab.Ref) (*ScreenshotsUpload, error) {
	if len(c.manifests) == 0 {
		return nil, nil
	}
	report := &ScreenshotsUpload{}
	if !operationSupported(c.proposalCaps, collab.OperationScreenshots) {
		report.Skipped = "the proposals provider does not offer screenshots"
		return report, nil
	}
	for _, manifest := range c.manifests {
		if _, err := c.recheck(ctx, f); err != nil {
			return nil, err
		}
		request, err := json.Marshal(collab.ProposalScreenshotsInput{Ref: proposal, Commit: c.head, Manifest: manifest.file})
		if err != nil {
			return nil, err
		}
		if f.Call == nil {
			return nil, errors.New("collaboration provider unavailable")
		}
		envelope, err := f.Call(ctx, collab.ContractProposals, collab.OperationScreenshots, request)
		if err != nil {
			return nil, fmt.Errorf("upload the screenshots of %s: %w", manifest.project, err)
		}
		if envelope.Outcome == collab.OutcomeUnsupported {
			report.Skipped = "the proposals provider answered unsupported"
			if envelope.Error != nil && envelope.Error.Message != "" {
				report.Skipped = envelope.Error.Message
			}
			return report, nil
		}
		if err := envelopeError(collab.ContractProposals, collab.OperationScreenshots, envelope); err != nil {
			return nil, fmt.Errorf("upload the screenshots of %s: %w", manifest.project, err)
		}
		var stored collab.ProposalScreenshotsResult
		if err := json.Unmarshal(envelope.Result, &stored); err != nil {
			return nil, fmt.Errorf("invalid proposals.screenshots result: %w", err)
		}
		if stored.Ref != proposal || stored.Commit != c.head || path.Clean(stored.Project) != manifest.project {
			return nil, fmt.Errorf("proposals.screenshots answered %s of %s/%s at %s, not %s of the published proposal at %s",
				stored.Project, stored.Ref.Source, stored.Ref.ID, stored.Commit, manifest.project, c.head)
		}
		report.Manifests = append(report.Manifests, UploadedManifest{Project: manifest.project, Captures: stored.Captures, Uploaded: stored.Uploaded})
	}
	return report, nil
}

// validWaiver checks the reason of --waive-screenshots: one line, at most
// maxWaiverBytes.
func validWaiver(reason string) error {
	if strings.TrimSpace(reason) == "" || strings.ContainsAny(reason, "\r\n") || len(reason) > maxWaiverBytes {
		return fmt.Errorf("--waive-screenshots needs a one-line reason of at most %d bytes", maxWaiverBytes)
	}
	return nil
}
