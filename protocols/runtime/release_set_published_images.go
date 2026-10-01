package runtime

// This file owns the private framework-to-reserved-provider transport for
// verified Docker publications accompanying a same-session release-set CAS.
// It deliberately does not extend Distribution's immutable release-set v1
// document: libraries remain the members of that snapshot, while these facts
// bind the workload images published by the same scheduler run.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// ReleaseSetMembersFileEnv names the private selected-member provenance file.
	ReleaseSetMembersFileEnv = "PUTNAMI_RELEASE_SET_MEMBERS_FILE"
	// ReleaseSetPublishedImagesFileEnv names the private provider evidence file.
	ReleaseSetPublishedImagesFileEnv = "PUTNAMI_RELEASE_SET_PUBLICATIONS_FILE"
	// ReleaseSetPublishedImagesVersion is the current evidence envelope version.
	ReleaseSetPublishedImagesVersion = 1
	// ReleaseSetPublishedImagesMaxBytes bounds the private evidence envelope.
	ReleaseSetPublishedImagesMaxBytes = 4 * 1024 * 1024
)

// ReleaseSetPublishedImage binds one canonical workspace project to the
// immutable OCI digest verified by its successful Docker publish task.
type ReleaseSetPublishedImage struct {
	Project string `json:"project"`
	Digest  string `json:"digest"`
}

type releaseSetPublishedImagesEnvelope struct {
	ProtocolVersion int                        `json:"protocolVersion"`
	Published       []ReleaseSetPublishedImage `json:"published"`
}

// MarshalReleaseSetPublishedImages produces the canonical bounded envelope.
func MarshalReleaseSetPublishedImages(images []ReleaseSetPublishedImage) ([]byte, error) {
	canonical, err := canonicalReleaseSetPublishedImages(images)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(releaseSetPublishedImagesEnvelope{
		ProtocolVersion: ReleaseSetPublishedImagesVersion,
		Published:       canonical,
	})
	if err != nil {
		return nil, fmt.Errorf("encode release-set published images: %w", err)
	}
	if len(data) > ReleaseSetPublishedImagesMaxBytes {
		return nil, fmt.Errorf("release-set published images exceed the bounded JSON contract")
	}
	return data, nil
}

// ParseReleaseSetPublishedImages strictly parses a canonical bounded envelope.
func ParseReleaseSetPublishedImages(data []byte) ([]ReleaseSetPublishedImage, error) {
	if len(data) == 0 || len(data) > ReleaseSetPublishedImagesMaxBytes {
		return nil, fmt.Errorf("release-set published images exceed the bounded JSON contract")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope releaseSetPublishedImagesEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode release-set published images: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("release-set published images contain trailing data")
	}
	if envelope.ProtocolVersion != ReleaseSetPublishedImagesVersion {
		return nil, fmt.Errorf("release-set published images protocolVersion %d is unsupported", envelope.ProtocolVersion)
	}
	canonical, err := canonicalReleaseSetPublishedImages(envelope.Published)
	if err != nil {
		return nil, err
	}
	if len(canonical) != len(envelope.Published) {
		return nil, fmt.Errorf("release-set published images are not canonical")
	}
	for index := range canonical {
		if canonical[index] != envelope.Published[index] {
			return nil, fmt.Errorf("release-set published images are not in canonical project order")
		}
	}
	return canonical, nil
}

// ReadReleaseSetPublishedImagesFile reads the private provider file. It rejects
// relative paths, symlinks, broad permissions, non-regular files and oversized
// content before decoding.
func ReadReleaseSetPublishedImagesFile(env map[string]string) ([]ReleaseSetPublishedImage, bool, error) {
	path := strings.TrimSpace(env[ReleaseSetPublishedImagesFileEnv])
	if path == "" {
		return nil, false, nil
	}
	if !filepath.IsAbs(path) {
		return nil, true, fmt.Errorf("release-set published images path is not absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, true, fmt.Errorf("stat release-set published images: %w", err)
	}
	if !info.Mode().IsRegular() || broadPermissions(info.Mode()) {
		return nil, true, fmt.Errorf("release-set published images file must be private and regular")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, true, fmt.Errorf("open release-set published images: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || broadPermissions(openedInfo.Mode()) || !os.SameFile(info, openedInfo) {
		return nil, true, fmt.Errorf("release-set published images file changed during private open")
	}
	data, err := io.ReadAll(io.LimitReader(file, ReleaseSetPublishedImagesMaxBytes+1))
	if err != nil {
		return nil, true, fmt.Errorf("read release-set published images: %w", err)
	}
	images, err := ParseReleaseSetPublishedImages(data)
	return images, true, err
}

func canonicalReleaseSetPublishedImages(images []ReleaseSetPublishedImage) ([]ReleaseSetPublishedImage, error) {
	canonical := append([]ReleaseSetPublishedImage(nil), images...)
	for _, image := range canonical {
		if image.Project == "" || image.Project != strings.TrimSpace(image.Project) || len(image.Project) > 512 {
			return nil, fmt.Errorf("release-set published image has invalid project identity")
		}
		if !releaseSetPublishedImageDigest(image.Digest) {
			return nil, fmt.Errorf("release-set published image %q has invalid immutable digest", image.Project)
		}
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Project < canonical[j].Project })
	for index := 1; index < len(canonical); index++ {
		if canonical[index-1].Project == canonical[index].Project {
			return nil, fmt.Errorf("release-set published image project %q appears more than once", canonical[index].Project)
		}
	}
	return canonical, nil
}

func releaseSetPublishedImageDigest(value string) bool {
	hex, ok := strings.CutPrefix(value, "sha256:")
	if !ok || len(hex) != 64 {
		return false
	}
	for _, char := range hex {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
