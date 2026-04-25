package etag

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestETag_Cache_BasicContract(t *testing.T) {
	ctx := context.Background()
	c := NewLRUCache(4)
	e := Entry{ETag: `"x"`, Body: []byte("hello"), Headers: http.Header{"Content-Type": {"text/plain"}}}

	if _, ok, err := c.Get(ctx, "k"); err != nil || ok {
		t.Fatalf("empty cache returned hit or error: ok=%v err=%v", ok, err)
	}
	if err := c.Add(ctx, "k", e); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected hit after Add")
	}
	if got.ETag != `"x"` || string(got.Body) != "hello" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.StoredAt.IsZero() {
		t.Fatal("default cache should populate StoredAt")
	}
	if err := c.Remove(ctx, "k"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("Remove did not evict")
	}
	// Remove-missing must be idempotent.
	if err := c.Remove(ctx, "nonexistent"); err != nil {
		t.Fatalf("Remove on missing key should be idempotent: %v", err)
	}
}

func mustAdd(t *testing.T, c Cache, key string, e Entry) {
	t.Helper()
	if err := c.Add(context.Background(), key, e); err != nil {
		t.Fatalf("Cache.Add(%q): %v", key, err)
	}
}

func mustGet(t *testing.T, c Cache, key string) (Entry, bool) {
	t.Helper()
	e, ok, err := c.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Cache.Get(%q): %v", key, err)
	}
	return e, ok
}

func TestETag_Cache_LRUEvictionByCount(t *testing.T) {
	c := NewLRUCache(2)
	mustAdd(t, c, "a", Entry{Body: []byte("1")})
	mustAdd(t, c, "b", Entry{Body: []byte("2")})
	mustAdd(t, c, "c", Entry{Body: []byte("3")})
	if _, ok := mustGet(t, c, "a"); ok {
		t.Fatal("a should have been evicted under count pressure")
	}
	if _, ok := mustGet(t, c, "b"); !ok {
		t.Fatal("b should still be present")
	}
	if _, ok := mustGet(t, c, "c"); !ok {
		t.Fatal("c should still be present")
	}
}

func TestETag_Cache_CompositeByteCapEnforced(t *testing.T) {
	c := NewLRUCache(1024).(*lruCache)
	c.setByteCap(500) // bytes
	mustAdd(t, c, "a", Entry{Body: make([]byte, 200)})
	mustAdd(t, c, "b", Entry{Body: make([]byte, 200)})
	mustAdd(t, c, "c", Entry{Body: make([]byte, 200)}) // total would be 600 > 500; a evicted
	if _, ok := mustGet(t, c, "a"); ok {
		t.Fatal("byte-budget should have evicted a")
	}
	if _, ok := mustGet(t, c, "b"); !ok {
		t.Fatal("b should still be present")
	}
	if _, ok := mustGet(t, c, "c"); !ok {
		t.Fatal("c should still be present")
	}
	// Adding a very large entry should evict everything in order to fit.
	mustAdd(t, c, "big", Entry{Body: make([]byte, 450)})
	if _, ok := mustGet(t, c, "b"); ok {
		t.Fatal("b should be evicted to make room for big")
	}
	if _, ok := mustGet(t, c, "big"); !ok {
		t.Fatal("big should be present")
	}
}

func TestETag_Cache_OverwriteKeyAccountsBytes(t *testing.T) {
	c := NewLRUCache(1024).(*lruCache)
	c.setByteCap(500)
	mustAdd(t, c, "k", Entry{Body: make([]byte, 400)})
	mustAdd(t, c, "k", Entry{Body: make([]byte, 100)}) // overwrite, total should now be 100
	// Now add another 400-byte entry; should fit without evicting k.
	mustAdd(t, c, "k2", Entry{Body: make([]byte, 400)})
	if _, ok := mustGet(t, c, "k"); !ok {
		t.Fatal("overwrite accounting was wrong; k evicted unexpectedly")
	}
	if _, ok := mustGet(t, c, "k2"); !ok {
		t.Fatal("k2 should fit")
	}
}

func TestETag_Cache_EntrySurvivesGracePeriod(t *testing.T) {
	// Guards against a future TTL re-enable: ttl=0 should mean no eviction
	// regardless of how much time passes.
	origNow := nowFn
	t.Cleanup(func() { nowFn = origNow })
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn = func() time.Time { return fixed }

	c := NewLRUCache(4)
	mustAdd(t, c, "k", Entry{Body: []byte("v")})
	nowFn = func() time.Time { return fixed.Add(6 * 30 * 24 * time.Hour) }
	if _, ok := mustGet(t, c, "k"); !ok {
		t.Fatal("entry should survive 6 months with ttl=0 (no TTL eviction)")
	}
}

func TestETag_Cache_ConcurrentAccessSafe(t *testing.T) {
	ctx := context.Background()
	c := NewLRUCache(1024)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range 100 {
				key := "k"
				if err := c.Add(ctx, key, Entry{Body: []byte{byte(id & 0xff), byte(j & 0xff)}}); err != nil {
					t.Errorf("Add under concurrency: %v", err)
					return
				}
				if _, _, err := c.Get(ctx, key); err != nil {
					t.Errorf("Get under concurrency: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
