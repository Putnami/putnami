package machineproc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// watchMask is what a directory's watch reports: every change to an entry's
// name, content or attributes, and the directory's own removal or move.
const watchMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO |
	unix.IN_MODIFY | unix.IN_ATTRIB | unix.IN_CLOSE_WRITE | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF |
	unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW | unix.IN_EXCL_UNLINK

// skippedDirs are the directories the default walk never enters (ADR 0113
// §1): no input is selected below them, so they are not watched.
var skippedDirs = map[string]bool{"node_modules": true, ".git": true, ".putnami": true, "out": true, "dist": true, "vendor": true}

// cookieDir is where a sync point writes its cookie: watched, not walked.
const cookieDir = ".putnami"

// errSyncTimeout is a sync point the watcher did not reach in time.
var errSyncTimeout = errors.New("machine: the watcher did not reach the sync point in time")

type watchedDir struct {
	path string
	root string
}

// Watcher watches worktrees with one inotify instance and keeps a journal per
// worktree (CLI ADR 0116 §7). It never decides: a path it cannot vouch for is
// broken in the journal, and its reader takes the stat.
type Watcher struct {
	file *os.File
	// fd is file's descriptor, kept apart: File.Fd would set it blocking,
	// and Close could then no longer end the reader.
	fd int

	mu        sync.Mutex
	dirs      map[int]watchedDir   // watch descriptor → directory
	wds       map[string]int       // directory → watch descriptor
	trees     map[string]*journal  // worktree root → journal
	unwatched map[string][]string  // worktree root → directories it could not watch
	cookies   map[string]chan bool // cookie path → its waiter
	budget    int
	disabled  bool
	cookieN   int

	// gate, when set, holds the reader before its next read until it is
	// closed: a test lets the kernel's queue overflow with it.
	gate atomic.Pointer[chan struct{}]
	done chan struct{}
}

// NewWatcher starts a watcher that adds at most budget watches; a budget of 0
// is half of the user's max_user_watches.
func NewWatcher(budget int) (*Watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	if budget <= 0 {
		budget = defaultWatchBudget()
	}
	w := &Watcher{
		file: os.NewFile(uintptr(fd), "inotify"), fd: fd, dirs: map[int]watchedDir{}, wds: map[string]int{},
		trees: map[string]*journal{}, unwatched: map[string][]string{}, cookies: map[string]chan bool{},
		budget: budget, done: make(chan struct{}),
	}
	go w.read()
	return w, nil
}

// defaultWatchBudget is half of the user's inotify watch limit: the rest stays
// for editors and other tools (CLI ADR 0116 §7).
func defaultWatchBudget() int {
	data, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 8192
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n <= 0 {
		return 8192
	}
	return n / 2
}

// Close stops the watcher.
func (w *Watcher) Close() error {
	err := w.file.Close()
	<-w.done
	return err
}

// Watch starts watching the worktree at root. A directory the watcher cannot
// watch stays broken in every journal read.
func (w *Watcher) Watch(root string) error {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("machine: %s is not absolute", root)
	}
	w.mu.Lock()
	if _, ok := w.trees[root]; !ok {
		w.trees[root] = newJournal(root)
	}
	w.mu.Unlock()
	w.watchTree(root, root)
	return nil
}

// watchTree adds watches on dir and every directory below it the walk
// enters; the cookie directory of root is watched without its content.
func (w *Watcher) watchTree(root, dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && skippedDirs[d.Name()] {
			if filepath.Dir(path) == root && d.Name() == cookieDir {
				w.add(root, path)
			}
			return filepath.SkipDir
		}
		if !w.add(root, path) {
			return filepath.SkipDir
		}
		return nil
	})
}

