package etag

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ghServer simulates GitHub's server-side ETag behaviour: it hashes the
// current request headers + body with our algorithm, returns that as the
// ETag on 200 responses, and checks incoming If-None-Match against the
// same hash to decide 304 vs 200.
func ghServer(t *testing.T, body []byte) (*httptest.Server, *int64) {
	t.Helper()
	var reqs int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reqs, 1)
		w.Header().Set("Content-Type", "application/json")
		expected := ComputeExpectedETag(r.Header, nil, body)
		quoted := `"` + expected + `"`
		if got := r.Header.Get("If-None-Match"); got != "" && NormaliseETag(got) == expected {
			w.Header().Set("ETag", quoted)
			w.Header().Set("X-RateLimit-Remaining", "4999")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", quoted)
		w.Header().Set("X-RateLimit-Remaining", "5000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s, &reqs
}

func newTestTransport(t *testing.T, opts ...Option) http.RoundTripper {
	t.Helper()
	rt, err := NewTransport(nil, opts...)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	return rt
}

func newTestClient(t *testing.T, opts ...Option) *http.Client {
	return &http.Client{Transport: newTestTransport(t, opts...)}
}

// getResult is a closed-body summary of an HTTP response, suitable for
// tests that only need to inspect status/headers. Returning a struct
// (rather than *http.Response) makes it clear to both readers and
// linters that the body has been drained and closed.
type getResult struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// doGet issues a GET, reads and closes the body, and returns the summary.
// Fails the test on any transport or I/O error.
func doGet(t *testing.T, c *http.Client, url string) getResult {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("GET %s read body: %v", url, readErr)
	}
	if closeErr != nil {
		t.Fatalf("GET %s Body.Close: %v", url, closeErr)
	}
	return getResult{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       body,
	}
}

func TestETag_ColdMissStoresEtagThenSendsIfNoneMatch(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	s, _ := ghServer(t, body)
	c := newTestClient(t)

	// Cold miss.
	resp, err := c.Get(s.URL + "/users/octocat")
	if err != nil {
		t.Fatalf("get 1: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b1, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(b1, body) {
		t.Fatalf("cold-miss body mismatch: got %q want %q", b1, body)
	}

	// Warm request: server sees If-None-Match and returns 304, transport
	// replays the cached body as a synthesised 200.
	resp2, err := c.Get(s.URL + "/users/octocat")
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != 200 {
		t.Fatalf("warm hit should synthesise 200; got %d", resp2.StatusCode)
	}
	b2, _ := io.ReadAll(resp2.Body)
	if !bytes.Equal(b2, body) {
		t.Fatalf("warm-hit body mismatch: got %q want %q", b2, body)
	}
}

func TestETag_304ReplayedAs200(t *testing.T) {
	body := []byte("payload")
	s, _ := ghServer(t, body)
	c := newTestClient(t)

	// Cold miss + warm hit.
	for range 2 {
		resp, err := c.Get(s.URL + "/meta")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		_ = resp.Body.Close()
	}
	resp, err := c.Get(s.URL + "/meta")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("want synthesised 200, got %d", resp.StatusCode)
	}
	// Fresh X-RateLimit-Remaining from the 304 must still reach the caller.
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "4999" {
		t.Fatalf("synthesised 200 should carry fresh rate-limit headers from the 304; got %q", got)
	}
}

func TestETag_TokenRotationSurvival(t *testing.T) {
	body := []byte(`{"x":1}`)
	s, reqs := ghServer(t, body)
	c := newTestClient(t)

	req1, _ := http.NewRequest("GET", s.URL+"/users/octocat", nil)
	req1.Header.Set("Authorization", "token AAA")
	r1, err := c.Do(req1)
	if err != nil {
		t.Fatalf("req1: %v", err)
	}
	_ = r1.Body.Close()

	// Rotate token mid-stream. Passive mode would miss on the server side
	// (different auth -> different ETag). Precompute mode recomputes with
	// the CURRENT Authorization and still gets a 304 that we replay.
	req2, _ := http.NewRequest("GET", s.URL+"/users/octocat", nil)
	req2.Header.Set("Authorization", "token BBB")
	r2, err := c.Do(req2)
	if err != nil {
		t.Fatalf("req2: %v", err)
	}
	_ = r2.Body.Close()

	if r2.StatusCode != 200 {
		t.Fatalf("token-rotated warm hit should still replay as 200; got %d", r2.StatusCode)
	}
	if *reqs != 2 {
		t.Fatalf("expected 2 upstream requests; got %d", *reqs)
	}
}

