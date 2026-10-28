// Command overlap-report reads the per-test coverage that overlap-shard.sh
// writes and reports which tests reach the same statements.
//
// Usage: overlap-report <artifacts dir> <repository root> <out dir>
package main

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"fmt"
	"hash/fnv"
	"math/bits"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type test struct {
	bin, dir, name, status, file, category string
	seconds, wall                          float64
	blocks                                 []int
	set                                    []uint64
	stmts, unique                          int
	subsumedBy                             int // index of a test that reaches every statement this one reaches, or -1
	identicalTo                            int // lowest index with the same set, or -1
}

var (
	blockID   = map[string]int{}
	blockStmt []int
)

func id(key string, stmts int) int {
	if i, ok := blockID[key]; ok {
		return i
	}
	i := len(blockStmt)
	blockID[key] = i
	blockStmt = append(blockStmt, stmts)
	return i
}

func readBlocks(path string) ([]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil // an empty file: the run wrote no coverage
	}
	var out []int
	s := bufio.NewScanner(z)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		k, n, ok := strings.Cut(s.Text(), " ")
		if !ok {
			continue
		}
		st, _ := strconv.Atoi(n)
		out = append(out, id(k, st))
	}
	return out, s.Err()
}

var guard = regexp.MustCompile(`ratchet|baseline|boundar|_pin|isolation|structural|decoupl|vocabular|guard|architecture`)

func categorize(t *test, root string) {
	files, _ := filepath.Glob(filepath.Join(t.dir, "*_test.go"))
	decl := "func " + t.name + "("
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err == nil && strings.Contains(string(b), decl) {
			t.file, _ = filepath.Rel(root, f)
			break
		}
	}
	rel, _ := filepath.Rel(root, t.dir)
	switch {
	case strings.Contains(rel, "internal/cli/e2e/"):
		t.category = "end-to-end"
	case guard.MatchString(filepath.Base(t.file)):
		t.category = "structural guard"
	default:
		t.category = "unit and integration"
	}
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: overlap-report <artifacts> <repo> <out>")
		os.Exit(2)
	}
	art, root, out := os.Args[1], os.Args[2], os.Args[3]
	_ = os.MkdirAll(out, 0o755)
	root, _ = filepath.Abs(root)

	// Universe first, so block ids cover every instrumented block.
	universes, _ := filepath.Glob(filepath.Join(art, "*", "universe", "*.gz"))
	for _, u := range universes {
		if _, err := readBlocks(u); err != nil {
			fmt.Fprintln(os.Stderr, u, err)
		}
	}
	runs, _ := filepath.Glob(filepath.Join(art, "*", "runs-*.tsv"))
	var tests []*test
	for _, r := range runs {
		b, err := os.ReadFile(r)
		if err != nil {
			panic(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			c := strings.Split(line, "\t")
			if len(c) != 6 {
				continue
			}
			t := &test{bin: c[0], dir: c[1], name: c[2], status: c[3], subsumedBy: -1, identicalTo: -1}
			t.seconds, _ = strconv.ParseFloat(c[4], 64)
			t.wall, _ = strconv.ParseFloat(c[5], 64)
			if c[4] == "" {
				t.seconds = t.wall
			}
			// The runner checkout path differs from the local one.
			if i := strings.Index(t.dir, "/tooling/cli"); i >= 0 {
				t.dir = filepath.Join(root, t.dir[i+1:])
			}
			cov := filepath.Join(filepath.Dir(r), "cov", t.bin+"."+t.name+".gz")
			t.blocks, err = readBlocks(cov)
			if err != nil {
				fmt.Fprintln(os.Stderr, cov, err)
			}
			categorize(t, root)
			tests = append(tests, t)
		}
	}
	sort.Slice(tests, func(i, j int) bool {
		if tests[i].bin != tests[j].bin {
			return tests[i].bin < tests[j].bin
		}
		return tests[i].name < tests[j].name
	})
	nb := len(blockStmt)
	words := (nb + 63) / 64
	cover := make([][]int32, nb) // block -> tests that reach it
	for i, t := range tests {
		t.set = make([]uint64, words)
		for _, b := range t.blocks {
			if t.set[b/64]&(1<<(b%64)) == 0 {
				t.set[b/64] |= 1 << (b % 64)
				cover[b] = append(cover[b], int32(i))
				t.stmts += blockStmt[b]
			}
		}
	}
	totalStmts, unionStmts := 0, 0
	for b, n := range blockStmt {
		totalStmts += n
		if len(cover[b]) > 0 {
			unionStmts += n
		}
		if len(cover[b]) == 1 {
			tests[cover[b][0]].unique += n
		}
	}

	// Identical sets.
	byHash := map[uint64][]int{}
	for i, t := range tests {
		if t.stmts == 0 {
			continue
		}
		h := fnv.New64a()
		for _, w := range t.set {
			var b [8]byte
			for k := range b {
				b[k] = byte(w >> (8 * k))
			}
			h.Write(b[:])
		}
		byHash[h.Sum64()] = append(byHash[h.Sum64()], i)
	}
	for _, g := range byHash {
		for _, i := range g[1:] {
			if equal(tests[g[0]].set, tests[i].set) {
				tests[i].identicalTo = g[0]
			}
		}
	}
	// Subsumed by one other test: the candidates are the tests that reach the
	// rarest block of this one.
	for i, t := range tests {
		if t.stmts == 0 || t.identicalTo >= 0 {
			continue
		}
		rare := -1
		for _, b := range t.blocks {
			if rare < 0 || len(cover[b]) < len(cover[rare]) {
				rare = b
			}
		}
		for _, j := range cover[rare] {
			if int(j) != i && tests[j].stmts >= t.stmts && subset(t.set, tests[j].set) {
				if !equal(t.set, tests[j].set) || int(j) < i {
					t.subsumedBy = int(j)
					break
				}
			}
		}
	}

	// Greedy weighted cover: the cheapest suite, in test seconds, that reaches
	// every statement the whole suite reaches.
	order := greedy(tests, words, unionStmts)

	writeReport(out, tests, order, totalStmts, unionStmts)
}

