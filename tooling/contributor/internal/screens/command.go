package screens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/protocol/screenshots"
)

// Git runs one git command in the workspace root and returns its standard
// output.
type Git func(ctx context.Context, args ...string) (string, error)

// SystemGit runs git in root.
func SystemGit(root string) Git {
	return func(ctx context.Context, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return string(out), nil
	}
}

// Launcher starts the capturer of a run and returns the function that stops
// it.
type Launcher func(ctx context.Context, browser string, state *StorageState) (Capturer, func() error, error)

// LaunchBrowser is the Launcher that starts a headless Chromium.
func LaunchBrowser(ctx context.Context, browser string, state *StorageState) (Capturer, func() error, error) {
	b, err := Launch(ctx, browser, state)
	if err != nil {
		return nil, nil, err
	}
	return b, b.Close, nil
}

// Command is one run of `putnami change screenshots`.
type Command struct {
	// Root is the workspace root, the directory putnami launches the
	// command in.
	Root string
	// ContextFile is the job context putnami passes with --putnamiContext.
	ContextFile string
	Git         Git
	Launch      Launcher
	Getenv      func(string) string
	LookPath    func(string) (string, error)
	Stdout      io.Writer
	Stderr      io.Writer
}

type options struct {
	project, base, baseURL, headURL string
	storageState, fixtures, out     string
	browser                         string
	routes                          []string
}

// Result is the JSON result of a run.
type Result struct {
	Manifest string   `json:"manifest,omitempty"`
	Project  string   `json:"project"`
	Pages    []Page   `json:"pages"`
	Captures int      `json:"captures"`
	Notes    []string `json:"notes,omitempty"`
}

// Run captures the pages args select and returns the exit code: 2 for a
// usage error, 1 for a failed run, 0 once the manifest is written or when the
// change touches no page.
func (c Command) Run(ctx context.Context, args []string) int {
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(c.Stderr, "change screenshots: %v\n", err)
		return 2
	}
	var launch *job.Context
	if c.ContextFile != "" {
		if launch, err = job.ParseFile(c.ContextFile); err != nil {
			fmt.Fprintf(c.Stderr, "change screenshots: %v\n", err)
			return 1
		}
	}
	base := c.Root
	if launch != nil && launch.InvocationDir != "" {
		base = launch.InvocationDir
	}
	result, err := c.run(ctx, opts, base)
	if err != nil {
		fmt.Fprintf(c.Stderr, "change screenshots: %v\n", err)
		return 1
	}
	if launch != nil && protocolcli.OutputMode(launch.Params.String("output")).IsMachineReadable() {
		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(c.Stderr, "change screenshots: %v\n", err)
			return 1
		}
		fmt.Fprintln(c.Stdout, string(data))
		return 0
	}
	if result.Manifest == "" {
		fmt.Fprintf(c.Stdout, "The change touches no page of %s: nothing to capture.\n", result.Project)
		return 0
	}
	fmt.Fprintf(c.Stdout, "Captured %s of %s in %s.\n", Plural(len(result.Pages), "page"), result.Project, Plural(result.Captures, "file"))
	for _, page := range result.Pages {
		fmt.Fprintf(c.Stdout, "  %s (%s): %s\n", page.Path, page.File, sidesText(page))
	}
	for _, note := range result.Notes {
		fmt.Fprintf(c.Stdout, "  note: %s\n", note)
	}
	fmt.Fprintf(c.Stdout, "Manifest: %s\n", result.Manifest)
	return 0
}

func sidesText(page Page) string {
	switch {
	case page.Before && page.After:
		return "before and after"
	case page.After:
		return "after only, a new page"
	default:
		return "before only, a removed page"
	}
}

// resolve makes a path the user typed absolute against dir.
func resolve(dir, file string) string {
	if file == "" || filepath.IsAbs(file) {
		return file
	}
	return filepath.Join(dir, file)
}