func TestETag_MismatchDetection(t *testing.T) {
	// Server returns a garbage ETag that does not match our recomputed hash.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"garbage-not-our-hash"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hi"))
	}))
	defer s.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	c := newTestClient(t, WithLogger(logger))
	resp, _ := c.Get(s.URL + "/repos/a/b")
	_ = resp.Body.Close()

	out := buf.String()
	if !strings.Contains(out, "etag_mismatch") {
		t.Fatalf("expected etag_mismatch warn; got %q", out)
	}
	// Must NOT log auth or hash prefixes.
	for _, banned := range []string{"auth_len", "ours_prefix", "theirs_prefix"} {
		if strings.Contains(out, banned) {
			t.Errorf("banned field %q appeared in log: %q", banned, out)
		}
	}
}

func TestETag_WeakETag(t *testing.T) {
	body := []byte("weak")
	// Server wraps the expected hash with W/... prefix. We should still
	// validate successfully because NormaliseETag strips it.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := ComputeExpectedETag(r.Header, nil, body)
		w.Header().Set("ETag", `W/"`+expected+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c := newTestClient(t, WithLogger(logger))
	resp, _ := c.Get(s.URL + "/users/octocat")
	_ = resp.Body.Close()
	if strings.Contains(buf.String(), "etag_mismatch") {
		t.Fatalf("weak ETag should validate; got mismatch log: %q", buf.String())
	}
}

func TestETag_OversizeBypass(t *testing.T) {
	// 32KB body with a 4KB cap -> should bypass cache.
	body := bytes.Repeat([]byte("A"), 32*1024)
	s, _ := ghServer(t, body)
	c := newTestClient(t, WithMaxBodyBytes(4*1024))

	resp, _ := c.Get(s.URL + "/x")
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if len(got) != len(body) {
		t.Fatalf("oversize bypass must not truncate body: got %d, want %d", len(got), len(body))
	}

	// Second request should be a cold miss, not a cache hit (oversize was not stored).
	resp2, _ := c.Get(s.URL + "/x")
	got2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if len(got2) != len(body) {
		t.Fatalf("second oversize fetch wrong body length: got %d want %d", len(got2), len(body))
	}
}

func TestETag_NonGetPassesThrough(t *testing.T) {
	var seenGet, seenPost string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			seenGet = r.Header.Get("If-None-Match")
			// Return a cacheable response so subsequent GETs would carry
			// If-None-Match if the transport tried to reuse it.
			expected := ComputeExpectedETag(r.Header, nil, []byte("x"))
			w.Header().Set("ETag", `"`+expected+`"`)
			w.WriteHeader(200)
			if _, err := w.Write([]byte("x")); err != nil {
				t.Errorf("server Write: %v", err)
			}
		case http.MethodPost:
			seenPost = r.Header.Get("If-None-Match")
			w.WriteHeader(200)
		}
	}))
	defer s.Close()
	c := newTestClient(t)
	// Seed the cache with a GET.
	r1, err := c.Get(s.URL + "/x")
	if err != nil {
		t.Fatalf("seeding GET: %v", err)
	}
	if err := r1.Body.Close(); err != nil {
		t.Fatalf("seeding Body.Close: %v", err)
	}
	// POST to the same URL must not carry If-None-Match.
	r2, err := c.Post(s.URL+"/x", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if err := r2.Body.Close(); err != nil {
		t.Fatalf("POST Body.Close: %v", err)
	}
	if seenPost != "" {
		t.Fatalf("POST carried If-None-Match %q; non-GET must pass through", seenPost)
	}
	// Sanity: the seeding GET was itself a cold miss (no If-None-Match).
	if seenGet != "" {
		t.Fatalf("first GET carried If-None-Match %q; cold miss should have none", seenGet)
	}
}

