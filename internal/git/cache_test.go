package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchCacheDedupsByKey(t *testing.T) {
	fc := newFetchCache(time.Hour, 8)
	ctx := context.Background()
	var clones int32
	clone := func(dir string) (string, error) {
		atomic.AddInt32(&clones, 1)
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644); err != nil {
			return "", err
		}
		return "commitAAA", nil
	}

	e1, err := fc.acquire(ctx, "repo@commitAAA", clone)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := fc.acquire(ctx, "repo@commitAAA", clone)
	if err != nil {
		t.Fatal(err)
	}
	if e1.dir != e2.dir {
		t.Errorf("cache returned different dirs for the same key: %q vs %q", e1.dir, e2.dir)
	}
	if got := atomic.LoadInt32(&clones); got != 1 {
		t.Errorf("expected exactly 1 clone for a shared key, got %d", got)
	}
	if e1.commit != "commitAAA" {
		t.Errorf("commit = %q", e1.commit)
	}

	// Both references released; the worktree must still exist (idle, within TTL).
	fc.release(e1)
	fc.release(e2)
	if _, err := os.Stat(filepath.Join(e1.dir, "marker")); err != nil {
		t.Errorf("idle worktree removed prematurely: %v", err)
	}

	// Re-acquire within TTL reuses the same worktree without a new clone.
	e3, err := fc.acquire(ctx, "repo@commitAAA", clone)
	if err != nil {
		t.Fatal(err)
	}
	if e3.dir != e1.dir || atomic.LoadInt32(&clones) != 1 {
		t.Errorf("re-acquire did not reuse the cached worktree (clones=%d)", atomic.LoadInt32(&clones))
	}
	fc.release(e3)
}

func TestFetchCacheConcurrentSingleFlight(t *testing.T) {
	fc := newFetchCache(time.Hour, 8)
	ctx := context.Background()
	var clones int32
	release := make(chan struct{})
	clone := func(dir string) (string, error) {
		atomic.AddInt32(&clones, 1)
		<-release // hold the first clone so concurrent callers must wait on it
		return "c", nil
	}
	var wg sync.WaitGroup
	dirs := make([]string, 5)
	for i := range dirs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := fc.acquire(ctx, "k", clone)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			dirs[i] = e.dir
			fc.release(e)
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if atomic.LoadInt32(&clones) != 1 {
		t.Errorf("expected single-flight to clone once, got %d", atomic.LoadInt32(&clones))
	}
	for i := 1; i < len(dirs); i++ {
		if dirs[i] != dirs[0] {
			t.Errorf("concurrent callers got different dirs: %q vs %q", dirs[i], dirs[0])
		}
	}
}

func TestFetchCacheTTLEviction(t *testing.T) {
	fc := newFetchCache(time.Millisecond, 8)
	ctx := context.Background()
	clone := func(dir string) (string, error) { return "c", nil }

	a, err := fc.acquire(ctx, "A", clone)
	if err != nil {
		t.Fatal(err)
	}
	dirA := a.dir
	fc.release(a)
	time.Sleep(10 * time.Millisecond)

	// A miss triggers eviction of the idle, expired entry A.
	b, err := fc.acquire(ctx, "B", clone)
	if err != nil {
		t.Fatal(err)
	}
	fc.release(b)
	if _, err := os.Stat(dirA); !os.IsNotExist(err) {
		t.Errorf("expired idle worktree not removed: stat err = %v", err)
	}
}

func TestFetchCacheCloneFailureCleansUp(t *testing.T) {
	fc := newFetchCache(time.Hour, 8)
	ctx := context.Background()
	var createdDir string
	clone := func(dir string) (string, error) {
		createdDir = dir
		return "", errors.New("boom")
	}
	_, err := fc.acquire(ctx, "bad", clone)
	if err == nil {
		t.Fatal("expected clone failure to propagate")
	}
	if _, ok := fc.entries["bad"]; ok {
		t.Errorf("failed entry left in cache map")
	}
	if _, statErr := os.Stat(createdDir); !os.IsNotExist(statErr) {
		t.Errorf("failed clone dir not cleaned up: %v", statErr)
	}
}

func TestFetchCacheMaxEntriesEviction(t *testing.T) {
	fc := newFetchCache(time.Hour, 2)
	ctx := context.Background()
	clone := func(dir string) (string, error) { return "c", nil }
	var dirs []string
	for _, k := range []string{"a", "b", "c"} {
		e, err := fc.acquire(ctx, k, clone)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, e.dir)
		fc.release(e)
	}
	if len(fc.entries) > 2 {
		t.Errorf("cache exceeded max entries: %d", len(fc.entries))
	}
	// The oldest idle entry ("a") should have been evicted from disk.
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Errorf("LRU entry not evicted: %v", err)
	}
}
