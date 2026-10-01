package oci

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// epoch is the fixed timestamp stamped on every tar entry and config field.
// Any real time in the image would break digest reproducibility.
var epoch = time.Unix(0, 0).UTC()

// writeLayerTar writes the layer's files as a deterministic uncompressed tar:
// synthesized parent directories first, then files, all sorted by path, with
// fixed epoch mtimes, root ownership, and the explicit modes from the spec.
func writeLayerTar(w io.Writer, layer Layer) error {
	return writePlannedLayerTar(w, layer, nil)
}

// writePlannedLayerTar verifies planned source fingerprints while the same
// bytes flow into the tar. This closes the plan/build mutation window without
// adding a separate verification read.
func writePlannedLayerTar(w io.Writer, layer Layer, plan *LayerPlan) error {
	files := append([]File(nil), layer.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var expected map[string]SourceFingerprint
	if plan != nil {
		expected = make(map[string]SourceFingerprint, len(plan.Files))
		for _, file := range plan.Files {
			if file.Fingerprint == nil {
				return fmt.Errorf("file %s has no bound source fingerprint", file.Path)
			}
			expected[file.Path] = *file.Fingerprint
		}
	}

	dirs := map[string]bool{}
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "/") {
			return fmt.Errorf("image path %q is not absolute", f.Path)
		}
		for d := path.Dir(f.Path); d != "/" && d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	dirPaths := make([]string, 0, len(dirs))
	for d := range dirs {
		dirPaths = append(dirPaths, d)
	}
	sort.Strings(dirPaths)

	tw := tar.NewWriter(w)
	for _, d := range dirPaths {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     strings.TrimPrefix(d, "/") + "/",
			Mode:     0o755,
			ModTime:  epoch,
		}); err != nil {
			return fmt.Errorf("writing dir header %s: %w", d, err)
		}
	}
	for _, f := range files {
		src, err := os.Open(f.Source)
		if err != nil {
			return err
		}
		info, err := src.Stat()
		if err != nil {
			src.Close()
			return err
		}
		fingerprint, verify := expected[f.Path]
		if verify && info.Size() != fingerprint.Size {
			src.Close()
			return fmt.Errorf("source %s changed since image plan", f.Source)
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     strings.TrimPrefix(f.Path, "/"),
			Size:     info.Size(),
			Mode:     f.Mode,
			ModTime:  epoch,
		}); err != nil {
			src.Close()
			return fmt.Errorf("writing file header %s: %w", f.Path, err)
		}
		destination := io.Writer(tw)
		var sourceHasher hash.Hash
		if verify {
			hasher := sha256.New()
			sourceHasher = hasher
			destination = io.MultiWriter(tw, hasher)
		}
		written, err := io.Copy(destination, src)
		if err != nil {
			src.Close()
			return fmt.Errorf("writing %s: %w", f.Path, err)
		}
		src.Close()
		if verify {
			digest := hex.EncodeToString(sourceHasher.Sum(nil))
			if written != fingerprint.Size || digest != fingerprint.Digest {
				return fmt.Errorf("source %s changed since image plan", f.Source)
			}
		}
	}
	return tw.Close()
}
