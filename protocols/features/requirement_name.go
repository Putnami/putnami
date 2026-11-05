package features

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"unicode/utf8"
)

// asciiFoldTable is what Unicode NFKD less its combining marks gives for each
// code point it drops or turns into ASCII letters or digits. Go has no NFKD in
// its standard library and D-055 keeps third-party modules out of this
// protocol, so the table is committed data, which the TypeScript test of
// @putnami/application derives from its runtime. (D-057)
//
//go:embed unicode/ascii-fold.json
var asciiFoldTable []byte

// asciiFold holds the parsed table: the sorted ranges of code points NFKD drops,
// and the ASCII-bearing decomposition of the others it keeps.
type asciiFold struct {
	drop [][2]rune
	fold map[rune]string
}

var asciiFolds = sync.OnceValue(func() asciiFold {
	var table struct {
		Drop [][2]string       `json:"drop"`
		Fold map[string]string `json:"fold"`
	}
	if err := json.Unmarshal(asciiFoldTable, &table); err != nil {
		panic("features: the embedded ASCII fold table is invalid: " + err.Error())
	}
	folds := asciiFold{drop: make([][2]rune, 0, len(table.Drop)), fold: make(map[rune]string, len(table.Fold))}
	for _, r := range table.Drop {
		folds.drop = append(folds.drop, [2]rune{codePoint(r[0]), codePoint(r[1])})
	}
	sort.Slice(folds.drop, func(i, j int) bool { return folds.drop[i][0] < folds.drop[j][0] })
	for cp, folded := range table.Fold {
		folds.fold[codePoint(cp)] = folded
	}
	return folds
})

func codePoint(hex string) rune {
	cp, err := strconv.ParseUint(hex, 16, 32)
	if err != nil || cp > utf8.MaxRune {
		panic("features: the embedded ASCII fold table holds an invalid code point: " + hex)
	}
	return rune(cp)
}

// drops reports whether NFKD less its combining marks leaves nothing of r.
func (f asciiFold) drops(r rune) bool {
	i := sort.Search(len(f.drop), func(i int) bool { return f.drop[i][1] >= r })
	return i < len(f.drop) && f.drop[i][0] <= r
}

// NormalizeRequirementName maps a name-based requirement to its ID. It
// decomposes the name to Unicode NFKD and drops the combining marks, then
// lowercases ASCII letters, keeps ASCII digits, turns each other run of
// characters into one hyphen and trims the ends. A letter with no ASCII
// decomposition, such as ß, stays a separator. A name with nothing left, or
// longer than the bound, has no ID. (D-057)
func NormalizeRequirementName(name string) (string, bool) {
	folds := asciiFolds()
	var out []byte
	pending := false
	write := func(c byte) {
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		default:
			pending = true
			return
		}
		if pending && len(out) > 0 {
			out = append(out, '-')
		}
		pending = false
		out = append(out, c)
	}
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		switch {
		case r < utf8.RuneSelf:
			write(name[i])
		case folds.drops(r):
		default:
			// A code point outside the table folds to nothing ASCII: a separator.
			folded, ok := folds.fold[r]
			if !ok {
				pending = true
			}
			for j := 0; j < len(folded); j++ {
				write(folded[j])
			}
		}
		i += size
	}
	if len(out) == 0 || len(out) > requirementNameMaxLength {
		return "", false
	}
	return string(out), true
}
