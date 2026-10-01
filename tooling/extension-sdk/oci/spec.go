// Package oci assembles COPY-only container images in-process and
// deterministically: pinned base image (by digest), fixed timestamps, sorted
// tar entries, and explicit file modes. Identical inputs produce a
// byte-identical image on any machine, so the image digest is reproducible
// across hosts — the property docker build cannot give (layer tar mtimes and
// config timestamps vary per build). No docker daemon is involved.
package oci

import (
	"encoding/json"
	"fmt"
)

// File is one file copied into the image.
type File struct {
	// Source is the local path of the file content. It is machine-specific
	// and therefore excluded from the canonical spec — only the content it
	// points at is hashed.
	Source string `json:"-"`
	// Path is the absolute in-image destination, e.g. "/app/my-app".
	Path string `json:"path"`
	// Mode is the explicit file mode (e.g. 0o755 for executables, 0o644
	// otherwise). It is never read from the local filesystem: checkout
	// umasks must not leak into the image identity.
	Mode int64 `json:"mode"`
}

// Layer is one image layer, mirroring one COPY instruction group. Keeping
// the docker-build layer granularity lets registries dedupe unchanged blobs
// (e.g. an unchanged binary layer when only assets changed).
type Layer struct {
	Files []File `json:"files"`
	// Tarball is an optional prebuilt filesystem layer. Its host path is
	// excluded from the canonical spec; its complete bytes are folded into the
	// content hash. A layer contains either Files or one Tarball, never both.
	Tarball string `json:"-"`
}

// Spec describes a COPY-only image to assemble on top of a pinned base.
type Spec struct {
	// BaseRef must pin the base by digest (e.g.
	// "gcr.io/distroless/static:nonroot@sha256:..."). A moving tag would
	// make the assembled digest depend on when the base was resolved.
	BaseRef string `json:"baseRef"`
	// Platform selects the manifest from the base index, e.g. "linux/amd64".
	Platform string  `json:"platform"`
	Layers   []Layer `json:"layers"`
	// Env entries ("KEY=VALUE", in order) are merged onto the base config
	// env with docker semantics: same key overrides, new keys append.
	Env []string `json:"env,omitempty"`
	// Entrypoint replaces the base entrypoint and, like docker build, resets
	// any base CMD.
	Entrypoint []string `json:"entrypoint,omitempty"`
	// WorkingDir, when non-empty, replaces the base working directory.
	WorkingDir string `json:"workingDir,omitempty"`
	// User, when non-empty, replaces the base user (otherwise inherited —
	// distroless :nonroot bases already run as nonroot).
	User string `json:"user,omitempty"`
	// ExposedPorts are added to the base config, e.g. "3000/tcp".
	ExposedPorts []string `json:"exposedPorts,omitempty"`
}

// CanonicalString returns the deterministic serialization of everything that
// shapes the image except file contents (and machine-specific source paths).
// It plays the role the generated Dockerfile played for content hashing
// before in-process assembly.
func (s Spec) CanonicalString() string {
	data, err := json.Marshal(s)
	if err != nil {
		// Spec is plain data; Marshal cannot fail on it.
		panic(fmt.Sprintf("oci: marshal spec: %v", err))
	}
	return string(data)
}

// ContentHash returns the versioned ImagePlan identity. New callers that also
// assemble the image should retain the plan returned by NewImagePlan so source
// fingerprints are not recomputed. Files listed in skipContent are treated as
// deterministic outputs derived from this identity; prefer an explicit,
// recipe-specific PlanOptions.DerivedFiles identity in new code.
func ContentHash(s Spec, skipContent ...string) (string, error) {
	derived := make(map[string]string, len(skipContent))
	for _, p := range skipContent {
		derived[p] = legacyDerivedIdentity
	}
	plan, err := NewImagePlan(s, PlanOptions{DerivedFiles: derived})
	if err != nil {
		return "", err
	}
	return plan.Identity, nil
}