// add watches dir, or records it unwatched when the budget or the kernel
// refuses: everything below it is then taken by stat.
func (w *Watcher) add(root, dir string) bool {
	w.mu.Lock()
	over := len(w.wds) >= w.budget
	w.mu.Unlock()
	var wd int
	err := error(unix.ENOSPC)
	if !over {
		wd, err = unix.InotifyAddWatch(w.fd, dir, watchMask)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.unwatched[root] = append(w.unwatched[root], dir)
		if j := w.trees[root]; j != nil {
			j.record(dir, true)
		}
		return false
	}
	w.dirs[wd] = watchedDir{path: dir, root: root}
	w.wds[dir] = wd
	return true
}

// Since is the journal of the worktree at root from seq.
func (w *Watcher) Since(root string, seq uint64) Journal {
	root = filepath.Clean(root)
	w.mu.Lock()
	j := w.trees[root]
	disabled := w.disabled
	unwatched := append([]string(nil), w.unwatched[root]...)
	w.mu.Unlock()
	if j == nil || disabled {
		var cur uint64
		if j != nil {
			cur = j.current()
		}
		return Journal{Seq: cur, Broken: []string{root}}
	}
	out := j.since(seq)
	if len(unwatched) > 0 {
		set := map[string]bool{}
		for _, b := range out.Broken {
			set[b] = true
		}
		for _, u := range unwatched {
			set[u] = true
		}
		out.Broken = sortedKeys(set)
	}
	return out
}

// Sync returns once every write that completed before it was called is in
// the journal of root: it writes a cookie in root's .putnami and waits for its
// event. A sync point not reached within timeout breaks the whole worktree.
func (w *Watcher) Sync(root string, timeout time.Duration) error {
	root = filepath.Clean(root)
	dir := filepath.Join(root, cookieDir)
	w.mu.Lock()
	_, watched := w.wds[dir]
	j := w.trees[root]
	w.cookieN++
	cookie := filepath.Join(dir, fmt.Sprintf(".machine-cookie-%d-%d", os.Getpid(), w.cookieN))
	seen := make(chan bool, 1)
	w.cookies[cookie] = seen
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.cookies, cookie)
		w.mu.Unlock()
		_ = os.Remove(cookie)
	}()
	fail := func(err error) error {
		if j != nil {
			j.record(root, true)
		}
		return err
	}
	if j == nil || !watched {
		return fail(fmt.Errorf("machine: %s is not watched", dir))
	}
	if err := os.WriteFile(cookie, nil, 0o600); err != nil {
		return fail(err)
	}
	select {
	case <-seen:
		return nil
	case <-time.After(timeout):
		return fail(errSyncTimeout)
	}
}

// FileSig is what a stat says of a path: the fields ADR 0009 compares.
type FileSig struct {
	Dev, Ino, Mode, Uid, Gid uint64
	Size, ModNano, CtimeNano int64
}

// SigOf is the signature of the path's lstat, false when it cannot be taken.
func SigOf(path string) (FileSig, bool) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return FileSig{}, false
	}
	return FileSig{
		Dev: st.Dev, Ino: st.Ino, Mode: uint64(st.Mode), Uid: uint64(st.Uid), Gid: uint64(st.Gid),
		Size: st.Size, ModNano: st.Mtim.Nano(), CtimeNano: st.Ctim.Nano(),
	}, true
}

// SelfCheck takes the stat of the paths of recorded that the journal of root
// from seq says held, and compares it with the signature recorded at seq. A
// difference means the watcher missed a change: every worktree breaks, and
// the watcher answers every later read as broken (CLI ADR 0116 §7).
func (w *Watcher) SelfCheck(root string, seq uint64, recorded map[string]FileSig) bool {
	j := w.Since(root, seq)
	for path, sig := range recorded {
		if !j.Holds(path) {
			continue
		}
		now, ok := SigOf(path)
		if ok && now == sig {
			continue
		}
		w.mu.Lock()
		w.disabled = true
		trees := make([]*journal, 0, len(w.trees))
		for _, t := range w.trees {
			trees = append(trees, t)
		}
		w.mu.Unlock()
		for _, t := range trees {
			t.record(t.root, true)
		}
		return false
	}
	return true
}

// Disabled reports whether a self-check found a missed change.
func (w *Watcher) Disabled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.disabled
}

