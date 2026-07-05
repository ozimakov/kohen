package git

import (
	"context"
	"os"
	"sync"
	"time"
)

// DefaultFetchCacheTTL is how long an idle cached worktree is retained before
// eviction. Reconciles on the poll interval re-fetch the same commit repeatedly;
// caching by repo+commit collapses those into a single clone (SPEC T2/T10).
const DefaultFetchCacheTTL = 10 * time.Minute

// DefaultFetchCacheMaxEntries bounds cache disk/inode footprint (SPEC T6).
const DefaultFetchCacheMaxEntries = 128

// fetchCache deduplicates clones by repo+commit. Entries are reference-counted
// so an in-use worktree is never removed; idle entries are evicted by TTL and an
// LRU cap. It is safe for concurrent use and single-flights concurrent misses
// for the same key.
type fetchCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	dir    string
	commit string
	ready  chan struct{} // closed when the clone completes (or fails)
	err    error

	// guarded by fetchCache.mu
	refs    int
	lastUse time.Time
	dead    bool // removed from the map; dir is deleted once refs hits 0
}

func newFetchCache(ttl time.Duration, max int) *fetchCache {
	if ttl <= 0 {
		ttl = DefaultFetchCacheTTL
	}
	if max <= 0 {
		max = DefaultFetchCacheMaxEntries
	}
	return &fetchCache{ttl: ttl, max: max, entries: map[string]*cacheEntry{}}
}

// acquire returns a ready worktree for key, cloning via clone on a miss. The
// returned entry is reference-counted; the caller MUST call release exactly once
// when done reading it. clone receives a fresh empty directory and returns the
// resolved commit SHA.
func (fc *fetchCache) acquire(ctx context.Context, key string, clone func(dir string) (string, error)) (*cacheEntry, error) {
	fc.mu.Lock()
	if e, ok := fc.entries[key]; ok {
		e.refs++
		fc.mu.Unlock()
		select {
		case <-e.ready:
		case <-ctx.Done():
			fc.release(e)
			return nil, ctx.Err()
		}
		if e.err != nil {
			fc.release(e)
			return nil, e.err
		}
		return e, nil
	}

	dir, err := os.MkdirTemp("", "kohen-git-*")
	if err != nil {
		fc.mu.Unlock()
		return nil, wrapError(ReasonFetchFailed, err, "creating cache directory")
	}
	e := &cacheEntry{dir: dir, ready: make(chan struct{}), refs: 1, lastUse: time.Now()}
	fc.entries[key] = e
	fc.mu.Unlock()

	commit, cerr := clone(dir)

	fc.mu.Lock()
	e.commit = commit
	e.err = cerr
	var toRemove []string
	if cerr != nil {
		delete(fc.entries, key)
		e.dead = true
	} else {
		toRemove = fc.evictLocked()
	}
	fc.mu.Unlock()
	close(e.ready)
	for _, d := range toRemove {
		_ = os.RemoveAll(d)
	}
	if cerr != nil {
		fc.release(e)
		return nil, cerr
	}
	return e, nil
}

// release drops one reference, removing the worktree if the entry is dead and
// no longer referenced.
func (fc *fetchCache) release(e *cacheEntry) {
	fc.mu.Lock()
	e.refs--
	e.lastUse = time.Now()
	remove := e.refs <= 0 && e.dead
	dir := e.dir
	fc.mu.Unlock()
	if remove {
		_ = os.RemoveAll(dir)
	}
}

// evictLocked marks idle entries past the TTL, or beyond the LRU cap, as dead
// and returns their directories for removal. Caller holds fc.mu.
func (fc *fetchCache) evictLocked() []string {
	now := time.Now()
	var removed []string
	for k, e := range fc.entries {
		if e.refs <= 0 && now.Sub(e.lastUse) > fc.ttl {
			e.dead = true
			delete(fc.entries, k)
			removed = append(removed, e.dir)
		}
	}
	for len(fc.entries) > fc.max {
		var oldestKey string
		var oldest *cacheEntry
		for k, e := range fc.entries {
			if e.refs > 0 {
				continue
			}
			if oldest == nil || e.lastUse.Before(oldest.lastUse) {
				oldestKey, oldest = k, e
			}
		}
		if oldest == nil {
			break // everything is in use
		}
		oldest.dead = true
		delete(fc.entries, oldestKey)
		removed = append(removed, oldest.dir)
	}
	return removed
}