func TestETag_ResponseWithoutEtagIsNotCached(t *testing.T) {
	body := []byte("no-etag")
	var seenIfNoneMatch atomic.Value
	seenIfNoneMatch.Store("")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture whatever If-None-Match the second request carries (if any).
		seenIfNoneMatch.Store(r.Header.Get("If-None-Match"))
		// Deliberately omit ETag on the response.
		w.WriteHeader(200)
		if _, err := w.Write(body); err != nil {
			t.Errorf("server Write: %v", err)
		}
	}))
	defer s.Close()
	c := newTestClient(t)

	// First request: response has no ETag; transport must not cache.
	r1, err := c.Get(s.URL + "/meta")
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	if err := r1.Body.Close(); err != nil {
		t.Fatalf("first Body.Close: %v", err)
	}

	// Second request: if the transport had cached (incorrectly), it would
	// now set If-None-Match on the outbound request.
	r2, err := c.Get(s.URL + "/meta")
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	if err := r2.Body.Close(); err != nil {
		t.Fatalf("second Body.Close: %v", err)
	}
	if got := seenIfNoneMatch.Load().(string); got != "" {
		t.Fatalf("server saw If-None-Match=%q; response without ETag must not be cached", got)
	}
}

func TestETag_LinkHeaderPreservedOn304Replay(t *testing.T) {
	body := []byte(`{"p":1}`)
	link := `<https://api.github.com/x?page=2>; rel="next"`
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := ComputeExpectedETag(r.Header, nil, body)
		w.Header().Set("ETag", `"`+expected+`"`)
		if r.Header.Get("If-None-Match") == "" {
			w.Header().Set("Link", link)
			w.WriteHeader(200)
			_, _ = w.Write(body)
			return
		}
		// 304 deliberately omits Link; we should fall back to the cached one.
		w.WriteHeader(304)
	}))
	defer s.Close()
	c := newTestClient(t)
	_ = doGet(t, c, s.URL+"/x")
	r2 := doGet(t, c, s.URL+"/x")
	if got := r2.Header.Get("Link"); got != link {
		t.Fatalf("304 replay should preserve cached Link header: got %q, want %q", got, link)
	}
}

func TestETag_WriteInvalidation_404(t *testing.T) {
	body := []byte("x")
	var count int32
	var lastIfNoneMatch atomic.Value // string
	lastIfNoneMatch.Store("")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		lastIfNoneMatch.Store(r.Header.Get("If-None-Match"))
		switch n {
		case 1:
			// Seed the cache.
			expected := ComputeExpectedETag(r.Header, nil, body)
			w.Header().Set("ETag", `"`+expected+`"`)
			w.WriteHeader(200)
			if _, err := w.Write(body); err != nil {
				t.Errorf("server Write: %v", err)
			}
		case 2:
			// 404 should invalidate the cached entry.
			w.WriteHeader(404)
		default:
			// Post-invalidation request should NOT carry If-None-Match.
			w.WriteHeader(500)
		}
	}))
	defer s.Close()

	c := newTestClient(t)

	// Seed.
	r1, err := c.Get(s.URL + "/users/octocat")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := r1.Body.Close(); err != nil {
		t.Fatalf("seed body close: %v", err)
	}
	// 404 triggers invalidation.
	r2, err := c.Get(s.URL + "/users/octocat")
	if err != nil {
		t.Fatalf("404 request: %v", err)
	}
	if err := r2.Body.Close(); err != nil {
		t.Fatalf("404 body close: %v", err)
	}
	// Post-invalidation: should be a cold miss (no If-None-Match).
	r3, err := c.Get(s.URL + "/users/octocat")
	if err != nil {
		t.Fatalf("post-invalidation: %v", err)
	}
	if err := r3.Body.Close(); err != nil {
		t.Fatalf("post-invalidation body close: %v", err)
	}
	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("expected 3 upstream requests after invalidation; got %d", got)
	}
	if got := lastIfNoneMatch.Load().(string); got != "" {
		t.Fatalf("post-invalidation request carried If-None-Match=%q; cache should have been evicted on 404", got)
	}
}

func TestETag_ConcurrentAccessSafe(t *testing.T) {
	body := []byte("x")
	s, _ := ghServer(t, body)
	c := newTestClient(t)
	// Warm the cache first.
	_ = doGet(t, c, s.URL+"/users/octocat")

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 20 {
				r, err := c.Get(s.URL + "/users/octocat")
				if err != nil {
					t.Errorf("get: %v", err)
					return
				}
				_, _ = io.ReadAll(r.Body)
				_ = r.Body.Close()
			}
		})
	}
	wg.Wait()
}

