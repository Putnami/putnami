package distributioncli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// siteContentGeneratedDir is the project-relative directory the build step
// cloud-site-content-generate writes: one subdirectory per declared generated
// section. Validation, the workspace probe, packaging and publication read a
// declared generated section from there and from nowhere else.
const siteContentGeneratedDir = ".gen/site-content"

// siteContentOutputPlaceholder is the text a generator argument carries where
// the command receives the absolute path of its section directory.
const siteContentOutputPlaceholder = "{output}"

// SiteContentDeclarationFile is the project-root file that declares a site
// project's generated sections. Its presence activates the package command,
// as image-layers.json does for an image project, so a site project whose
// only sections are generated still packages and publishes them.
const SiteContentDeclarationFile = "site-content.json"

// siteContentGeneratorTailLines caps the output lines a failing generator's
// error restates. Every line also streams as the command runs.
const siteContentGeneratorTailLines = 20

// siteContentGenerator is one declared generated section: its name and the
// command that writes its files.
type siteContentGenerator struct {
	section string
	command []string
}

// siteContentDeclaration is the shape of site-content.json. Unknown members
// are refused at every level, so a misspelled key fails instead of declaring
// nothing.
type siteContentDeclaration struct {
	Generated map[string]siteContentGeneratorDeclaration `json:"generated"`
}

type siteContentGeneratorDeclaration struct {
	Command []string `json:"command"`
}

// readSiteContentGenerators returns, sorted by section, the generated sections
// that site-content.json in projectDir declares. An absent file, or one that
// declares no generated section, returns none. The file must hold one JSON
// object whose only member is generated, and each section one object whose
// only member is command. A section name must be a section slug, and each
// command must be a non-empty argument list whose program is named and that
// passes {output} to the command at least once.
func readSiteContentGenerators(projectDir string) ([]siteContentGenerator, error) {
	file := filepath.Join(projectDir, SiteContentDeclarationFile)
	data, err := os.ReadFile(file) //nolint:gosec // G304: the project's own declaration file
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, clicore.NewError("read "+file+": "+err.Error(), clicore.ExitUsage)
	}
	shape := "it must be one object whose only member is generated, a map of section name to {\"command\": [...]}"
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, clicore.NewError("decode "+file+": "+shape, clicore.ExitUsage)
	}
	var declaration siteContentDeclaration
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&declaration); err != nil {
		return nil, clicore.NewError("decode "+file+": "+shape+": "+err.Error(), clicore.ExitUsage)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, clicore.NewError("decode "+file+": "+shape+": data follows the object", clicore.ExitUsage)
	}
	generators := make([]siteContentGenerator, 0, len(declaration.Generated))
	for section, generator := range declaration.Generated {
		where := fmt.Sprintf("generated[%q] of %s", section, file)
		if err := validGeneratedSectionName(section); err != nil {
			return nil, clicore.NewError(where+": "+err.Error(), clicore.ExitUsage)
		}
		if len(generator.Command) == 0 || strings.TrimSpace(generator.Command[0]) == "" {
			return nil, clicore.NewError(where+": command must be a non-empty list whose first item names the program", clicore.ExitUsage)
		}
		if !slices.ContainsFunc(generator.Command, func(arg string) bool { return strings.Contains(arg, siteContentOutputPlaceholder) }) {
			return nil, clicore.NewError(where+": command must pass "+siteContentOutputPlaceholder+", the directory it writes the section to", clicore.ExitUsage)
		}
		generators = append(generators, siteContentGenerator{section: section, command: generator.Command})
	}
	slices.SortFunc(generators, func(a, b siteContentGenerator) int { return strings.Compare(a.section, b.section) })
	return generators, nil
}