func (c Command) run(ctx context.Context, opts options, invocation string) (Result, error) {
	project := path.Clean(filepath.ToSlash(opts.project))
	if strings.HasPrefix(project, "../") || project == ".." || path.IsAbs(project) {
		return Result{}, fmt.Errorf("--project %s must be a directory inside the workspace", opts.project)
	}
	if info, err := os.Stat(filepath.Join(c.Root, project)); err != nil || !info.IsDir() {
		return Result{}, fmt.Errorf("--project %s names no directory of the workspace", opts.project)
	}
	result := Result{Project: project}
	head, err := c.revParse(ctx, "HEAD")
	if err != nil {
		return result, err
	}
	baseRef, err := c.baseRef(ctx, opts.base)
	if err != nil {
		return result, err
	}
	mergeBase, err := c.Git(ctx, "merge-base", baseRef, head)
	if err != nil {
		return result, err
	}
	mergeBase = strings.TrimSpace(mergeBase)
	dirty, err := c.Git(ctx, "status", "--porcelain", "--untracked-files=normal", "--", project)
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(dirty) != "" {
		return result, fmt.Errorf("%s holds uncommitted changes: commit them, so the head server serves the committed head", project)
	}
	changedText, err := c.Git(ctx, "diff", "--name-only", "--no-renames", "--relative", "-z", mergeBase, head)
	if err != nil {
		return result, err
	}
	var changed []string
	for _, p := range strings.Split(changedText, "\x00") {
		if p != "" {
			changed = append(changed, p)
		}
	}
	sel := Selection{Project: project, Changed: changed, Routes: opts.routes}
	if sel.Head, err = c.inventoryAt(ctx, head, project); err != nil {
		return result, err
	}
	if sel.Base, err = c.inventoryAt(ctx, mergeBase, project); err != nil {
		return result, err
	}
	if len(sel.Head.Pages()) == 0 && len(sel.Base.Pages()) == 0 {
		return result, fmt.Errorf("%s is no web project: its %s declares no page at the head or the base", project, InventoryPath)
	}
	var fixtures map[string]map[string]string
	if opts.fixtures != "" {
		data, err := readBounded(resolve(invocation, opts.fixtures), 1<<20)
		if err != nil {
			return result, fmt.Errorf("--fixtures %s: %w", opts.fixtures, err)
		}
		if fixtures, err = ParseFixtures(data); err != nil {
			return result, fmt.Errorf("--fixtures %s: %w", opts.fixtures, err)
		}
	}
	pages, err := Select(sel, fixtures)
	if err != nil {
		return result, err
	}
	result.Pages = pages
	if len(pages) == 0 {
		return result, nil
	}
	urls := map[string]string{SideBefore: opts.baseURL, SideAfter: opts.headURL}
	for _, page := range pages {
		if page.Before && opts.baseURL == "" {
			return result, fmt.Errorf("%s exists at the base: --base-url is required", page.Route)
		}
	}
	var state *StorageState
	if opts.storageState != "" {
		if state, err = ReadStorageState(resolve(invocation, opts.storageState)); err != nil {
			return result, err
		}
	}
	browser, err := FindBrowser(opts.browser, c.Getenv, c.LookPath)
	if err != nil {
		return result, err
	}
	out := resolve(invocation, opts.out)
	if out == "" {
		out = filepath.Join(c.Root, ".putnami", "screenshots", Slug(project))
	}
	capturer, stop, err := c.Launch(ctx, browser, state)
	if err != nil {
		return result, err
	}
	files, captures, notes, err := captureAll(ctx, capturer, pages, urls)
	stopErr := stop()
	if err != nil {
		return result, err
	}
	if stopErr != nil {
		fmt.Fprintf(c.Stderr, "change screenshots: stop the browser: %v\n", stopErr)
	}
	manifest := Manifest{ProtocolVersion: screenshots.ProtocolVersion, Project: project, BaseSHA: mergeBase, HeadSHA: head,
		Pages: pages, Captures: captures}
	file, err := writeOutput(out, files, manifest)
	if err != nil {
		return result, err
	}
	result.Manifest = file
	result.Captures = len(captures)
	result.Notes = notes
	return result, nil
}

// captureAll takes every capture of every page, in page order, then side,
// viewport and scheme. A failed navigation or an error status stops the run.
// A page that ends at another URL is captured, with a note for the reader:
// a redirect to a sign-in page shows no change.
func captureAll(ctx context.Context, capturer Capturer, pages []Page, urls map[string]string) (map[string][]byte, []Capture, []string, error) {
	files := map[string][]byte{}
	var captures []Capture
	var notes []string
	noted := map[string]bool{}
	for i, page := range pages {
		for _, side := range []string{SideBefore, SideAfter} {
			if side == SideBefore && !page.Before || side == SideAfter && !page.After {
				continue
			}
			target, err := joinURL(urls[side], page.Path)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, viewport := range Viewports {
				for _, scheme := range Schemes {
					got, err := capturer.Capture(ctx, Shot{URL: target, Viewport: viewport, Scheme: scheme})
					if err != nil {
						return nil, nil, nil, fmt.Errorf("%s %s (%s %s): %w", side, page.Path, viewport.Name, scheme, err)
					}
					if got.Status >= 400 {
						return nil, nil, nil, fmt.Errorf("%s %s answered HTTP %d at %s", side, page.Path, got.Status, target)
					}
					if got.URL != "" && got.URL != target && !noted[side+target] {
						noted[side+target] = true
						notes = append(notes, fmt.Sprintf("%s %s ended at %s", side, page.Path, got.URL))
					}
					if len(got.PNG) > screenshots.MaxCaptureBytes {
						return nil, nil, nil, fmt.Errorf("%s %s (%s %s): the capture is %d bytes, more than the %d a capture may be",
							side, page.Path, viewport.Name, scheme, len(got.PNG), screenshots.MaxCaptureBytes)
					}
					name := CaptureName(i, page, side, viewport, scheme)
					files[name] = got.PNG
					captures = append(captures, Capture{File: name, Route: page.Route, Side: side, Viewport: viewport.Name,
						Scheme: scheme, SHA256: Digest(got.PNG), Status: got.Status, URL: got.URL})
				}
			}
		}
	}
	return files, captures, notes, nil
}

