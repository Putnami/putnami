package toolchain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// TestRuntimeToolchainIdentityHashesItsFieldsInOrder pins the inputs of the
// identity every cache key carries for a toolchain, and their order: the
// declaration, the version, the host platform, the host's integrity, for a
// declaration with a provisioned candidate the executable of the host's
// archive, then the availability. The expected digest frames each field by
// hand, each label length and value size an unsigned 64-bit little-endian
// integer before its bytes, so a change to a label, an input or the order
// moves the digest here before it moves a key. The declarations are literals,
// so a change to the declaration's JSON encoding fails here too. A
// declaration without a provisioned candidate does not bind the archives the
// entry records.
func TestRuntimeToolchainIdentityHashesItsFieldsInOrder(t *testing.T) {
	t.Parallel()
	host := runtime.GOOS + "/" + runtime.GOARCH
	entry := lockfile.LockEntry{
		Version:     "1.2.3",
		Archives:    map[string]lockfile.LockArchive{host: {URL: "https://example.test/compiler.tar.gz", Format: "tar.gz", Executable: "compiler/bin/compiler"}},
		Integrities: map[string]string{host: "integrity-a"},
	}
	provisioned := runtimeToolchainFixture("compiler")
	provisioned.Candidates = []extensionproto.RuntimeToolchainCandidate{{From: extensionproto.RuntimeToolchainCandidateProvisioned}}
	cases := []struct {
		name        string
		requirement extensionproto.RuntimeToolchain
		declaration string
		executable  bool
	}{
		{
			name:        "path candidate",
			requirement: runtimeToolchainFixture("compiler"),
			declaration: `{"lock":"compiler","candidates":[{"from":"path","path":"compiler"}],` +
				`"probe":{"args":["--version"],"expect":"{version}"},` +
				`"environment":{"COMPILER_ROOT":{"from":"ancestor","levels":2}},"prependPath":true}`,
		},
		{
			name:        "provisioned candidate",
			requirement: provisioned,
			declaration: `{"lock":"compiler","candidates":[{"from":"provisioned"}],` +
				`"probe":{"args":["--version"],"expect":"{version}"},` +
				`"environment":{"COMPILER_ROOT":{"from":"ancestor","levels":2}},"prependPath":true}`,
			executable: true,
		},
	}
	for _, tc := range cases {
		encoded, err := json.Marshal(tc.requirement)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != tc.declaration {
			t.Fatalf("%s: declaration encodes as %s, want %s; every toolchain identity moves with it", tc.name, encoded, tc.declaration)
		}
		for _, available := range []bool{true, false} {
			availability := "unavailable"
			if available {
				availability = "available"
			}
			fields := [][2]string{
				{"declaration", tc.declaration},
				{"version", "1.2.3"},
				{"platform", host},
				{"integrity", "integrity-a"},
			}
			if tc.executable {
				fields = append(fields, [2]string{"executable", "compiler/bin/compiler"})
			}
			var framed []byte
			for _, field := range append(fields, [2]string{"availability", availability}) {
				framed = binary.LittleEndian.AppendUint64(framed, uint64(len(field[0])))
				framed = append(framed, field[0]...)
				framed = binary.LittleEndian.AppendUint64(framed, uint64(len(field[1])))
				framed = append(framed, field[1]...)
			}
			sum := sha256.Sum256(framed)
			want := hex.EncodeToString(sum[:])
			if got := runtimeToolchainIdentity(tc.requirement, entry, available); got != want {
				t.Errorf("%s: runtimeToolchainIdentity(available=%v) = %s, want %s", tc.name, available, got, want)
			}
		}
	}
}
