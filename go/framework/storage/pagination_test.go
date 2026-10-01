package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func collectAllPages(t *testing.T, b Backend, bucket string, pageSize int) []string {
	t.Helper()
	ctx := context.Background()
	var keys []string
	seen := make(map[string]bool)
	opts := &ListOptions{MaxKeys: pageSize}
	for i := 0; ; i++ {
		if i > 1000 {
			t.Fatal("pagination did not terminate (continuation token ignored?)")
		}
		page, err := b.List(ctx, bucket, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Objects {
			if seen[o.Key] {
				t.Fatalf("duplicate key across pages: %s", o.Key)
			}
			seen[o.Key] = true
			keys = append(keys, o.Key)
		}
		if !page.IsTruncated {
			break
		}
		opts.ContinuationToken = page.ContinuationToken
	}
	return keys
}

func TestList_PaginationResumes(t *testing.T) {
	backends := []struct {
		name string
		make func() Backend
	}{
		{"memory", func() Backend { return NewMemoryBackend() }},
		{"file", func() Backend { return NewFileBackend(t.TempDir()) }},
	}
	for _, tc := range backends {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.make()
			ctx := context.Background()
			var want []string
			for i := 0; i < 10; i++ {
				key := fmt.Sprintf("obj-%02d", i)
				want = append(want, key)
				if _, err := b.Put(ctx, "bucket", key, strings.NewReader("x"), nil); err != nil {
					t.Fatal(err)
				}
			}
			sort.Strings(want)

			got := collectAllPages(t, b, "bucket", 3)
			if len(got) != len(want) {
				t.Fatalf("paginated keys = %v (%d), want %d total", got, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("key[%d] = %s, want %s", i, got[i], want[i])
				}
			}
		})
	}
}
