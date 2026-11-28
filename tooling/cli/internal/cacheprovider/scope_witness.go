package cacheprovider

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// scopeWitnessMargin is the age a scope file needs for its stat to answer for
// its content: a file written again within it can keep an equal stat.
const scopeWitnessMargin = 2 * time.Second

// ScopeWitness is the witness of a provider's marker scope
// (cache.MarkerScope), taken when the provider read it: the stat of each file
// and a salted digest of each variable. It holds while none of them moved.
type ScopeWitness struct {
	ScopeKey  string      `json:"scopeKey"`
	Files     []scopeFile `json:"files"`
	Env       []string    `json:"env,omitempty"`
	Prefixes  []string    `json:"envPrefixes,omitempty"`
	Salt      string      `json:"salt"`
	EnvDigest string      `json:"envDigest"`
}

type scopeFile struct {
	Path string `json:"path"`
	// Link is the path's own stat and Target the stat of what it names; nil
	// is absent.
	Link   *fileindex.Stat `json:"link,omitempty"`
	Target *fileindex.Stat `json:"target,omitempty"`
}

// WitnessScope is the witness of scope, read now. It is false when the scope
// cannot be witnessed: no scope or key, a file whose directory's stats are not
// trusted (fileindex.TrustsStats), or a file written within the margin of now.
func WitnessScope(scope *cache.MarkerScope, now time.Time) (*ScopeWitness, bool) {
	if scope == nil || strings.TrimSpace(scope.Key) == "" {
		return nil, false
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, false
	}
	w := &ScopeWitness{
		ScopeKey: scope.Key, Salt: hex.EncodeToString(salt),
		Env: slices.Sorted(slices.Values(scope.Env)), Prefixes: slices.Sorted(slices.Values(scope.EnvPrefixes)),
	}
	for _, path := range slices.Sorted(slices.Values(scope.Files)) {
		if !filepath.IsAbs(path) || !fileindex.TrustsStats(filepath.Dir(path)) {
			return nil, false
		}
		file := statScopeFile(path)
		for _, st := range []*fileindex.Stat{file.Link, file.Target} {
			if st != nil && now.Sub(time.Unix(0, max(st.ModNano, st.ChangeNano))) < scopeWitnessMargin {
				return nil, false
			}
		}
		w.Files = append(w.Files, file)
	}
	w.EnvDigest = w.envDigest(os.Environ())
	return w, true
}

// Holds reports whether every file has its stat and every variable its value.
func (w *ScopeWitness) Holds() bool {
	if w == nil || w.ScopeKey == "" || w.Salt == "" {
		return false
	}
	for _, file := range w.Files {
		if now := statScopeFile(file.Path); !sameStat(now.Link, file.Link) || !sameStat(now.Target, file.Target) {
			return false
		}
	}
	return w.envDigest(os.Environ()) == w.EnvDigest
}

func statScopeFile(path string) scopeFile {
	file := scopeFile{Path: path}
	if st, ok := fileindex.LstatAny(path); ok {
		file.Link = &st
	}
	if st, ok := fileindex.StatAny(path); ok {
		file.Target = &st
	}
	return file
}

func sameStat(a, b *fileindex.Stat) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// envDigest digests, salted, the values of the named variables, an unset one
// told apart from an empty one, and of every variable of environ whose name
// starts with a prefix. The salt keeps a token's value from being found from
// the digest the file holds.
func (w *ScopeWitness) envDigest(environ []string) string {
	values := map[string]string{}
	for _, variable := range environ {
		name, value, _ := strings.Cut(variable, "=")
		values[name] = value
	}
	selected := map[string]bool{}
	for _, name := range w.Env {
		selected[name] = true
	}
	for name := range values {
		for _, prefix := range w.Prefixes {
			if prefix != "" && strings.HasPrefix(name, prefix) {
				selected[name] = true
			}
		}
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	slices.Sort(names)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d:%s", len(w.Salt), w.Salt)
	for _, name := range names {
		value, set := values[name]
		_, _ = fmt.Fprintf(h, "%d:%s%t%d:%s", len(name), name, set, len(value), value)
	}
	return hex.EncodeToString(h.Sum(nil))
}
