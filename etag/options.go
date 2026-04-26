package etag

import (
	"cmp"
	"log/slog"
)

// Option configures a Transport. The interface form (rather than a bare
// `func(*config)`) lets us evolve the API without a breaking change: new
// option shapes can introduce richer concrete types that still satisfy
// Option.
type Option interface{ apply(*config) }

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

type config struct {
	cache         Cache
	callerCache   bool // true if cache was supplied via WithCache (vs. default)
	keyScope      string
	maxBodyBytes  int
	maxCacheCap   int64
	logger        *slog.Logger
	driftCallback func(DriftEvent)
}

// defaultPerEntryCap is the per-entry response body cap when WithMaxBodyBytes
// is not set. 4 MiB comfortably exceeds GitHub REST JSON payloads while
// capping worst-case allocation.
const defaultPerEntryCap = 4 << 20

// bodyBufferFloor is the minimum initial allocation when we have no
// Content-Length hint. Avoids thrashing on tiny responses.
const bodyBufferFloor = 8 << 10

// defaultMaxCacheBytes is the total in-memory byte budget enforced by
// NewLRUCache when WithMaxCacheBytes is not set. 256 MiB.
const defaultMaxCacheBytes = 256 << 20

func newConfig(opts []Option) *config {
	c := &config{
		maxBodyBytes: defaultPerEntryCap,
		maxCacheCap:  defaultMaxCacheBytes,
	}
	for _, o := range opts {
		o.apply(c)
	}
	// Silent by default: a nil logger becomes a discard logger so call sites
	// can write unconditionally without nil-checks.
	c.logger = cmp.Or(c.logger, slog.New(slog.DiscardHandler))
	return c
}

// WithCache supplies the storage backend. When omitted, the Transport uses
// NewLRUCache(4096). If a Cache is supplied, WithKeyScope is REQUIRED to
// prevent cross-tenant body leaks (different callers writing to the same URL
// under different auth).
//
// Passing nil marks the option as caller-set but with a nil value; NewTransport
// rejects this with ErrNilCache. If you want the default LRU, omit the option
// entirely.
func WithCache(c Cache) Option {
	return optionFunc(func(cfg *config) {
		cfg.cache = c
		cfg.callerCache = true
	})
}

// WithKeyScope namespaces cache entries. The scope string is hashed with
// SHA256 into the cache key, so two callers sharing a Cache with different
// scopes never collide. Scopes are treated as opaque: do NOT embed secrets
// in the scope value.
func WithKeyScope(scope string) Option {
	return optionFunc(func(cfg *config) { cfg.keyScope = scope })
}

// WithMaxBodyBytes caps the per-entry body size the transport will buffer
// and cache. Responses exceeding this cap pass through to the caller
// uncached. Values below the 8 KiB internal floor are accepted but the
// initial allocation stays at the floor; the caller-supplied value is the
// cap. Default: 4 MiB.
func WithMaxBodyBytes(n int) Option {
	return optionFunc(func(cfg *config) {
		if n > 0 {
			cfg.maxBodyBytes = n
		}
	})
}

// WithMaxCacheBytes caps the total byte budget held by the default
// NewLRUCache. Exceeding the budget evicts oldest entries. Has no effect
// when a caller-supplied Cache is used; custom backends enforce their own
// budgets. Default: 256 MiB.
func WithMaxCacheBytes(n int64) Option {
	return optionFunc(func(cfg *config) {
		if n > 0 {
			cfg.maxCacheCap = n
		}
	})
}

// WithLogger supplies the slog.Logger the transport emits events to.
// Pass nil (or omit the option) to silence the package; a nil logger is
// replaced with slog.New(slog.DiscardHandler) at construction.
func WithLogger(l *slog.Logger) Option {
	return optionFunc(func(cfg *config) { cfg.logger = l })
}

// WithDriftDetected registers a callback fired on each drift state
// transition: Recovered=false on detection, Recovered=true on probe-back
// recovery. Without this option the transport still detects drift and
// degrades transparently; only the user-visible signal is omitted.
//
// The callback runs synchronously inside RoundTrip; keep it fast and
// non-blocking. Panics are contained by a recover guard so a misbehaving
// callback cannot crash the transport.
func WithDriftDetected(cb func(DriftEvent)) Option {
	return optionFunc(func(cfg *config) {
		if cb != nil {
			cfg.driftCallback = cb
		}
	})
}