func equal(a, b []uint64) bool {
	for k := range a {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}

func subset(a, b []uint64) bool {
	for k := range a {
		if a[k]&^b[k] != 0 {
			return false
		}
	}
	return true
}

type item struct {
	i     int
	gain  int
	ratio float64
}
type pq []item

func (p pq) Len() int           { return len(p) }
func (p pq) Less(a, b int) bool { return p[a].ratio > p[b].ratio }
func (p pq) Swap(a, b int)      { p[a], p[b] = p[b], p[a] }
func (p *pq) Push(x any)        { *p = append(*p, x.(item)) }
func (p *pq) Pop() any          { o := *p; it := o[len(o)-1]; *p = o[:len(o)-1]; return it }
func cost(t *test) float64      { return max(t.seconds, 0.01) }
func gainOf(t *test, have []uint64) int {
	g := 0
	for k, w := range t.set {
		nw := w &^ have[k]
		for nw != 0 {
			b := k*64 + bits.TrailingZeros64(nw)
			g += blockStmt[b]
			nw &= nw - 1
		}
	}
	return g
}

func greedy(tests []*test, words, target int) []int {
	have := make([]uint64, words)
	h := &pq{}
	for i, t := range tests {
		if t.stmts > 0 {
			heap.Push(h, item{i, t.stmts, float64(t.stmts) / cost(t)})
		}
	}
	got := 0
	var order []int
	for h.Len() > 0 && got < target {
		it := heap.Pop(h).(item)
		g := gainOf(tests[it.i], have)
		if g == 0 {
			continue
		}
		if g < it.gain { // stale: re-queue with the current gain
			heap.Push(h, item{it.i, g, float64(g) / cost(tests[it.i])})
			continue
		}
		for k, w := range tests[it.i].set {
			have[k] |= w
		}
		got += g
		order = append(order, it.i)
	}
	return order
}

func writeReport(out string, tests []*test, order []int, total, union int) {
	f, _ := os.Create(filepath.Join(out, "tests.tsv"))
	fmt.Fprintln(f, "package\ttest\tfile\tcategory\tstatus\tseconds\tstatements\tunique\tidentical_to\tsubsumed_by")
	name := func(i int) string {
		if i < 0 {
			return ""
		}
		return tests[i].bin + "." + tests[i].name
	}
	for _, t := range tests {
		fmt.Fprintf(f, "%s\t%s\t%s\t%s\t%s\t%.3f\t%d\t%d\t%s\t%s\n", t.bin, t.name, t.file, t.category, t.status,
			t.seconds, t.stmts, t.unique, name(t.identicalTo), name(t.subsumedBy))
	}
	f.Close()

	type agg struct {
		n                                   int
		s, zeroS, emptyS, identS, subS      float64
		zero, empty, ident, sub, fail, skip int
	}
	cats := map[string]*agg{}
	all := &agg{}
	for _, t := range tests {
		if cats[t.category] == nil {
			cats[t.category] = &agg{}
		}
		for _, a := range []*agg{all, cats[t.category]} {
			a.n++
			a.s += t.seconds
			switch {
			case t.stmts == 0:
				a.empty++
				a.emptyS += t.seconds
			case t.identicalTo >= 0:
				a.ident++
				a.identS += t.seconds
			case t.subsumedBy >= 0:
				a.sub++
				a.subS += t.seconds
			}
			if t.stmts > 0 && t.unique == 0 {
				a.zero++
				a.zeroS += t.seconds
			}
			if t.status == "FAIL" || t.status == "NONE" {
				a.fail++
			}
			if t.status == "SKIP" {
				a.skip++
			}
		}
	}
	greedyS := 0.0
	for _, i := range order {
		greedyS += tests[i].seconds
	}
	r, _ := os.Create(filepath.Join(out, "report.md"))
	defer r.Close()
	fmt.Fprintf(r, "Tests: %d, %.1f s summed (in-test time, coverage on).\n", all.n, all.s)
	fmt.Fprintf(r, "Statements: %d instrumented, %d reached by at least one test (%.1f%%).\n\n", total, union, 100*float64(union)/float64(total))
	fmt.Fprintf(r, "Greedy cover: %d tests, %.0f s, reach every statement the suite reaches.\n\n", len(order), greedyS)
	fmt.Fprintln(r, "| Category | Tests | Seconds | Reach no statement | Same set as another | Inside one other test | No unique statement | Failed or no result | Skipped |")
	fmt.Fprintln(r, "|---|---|---|---|---|---|---|---|---|")
	keys := []string{}
	for k := range cats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	row := func(k string, a *agg) {
		fmt.Fprintf(r, "| %s | %d | %.0f | %d (%.0f s) | %d (%.0f s) | %d (%.0f s) | %d (%.0f s) | %d | %d |\n",
			k, a.n, a.s, a.empty, a.emptyS, a.ident, a.identS, a.sub, a.subS, a.zero, a.zeroS, a.fail, a.skip)
	}
	for _, k := range keys {
		row(k, cats[k])
	}
	row("all", all)

	fmt.Fprintln(r, "\nSlowest tests inside one other test or with the same set as another:")
	var cand []*test
	for _, t := range tests {
		if t.stmts > 0 && (t.identicalTo >= 0 || t.subsumedBy >= 0) {
			cand = append(cand, t)
		}
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].seconds > cand[j].seconds })
	for _, t := range cand[:min(25, len(cand))] {
		by := t.subsumedBy
		if t.identicalTo >= 0 {
			by = t.identicalTo
		}
		fmt.Fprintf(r, "- %.1f s `%s` (%s) inside `%s`\n", t.seconds, t.name, t.file, tests[by].name)
	}
}
