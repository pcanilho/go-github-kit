package ghkit_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
