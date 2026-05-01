package ghkit_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
	"github.com/pcanilho/go-github-kit/ratelimit"
	"golang.org/x/oauth2"
)

// --- Config validation ---

func TestGHKit_ErrorOnConflictingAuth(t *testing.T) {
	_, err := ghkit.HTTPClient(
		ghkit.WithToken("abc"),
		ghkit.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"})),
	)
	if !errors.Is(err, ghkit.ErrConflictingAuth) {
		t.Fatalf("want ErrConflictingAuth; got %v", err)
	}
}

type notAnHTTPTransport struct{}

func (notAnHTTPTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("nope")
}

func TestGHKit_ErrorOnPreAuthedBaseWithOurAuth(t *testing.T) {
	_, err := ghkit.HTTPClient(
		ghkit.WithToken("abc"),
		ghkit.WithBaseTransport(notAnHTTPTransport{}),
	)
	if !errors.Is(err, ghkit.ErrPreAuthedBaseWithAuth) {
		t.Fatalf("want ErrPreAuthedBaseWithAuth; got %v", err)
	}
}

func TestGHKit_BaseTransportNilTreatedAsUnset(t *testing.T) {
	hc, err := ghkit.HTTPClient(ghkit.WithBaseTransport(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hc == nil {
		t.Fatal("want non-nil *http.Client")
	}
}

func TestGHKit_ErrorOnNonPositiveRPS(t *testing.T) {
	cases := []struct {
		rps   float64
		burst int
	}{
		{-1, 1},
		{0, 1},
		{1, 0},
		{1, -1},
	}
	for _, c := range cases {
		_, err := ghkit.HTTPClient(ghkit.WithRequestsPerSecond(c.rps, c.burst))
		if !errors.Is(err, ghkit.ErrNonPositiveRPS) {
			t.Errorf("rps=%v burst=%d: want ErrNonPositiveRPS; got %v", c.rps, c.burst, err)
		}
	}
	// Sanity: valid values accept.
	if _, err := ghkit.HTTPClient(ghkit.WithRequestsPerSecond(1.5, 2)); err != nil {
		t.Errorf("valid rps should not error: %v", err)
	}
}

func TestGHKit_PreAuthedBaseErrorContainsType(t *testing.T) {
	// ErrPreAuthedBaseWithAuth must be wrapped with the concrete base type
	// so the error message tells the caller what they passed.
	_, err := ghkit.HTTPClient(
		ghkit.WithToken("abc"),
		ghkit.WithBaseTransport(notAnHTTPTransport{}),
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "notAnHTTPTransport") {
		t.Fatalf("error should mention the base type; got: %v", err)
	}
}

func TestGHKit_ErrorOnSharedCacheWithoutKeyScope(t *testing.T) {
	_, err := ghkit.HTTPClient(
		ghkit.WithETagCache(etag.WithCache(etag.NewLRUCache(4))),
	)
	if err == nil || !strings.Contains(err.Error(), "WithKeyScope") {
		t.Fatalf("want etag WithKeyScope error; got %v", err)
	}
}

func TestGHKit_RateLimitDisabledWithCallbacksReturnsError(t *testing.T) {
	_, err := ghkit.HTTPClient(
		ghkit.WithRateLimit(ratelimit.WithPrimaryLimitDetected(func(*ratelimit.PrimaryEvent) {})),
		ghkit.WithRateLimitDisabled(),
	)
	if !errors.Is(err, ghkit.ErrConflictingRateLimit) {
		t.Fatalf("want ErrConflictingRateLimit; got %v", err)
	}
}

// --- Transport composition ---

func TestGHKit_DefaultRateLimitEnabled(t *testing.T) {
	hc, err := ghkit.HTTPClient(ghkit.WithToken("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hc.Transport.(*oauth2.Transport); ok {
		t.Fatal("default should wrap oauth2 with rate-limit layer; got bare oauth2.Transport")
	}
}

func TestGHKit_RateLimitDisabled(t *testing.T) {
	hc, err := ghkit.HTTPClient(
		ghkit.WithToken("x"),
		ghkit.WithRateLimitDisabled(),
	)
	if err != nil {
		t.Fatal(err)
	}
	// With rate limit off, top should be oauth2.Transport directly.
	if _, ok := hc.Transport.(*oauth2.Transport); !ok {
		t.Fatalf("expected bare oauth2.Transport; got %T", hc.Transport)
	}
}

func TestGHKit_UserAgentApplied(t *testing.T) {
	var captured atomic.Value // string
	captured.Store("")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(200)
	}))
	defer s.Close()

	const want = "my-app/1.0"
	hc, err := ghkit.HTTPClient(
		ghkit.WithUserAgent(want),
		ghkit.WithRateLimitDisabled(),
	)
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	resp, err := hc.Get(s.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Body.Close: %v", err)
	}
	if got := captured.Load().(string); got != want {
		t.Fatalf("User-Agent at server = %q, want %q", got, want)
	}
}