// Watches is the number of watches the watcher holds.
func (w *Watcher) Watches() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.wds)
}

// read drains the inotify queue until the watcher closes.
func (w *Watcher) read() {
	defer close(w.done)
	buf := make([]byte, 64<<10)
	for {
		if gate := w.gate.Load(); gate != nil {
			<-*gate
		}
		n, err := w.file.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) || errors.Is(err, fs.ErrClosed) {
				return
			}
			if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) {
				continue
			}
			// An unreadable queue loses events: every worktree breaks.
			w.breakAll()
			return
		}
		w.handle(buf[:n])
	}
}

func (w *Watcher) breakAll() {
	w.mu.Lock()
	trees := make([]*journal, 0, len(w.trees))
	for _, t := range w.trees {
		trees = append(trees, t)
	}
	w.mu.Unlock()
	for _, t := range trees {
		t.record(t.root, true)
	}
}

// handle records the events in buf.
func (w *Watcher) handle(buf []byte) {
	for off := 0; off+unix.SizeofInotifyEvent <= len(buf); {
		ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
		nameLen := int(ev.Len)
		name := ""
		if nameLen > 0 && off+unix.SizeofInotifyEvent+nameLen <= len(buf) {
			name = strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+nameLen]), "\x00")
		}
		off += unix.SizeofInotifyEvent + nameLen
		w.event(int(ev.Wd), ev.Mask, name)
	}
}

func (w *Watcher) event(wd int, mask uint32, name string) {
	if mask&unix.IN_Q_OVERFLOW != 0 {
		w.breakAll()
		return
	}
	w.mu.Lock()
	dir, ok := w.dirs[wd]
	j := w.trees[dir.root]
	w.mu.Unlock()
	if !ok || j == nil {
		return
	}
	path := dir.path
	if name != "" {
		path = filepath.Join(dir.path, name)
	}
	if mask&(unix.IN_IGNORED|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0 {
		// The directory moved, went away or its file system was unmounted:
		// its watch and those below it name paths that no longer hold what
		// they watched. The subtree stays broken until a directory is
		// created or moved in at its path again.
		w.forgetTree(dir.root, dir.path)
		j.record(dir.path, true)
		return
	}
	if name != "" && filepath.Dir(path) == filepath.Join(dir.root, cookieDir) {
		w.mu.Lock()
		seen := w.cookies[path]
		w.mu.Unlock()
		if seen != nil && mask&unix.IN_CREATE != 0 {
			seen <- true
		}
		return
	}
	j.record(path, false)
	if mask&unix.IN_ISDIR != 0 && mask&(unix.IN_DELETE|unix.IN_MOVED_FROM) != 0 {
		// A directory left this one: its watches name paths that no longer
		// hold it. Break it now, before a watch of its new path, which
		// inotify gives the same descriptor, can be confused with them.
		w.forgetTree(dir.root, path)
		j.record(path, true)
		return
	}
	if mask&unix.IN_ISDIR != 0 && mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 && !skippedDirs[name] {
		// A new directory: what was written in it before its watch was added
		// is unseen, so its subtree breaks once, then is watched.
		j.record(path, true)
		w.mu.Lock()
		w.unwatched[dir.root] = outside(w.unwatched[dir.root], path)
		w.mu.Unlock()
		w.watchTree(dir.root, path)
	}
}

// forgetTree drops the watches of dir and of every directory below it, and
// keeps dir unwatched.
func (w *Watcher) forgetTree(root, dir string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for wd, d := range w.dirs {
		if within(d.path, dir) {
			delete(w.dirs, wd)
			delete(w.wds, d.path)
			_, _ = unix.InotifyRmWatch(w.fd, uint32(wd))
		}
	}
	w.unwatched[root] = append(outside(w.unwatched[root], dir), dir)
}

// outside keeps the paths that do not lie at or below dir.
func outside(paths []string, dir string) []string {
	var out []string
	for _, p := range paths {
		if !within(p, dir) {
			out = append(out, p)
		}
	}
	return out
}
