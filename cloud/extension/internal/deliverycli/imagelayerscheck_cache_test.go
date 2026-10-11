package deliverycli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
)

// The cloud-image-layers-check task is cached, and a cache hit replays the
// verdict of an earlier run. These tests prove the task's file ports hold
// every file the check reads.

const imageLayersCheckTaskName = "cloud-image-layers-check"

// imageLayersCheckKeyPorts returns the file patterns of the task's project
// ports and of its workspace ports. The task declares no other kind of port.
func imageLayersCheckKeyPorts(t *testing.T) (project, workspace []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Inputs map[string]struct {
				From  string   `json:"from"`
				Files []string `json:"files"`
			} `json:"inputs"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for name, port := range manifest.Tasks[imageLayersCheckTaskName].Inputs {
		switch port.From {
		case "project":
			project = append(project, port.Files...)
		case "workspace":
			workspace = append(workspace, port.Files...)
		default:
			t.Fatalf("port %s reads from %q: the check reads files only", name, port.From)
		}
	}
	if len(project) == 0 || len(workspace) == 0 {
		t.Fatalf("%s keys project files %v and workspace files %v", imageLayersCheckTaskName, project, workspace)
	}
	return project, workspace
}

// imageLayersKeyedFiles expands file patterns the way the cache hasher does
// for a pattern without "**": filepath.Glob relative to dir, files only. It
// returns paths relative to dir.
func imageLayersKeyedFiles(t *testing.T, dir string, patterns []string) []string {
	t.Helper()
	var files []string
	for _, pattern := range patterns {
		if strings.Contains(pattern, "**") || strings.HasPrefix(pattern, "!") {
			t.Fatalf("pattern %q: the check keys exact files and one-level globs", pattern)
		}
		matches, err := filepath.Glob(filepath.Join(dir, filepath.FromSlash(pattern)))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			if info, err := os.Stat(match); err != nil || info.IsDir() {
				continue
			}
			rel, err := filepath.Rel(dir, match)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, filepath.ToSlash(rel))
		}
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// imageLayersCheckKeyFixture extends the check fixture with a cloud-cli layer,
// so every producer family is present and every keyed file feeds a layer. It
// also writes files the ports leave out.
func imageLayersCheckKeyFixture(t *testing.T) (workspaceRoot, project string) {
	t.Helper()
	workspaceRoot, project = imageLayersCheckFixture(t)
	spec := imageLayerSpec{Name: "putnami-ci-next", Producer: "cloud-cli", Path: "/usr/local/libexec/putnami-ci-next"}
	manifest := strings.Replace(imageLayersCheckManifest, "\n  ]\n}",
		",\n    {\"name\": \""+spec.Name+"\", \"producer\": \""+spec.Producer+"\", \"path\": \""+spec.Path+"\"}\n  ]\n}", 1)
	if manifest == imageLayersCheckManifest {
		t.Fatal("the fixture manifest no longer ends its layer list as expected")
	}
	writeImageLayersFixtureFile(t, filepath.Join(project, imageLayersManifestName), manifest)

	writeImageLayersFixtureFile(t, filepath.Join(workspaceRoot, ".putnami/out/cloud/extension/build/bin/linux-x64/putnami-cloud"), "workspace CLI prerequisite bytes")
	info := &debug.BuildInfo{
		GoVersion: "go1.26.1", Path: imageLayersCloudCLIModule + "/cmd/putnami-cloud",
		Main:     debug.Module{Path: imageLayersCloudCLIModule, Version: "(devel)"},
		Settings: []debug.BuildSetting{{Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "amd64"}, {Key: "CGO_ENABLED", Value: "0"}, {Key: "-trimpath", Value: "true"}},
	}
	previous := imageLayersReadBuildInfo
	t.Cleanup(func() { imageLayersReadBuildInfo = previous })
	// The build info is read from the binary, so a missing binary has none.
	imageLayersReadBuildInfo = func(path string) (*debug.BuildInfo, error) {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
		return info, nil
	}
	outDir := imageLayersOutputDir(project)
	record, err := imageLayersProduceCloudCLI(context.Background(), spec, workspaceRoot, outDir, nil, clicore.IO{})
	if err != nil {
		t.Fatalf("produce the %s layer: %v", spec.Name, err)
	}
	if err := imageLayersWriteRecord(outDir, record); err != nil {
		t.Fatal(err)
	}

	// Files the ports leave out. Each holds other bytes for a declared layer.
	writeImageLayersFixtureFile(t, filepath.Join(outDir, "staticcheck"), "another staticcheck\n")
	writeImageLayersFixtureFile(t, filepath.Join(outDir, "old", "go-toolchain.tar"), "another toolchain\n")
	writeImageLayersFixtureFile(t, filepath.Join(outDir, "old", "bun.json"), `{"name":"bun","version":"another"}`)
	writeImageLayersFixtureFile(t, filepath.Join(project, "putnami.lock.json"), `{"cli":{"version":"another"}}`)
	return workspaceRoot, project
}

func writeImageLayersFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// imageLayersCheckOutcomeOf runs the check. The result holds the error and
// the printed verdict, without the two directory names.
func imageLayersCheckOutcomeOf(t *testing.T, workspaceRoot, project string) string {
	t.Helper()
	out, err := imageLayersCheck(t, workspaceRoot, project)
	outcome := fmt.Sprintf("error: %v\nstdout: %s", err, strings.Join(out, "\n"))
	return strings.NewReplacer(project, "<project>", workspaceRoot, "<workspace>").Replace(outcome)
}

// TestImageLayersCheckReadsOnlyKeyedFiles proves the key covers the read set.
// It copies only the files the task's ports select into a second workspace
// and a second project, and requires the same outcome in both. Removing any
// selected file must change the outcome, so the fixture exercises every
// selected file.
func TestImageLayersCheckReadsOnlyKeyedFiles(t *testing.T) {
	projectPatterns, workspacePatterns := imageLayersCheckKeyPorts(t)
	workspaceRoot, project := imageLayersCheckKeyFixture(t)
	baseline := imageLayersCheckOutcomeOf(t, workspaceRoot, project)
	if !strings.Contains(baseline, "error: <nil>") || strings.Count(baseline, `"version"`) != 8 {
		t.Fatalf("fixture does not verify its eight layers:\n%s", baseline)
	}

	prunedWorkspace, prunedProject := t.TempDir(), t.TempDir()
	type keyedFile struct{ dir, rel string }
	var keyed []keyedFile
	for _, source := range []struct {
		from, to string
		patterns []string
	}{
		{from: project, to: prunedProject, patterns: projectPatterns},
		{from: workspaceRoot, to: prunedWorkspace, patterns: workspacePatterns},
	} {
		for _, rel := range imageLayersKeyedFiles(t, source.from, source.patterns) {
			data, err := os.ReadFile(filepath.Join(source.from, filepath.FromSlash(rel))) //nolint:gosec // G304: a path this fixture just wrote
			if err != nil {
				t.Fatal(err)
			}
			writeImageLayersFixtureFile(t, filepath.Join(source.to, filepath.FromSlash(rel)), string(data))
			keyed = append(keyed, keyedFile{dir: source.to, rel: rel})
		}
	}
	// One manifest, eight records and eight produced files, four pin files and
	// the Cloud CLI binary.
	if len(keyed) != 22 {
		t.Fatalf("the ports select %d files, want 22: %v", len(keyed), keyed)
	}
	if got := imageLayersCheckOutcomeOf(t, prunedWorkspace, prunedProject); got != baseline {
		t.Fatalf("the check reads a file its ports do not select (keyed: %v)\nfull tree:\n%s\nkeyed files only:\n%s", keyed, baseline, got)
	}

	for _, file := range keyed {
		path := filepath.Join(file.dir, filepath.FromSlash(file.rel))
		data, err := os.ReadFile(path) //nolint:gosec // G304: a path this test just wrote
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if got := imageLayersCheckOutcomeOf(t, prunedWorkspace, prunedProject); got == baseline {
			t.Errorf("removing %s does not change the outcome, so the fixture does not exercise it", file.rel)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestImageLayersCheckRefusesARecordThatNamesAnotherFile covers the one read a
// record could move outside the key. Each case points a record at a file that
// holds bytes the record describes, so only the produced-file rule refuses it.
func TestImageLayersCheckRefusesARecordThatNamesAnotherFile(t *testing.T) {
	for name, tc := range map[string]struct {
		layer, artifact, produced string
		tarball                   bool
	}{
		"a file above the output directory": {layer: "staticcheck", artifact: "../staticcheck", produced: "bin/staticcheck"},
		"a file in another directory":       {layer: "staticcheck", artifact: "old/staticcheck", produced: "bin/staticcheck"},
		"the file of another layer":         {layer: "staticcheck", artifact: "bin/bun", produced: "bin/staticcheck"},
		"a tarball in another directory":    {layer: "go-toolchain", artifact: "old/go-toolchain.tar", produced: "go-toolchain.tar", tarball: true},
		"a tarball recorded as a file":      {layer: "go-toolchain", artifact: "bin/go-toolchain", produced: "go-toolchain.tar"},
	} {
		t.Run(name, func(t *testing.T) {
			workspaceRoot, project := imageLayersCheckFixture(t)
			outDir := imageLayersOutputDir(project)
			record, err := imageLayersLoadRecord(outDir, tc.layer)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(outDir, filepath.FromSlash(tc.artifact))
			if _, err := os.Stat(target); os.IsNotExist(err) {
				writeImageLayersFixtureFile(t, target, "bytes outside the key\n")
			}
			if record.SHA256, record.Size, err = imageLayersHashFile(target); err != nil {
				t.Fatal(err)
			}
			record.Tar, record.File = "", tc.artifact
			if tc.tarball {
				record.Tar, record.File = tc.artifact, ""
			}
			if err := imageLayersWriteRecord(outDir, record); err != nil {
				t.Fatal(err)
			}

			_, err = imageLayersCheck(t, workspaceRoot, project)
			if err == nil {
				t.Fatal("want a refusal when a record names a file the producer does not write for its layer")
			}
			for _, want := range []string{tc.layer, tc.artifact, "is produced as " + tc.produced} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not name %q", err, want)
				}
			}
		})
	}
}