func TestGHKit_UserAgentEmptyStringIsNoOp(t *testing.T) {
	// WithUserAgent("") must not insert the middleware; the stdlib default
	// User-Agent flows through unchanged.
	var captured atomic.Value
	captured.Store("")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Store(r.Header.Get("User-Agent"))
		w.WriteHeader(200)
	}))
	defer s.Close()

	hc, err := ghkit.HTTPClient(
		ghkit.WithUserAgent(""),
		ghkit.WithRateLimitDisabled(),
	)
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	resp, err := hc.Get(s.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Body.Close: %v", err)
	}
	if got := captured.Load().(string); !strings.HasPrefix(got, "Go-http-client") {
		t.Fatalf("expected stdlib default UA; got %q", got)
	}
}

func TestGHKit_TimeoutApplied(t *testing.T) {
	hc, err := ghkit.HTTPClient(ghkit.WithTimeout(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if hc.Timeout != 3*time.Second {
		t.Fatalf("timeout not applied: %v", hc.Timeout)
	}
}

// --- Generic New ---

// fakeClient is a stand-in for a go-github-style client.
type fakeClient struct {
	hc        *http.Client
	userAgent string
}

func newFakeClient(hc *http.Client) *fakeClient { return &fakeClient{hc: hc} }

func TestGHKit_NewGenericFactory(t *testing.T) {
	fc, err := ghkit.New(newFakeClient,
		ghkit.WithToken("abc"),
		ghkit.WithRateLimitDisabled(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if fc == nil || fc.hc == nil {
		t.Fatal("factory received a nil http.Client")
	}
}

func TestGHKit_NewGenericFactory_ClosureCustomisation(t *testing.T) {
	const want = "my-app/1.0"
	factory := func(hc *http.Client) *fakeClient {
		return &fakeClient{hc: hc, userAgent: want}
	}
	fc, err := ghkit.New(factory,
		ghkit.WithToken("abc"),
		ghkit.WithRateLimitDisabled(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if fc.userAgent != want {
		t.Fatalf("closure customisation lost: got %q, want %q", fc.userAgent, want)
	}
}

func TestGHKit_NewNilFactory(t *testing.T) {
	// Nil factory must return ErrNilFactory, not panic.
	fc, err := ghkit.New[*fakeClient](nil, ghkit.WithToken("abc"))
	if !errors.Is(err, ghkit.ErrNilFactory) {
		t.Fatalf("want ErrNilFactory; got %v", err)
	}
	if fc != nil {
		t.Fatalf("expected nil client on error; got %+v", fc)
	}
}

func TestGHKit_NewGenericFactoryErrorProxies(t *testing.T) {
	// HTTPClient-construction errors must propagate through New.
	fc, err := ghkit.New(newFakeClient,
		ghkit.WithToken("abc"),
		ghkit.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"})),
	)
	if !errors.Is(err, ghkit.ErrConflictingAuth) {
		t.Fatalf("want ErrConflictingAuth proxied through New; got %v", err)
	}
	if fc != nil {
		t.Fatalf("expected nil client on error; got %+v", fc)
	}
}

// --- End-to-end ---

// ghServer reproduces GitHub's ETag behaviour against a configurable body.
func ghServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := etag.ComputeExpectedETag(r.Header, nil, body)
		if etag.NormaliseETag(r.Header.Get("If-None-Match")) == expected {
			w.Header().Set("ETag", `"`+expected+`"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"`+expected+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestGHKit_E2E_StaticPATColdWarm(t *testing.T) {
	body := []byte(`{"login":"octocat"}`)
	s := ghServer(t, body)

	hc, err := ghkit.HTTPClient(
		ghkit.WithToken("pat-xyz"),
		ghkit.WithETagCache(),
		ghkit.WithRateLimitDisabled(),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Cold miss.
	r1, err := hc.Get(s.URL + "/users/octocat")
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(r1.Body)
	_ = r1.Body.Close()
	if !bytes.Equal(b1, body) {
		t.Fatalf("cold-miss body mismatch: %q", b1)
	}

	// Warm hit: the server returns 304 and the etag layer synthesises 200.
	r2, err := hc.Get(s.URL + "/users/octocat")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(r2.Body)
	_ = r2.Body.Close()
	if r2.StatusCode != 200 || !bytes.Equal(b2, body) {
		t.Fatalf("warm hit not replayed: status=%d body=%q", r2.StatusCode, b2)
	}
}

func TestGHKit_E2E_MultiTenantIsolation(t *testing.T) {
	// Two separate ghkit clients, each with its own TokenSource and
	// KeyScope, sharing one Cache. Bodies differ per tenant.
	serveFn := func(tenant string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := []byte(`{"tenant":"` + tenant + `"}`)
			expected := etag.ComputeExpectedETag(r.Header, nil, body)
			if etag.NormaliseETag(r.Header.Get("If-None-Match")) == expected {
				w.Header().Set("ETag", `"`+expected+`"`)
				w.WriteHeader(304)
				return
			}
			w.Header().Set("ETag", `"`+expected+`"`)
			w.WriteHeader(200)
			_, _ = w.Write(body)
		})
	}
	// One server that picks tenant based on the token suffix in the
	// Authorization header. Match on the "-tenantX" suffix, not a bare
	// letter, so the "Bearer" literal doesn't trip a false match.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tenant := "A"
		if strings.HasSuffix(auth, "-tenantB") {
			tenant = "B"
		}
		serveFn(tenant).ServeHTTP(w, r)
	}))
	defer s.Close()

	cache := etag.NewLRUCache(16)
	mkClient := func(scope, token string) *http.Client {
		hc, err := ghkit.HTTPClient(
			ghkit.WithToken(token),
			ghkit.WithETagCache(etag.WithCache(cache), etag.WithKeyScope(scope)),
			ghkit.WithRateLimitDisabled(),
		)
		if err != nil {
			t.Fatal(err)
		}
		return hc
	}
	cA := mkClient("tenant-A", "secret-tenantA")
	cB := mkClient("tenant-B", "secret-tenantB")

	do := func(c *http.Client) string {
		r, err := c.Get(s.URL + "/users/octocat")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	// First call from each: cold miss, own body.
	if !strings.Contains(do(cA), `"tenant":"A"`) {
		t.Fatal("A should see A's body")
	}
	if !strings.Contains(do(cB), `"tenant":"B"`) {
		t.Fatal("B should see B's body")
	}
	// Warm hits: still each gets their own body; no cross-contamination.
	if !strings.Contains(do(cA), `"tenant":"A"`) {
		t.Fatal("A's warm hit should replay A")
	}
	if !strings.Contains(do(cB), `"tenant":"B"`) {
		t.Fatal("B's warm hit should replay B")
	}
}

// cooldownServer simulates GitHub's secondary-rate-limit response shape.
// First hit sets cooldownUntil = now + cooldownLength. Hits inside the
// window return 403 + Retry-After + the JSON body gofri's
// isSecondaryRateLimit prefix-matches against. Hits after the window
// return 200.
type cooldownServer struct {
	mu             sync.Mutex
	cooldownStart  time.Time
	cooldownUntil  time.Time
	cooldownLength time.Duration
	hits           []time.Time
}

const secondaryLimitBody = `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#secondary-rate-limits"}`

func (s *cooldownServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	now := time.Now()
	s.hits = append(s.hits, now)
	if s.cooldownUntil.IsZero() {
		s.cooldownStart = now
		s.cooldownUntil = now.Add(s.cooldownLength)
	}
	inCooldown := now.Before(s.cooldownUntil)
	cooldownUntil := s.cooldownUntil
	s.mu.Unlock()

	if inCooldown {
		retryAfter := int(math.Ceil(time.Until(cooldownUntil).Seconds()))
		if retryAfter < 1 {
			retryAfter = 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(secondaryLimitBody))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *cooldownServer) hitsCopy() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.hits...)
}

func (s *cooldownServer) cooldownStartCopy() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cooldownStart
}

// TestGHKit_E2E_PostCooldownReleaseBoundedByBurst is the headline F2 test.
// Under the inverted chain, N post-barrier requests park inside ratelimit
// without consuming throttle tokens; at cooldown end the bucket is at
// burst, so exactly burst-many fire simultaneously and the rest are
// rps-paced. Under the previous (1.3.x) chain order all parked requests
// stampede the server simultaneously. Gap is the discriminator.
func TestGHKit_E2E_PostCooldownReleaseBoundedByBurst(t *testing.T) {
	const (
		cooldownLength = time.Second
		rps            = 10.0
		burst          = 3
		n              = 10
	)

	cs := &cooldownServer{cooldownLength: cooldownLength}
	srv := httptest.NewServer(cs)
	defer srv.Close()

	firstHitDone := make(chan struct{})
	var once sync.Once
	cb := func(_ *ratelimit.SecondaryEvent) {
		once.Do(func() { close(firstHitDone) })
	}

	hc, err := ghkit.HTTPClient(
		ghkit.WithRateLimit(ratelimit.WithSecondaryLimitDetected(cb)),
		ghkit.WithRequestsPerSecond(rps, burst),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var trigWG sync.WaitGroup
	trigWG.Add(1)
	go func() {
		defer trigWG.Done()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
		r, err := hc.Do(req)
		if err != nil {
			t.Errorf("trigger: %v", err)
			return
		}
		_ = r.Body.Close()
	}()

	select {
	case <-firstHitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("trigger barrier did not close within 2s")
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
			r, err := hc.Do(req)
			if err != nil {
				t.Errorf("post-barrier: %v", err)
				return
			}
			_ = r.Body.Close()
		}()
	}
	wg.Wait()
	trigWG.Wait()

	h := cs.hitsCopy()
	sort.Slice(h, func(i, j int) bool { return h[i].Before(h[j]) })
	cooldownEnd := cs.cooldownStartCopy().Add(cooldownLength)

	postCool := -1
	for i, ts := range h {
		if !ts.Before(cooldownEnd) {
			postCool = i
			break
		}
	}
	if postCool < 0 {
		t.Fatalf("no post-cooldown hit observed; total hits=%d", len(h))
	}

	clusterEnd := postCool + burst
	postCoolCount := len(h) - postCool
	if postCoolCount < burst+1 {
		t.Fatalf("expected >= %d post-cooldown hits, got %d (total %d, postCool %d)", burst+1, postCoolCount, len(h), postCool)
	}
	clusterSpread := h[clusterEnd-1].Sub(h[postCool])
	if clusterSpread >= 100*time.Millisecond {
		t.Fatalf("cluster spread %v exceeds 100ms (current chain stampede signature)", clusterSpread)
	}
	gapAfterCluster := h[clusterEnd].Sub(h[clusterEnd-1])
	// rate.Limiter's float arithmetic plus server-side timestamp jitter
	// reliably produces gaps a few ms below 1/rps. Real stampede gaps
	// are sub-ms, so a 10ms tolerance preserves the discriminator.
	minGap := time.Duration(float64(time.Second)/rps) - 10*time.Millisecond
	if gapAfterCluster < minGap {
		t.Fatalf("gap after burst cluster %v < %v (1/rps - 10ms); throttle did not pace post-cooldown release", gapAfterCluster, minGap)
	}
}

// TestGHKit_E2E_RateLimitParksDuringSecondaryLimit confirms that
// post-barrier requests do NOT hit the server during the cooldown window.
// Uses half-open interval [cooldownStart, cooldownEnd): the trigger's
// recursive retry hits at cooldownEnd ± jitter and is correctly excluded.
func TestGHKit_E2E_RateLimitParksDuringSecondaryLimit(t *testing.T) {
	const (
		cooldownLength = time.Second
		rps            = 10.0
		burst          = 1
		n              = 5
	)

	cs := &cooldownServer{cooldownLength: cooldownLength}
	srv := httptest.NewServer(cs)
	defer srv.Close()

	firstHitDone := make(chan struct{})
	var once sync.Once
	cb := func(_ *ratelimit.SecondaryEvent) {
		once.Do(func() { close(firstHitDone) })
	}

	hc, err := ghkit.HTTPClient(
		ghkit.WithRateLimit(ratelimit.WithSecondaryLimitDetected(cb)),
		ghkit.WithRequestsPerSecond(rps, burst),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var trigWG sync.WaitGroup
	trigWG.Add(1)
	go func() {
		defer trigWG.Done()
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
		r, err := hc.Do(req)
		if err != nil {
			t.Errorf("trigger: %v", err)
			return
		}
		_ = r.Body.Close()
	}()

	select {
	case <-firstHitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("trigger barrier did not close within 2s")
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
			r, err := hc.Do(req)
			if err != nil {
				t.Errorf("post-barrier: %v", err)
				return
			}
			_ = r.Body.Close()
		}()
	}
	wg.Wait()
	trigWG.Wait()

	cooldownStart := cs.cooldownStartCopy()
	cooldownEnd := cooldownStart.Add(cooldownLength)
	inWindow := 0
	for _, ts := range cs.hitsCopy() {
		if !ts.Before(cooldownStart) && ts.Before(cooldownEnd) {
			inWindow++
		}
	}
	if inWindow != 1 {
		t.Fatalf("expected exactly 1 hit in [cooldownStart, cooldownEnd) (the trigger's initial 403); got %d", inWindow)
	}
}

// TestGHKit_E2E_AbortPropagatesUnderInvertedChain pins gofri's abort path:
// WithTotalSleepLimit(1ms) makes the very first 403 trip
// IsAboveTotalSleepLimit, which fires onTotalLimitExceeded and returns
// shouldRetry=false WITHOUT writing resetTime. The 403 surfaces with
// err == nil. After the server's cooldown elapses, a fresh request
// returns 200 (no zombie park).
func TestGHKit_E2E_AbortPropagatesUnderInvertedChain(t *testing.T) {
	const cooldownLength = time.Second

	cs := &cooldownServer{cooldownLength: cooldownLength}
	srv := httptest.NewServer(cs)
	defer srv.Close()

	hc, err := ghkit.HTTPClient(
		ghkit.WithRateLimit(ratelimit.WithTotalSleepLimit(time.Millisecond)),
	)
	if err != nil {
		t.Fatal(err)
	}

	req1, _ := http.NewRequest("GET", srv.URL, nil)
	resp1, err := hc.Do(req1)
	if err != nil {
		t.Fatalf("request 1: err = %v; abort path should surface 403 with nil err", err)
	}
	if resp1.StatusCode != http.StatusForbidden {
		t.Fatalf("request 1: status = %d; want 403", resp1.StatusCode)
	}
	_ = resp1.Body.Close()

	time.Sleep(cooldownLength + 100*time.Millisecond)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, "GET", srv.URL, nil)
	resp2, err := hc.Do(req2)
	if err != nil {
		t.Fatalf("request 2: err = %v; expected 200 (no zombie park)", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("request 2: status = %d; want 200", resp2.StatusCode)
	}
	_ = resp2.Body.Close()

	if got := len(cs.hitsCopy()); got != 2 {
		t.Fatalf("expected exactly 2 server hits (no spurious abort retries); got %d", got)
	}
}

// TestGHKit_E2E_ContextCancelDuringCooldown covers the two cancel paths
// under the inverted chain that have distinct semantics.
func TestGHKit_E2E_ContextCancelDuringCooldown(t *testing.T) {
	t.Run("cancelWhileParkedInRateLimit", func(t *testing.T) {
		const cooldownLength = time.Second

		cs := &cooldownServer{cooldownLength: cooldownLength}
		srv := httptest.NewServer(cs)
		defer srv.Close()

		firstHitDone := make(chan struct{})
		var once sync.Once
		cb := func(_ *ratelimit.SecondaryEvent) {
			once.Do(func() { close(firstHitDone) })
		}

		hc, err := ghkit.HTTPClient(
			ghkit.WithRateLimit(ratelimit.WithSecondaryLimitDetected(cb)),
		)
		if err != nil {
			t.Fatal(err)
		}

		var trigWG sync.WaitGroup
		trigWG.Add(1)
		go func() {
			defer trigWG.Done()
			r, err := hc.Get(srv.URL)
			if err == nil {
				_ = r.Body.Close()
			}
		}()

		select {
		case <-firstHitDone:
		case <-time.After(2 * time.Second):
			t.Fatal("trigger barrier did not close within 2s")
		}

		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
		done := make(chan error, 1)
		go func() {
			r, err := hc.Do(req)
			if err == nil {
				_ = r.Body.Close()
			}
			done <- err
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v; want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled request did not return within 2s")
		}

		trigWG.Wait()
		// Trigger initial hit + trigger recursive retry post-cooldown = 2.
		// Cancelled request never reaches the server.
		hits := len(cs.hitsCopy())
		if hits > 2 {
			t.Fatalf("server hit count %d > 2; cancelled request reached the server", hits)
		}
	})

	t.Run("cancelWhileWaitingInThrottle", func(t *testing.T) {
		handlerEntered := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseAll := func() { releaseOnce.Do(func() { close(release) }) }

		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			select {
			case handlerEntered <- struct{}{}:
			default:
			}
			<-release
			w.WriteHeader(http.StatusOK)
		}))
		defer slow.Close()
		defer releaseAll()

		hc, err := ghkit.HTTPClient(
			ghkit.WithRequestsPerSecond(2, 1),
		)
		if err != nil {
			t.Fatal(err)
		}

		consumerDone := make(chan struct{})
		go func() {
			defer close(consumerDone)
			r, err := hc.Get(slow.URL)
			if err == nil {
				_ = r.Body.Close()
			}
		}()

		select {
		case <-handlerEntered:
		case <-time.After(2 * time.Second):
			t.Fatal("consumer did not enter handler within 2s")
		}

		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", slow.URL, nil)
		cancelStart := time.Now()
		done := make(chan error, 1)
		go func() {
			r, err := hc.Do(req)
			if err == nil {
				_ = r.Body.Close()
			}
			done <- err
		}()
		time.Sleep(10 * time.Millisecond)
		cancel()

		var cancelLatency time.Duration
		select {
		case err := <-done:
			cancelLatency = time.Since(cancelStart)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v; want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled request did not return within 2s")
		}
		if cancelLatency >= 200*time.Millisecond {
			t.Fatalf("cancel latency %v >= 200ms; cancel did not hit limiter.Wait", cancelLatency)
		}

		releaseAll()
		select {
		case <-consumerDone:
		case <-time.After(2 * time.Second):
			t.Fatal("consumer goroutine did not finish within 2s")
		}
	})
}

