package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestReleaseSetPublishedImagesCanonicalRoundTrip(t *testing.T) {
	images := []ReleaseSetPublishedImage{
		{Project: "svc/beta", Digest: "sha256:" + strings.Repeat("b", 64)},
		{Project: "svc/alpha", Digest: "sha256:" + strings.Repeat("a", 64)},
	}
	encoded, err := MarshalReleaseSetPublishedImages(images)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseReleaseSetPublishedImages(encoded)
	if err != nil || len(parsed) != 2 || parsed[0].Project != "svc/alpha" || parsed[1].Project != "svc/beta" {
		t.Fatalf("parsed = %+v, err=%v", parsed, err)
	}
	path := filepath.Join(t.TempDir(), "published.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	read, present, err := ReadReleaseSetPublishedImagesFile(map[string]string{ReleaseSetPublishedImagesFileEnv: path})
	if err != nil || !present || len(read) != 2 || read[0] != parsed[0] || read[1] != parsed[1] {
		t.Fatalf("read = %+v, present=%v, err=%v", read, present, err)
	}
}

func TestReleaseSetPublishedImagesRejectsNonCanonicalOrUntrustedFiles(t *testing.T) {
	for name, data := range map[string]string{
		"unsorted":      `{"protocolVersion":1,"published":[{"project":"z","digest":"sha256:` + strings.Repeat("a", 64) + `"},{"project":"a","digest":"sha256:` + strings.Repeat("b", 64) + `"}]}`,
		"duplicate":     `{"protocolVersion":1,"published":[{"project":"a","digest":"sha256:` + strings.Repeat("a", 64) + `"},{"project":"a","digest":"sha256:` + strings.Repeat("b", 64) + `"}]}`,
		"unknown":       `{"protocolVersion":1,"published":[],"extra":true}`,
		"mutable":       `{"protocolVersion":1,"published":[{"project":"a","digest":"latest"}]}`,
		"uppercase":     `{"protocolVersion":1,"published":[{"project":"a","digest":"sha256:` + strings.Repeat("A", 64) + `"}]}`,
		"spaced":        `{"protocolVersion":1,"published":[{"project":" a","digest":"sha256:` + strings.Repeat("a", 64) + `"}]}`,
		"wrong version": `{"protocolVersion":2,"published":[]}`,
		"trailing":      `{"protocolVersion":1,"published":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseReleaseSetPublishedImages([]byte(data)); err == nil {
				t.Fatal("malformed published images were accepted")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "published.json")
	if err := os.WriteFile(path, []byte(`{"protocolVersion":1,"published":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Windows mode bits carry no access information, so only Unix rejects a
	// group- or world-readable file.
	if _, present, err := ReadReleaseSetPublishedImagesFile(map[string]string{ReleaseSetPublishedImagesFileEnv: path}); !present || (err == nil) != (goruntime.GOOS == "windows") {
		t.Fatalf("broad file on %s = present:%v err:%v", goruntime.GOOS, present, err)
	}
	if _, present, err := ReadReleaseSetPublishedImagesFile(map[string]string{ReleaseSetPublishedImagesFileEnv: "relative.json"}); !present || err == nil {
		t.Fatalf("relative file = present:%v err:%v, want rejected", present, err)
	}
	privatePath := filepath.Join(t.TempDir(), "private.json")
	if err := os.WriteFile(privatePath, []byte(`{"protocolVersion":1,"published":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := privatePath + ".link"
	if err := os.Symlink(privatePath, symlinkPath); err == nil {
		if _, present, readErr := ReadReleaseSetPublishedImagesFile(map[string]string{ReleaseSetPublishedImagesFileEnv: symlinkPath}); !present || readErr == nil {
			t.Fatalf("symlink file = present:%v err:%v, want rejected", present, readErr)
		}
	}
	if _, err := ParseReleaseSetPublishedImages(make([]byte, ReleaseSetPublishedImagesMaxBytes+1)); err == nil {
		t.Fatal("oversized published images were accepted")
	}
}