// validGeneratedSectionName refuses a generated section name that is not one
// path segment, starts with a dot, or cannot be a section mount.
func validGeneratedSectionName(section string) error {
	if section == "" || strings.ContainsAny(section, `/\`) || strings.HasPrefix(section, ".") || !filepath.IsLocal(section) {
		return fmt.Errorf("section name %q must be one directory name that does not start with a dot", section)
	}
	if _, err := siteContentMount("", section); err != nil {
		return fmt.Errorf("section name %q is not a lowercase URL-safe slug (letters, digits, '.', '_', '-')", section)
	}
	return nil
}

// siteContentGeneratedSectionDir returns the directory a declared generated
// section of the site directory siteDir is read from.
func siteContentGeneratedSectionDir(siteDir, section string) string {
	return filepath.Join(siteDir, filepath.FromSlash(siteContentGeneratedDir), section)
}

// GenerateSiteContent backs the build step cloud-site-content-generate. In
// the project of its working directory it runs the command of each declared
// generated section, sorted by section, with every {output} in an argument
// replaced by the absolute path of .gen/site-content/<section>. It first
// removes .gen/site-content, so the directory holds exactly the declared
// sections. The command runs in the project directory with the job's
// environment, so its program must be on PATH.
//
// It fails when a command fails or writes no regular file. It succeeds with
// nothing to do when the project declares no generated section. It reads only
// site-content.json, which the task's key holds, and sends no network
// request. It does not check whether a generated name collides with a
// committed section, because that reads the Git candidate files the key does
// not hold. Validation, which keys on them, refuses the collision.
func GenerateSiteContent(params map[string]any, args []string, _ string, ioctx clicore.IO) error {
	if positional := clicore.Positionals(args); len(positional) > 0 {
		return clicore.NewError("generate-site-content takes no argument: it generates the sections of the project in its working directory", clicore.ExitUsage)
	}
	projectDir, err := os.Getwd()
	if err != nil {
		return clicore.NewError("generate site-content: read the working directory: "+err.Error(), clicore.ExitUsage)
	}
	generators, err := readSiteContentGenerators(projectDir)
	if err != nil {
		return err
	}
	root := filepath.Join(projectDir, filepath.FromSlash(siteContentGeneratedDir))
	if err := os.RemoveAll(root); err != nil {
		return clicore.NewError("generate site-content: clear "+root+": "+err.Error(), clicore.ExitFailure)
	}
	if len(generators) == 0 {
		writePublishResult(map[string]any{"status": "skipped", "reason": "project declares no generated site-content section"},
			params, ioctx, "No generated site-content section declared — nothing to generate.")
		return nil
	}
	ctx := ioctx.Context
	if ctx == nil {
		ctx = context.Background()
	}
	sections := make([]map[string]any, 0, len(generators))
	total := 0
	for _, generator := range generators {
		files, err := runSiteContentGenerator(ctx, projectDir, generator, ioctx)
		if err != nil {
			return err
		}
		total += files
		sections = append(sections, map[string]any{"section": generator.section, "files": files})
	}
	writePublishResult(map[string]any{"status": "generated", "sections": sections, "files": total}, params, ioctx,
		fmt.Sprintf("Generated %d site-content section(s), %d file(s), under %s.", len(sections), total, siteContentGeneratedDir))
	return nil
}

// runSiteContentGenerator runs one generator into a fresh section directory
// and returns the number of regular files it wrote.
func runSiteContentGenerator(ctx context.Context, projectDir string, generator siteContentGenerator, ioctx clicore.IO) (int, error) {
	output := siteContentGeneratedSectionDir(projectDir, generator.section)
	if err := os.MkdirAll(output, 0o750); err != nil {
		return 0, clicore.NewError("generate site-content: create "+output+": "+err.Error(), clicore.ExitFailure)
	}
	argv := make([]string, len(generator.command))
	for i, arg := range generator.command {
		argv[i] = strings.ReplaceAll(arg, siteContentOutputPlaceholder, output)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: the project declares this command; running it is the feature
	cmd.Dir = projectDir
	lines := &siteContentGeneratorOutput{}
	stdout := &siteContentGeneratorStream{output: lines, emit: ioctx.Stdout}
	stderr := &siteContentGeneratorStream{output: lines, emit: ioctx.Stderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	stdout.flush()
	stderr.flush()
	subject := fmt.Sprintf("generate site-content section %q: %s", generator.section, argv[0])
	if runErr != nil {
		var message strings.Builder
		if exitErr, exited := errors.AsType[*exec.ExitError](runErr); exited {
			fmt.Fprintf(&message, "%s exited with status %d", subject, exitErr.ExitCode())
		} else if errors.Is(runErr, exec.ErrNotFound) {
			fmt.Fprintf(&message, "%s is not on the job's PATH", subject)
		} else {
			fmt.Fprintf(&message, "%s failed: %s", subject, runErr)
		}
		for _, line := range lines.tail {
			message.WriteString("\n  " + line)
		}
		return 0, clicore.NewError(message.String(), clicore.ExitFailure)
	}
	files, err := countSiteContentGeneratedFiles(output)
	if err != nil {
		return 0, clicore.NewError(subject+": read "+output+": "+err.Error(), clicore.ExitFailure)
	}
	if files == 0 {
		return 0, clicore.NewError(fmt.Sprintf("%s wrote no file to %s; a generated section must ship content", subject, output), clicore.ExitFailure)
	}
	return files, nil
}

// countSiteContentGeneratedFiles counts the regular files under dir.
func countSiteContentGeneratedFiles(dir string) (int, error) {
	files := 0
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			files++
		}
		return nil
	})
	return files, err
}

// siteContentGeneratorOutput keeps the last siteContentGeneratorTailLines
// non-blank lines a generator wrote to either stream, in the order they
// completed, for the error of a failing run. os/exec copies each stream on
// its own goroutine, so the lock orders the two streams' lines.
type siteContentGeneratorOutput struct {
	mu   sync.Mutex
	tail []string
}

// siteContentGeneratorStream forwards one stream of a generator line by line
// to its own emitter: stdout to the job's stdout, stderr to its stderr. os/exec
// calls Write on one stream from one goroutine.
type siteContentGeneratorStream struct {
	output  *siteContentGeneratorOutput
	emit    func(string)
	pending []byte
}

func (w *siteContentGeneratorStream) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		index := bytes.IndexByte(w.pending, '\n')
		if index < 0 {
			return len(p), nil
		}
		w.line(string(w.pending[:index]))
		w.pending = w.pending[index+1:]
	}
}

func (w *siteContentGeneratorStream) flush() {
	if len(w.pending) > 0 {
		w.line(string(w.pending))
		w.pending = nil
	}
}

func (w *siteContentGeneratorStream) line(line string) {
	line = strings.TrimRight(line, "\r")
	w.output.mu.Lock()
	defer w.output.mu.Unlock()
	if w.emit != nil {
		w.emit(line)
	}
	if strings.TrimSpace(line) == "" {
		return
	}
	if len(w.output.tail) == siteContentGeneratorTailLines {
		w.output.tail = w.output.tail[1:]
	}
	w.output.tail = append(w.output.tail, line)
}