func TestETag_MultiTenantIsolation(t *testing.T) {
	bodyA := []byte(`{"tenant":"A"}`)
	bodyB := []byte(`{"tenant":"B"}`)
	// Server returns different bodies based on Authorization header.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := bodyA
		if strings.Contains(r.Header.Get("Authorization"), "B") {
			body = bodyB
		}
		expected := ComputeExpectedETag(r.Header, nil, body)
		if NormaliseETag(r.Header.Get("If-None-Match")) == expected {
			w.Header().Set("ETag", `"`+expected+`"`)
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"`+expected+`"`)
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	sharedCache := NewLRUCache(16)
	rtA, err := NewTransport(nil, WithCache(sharedCache), WithKeyScope("tenant-A"))
	if err != nil {
		t.Fatal(err)
	}
	rtB, err := NewTransport(nil, WithCache(sharedCache), WithKeyScope("tenant-B"))
	if err != nil {
		t.Fatal(err)
	}

	cA := &http.Client{Transport: rtA}
	cB := &http.Client{Transport: rtB}

	do := func(c *http.Client, tok string) []byte {
		req, _ := http.NewRequest("GET", s.URL+"/users/octocat", nil)
		req.Header.Set("Authorization", "token "+tok)
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		return b
	}

	if !bytes.Equal(do(cA, "A-token"), bodyA) {
		t.Fatal("A's first call should get A's body")
	}
	if !bytes.Equal(do(cB, "B-token"), bodyB) {
		t.Fatal("B's first call should get B's body")
	}
	// Re-query both; should still get their own bodies, not each other's.
	if !bytes.Equal(do(cA, "A-token"), bodyA) {
		t.Fatal("A's second call should still get A's body")
	}
	if !bytes.Equal(do(cB, "B-token"), bodyB) {
		t.Fatal("B's second call should still get B's body")
	}
}

func TestETag_RefusesSharedCacheWithoutScope(t *testing.T) {
	_, err := NewTransport(nil, WithCache(NewLRUCache(4)))
	if !errors.Is(err, ErrKeyScopeRequired) {
		t.Fatalf("want ErrKeyScopeRequired; got %v", err)
	}
}

func TestETag_RejectsNilCache(t *testing.T) {
	// WithCache(nil) with a scope set must surface ErrNilCache rather than
	// silently substituting the default LRU.
	_, err := NewTransport(nil, WithCache(nil), WithKeyScope("t"))
	if !errors.Is(err, ErrNilCache) {
		t.Fatalf("want ErrNilCache; got %v", err)
	}
	// Also verify the precedence: nil cache fails BEFORE the scope check,
	// so WithCache(nil) alone still surfaces ErrNilCache, not ErrKeyScopeRequired.
	_, err = NewTransport(nil, WithCache(nil))
	if !errors.Is(err, ErrNilCache) {
		t.Fatalf("want ErrNilCache without scope; got %v", err)
	}
}

func TestETag_RejectsDoubleWrap(t *testing.T) {
	inner, err := NewTransport(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewTransport(inner)
	if !errors.Is(err, ErrDoubleWrap) {
		t.Fatalf("want ErrDoubleWrap; got %v", err)
	}
}

type notAnHTTPTransport struct{}

func (notAnHTTPTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("nope")
}

func TestETag_RejectsNonHTTPTransportBase(t *testing.T) {
	_, err := NewTransport(notAnHTTPTransport{})
	if !errors.Is(err, ErrBaseTransportType) {
		t.Fatalf("want ErrBaseTransportType; got %v", err)
	}
}

func TestETag_CompressionInvariantHeld(t *testing.T) {
	var outbound string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outbound = r.Header.Get("Accept-Encoding")
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("x"))
	}))
	defer s.Close()
	c := newTestClient(t)
	_ = doGet(t, c, s.URL+"/users/a")
	if strings.Contains(strings.ToLower(outbound), "gzip") {
		t.Fatalf("DisableCompression should prevent gzip Accept-Encoding; got %q", outbound)
	}
}

func TestETag_ContextCancelDuringRead(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(200)
		// Hold the response open; caller will cancel.
		<-r.Context().Done()
	}))
	defer s.Close()
	c := newTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", s.URL+"/x", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	resp, err := c.Do(req)
	if err == nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("expected ctx-cancel error from transport")
	}
	// Defensive body-close: the stdlib typically returns nil resp alongside
	// a non-nil err on cancellation, but bodyclose does not know that.
	if resp != nil {
		_ = resp.Body.Close()
	}
}

func TestETag_ConcurrentColdMissDedup(t *testing.T) {
	// N=8 concurrent cold GETs. We do NOT do singleflight dedup today; assert
	// all 8 end up calling Cache.Add. This test pins current behaviour so a
	// future singleflight patch deliberately flips the assertion.
	body := []byte("x")
	s, _ := ghServer(t, body)

	counting := &countingCache{inner: NewLRUCache(16)}
	rt, err := NewTransport(nil, WithCache(counting), WithKeyScope("test"))
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			r, err := c.Get(s.URL + "/users/concurrent")
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			_ = r.Body.Close()
		})
	}
	close(start)
	wg.Wait()

	// No singleflight: 8 concurrent cold misses each enter Add. Allow 2..8
	// because scheduler pressure can let one goroutine finish a full
	// RoundTrip before others read; got==1 would mean accidental dedup.
	got := atomic.LoadInt64(&counting.adds)
	if got < 2 || got > 8 {
		t.Fatalf("Add count = %d, want 2..8 (got==1 indicates accidental dedup)", got)
	}
}

type countingCache struct {
	inner Cache
	adds  int64
}

func (c *countingCache) Get(ctx context.Context, k string) (Entry, bool, error) {
	return c.inner.Get(ctx, k)
}
func (c *countingCache) Add(ctx context.Context, k string, e Entry) error {
	atomic.AddInt64(&c.adds, 1)
	return c.inner.Add(ctx, k, e)
}
func (c *countingCache) Remove(ctx context.Context, k string) error {
	return c.inner.Remove(ctx, k)
}

func TestETag_HitLogIncludesAge(t *testing.T) {
	origNow := nowFn
	t.Cleanup(func() { nowFn = origNow })
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowFn = func() time.Time { return base }

	body := []byte("x")
	s, _ := ghServer(t, body)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := newTestClient(t, WithLogger(logger))

	// Cold miss stores.
	_ = doGet(t, c, s.URL+"/users/a")

	// Advance time 2 seconds.
	nowFn = func() time.Time { return base.Add(2 * time.Second) }

	buf.Reset()
	// Warm hit.
	_ = doGet(t, c, s.URL+"/users/a")

	out := buf.String()
	if !strings.Contains(out, "age_ms") {
		t.Fatalf("expected age_ms in hit log; got %q", out)
	}
}

type failingAddCache struct{ inner Cache }

func (f failingAddCache) Get(ctx context.Context, k string) (Entry, bool, error) {
	return f.inner.Get(ctx, k)
}
func (failingAddCache) Add(context.Context, string, Entry) error {
	return errors.New("backend down")
}
func (f failingAddCache) Remove(ctx context.Context, k string) error {
	return f.inner.Remove(ctx, k)
}

func TestETag_CacheAddErrorBodyStillReadable(t *testing.T) {
	body := []byte(`{"payload":"readable"}`)
	s, _ := ghServer(t, body)
	fc := failingAddCache{inner: NewLRUCache(4)}
	rt, err := NewTransport(nil, WithCache(fc), WithKeyScope("t"))
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}
	resp, err := c.Get(s.URL + "/users/a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("caller should still read full body when Add fails: got %q want %q", got, body)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength mismatch: got %d want %d", resp.ContentLength, len(body))
	}
}

func TestETag_LogHygiene(t *testing.T) {
	// Synthesise a mismatch and inspect all captured log lines for banned
	// field names.
	body := []byte("x")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"wrong-hash"`)
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := newTestClient(t, WithLogger(logger))
	_ = doGet(t, c, s.URL+"/users/a")

	out := buf.String()
	banned := []string{"auth_len=", "ours_prefix=", "theirs_prefix=", "Authorization=", "Cookie=", "Set-Cookie="}
	for _, b := range banned {
		if strings.Contains(out, b) {
			t.Errorf("log contained banned field %q:\n%s", b, out)
		}
	}
}

