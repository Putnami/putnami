package dart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/stacks"
)

// testTool is a program the test task runs, from a package pub activates.
type testTool struct {
	executable string
	pkg        string
	constraint string
}

// testTools are the programs of the test task: tojunit converts the test JSON
// report, format_coverage the VM coverage of a pure Dart package. coverage
// takes the newest 1.x the selected SDK resolves.
var testTools = []testTool{
	{executable: "tojunit", pkg: "junitreport", constraint: "2.0.2"},
	{executable: "format_coverage", pkg: "coverage", constraint: "^1.6.0"},
}

// Tools makes tojunit and format_coverage available to the test task, each
// only when the PATH of env holds none: it activates their package with the
// task's dart into a pub cache under the Putnami home, and returns the
// directory of the scripts that run them from that cache with that dart
// (ADR 0001).
func Tools(ctx context.Context, task string, meta stacks.Metadata, env []string, info func(string)) ([]string, error) {
	if task != stacks.TaskTest {
		return nil, nil
	}
	var missing []testTool
	for _, t := range testTools {
		if _, ok := stacks.LookPathIn(env, t.executable); !ok {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	dart, ok := taskDart(meta, env)
	if !ok {
		return nil, errNoDart
	}
	// A package pub activates for one SDK may refuse another: each SDK
	// version has its own pub cache and launchers.
	version, err := dartVersion(ctx, dart, env)
	if err != nil {
		return nil, err
	}
	base, release, err := lockedHome()
	if err != nil {
		return nil, err
	}
	defer release()
	cache := filepath.Join(base, "pub-cache-"+version)
	// The launchers name their dart: each dart of one version has its own.
	sum := sha256.Sum256([]byte(dart))
	bin := filepath.Join(base, "bin-"+version+"-"+hex.EncodeToString(sum[:6]))
	for _, dir := range []string{cache, bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	for _, t := range missing {
		marker := filepath.Join(cache, ".activated-"+t.pkg+"-"+strings.Trim(t.constraint, "^<>= "))
		if _, err := os.Stat(marker); err != nil {
			info("activating " + t.pkg + " " + t.constraint + " into " + cache)
			cmd := exec.CommandContext(ctx, dart, "pub", "global", "activate", t.pkg, t.constraint) //nolint:gosec // the task's dart, with a pinned package
			cmd.Env = append(withoutPubCache(env), "PUB_CACHE="+cache)
			// The job's stdout carries its protocol.
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return nil, fmt.Errorf("dart pub global activate %s %s: %w", t.pkg, t.constraint, err)
			}
			if err := os.WriteFile(marker, nil, 0o644); err != nil { //nolint:gosec // a marker in the extension's own folder
				return nil, err
			}
		}
		if err := writeLauncher(bin, cache, dart, t); err != nil {
			return nil, err
		}
	}
	return []string{bin}, nil
}

// taskDart is the dart of the task's SDK: the one beside the selected
// flutter for a package that resolves with Flutter, whatever dart comes first
// on PATH, else the first on the PATH of env.
func taskDart(meta stacks.Metadata, env []string) (string, bool) {
	if meta.Variant != VariantFlutter && meta.Variant != VariantDartInFlutter {
		return stacks.LookPathIn(env, "dart")
	}
	flutter, ok := stacks.LookPathIn(env, "flutter")
	if !ok {
		return "", false
	}
	return stacks.LookPathIn([]string{"PATH=" + filepath.Dir(flutter)}, "dart")
}

// writeLauncher writes the script that runs a tool from the extension's pub
// cache with dart. A launcher another task may run is replaced whole, and only
// when it changes.
func writeLauncher(bin, cache, dart string, t testTool) error {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	name, script := t.executable, "#!/bin/sh\nPUB_CACHE="+quote(cache)+" exec "+quote(dart)+" pub global run "+t.pkg+":"+t.executable+" \"$@\"\n"
	if strings.HasPrefix(hostPlatform, "windows/") {
		name, script = t.executable+".bat", "@echo off\r\nset \"PUB_CACHE="+cache+"\"\r\n\""+dart+"\" pub global run "+t.pkg+":"+t.executable+" %*\r\n"
	}
	path := filepath.Join(bin, name)
	if current, err := os.ReadFile(path); err == nil && string(current) == script { //nolint:gosec // a launcher in the extension's own folder
		return nil
	}
	staged := path + ".tmp"
	if err := os.WriteFile(staged, []byte(script), 0o755); err != nil { //nolint:gosec // a launcher in the extension's own folder
		return err
	}
	return robustio.Rename(staged, path)
}

// dartVersionPattern reads the version dart --version prints.
var dartVersionPattern = regexp.MustCompile(`Dart SDK version: ([0-9][0-9A-Za-z.\-+]*)`)

// dartVersion reads the version of the dart at program.
func dartVersion(ctx context.Context, program string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, program, "--version") //nolint:gosec // the task's dart
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", program, err)
	}
	m := dartVersionPattern.FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("%s --version printed no Dart SDK version", program)
	}
	return string(m[1]), nil
}

// withoutPubCache drops the machine's pub cache from env.
func withoutPubCache(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if key, _, _ := strings.Cut(entry, "="); !strings.EqualFold(key, "PUB_CACHE") {
			out = append(out, entry)
		}
	}
	return out
}
