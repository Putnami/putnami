package cachekey

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey/framing"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const extensionRuntimeDigestVersion = "putnami-extension-runtime-v1"

func isLocalDevExtensionSource(ws *workspace.Workspace, job *jobmodel.ScheduledJob) bool {
	if ws == nil || ws.Root == "" || job == nil || job.Extension == nil || !job.Extension.LocalSource {
		return false
	}
	if job.Extension.RelPath == "" {
		return true
	}
	root := JobExtensionRoot(ws, job)
	rel, err := filepath.Rel(ws.Root, root)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func ExtensionRuntimeDigest(ext *extension.ExtensionDescription) (string, error) {
	if ext == nil || ext.Runtime == nil || ext.Runtime.Prepare == nil {
		return "", errors.New("runtime preparation is not declared")
	}
	return ExtensionRuntimeDigestWithToolchains(ext, toolchain.RuntimeToolchainIdentityForRefs(ext, ext.Runtime.Prepare.Toolchains))
}

// ExtensionRuntimeDigestWithToolchains is ExtensionRuntimeDigest with the
// identity of the prepare toolchains given, rather than read from the
// resolutions recorded on ext. A hosted run of a source workspace computes it
// from the workspace lock alone (toolchain.LockedRuntimeToolchainIdentityForRefs).
func ExtensionRuntimeDigestWithToolchains(ext *extension.ExtensionDescription, toolchains string) (string, error) {
	if ext == nil || ext.Runtime == nil || ext.Runtime.Prepare == nil {
		return "", errors.New("runtime preparation is not declared")
	}
	// The root is walked, so it must be the directory itself: a directory
	// link at ext.Path, a junction on Windows included, is followed first.
	root, err := dirlink.Resolve(ext.Path)
	if err != nil {
		return "", fmt.Errorf("resolve extension root: %w", err)
	}
	inputs, err := CollectRuntimeInputs(root, ext.Runtime.Prepare.Inputs)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	framing.HashField(h, "schema", []byte(extensionRuntimeDigestVersion))
	framing.HashField(h, "extension", []byte(ext.Name))
	framing.HashField(h, "version", []byte(ext.Version))
	framing.HashField(h, "platform", []byte(runtime.GOOS+"/"+runtime.GOARCH))
	framing.HashField(h, "runtime-abi", []byte(fmt.Sprintf("%d", runtimeproto.RuntimeABIVersion)))
	framing.HashField(h, "toolchains", []byte(toolchains))
	declaration, err := json.Marshal(ext.Runtime)
	if err != nil {
		return "", fmt.Errorf("marshal runtime declaration: %w", err)
	}
	framing.HashField(h, "declaration", declaration)
	for _, input := range ext.Runtime.Prepare.Inputs {
		framing.HashField(h, "input-pattern", []byte(input))
	}
	for _, rel := range inputs {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return "", fmt.Errorf("stat runtime input %s: %w", rel, err)
		}
		framing.HashField(h, "input/"+rel+"/mode", []byte(fmt.Sprintf("%o", info.Mode()&(os.ModeType|0o111))))
		file, err := os.Open(full)
		if err != nil {
			return "", fmt.Errorf("open runtime input %s: %w", rel, err)
		}
		if err := framing.HashStream(h, "input/"+rel, info.Size(), file); err != nil {
			_ = file.Close()
			return "", fmt.Errorf("hash runtime input %s: %w", rel, err)
		}
		if err := file.Close(); err != nil {
			return "", fmt.Errorf("close runtime input %s: %w", rel, err)
		}
	}
	replacements, err := RuntimeReplacementClosure(root)
	if err != nil {
		return "", err
	}
	for _, replacement := range replacements {
		if err := hashRuntimeReplacementTree(h, "module-replacement/"+replacement.label, replacement.Root); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CollectRuntimeInputs returns the slash-form paths, relative to root, of the
// regular files under root that patterns select, sorted. A directory link at
// root, a junction on Windows included, is followed first, so the walk reads
// the directory the digest hashes and staging copies, never the link itself.
func CollectRuntimeInputs(root string, patterns []string) ([]string, error) {
	resolved, err := dirlink.Resolve(root)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime input root: %w", err)
	}
	root = filepath.Clean(resolved)
	matches := make(map[string]bool)
	err = filepath.WalkDir(root, func(full string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		matched := false
		for _, pattern := range patterns {
			if matchRuntimeInput(pattern, rel) {
				matched = true
				break
			}
		}
		if !matched {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime input %q is a symlink", rel)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("runtime input %q is not a regular file", rel)
		}
		matches[rel] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for rel := range matches {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

func matchRuntimeInput(pattern, rel string) bool {
	patternParts := strings.Split(path.Clean(pattern), "/")
	relParts := strings.Split(path.Clean(rel), "/")
	var match func(int, int) bool
	match = func(pi, ri int) bool {
		if pi == len(patternParts) {
			return ri == len(relParts)
		}
		if patternParts[pi] == "**" {
			if match(pi+1, ri) {
				return true
			}
			return ri < len(relParts) && match(pi, ri+1)
		}
		if ri == len(relParts) {
			return false
		}
		ok, err := path.Match(patternParts[pi], relParts[ri])
		return err == nil && ok && match(pi+1, ri+1)
	}
	return match(0, 0)
}

// RuntimeReplacement is one module a local replace directive reaches. Root is
// the physical directory its tree is read from. Stage is where the declaring
// module's go.mod expects it: a relative directive joins the declaring
// module's stage path lexically, as the go command does, so a replacement
// behind a directory link is staged at the path the go.mod names rather than
// where the link points. An absolute directive keeps the physical root, since
// the build reads that path outside the staged view.
type RuntimeReplacement struct {
	label string
	Root  string
	Stage string
}

func RuntimeReplacementClosure(extensionRoot string) ([]RuntimeReplacement, error) {
	type pendingModule struct {
		label string
		root  string
		stage string
	}
	queue := []pendingModule{{label: "extension", root: extensionRoot, stage: filepath.Clean(extensionRoot)}}
	seen := map[string]bool{filepath.Clean(extensionRoot): true}
	var replacements []RuntimeReplacement
	for len(queue) > 0 {
		module := queue[0]
		queue = queue[1:]
		goMod := filepath.Join(module.root, "go.mod")
		if _, err := os.Stat(goMod); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		paths, err := localDirectivePaths(goMod, "replace")
		if err != nil {
			return nil, err
		}
		for _, declared := range paths {
			replacementRoot := declared
			if !filepath.IsAbs(replacementRoot) {
				replacementRoot = filepath.Join(module.root, replacementRoot)
			}
			replacementRoot, err = dirlink.Resolve(replacementRoot)
			if err != nil {
				return nil, fmt.Errorf("resolve local replacement %q from %s: %w", declared, goMod, err)
			}
			replacementRoot = filepath.Clean(replacementRoot)
			if seen[replacementRoot] {
				continue
			}
			seen[replacementRoot] = true
			label := module.label + "/" + filepath.ToSlash(declared)
			stage := replacementRoot
			if !filepath.IsAbs(declared) {
				stage = filepath.Join(module.stage, declared)
			}
			replacements = append(replacements, RuntimeReplacement{label: label, Root: replacementRoot, Stage: stage})
			queue = append(queue, pendingModule{label: label, root: replacementRoot, stage: stage})
		}
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].label < replacements[j].label })
	return replacements, nil
}

// RuntimeTreeEntry is one member of a replaced module's tree. Staging and
// hashing consume the same enumeration, so a prepared runtime's digest always
// describes the tree its build actually reads.
type RuntimeTreeEntry struct {
	Rel string
	// Symlink marks an entry staged as a link rather than copied bytes.
	Symlink bool
	// Target is the raw link target of a symlink entry, empty otherwise.
	Target string
}

func CollectRuntimeTree(root string) ([]RuntimeTreeEntry, error) {
	root = filepath.Clean(root)
	var out []RuntimeTreeEntry
	err := filepath.WalkDir(root, func(full string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if runtimeReplacementIgnoredDir(rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			if !runtimeReplacementLinkContained(rel, target) {
				return fmt.Errorf(
					"module replacement input %q is a symlink to %q outside the replacement root",
					rel, target)
			}
			out = append(out, RuntimeTreeEntry{Rel: rel, Symlink: true, Target: target})
			return nil
		}
		if runtime.GOOS == "windows" && info.Mode()&os.ModeIrregular != 0 {
			return runtimeTreeReparsePointError(full, rel, info)
		}
		if info.Mode().IsRegular() {
			out = append(out, RuntimeTreeEntry{Rel: rel})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, err
}

// runtimeTreeReparsePointError refuses a replaced module's entry that Go
// reports as irregular on Windows. A directory junction stores an absolute
// target, which a staged copy at another path cannot reproduce; any other
// reparse point has no content the walk can hash or stage.
func runtimeTreeReparsePointError(full, rel string, info os.FileInfo) error {
	if dirlink.IsLink(full, info) {
		target, _ := os.Readlink(full)
		return fmt.Errorf(
			"module replacement input %q is a directory junction to %q: a staged copy cannot reproduce its absolute target, "+
				"so replace it with the directory itself or with a relative symbolic link inside the module",
			rel, target)
	}
	return fmt.Errorf(
		"module replacement input %q is a reparse point that cannot be staged (mode %v): replace it with a regular file",
		rel, info.Mode())
}

// runtimeReplacementLinkContained reports whether a symlink of the replaced
// module stays inside that module. Containment is decided lexically, without
// resolving intermediate links: the staged view is a copy at another path, so
// only a relative target that stays under the replacement root still points at
// staged bytes once it is recreated there. An absolute target, or one that
// climbs out, would let the content-addressed build read bytes from outside the
// tree this digest covers, which is exactly what the previous blanket refusal
// protected against.
func runtimeReplacementLinkContained(rel, target string) bool {
	slashed := filepath.ToSlash(target)
	// filepath.IsAbs alone misses a rooted "/x" on Windows, path.IsAbs alone
	// misses a volume-qualified "C:\x"; either match means the target is not relative.
	if target == "" || filepath.IsAbs(target) || path.IsAbs(slashed) {
		return false
	}
	// path.Join cleans its result, so a target that climbs above the module
	// root keeps a leading "..".
	resolved := path.Join(path.Dir(rel), slashed)
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}

// runtimeReplacementStagedProjectExclusions are the project-walk exclusions a
// replaced module keeps: vendor/ is build input whenever a module vendors its
// dependencies, and dist/ is ordinary source here — the planner skips those
// names while LOOKING for projects, not while building one.
var runtimeReplacementStagedProjectExclusions = map[string]bool{"vendor": true, "dist": true}

// A local replacement module is source, regardless of directory names such as
// coverage, dist, generated, or tools. Only orchestrator/VCS state, the
// scheduler-owned .gen output tree, and the install output the planner already
// refuses to walk are excluded unconditionally. Build and generate refresh files
// throughout .gen, and any install rewrites its own output tree; hashing or
// staging either makes the prepared-runtime digest follow the previous task
// invocation, or the last install, instead of the replacement module's source.
// That output is also where an install plants the ordinary bin links a staged
// copy cannot resolve. The install-directory names come from the planner's
// shared exclusion table rather than being spelled here, because core does not
// name package managers. The match is by base name and applies to any entry:
// an install may plant those names as symlinks rather than directories.
func runtimeReplacementIgnoredDir(rel string) bool {
	base := path.Base(rel)
	if base == ".git" || base == ".putnami" || base == ".gen" {
		return true
	}
	return ProjectWalkExcludedDirs[base] && !runtimeReplacementStagedProjectExclusions[base]
}

// hashRuntimeReplacementTree fingerprints exactly the entries stageRuntimeSourceView
// stages: both read the tree through CollectRuntimeTree, so the digest can never
// accept a tree the staging walk refuses, nor cover bytes the build never sees.
func hashRuntimeReplacementTree(h hash.Hash, label, root string) error {
	entries, err := CollectRuntimeTree(root)
	if err != nil {
		return fmt.Errorf("walk runtime replacement %s: %w", label, err)
	}
	for _, entry := range entries {
		rel := entry.Rel
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		framing.HashField(h, label+"/"+rel+"/mode", []byte(fmt.Sprintf("%o", info.Mode()&(os.ModeType|0o111))))
		if entry.Symlink {
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			framing.HashField(h, label+"/"+rel, []byte(target))
			continue
		}
		if !info.Mode().IsRegular() {
			// The entry changed type under the walk. Refuse rather than fold a
			// mode-only field into a digest whose staging would refuse the tree.
			return fmt.Errorf("module replacement input %q is not a regular file", rel)
		}
		file, err := os.Open(full)
		if err != nil {
			return err
		}
		if err := framing.HashStream(h, label+"/"+rel, info.Size(), file); err != nil {
			_ = file.Close()
			return fmt.Errorf("hash runtime replacement %s/%s: %w", label, rel, err)
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

// localDirectivePaths extracts local filesystem operands from simple Go
// replace directives without invoking the toolchain. Comments and both
// block/single-line forms are supported.
func localDirectivePaths(filename, directive string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var paths []string
	inBlock := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "//", 2)[0])
		if line == "" {
			continue
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			if local := directiveLocalOperand(line, directive); local != "" {
				paths = append(paths, local)
			}
			continue
		}
		prefix := directive + " "
		if line == directive+" (" {
			inBlock = true
			continue
		}
		if strings.HasPrefix(line, prefix) {
			if local := directiveLocalOperand(strings.TrimSpace(strings.TrimPrefix(line, prefix)), directive); local != "" {
				paths = append(paths, local)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return dedupeStrings(paths), nil
}

func directiveLocalOperand(line, directive string) string {
	fields := strings.Fields(line)
	if directive == "use" {
		if len(fields) > 0 && isLocalPath(fields[0]) {
			return fields[0]
		}
		return ""
	}
	for i, field := range fields {
		if field == "=>" && i+1 < len(fields) && isLocalPath(fields[i+1]) {
			return fields[i+1]
		}
	}
	return ""
}

func isLocalPath(value string) bool {
	return filepath.IsAbs(value) || value == "." || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../")
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
