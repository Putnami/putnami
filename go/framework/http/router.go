// Package http provides the HTTP server, trie-based router, request context,
// middleware chain, endpoint builder, and response helpers for the Putnami
// Go framework.
package http

import (
	"fmt"
	"strings"
)

// Router is a trie-based HTTP route matcher with support for path parameters,
// named catch-alls, and content negotiation.
type Router[T any] struct {
	root *trieNode[T]
}

// NewRouter creates a new empty router.
func NewRouter[T any]() *Router[T] {
	return &Router[T]{
		root: &trieNode[T]{
			children: make(map[byte]*trieNode[T]),
		},
	}
}

// Add registers a handler at the given route pattern.
//
// Supported patterns:
//
//	/users                          exact match
//	/users/{id}                     single-segment path parameter (no slashes)
//	/files/{path...}                catch-all parameter (captures one or more
//	                                segments, slashes included). May appear at
//	                                the end of the pattern or before a fixed
//	                                suffix, e.g. /{module...}/-/blobs/upload.
//	                                Fixed-suffix routes win over a bare catch-all
//	                                registered at the same position, so /{m...}
//	                                never shadows /{m...}/@latest.
//
// A bare "*" segment (the removed anonymous wildcard) panics with migration
// guidance: silently storing it as a literal would register a route that never
// matches.
func (r *Router[T]) Add(pattern string, handler T) {
	node := r.root
	parts := splitPath(pattern)

	for idx, part := range parts {
		// Cross an explicit '/' boundary node between segments. Static segments
		// are otherwise stored character-by-character, so without this marker
		// the trie for "/health" (one segment) is byte-identical to "/hea/lth"
		// (two segments) — a route collision and a path-confusion match. The
		// '/' byte can never appear inside a segment (splitPath splits on it),
		// so it is a safe, unambiguous separator.
		if idx > 0 {
			sep, ok := node.children['/']
			if !ok {
				sep = &trieNode[T]{
					children: make(map[byte]*trieNode[T]),
				}
				node.children['/'] = sep
			}
			node = sep
		}

		if part == "*" {
			panic("anonymous wildcard removed; use {name...}")
		}

		if len(part) > 2 && part[0] == '{' && part[len(part)-1] == '}' {
			paramName := part[1 : len(part)-1]
			if strings.HasSuffix(paramName, "...") {
				paramName = paramName[:len(paramName)-3]
				if paramName == "" {
					panic(fmt.Sprintf("http.Router: catch-all param requires a name in pattern %q", pattern))
				}
				if node.catchAllChild == nil {
					node.catchAllChild = &trieNode[T]{
						children:  make(map[byte]*trieNode[T]),
						paramName: paramName,
					}
				} else if node.catchAllChild.paramName != paramName {
					panic(fmt.Sprintf("http.Router: conflicting catch-all param names %q and %q at the same position", node.catchAllChild.paramName, paramName))
				}
				node = node.catchAllChild
				continue
			}
			if node.paramChild == nil {
				node.paramChild = &trieNode[T]{
					children:  make(map[byte]*trieNode[T]),
					paramName: paramName,
				}
			}
			node = node.paramChild
			continue
		}

		// Static segment — insert character by character
		for i := 0; i < len(part); i++ {
			ch := part[i]
			child, ok := node.children[ch]
			if !ok {
				child = &trieNode[T]{
					children: make(map[byte]*trieNode[T]),
				}
				node.children[ch] = child
			}
			node = child
		}
	}

	if len(node.handlers) > 0 {
		panic(fmt.Sprintf("http.Router: duplicate route registration for pattern %q", pattern))
	}
	node.handlers = append(node.handlers, handler)
	node.pattern = pattern
}

// lookup finds handlers matching the given path, extracting any path parameters.
// Returns nil if no match is found.
func (r *Router[T]) lookup(path string) *routeMatch[T] {
	var params map[string]string
	node := r.match(r.root, splitPath(path), &params)
	if node == nil || len(node.handlers) == 0 {
		return nil
	}
	return &routeMatch[T]{
		Handlers: node.handlers,
		Params:   params,
		Pattern:  node.pattern,
	}
}

// Routes returns all registered route patterns.
func (r *Router[T]) Routes() []string {
	var routes []string
	r.root.collectRoutes(&routes)
	return routes
}

// wrapAll applies a transformation function to all registered handlers in-place.
func (r *Router[T]) wrapAll(fn func(T) T) {
	r.root.wrapHandlers(fn)
}

// routeMatch holds the result of a successful route match.
type routeMatch[T any] struct {
	Handlers []T
	Params   map[string]string
	Pattern  string
}

// --- trie implementation ---

type trieNode[T any] struct {
	children      map[byte]*trieNode[T]
	paramChild    *trieNode[T]
	catchAllChild *trieNode[T]
	handlers      []T
	pattern       string
	paramName     string
}

func (n *trieNode[T]) wrapHandlers(fn func(T) T) {
	for i, h := range n.handlers {
		n.handlers[i] = fn(h)
	}
	for _, child := range n.children {
		child.wrapHandlers(fn)
	}
	if n.paramChild != nil {
		n.paramChild.wrapHandlers(fn)
	}
	if n.catchAllChild != nil {
		n.catchAllChild.wrapHandlers(fn)
	}
}