// joinURL appends an escaped page path to a server URL, keeping the
// server's own path prefix.
func joinURL(server, page string) (string, error) {
	parsed, err := url.Parse(server)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("%q is no http or https URL", server)
	}
	return parsed.Scheme + "://" + parsed.Host + strings.TrimSuffix(parsed.EscapedPath(), "/") + page, nil
}

// writeOutput writes the captures and the manifest into out. It serializes
// the manifest first, so a manifest the protocol refuses writes nothing. It
// then removes the files a previous manifest there lists, and nothing else.
func writeOutput(out string, files map[string][]byte, manifest Manifest) (string, error) {
	data, err := screenshots.Marshal(manifest)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	file := filepath.Join(out, ManifestFile)
	// The previous manifest is parsed, not validated: a manifest the contract
	// refuses still names the files its run wrote.
	if previous, err := readBounded(file, screenshots.MaxManifestBytes); err == nil {
		if listed, _ := screenshots.Parse(previous); listed != nil {
			for _, capture := range listed.Captures {
				if ownFile(capture.File) {
					_ = os.Remove(filepath.Join(out, capture.File))
				}
			}
		}
	}
	_ = os.Remove(file)
	for _, capture := range manifest.Captures {
		if err := os.WriteFile(filepath.Join(out, capture.File), files[capture.File], 0o644); err != nil {
			return "", err
		}
	}
	return file, os.WriteFile(file, data, 0o644)
}

func (c Command) revParse(ctx context.Context, ref string) (string, error) {
	out, err := c.Git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s names no commit", ref)
	}
	return strings.TrimSpace(out), nil
}

// baseRef resolves --base: origin/<base> when the remote-tracking branch
// exists, else <base> itself.
func (c Command) baseRef(ctx context.Context, base string) (string, error) {
	if !strings.HasPrefix(base, "origin/") {
		if sha, err := c.revParse(ctx, "refs/remotes/origin/"+base); err == nil {
			return sha, nil
		}
	}
	sha, err := c.revParse(ctx, base)
	if err != nil {
		return "", fmt.Errorf("--base %s names no commit and no origin branch", base)
	}
	return sha, nil
}

// inventoryAt reads the project's route inventory at a commit; nil when the
// commit has none.
func (c Command) inventoryAt(ctx context.Context, commit, project string) (*Inventory, error) {
	// A ./ path is relative to the workspace root, which may lie below the
	// repository's top level.
	file := path.Join(project, InventoryPath)
	if _, err := c.Git(ctx, "cat-file", "-e", commit+":./"+file); err != nil {
		return nil, nil
	}
	data, err := c.Git(ctx, "show", commit+":./"+file)
	if err != nil {
		return nil, err
	}
	inventory, err := ParseInventory([]byte(data))
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", file, short(commit), err)
	}
	return inventory, nil
}

// parseOptions reads the flags, each as --name value or --name=value.
// --route may repeat; every other flag is given at most once.
func parseOptions(args []string) (options, error) {
	var opts options
	seen := map[string]bool{}
	single := map[string]*string{"--project": &opts.project, "--base": &opts.base, "--base-url": &opts.baseURL,
		"--head-url": &opts.headURL, "--storage-state": &opts.storageState, "--fixtures": &opts.fixtures,
		"--out": &opts.out, "--browser": &opts.browser}
	for len(args) > 0 {
		raw := args[0]
		args = args[1:]
		name, value, hasValue := strings.Cut(raw, "=")
		target, known := single[name]
		if !known && name != "--route" {
			return opts, fmt.Errorf("unexpected argument %q", bounded(raw))
		}
		if !hasValue {
			if len(args) == 0 {
				return opts, fmt.Errorf("%s needs a value", name)
			}
			value, args = args[0], args[1:]
		}
		if value == "" || strings.HasPrefix(value, "--") {
			return opts, fmt.Errorf("%s needs a nonempty value", name)
		}
		if name == "--route" {
			opts.routes = append(opts.routes, value)
			continue
		}
		if seen[name] {
			return opts, fmt.Errorf("%s given twice", name)
		}
		seen[name] = true
		*target = value
	}
	var missing []string
	for _, name := range []string{"--project", "--base", "--head-url"} {
		if *single[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return opts, errors.New(strings.Join(missing, ", ") + " required")
	}
	return opts, nil
}

func bounded(text string) string {
	if len(text) > 64 {
		return text[:64] + "..."
	}
	return text
}