// TestGHKit_E2E_NetworkErrorDoesNotPark confirms that gofri's RoundTrip
// returns a transport error before parseSecondaryLimitTime runs, so a
// network error cannot trigger a spurious park.
func TestGHKit_E2E_NetworkErrorDoesNotPark(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := lis.Addr().String()
	_ = lis.Close()
	deadURL := "http://" + deadAddr

	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer working.Close()

	hc, err := ghkit.HTTPClient(ghkit.WithRateLimit())
	if err != nil {
		t.Fatal(err)
	}

	resp1, err := hc.Get(deadURL)
	if err == nil {
		_ = resp1.Body.Close()
		t.Fatal("expected error from dead URL; got nil")
	}

	start := time.Now()
	r, err := hc.Get(working.URL)
	if err != nil {
		t.Fatalf("working URL: %v", err)
	}
	_ = r.Body.Close()
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Fatalf("working URL took %v; expected <1s (proves no park)", elapsed)
	}
}

func TestGHKit_E2E_RateLimitPlusThrottle(t *testing.T) {
	// Throttle at 5 rps, burst 1; assert 3 requests take >=400ms.
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer s.Close()

	hc, err := ghkit.HTTPClient(
		ghkit.WithRequestsPerSecond(5, 1),
	)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	ctx := context.Background()
	for range 3 {
		req, _ := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
		r, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
	}
	// 1st instant (burst), 2nd+3rd each wait 200ms = 400ms minimum.
	if elapsed := time.Since(start); elapsed < 350*time.Millisecond {
		t.Fatalf("expected rps throttle to pace requests; elapsed %v", elapsed)
	}
	if hits.Load() != 3 {
		t.Fatalf("expected 3 upstream hits; got %d", hits.Load())
	}
}
