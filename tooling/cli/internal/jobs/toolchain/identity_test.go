package toolchain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"testing"
)

// TestRuntimeToolchainIdentityHashesItsFieldsInOrder pins the inputs of the
// identity every cache key carries for a toolchain, and their order: the
// declaration, the version, the host platform, the integrity, then the
// availability. The expected digest frames each field by hand, each label
// length and value size an unsigned 64-bit little-endian integer before its
// bytes, so a change to a label, an input or the order moves the digest here
// before it moves a key. The declaration is a literal, so a change to the
// declaration's JSON encoding fails here too.
func TestRuntimeToolchainIdentityHashesItsFieldsInOrder(t *testing.T) {
	t.Parallel()
	const declaration = `{"lock":"compiler","candidates":[{"from":"path","path":"compiler"}],` +
		`"probe":{"args":["--version"],"expect":"{version}"},` +
		`"environment":{"COMPILER_ROOT":{"from":"ancestor","levels":2}},"prependPath":true}`
	requirement := runtimeToolchainFixture("compiler")
	encoded, err := json.Marshal(requirement)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != declaration {
		t.Fatalf("declaration encodes as %s, want %s; every toolchain identity moves with it", encoded, declaration)
	}
	for _, available := range []bool{true, false} {
		availability := "unavailable"
		if available {
			availability = "available"
		}
		var framed []byte
		for _, field := range [][2]string{
			{"declaration", declaration},
			{"version", "1.2.3"},
			{"platform", runtime.GOOS + "/" + runtime.GOARCH},
			{"integrity", "integrity-a"},
			{"availability", availability},
		} {
			framed = binary.LittleEndian.AppendUint64(framed, uint64(len(field[0])))
			framed = append(framed, field[0]...)
			framed = binary.LittleEndian.AppendUint64(framed, uint64(len(field[1])))
			framed = append(framed, field[1]...)
		}
		sum := sha256.Sum256(framed)
		want := hex.EncodeToString(sum[:])
		if got := runtimeToolchainIdentity(requirement, "1.2.3", "integrity-a", available); got != want {
			t.Errorf("runtimeToolchainIdentity(available=%v) = %s, want %s", available, got, want)
		}
	}
}