func TestETag_LogHygiene_DriftEvents(t *testing.T) {
	// Asserts the log hygiene contract holds for the drift events
	// (etag_drift_detected, etag_drift_recovered, drift_callback_panic).
	rt, err := NewTransport(nil)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	tr := rt.(*Transport)

	var buf bytes.Buffer
	tr.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Trip drift.
	for range driftThreshold {
		if evt, fire := tr.recordMismatch(); fire {
			tr.fireDriftEvent(context.Background(), evt)
		}
	}
	// Recover via direct successes after backdated cooldown.
	tr.driftDegradedAt.Store(time.Now().Add(-2 * driftCooldown).UnixNano())
	for range driftRecoverAfterN {
		if evt, fire := tr.recordSuccess(); fire {
			tr.fireDriftEvent(context.Background(), evt)
		}
	}
	// Force a callback panic to also exercise the drift_callback_panic path.
	tr.driftCallback = func(DriftEvent) { panic("boom") }
	for range driftThreshold {
		if evt, fire := tr.recordMismatch(); fire {
			tr.fireDriftEvent(context.Background(), evt)
		}
	}

	out := buf.String()
	if !strings.Contains(out, "etag_drift_detected") || !strings.Contains(out, "etag_drift_recovered") {
		t.Fatalf("expected drift events in log; got %q", out)
	}
	if !strings.Contains(out, "drift_callback_panic") {
		t.Fatalf("expected drift_callback_panic event in log; got %q", out)
	}
	banned := []string{"auth_len=", "ours_prefix=", "theirs_prefix=", "Authorization=", "Cookie=", "Set-Cookie="}
	for _, b := range banned {
		if strings.Contains(out, b) {
			t.Errorf("drift event log contained banned field %q:\n%s", b, out)
		}
	}
}

