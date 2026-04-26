package etag

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// Entry is what the transport caches: the server's ETag, the full body as
// last read, and the response headers. StoredAt is populated by
// NewLRUCache; custom backends may populate it too, or leave it zero if
// freshness tracking isn't useful.
//
// Use named-field struct literals so future field additions remain
// non-breaking.
type Entry struct {
	ETag     string
	Body     []byte
	Headers  http.Header
	StoredAt time.Time
}

// Cache is the minimal interface any backend must implement. Implementors
// must be safe for concurrent use. Methods take a context so network-backed
// backends can honour deadlines and cancellation; the default in-memory LRU
// ignores it. Add overwrites; on error the transport treats the response
// as uncached. Get returning (zero, false, err) is treated as a miss.
// Remove is idempotent.
type Cache interface {
	Get(ctx context.Context, key string) (Entry, bool, error)
	Add(ctx context.Context, key string, e Entry) error
	Remove(ctx context.Context, key string) error
}

// defaultLRUSize is used when NewLRUCache is called with size <= 0.
const defaultLRUSize = 4096

// nowFn is swappable in tests to pin time-dependent behaviour without
// wall-clock sleeps. Not part of the public API.
var nowFn = time.Now

// NewLRUCache returns the default in-process Cache: a bounded LRU with no
// TTL-based eviction. size <= 0 uses 4096. The returned Cache is safe for
// concurrent use and spawns no background goroutines: hashicorp/golang-lru/v2
// starts a reaper only when ttl > 0; we pass 0 to turn that off.
func NewLRUCache(size int) Cache {
	if size <= 0 {
		size = defaultLRUSize
	}
	return &lruCache{
		lru: expirable.NewLRU[string, Entry](size, nil, 0),
	}
}

type lruCache struct {
	mu  sync.Mutex // guards byteTotal; lru itself is already thread-safe
	lru *expirable.LRU[string, Entry]

	// Composite byte-budget accounting. byteCap == 0 means no budget.
	byteCap   int64
	byteTotal int64
}

func (c *lruCache) Get(_ context.Context, key string) (Entry, bool, error) {
	e, ok := c.lru.Get(key)
	return e, ok, nil
}

func (c *lruCache) Add(_ context.Context, key string, e Entry) error {
	if e.StoredAt.IsZero() {
		e.StoredAt = nowFn()
	}
	entrySize := int64(len(e.Body))

	c.mu.Lock()
	defer c.mu.Unlock()

	// If an older entry for this key existed, its size came out of the budget.
	if old, ok := c.lru.Peek(key); ok {
		c.byteTotal -= int64(len(old.Body))
		if c.byteTotal < 0 {
			c.byteTotal = 0
		}
	}

	// If a byte budget is configured, evict oldest entries until the new
	// entry fits or the LRU is empty.
	if c.byteCap > 0 {
		for c.byteTotal+entrySize > c.byteCap {
			k, victim, ok := c.lru.GetOldest()
			if !ok {
				break
			}
			if !c.lru.Remove(k) {
				break
			}
			c.byteTotal -= int64(len(victim.Body))
			if c.byteTotal < 0 {
				c.byteTotal = 0
			}
		}
	}

	c.lru.Add(key, e)
	c.byteTotal += entrySize
	return nil
}

func (c *lruCache) Remove(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if old, ok := c.lru.Peek(key); ok {
		c.byteTotal -= int64(len(old.Body))
		if c.byteTotal < 0 {
			c.byteTotal = 0
		}
	}
	c.lru.Remove(key)
	return nil
}

// setByteCap configures the composite byte budget on a defaultLRU. Called
// only from within the etag package via WithMaxCacheBytes when the Cache
// was also built by NewLRUCache. Custom Cache implementations enforce their
// own budgets; this option is a no-op for them.
func (c *lruCache) setByteCap(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byteCap = n
}
