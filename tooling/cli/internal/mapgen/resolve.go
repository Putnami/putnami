package mapgen

import (
	"sync"

	"go.putnami.dev/cli/model/workspace"
)

// Resolution is one in-memory map build: the complete document plus how much of
// it came from cached fragments.
type Resolution struct {
	Map             *RepoMap
	FragmentsReused int
	FragmentsBuilt  int
}

// FragmentCache holds the fragments a long-lived process built, keyed by
// project id, so its next map build reuses every fragment whose recorded inputs
// digest still matches the working tree. It lives in memory only: nothing
// reads or writes a fragment on disk.
//
// It is safe for concurrent use. A fragment is a pure function of the inputs
// its digest covers, so an entry another call replaced or dropped can only turn
// a hit into a miss, never serve a stale fragment. Callers must not mutate a
// fragment they receive: it may be shared with a later build.
//
// A nil *FragmentCache is valid and caches nothing.
type FragmentCache struct {
	mu        sync.Mutex
	fragments map[string]*Fragment
}

// NewFragmentCache returns an empty cache.
func NewFragmentCache() *FragmentCache {
	return &FragmentCache{fragments: make(map[string]*Fragment)}
}

func (c *FragmentCache) lookup(projectID string) *Fragment {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fragments[projectID]
}

func (c *FragmentCache) store(projectID string, fragment *Fragment) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fragments[projectID] = fragment
}

// retain drops every entry whose project is not in live, so a deleted or
// renamed project does not hold its fragment for the rest of the session.
func (c *FragmentCache) retain(live map[string]bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.fragments {
		if !live[id] {
			delete(c.fragments, id)
		}
	}
}

// ResolveMap renders the complete map in memory and writes NOTHING. Every live
// project's fragment is validated against its inputs on disk and reused from
// cache, or rebuilt when the digest moved, so a warm call is O(changed) and a
// cold one O(projects) while the render is always complete and always current.
// A nil cache builds every fragment.
func ResolveMap(ws *workspace.Workspace, cache *FragmentCache) (Resolution, error) {
	live := LiveProjects(ws)
	fragments := make(map[string]*Fragment, len(live))
	liveIDs := make(map[string]bool, len(live))
	var resolution Resolution
	for _, p := range live {
		fragment, reused, err := ResolveFragment(ws.Root, p, cache)
		if err != nil {
			return Resolution{}, err
		}
		fragments[p.ID] = fragment
		liveIDs[p.ID] = true
		if reused {
			resolution.FragmentsReused++
		} else {
			resolution.FragmentsBuilt++
		}
	}
	cache.retain(liveIDs)
	repoMap, err := Reduce(ws, fragments)
	if err != nil {
		return Resolution{}, err
	}
	resolution.Map = repoMap
	return resolution, nil
}