type tenantKey struct{}

func TestETag_AutoKeyScope_MultiTenantIsolation(t *testing.T) {
	bodyA := []byte(`{"tenant":"A"}`)
	bodyB := []byte(`{"tenant":"B"}`)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := bodyA
		if strings.Contains(r.Header.Get("Authorization"), "B") {
			body = bodyB
		}
		expected := ComputeExpectedETag(r.Header, nil, body)
		if NormaliseETag(r.Header.Get("If-None-Match")) == expected {
			w.Header().Set("ETag", `"`+expected+`"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"`+expected+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer s.Close()

	shared := NewLRUCache(16)
	rt, err := NewTransport(nil,
		WithCache(shared),
		WithAutoKeyScope(func(req *http.Request) (string, error) {
			tenant, _ := req.Context().Value(tenantKey{}).(string)
			return tenant, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}

	do := func(tenant, tok string) []byte {
		req, _ := http.NewRequest("GET", s.URL+"/users/octocat", nil)
		req = req.WithContext(context.WithValue(req.Context(), tenantKey{}, tenant))
		req.Header.Set("Authorization", "token "+tok)
		r, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		return b
	}

	if !bytes.Equal(do("A", "A-token"), bodyA) {
		t.Fatal("tenant A first call should get A's body")
	}
	if !bytes.Equal(do("B", "B-token"), bodyB) {
		t.Fatal("tenant B first call should get B's body")
	}
	if !bytes.Equal(do("A", "A-token"), bodyA) {
		t.Fatal("tenant A second call: cross-tenant aliasing under one Transport")
	}
	if !bytes.Equal(do("B", "B-token"), bodyB) {
		t.Fatal("tenant B second call: cross-tenant aliasing under one Transport")
	}
}

func TestETag_AutoKeyScope_ConflictsWithKeyScope(t *testing.T) {
	_, err := NewTransport(nil,
		WithKeyScope("static"),
		WithAutoKeyScope(func(*http.Request) (string, error) { return "dyn", nil }),
	)
	if !errors.Is(err, ErrConflictingScope) {
		t.Fatalf("want ErrConflictingScope; got %v", err)
	}
}

func TestETag_AutoKeyScope_SatisfiesSharedCacheRequirement(t *testing.T) {
	_, err := NewTransport(nil,
		WithCache(NewLRUCache(4)),
		WithAutoKeyScope(func(*http.Request) (string, error) { return "x", nil }),
	)
	if err != nil {
		t.Fatalf("WithAutoKeyScope should satisfy the shared-cache scope requirement; got %v", err)
	}
}

func TestETag_AutoKeyScope_EmptyScopeIsError(t *testing.T) {
	body := []byte(`{}`)
	s, _ := ghServer(t, body)

	rt, err := NewTransport(nil,
		WithAutoKeyScope(func(*http.Request) (string, error) { return "", nil }),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}

	resp, err := c.Get(s.URL + "/users/octocat")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("want error from c.Get; got nil")
	}
	if !errors.Is(err, ErrEmptyScope) {
		t.Fatalf("want wrapped ErrEmptyScope; got %v", err)
	}
}

func TestETag_AutoKeyScope_ErrorPropagates(t *testing.T) {
	body := []byte(`{}`)
	s, _ := ghServer(t, body)

	sentinel := errors.New("no tenant in context")
	rt, err := NewTransport(nil,
		WithAutoKeyScope(func(*http.Request) (string, error) { return "", sentinel }),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt}

	resp, err := c.Get(s.URL + "/users/octocat")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("want error from c.Get; got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped sentinel; got %v", err)
	}
}