func (n *trieNode[T]) collectRoutes(routes *[]string) {
	if n.pattern != "" && len(n.handlers) > 0 {
		*routes = append(*routes, n.pattern)
	}
	for _, child := range n.children {
		child.collectRoutes(routes)
	}
	if n.paramChild != nil {
		n.paramChild.collectRoutes(routes)
	}
	if n.catchAllChild != nil {
		n.catchAllChild.collectRoutes(routes)
	}
}

func (r *Router[T]) match(node *trieNode[T], parts []string, params *map[string]string) *trieNode[T] {
	if len(parts) == 0 {
		if len(node.handlers) > 0 {
			return node
		}
		return nil
	}

	part := parts[0]
	remaining := parts[1:]

	// matchRemaining continues from the node reached at the end of the current
	// segment (`end`). When more segments follow, it crosses the explicit '/'
	// boundary node inserted by Add; a path with a different segmentation but
	// identical bytes (e.g. "/hea/lth") therefore cannot reach a handler
	// registered for another segmentation ("/health").
	matchRemaining := func(end *trieNode[T]) *trieNode[T] {
		if len(remaining) == 0 {
			return r.match(end, remaining, params)
		}
		// A single trailing empty segment means the request path ended in "/"
		// (splitPath("/v2/") == {"v2", ""}). Treat that trailing slash as
		// optional so "/v2/" reaches a handler registered for "/v2" — the
		// conventional trailing-slash tolerance every OCI/Docker client and most
		// HTTP routers assume (the OCI Distribution base endpoint is literally
		// "/v2/"). This does not weaken the '/'-boundary guard against path
		// confusion: that guard only concerns interior segments that carry
		// bytes, never a trailing empty one.
		if len(remaining) == 1 && remaining[0] == "" {
			if result := r.match(end, remaining[1:], params); result != nil {
				return result
			}
		}
		sep, ok := end.children['/']
		if !ok {
			return nil
		}
		return r.match(sep, remaining, params)
	}

	// Try exact static match first
	current := node
	matched := true
	for i := 0; i < len(part); i++ {
		child, ok := current.children[part[i]]
		if !ok {
			matched = false
			break
		}
		current = child
	}
	if matched {
		if result := matchRemaining(current); result != nil {
			return result
		}
	}

	// Try parameter match
	if node.paramChild != nil {
		if *params == nil {
			*params = make(map[string]string)
		}
		(*params)[node.paramChild.paramName] = part
		if result := matchRemaining(node.paramChild); result != nil {
			return result
		}
		delete(*params, node.paramChild.paramName)
	}

	// Try catch-all match. The catch-all greedily consumes one or more segments,
	// then matches the remaining suffix against the catch-all's subtree. Routes
	// with a fixed suffix (/{m...}/@v/list) are more specific than the bare
	// terminal (/{m...}), so every suffix split is tried first — longest capture
	// first, backing off one segment at a time — and only when no suffix route
	// matches may the terminal consume the whole path. Terminal-first ordering
	// would let a registered bare catch-all shadow its sibling suffix routes,
	// making them unreachable.
	//
	// The capture for the longest prefix (take == len(all)) is the full join of
	// all remaining segments; every shorter candidate is a prefix of it that
	// drops one trailing "/segment" chunk. Joining once and trimming the tail by
	// byte length per back-off makes each candidate O(1) (a sub-string re-slice,
	// no new allocation) instead of re-running strings.Join — turning the
	// back-tracking loop from O(n²) into O(n) in the path length while keeping
	// the captured value byte-identical to strings.Join(all[:take], "/").
	if node.catchAllChild != nil {
		all := parts
		paramName := node.catchAllChild.paramName
		if *params == nil {
			*params = make(map[string]string)
		}
		// full == strings.Join(all[:take], "/") for take == len(all).
		full := strings.Join(all, "/")
		if sep, ok := node.catchAllChild.children['/']; ok {
			joined := full
			for take := len(all) - 1; take >= 1; take-- {
				// Drop the last segment (and its leading "/") to derive this
				// take's capture from the previous one.
				joined = joined[:len(joined)-len(all[take])-1]
				(*params)[paramName] = joined
				// The catch-all consumed all[:take]; match the fixed suffix
				// all[take:] from the '/' boundary node placed by Add.
				if result := r.match(sep, all[take:], params); result != nil {
					return result
				}
			}
		}
		(*params)[paramName] = full
		if result := r.match(node.catchAllChild, nil, params); result != nil {
			return result
		}
		delete(*params, paramName)
	}

	return nil
}

// splitPath splits a URL path into segments without heap-allocating for
// paths with up to 8 segments (covers the vast majority of real routes).
func splitPath(path string) []string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return nil
	}

	// Count segments to size the result.
	n := 1
	for i := range len(path) {
		if path[i] == '/' {
			n++
		}
	}

	// Use a stack-allocated backing array for common cases (≤8 segments).
	var buf [8]string
	var parts []string
	if n <= len(buf) {
		parts = buf[:0]
	} else {
		parts = make([]string, 0, n)
	}

	for {
		idx := strings.IndexByte(path, '/')
		if idx < 0 {
			parts = append(parts, path)
			break
		}
		parts = append(parts, path[:idx])
		path = path[idx+1:]
	}
	return parts
}
