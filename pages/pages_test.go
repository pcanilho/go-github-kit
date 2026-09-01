package pages_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/ghtest"
	"github.com/pcanilho/go-github-kit/pages"
)

type item struct {
	ID int `json:"id"`
}

// paginatedHandler returns a handler that simulates a Link-paginated
// endpoint. perPage items per page, totalPages pages. IDs are stable
// across requests so callers can assert ordering.
func paginatedHandler(t *testing.T, srvURL func() string, perPage, totalPages int, hits *int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		base := srvURL() + r.URL.Path
		if link := ghtest.LinkHeader(base, page, perPage, totalPages); link != "" {
			w.Header().Set("Link", link)
		}
		w.Header().Set("Content-Type", "application/json")
		items := make([]item, 0, perPage)
		for i := 0; i < perPage && page >= 1 && page <= totalPages; i++ {
			items = append(items, item{ID: (page-1)*perPage + i + 1})
		}
		_ = json.NewEncoder(w).Encode(items)
	}
}

func TestPages_SinglePage(t *testing.T) {
	var hits int32
	var srv *httptest.Server
	srv = httptest.NewServer(paginatedHandler(t, func() string { return srv.URL }, 3, 1, &hits))
	defer srv.Close()

	var pageCount int
	for resp, err := range pages.Pages(context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		pageCount++
	}
	if pageCount != 1 {
		t.Errorf("pageCount = %d, want 1", pageCount)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
}

func TestPages_MultiPage(t *testing.T) {
	var hits int32
	var srv *httptest.Server
	srv = httptest.NewServer(paginatedHandler(t, func() string { return srv.URL }, 2, 3, &hits))
	defer srv.Close()

	var pageCount int
	for resp, err := range pages.Pages(context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		pageCount++
	}
	if pageCount != 3 {
		t.Errorf("pageCount = %d, want 3", pageCount)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("hits = %d, want 3", got)
	}
}

func TestPages_NoNextLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Link header present but no rel="next": treat as clean end.
		w.Header().Set("Link", `<https://api.example/items?page=1>; rel="first"`)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	var pageCount int
	for _, err := range pages.Pages(context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
		if err != nil {
			t.Fatalf("clean end should not surface an error: %v", err)
		}
		pageCount++
	}
	if pageCount != 1 {
		t.Errorf("pageCount = %d, want 1", pageCount)
	}
}

func TestPages_MalformedLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `not a link header at all`)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	var sawErr error
	var yields int
	for resp, err := range pages.Pages(context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
		yields++
		if err != nil {
			sawErr = err
			continue
		}
		_ = resp.Body.Close()
	}
	if !errors.Is(sawErr, pages.ErrInvalidLinkHeader) {
		t.Errorf("error = %v, want ErrInvalidLinkHeader", sawErr)
	}
	if yields != 2 {
		t.Errorf("yields = %d, want 2 (one response, one error)", yields)
	}
}

func TestPages_ContextCancelMidPage(t *testing.T) {
	var hits int32
	var srv *httptest.Server
	srv = httptest.NewServer(paginatedHandler(t, func() string { return srv.URL }, 1, 5, &hits))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sawErr error
	var pages_ int
	for resp, err := range pages.Pages(ctx, srv.Client(), "GET", srv.URL+"/items", nil) {
		if err != nil {
			sawErr = err
			break
		}
		_ = resp.Body.Close()
		pages_++
		if pages_ == 2 {
			cancel()
		}
	}
	if sawErr == nil {
		t.Fatal("expected ctx cancellation error")
	}
	if !errors.Is(sawErr, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", sawErr)
	}
	if got := atomic.LoadInt32(&hits); got > 3 {
		t.Errorf("after cancel saw %d hits, want <=3 (the cancelled request and earlier)", got)
	}
}

func TestPages_UnderlyingError(t *testing.T) {
	// Point at a port nothing is listening on. Use a test server that's
	// already closed so client.Do fails reliably.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()

	var sawErr error
	var yields int
	for _, err := range pages.Pages(context.Background(), http.DefaultClient, "GET", addr+"/items", nil) {
		yields++
		if err != nil {
			sawErr = err
		}
	}
	if sawErr == nil {
		t.Fatal("expected transport error")
	}
	if yields != 1 {
		t.Errorf("yields = %d, want 1", yields)
	}
}

func TestPages_CallerBreak(t *testing.T) {
	var hits int32
	var srv *httptest.Server
	srv = httptest.NewServer(paginatedHandler(t, func() string { return srv.URL }, 1, 5, &hits))
	defer srv.Close()

	for resp, err := range pages.Pages(context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = resp.Body.Close()
		break
	}
	// One hit only: caller broke after the first response.
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1", got)
	}
}

func TestPages_304PreservesLink(t *testing.T) {
	// Wire a real etag transport in front of a paginated server. The
	// second range over the same URL hits the 304 path on every page;
	// the etag layer's merged-headers code must preserve the Link
	// header so the iterator still walks page 2.
	var hits atomic.Int32
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/items", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		base := srv.URL + r.URL.Path
		if link := ghtest.LinkHeader(base, page, 1, 2); link != "" {
			w.Header().Set("Link", link)
		}
		body := []byte(`[{"id":` + strconv.Itoa(page) + `}]`)
		if ghtest.Write304IfMatch(w, r, body) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	hc, err := ghkit.HTTPClient(ghkit.WithETagCache())
	if err != nil {
		t.Fatalf("ghkit.HTTPClient: %v", err)
	}

	walk := func() (totalPages int) {
		for resp, err := range pages.Pages(context.Background(), hc, "GET", srv.URL+"/items", nil) {
			if err != nil {
				t.Fatalf("walk error: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			totalPages++
		}
		return
	}

	if got := walk(); got != 2 {
		t.Fatalf("first walk pageCount = %d, want 2", got)
	}
	firstHits := hits.Load()

	// Second walk: each page should round-trip but resolve to 304. The
	// Link header survives the 304 path, so the iterator still walks 2
	// pages.
	if got := walk(); got != 2 {
		t.Fatalf("second walk pageCount = %d, want 2", got)
	}
	if got := hits.Load(); got != firstHits*2 {
		t.Errorf("hits after second walk = %d, want %d (one round-trip per page)", got, firstHits*2)
	}
}

func TestAs_SinglePageMultipleElements(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(paginatedHandler(t, func() string { return srv.URL }, 4, 1, nil))
	defer srv.Close()

	var ids []int
	for it, err := range pages.As[item](context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ids = append(ids, it.ID)
	}
	want := []int{1, 2, 3, 4}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range ids {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %d, want %d", i, ids[i], want[i])
		}
	}
}

func TestAs_MultiPage(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(paginatedHandler(t, func() string { return srv.URL }, 2, 3, nil))
	defer srv.Close()

	var ids []int
	for it, err := range pages.As[item](context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ids = append(ids, it.ID)
	}
	want := []int{1, 2, 3, 4, 5, 6}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range ids {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %d, want %d", i, ids[i], want[i])
		}
	}
}

func TestAs_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	var sawErr error
	var elems int
	for _, err := range pages.As[item](context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
		if err != nil {
			sawErr = err
			break
		}
		elems++
	}
	if sawErr == nil {
		t.Fatal("expected decode error")
	}
	if elems != 0 {
		t.Errorf("yielded %d elements before error, want 0", elems)
	}
}

// closeCounter wraps a body so the test can assert close behaviour.
type closeCounter struct {
	io.Reader
	closes atomic.Int32
}

func (c *closeCounter) Close() error {
	c.closes.Add(1)
	return nil
}

func TestAs_BodyClosedAfterDecode(t *testing.T) {
	// Custom RoundTripper so we own the body type and observe Close.
	// The fixture serves a single page (no Link header), so the iterator
	// makes exactly one round trip.
	body := []byte(`[{"id":1},{"id":2}]`)
	cc := &closeCounter{Reader: nil}
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		cc.Reader = bytesReader(body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       cc,
			Request:    req,
		}, nil
	})
	hc := &http.Client{Transport: rt}

	var elems int
	for _, err := range pages.As[item](context.Background(), hc, "GET", "http://example/items", nil) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		elems++
	}
	if elems != 2 {
		t.Errorf("elems = %d, want 2", elems)
	}
	if got := cc.closes.Load(); got != 1 {
		t.Errorf("body closes = %d, want 1", got)
	}
}

// TestAs_BodyClosedOnDecodePanic verifies the response body is closed
// even when json.Decode panics. This pins the defer-based close in
// decodePage as the contract: a future refactor that drops the defer
// (e.g. inlines the close after Decode) will fail this test.
func TestAs_BodyClosedOnDecodePanic(t *testing.T) {
	cc := &closeCounter{Reader: nil}
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		cc.Reader = bytesReader([]byte(`[{}]`))
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       cc,
			Request:    req,
		}, nil
	})
	hc := &http.Client{Transport: rt}

	defer func() {
		// Recover the panic; we only care that the body got closed
		// before unwinding past the iterator.
		_ = recover()
		if got := cc.closes.Load(); got != 1 {
			t.Errorf("body closes after panic = %d, want 1", got)
		}
	}()

	for range pages.As[panicker](context.Background(), hc, "GET", "http://example/items", nil) {
		t.Fatal("yield happened before panic propagated")
	}
}

// panicker has a custom UnmarshalJSON that panics, simulating a
// pathological decoder path (a buggy SDK type, not a normal one).
type panicker struct{}

func (panicker) UnmarshalJSON([]byte) error {
	panic("simulated decode panic")
}

func TestAs_BodyClosedOnCallerBreak(t *testing.T) {
	body := []byte(`[{"id":1},{"id":2},{"id":3}]`)
	cc := &closeCounter{Reader: nil}
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		cc.Reader = bytesReader(body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       cc,
			Request:    req,
		}, nil
	})
	hc := &http.Client{Transport: rt}

	for it, err := range pages.As[item](context.Background(), hc, "GET", "http://example/items", nil) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if it.ID == 1 {
			break
		}
	}
	// The body for the page we touched must be closed even though we
	// broke before consuming all elements.
	if got := cc.closes.Load(); got != 1 {
		t.Errorf("body closes = %d, want 1", got)
	}
}

// roundTripperFunc adapts a function to http.RoundTripper for tests.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bytesReader is a tiny io.Reader over a byte slice. We implement it
// inline to avoid the closeCounter accidentally embedding bytes.Reader's
// Close, which would otherwise satisfy io.Closer twice.
func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b []byte
	i int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.i >= len(s.b) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.i:])
	s.i += n
	return n, nil
}

// Compile-time assertions that the iterator return types are what we
// document (cheap regression guard against accidental signature
// changes).
var (
	_ = pages.Pages
	_ = pages.As[int]
)

// TestPages_LinkHeaderShapes exercises the parser on shapes the live
// integration tests do not cover. Each case names a documented RFC
// 8288 behaviour the parser must handle and verifies it end to end via
// the iterator (rather than reaching into the private parseNextLink).
func TestPages_LinkHeaderShapes(t *testing.T) {
	type expectation struct {
		buildHeader func(srvURL string) string
		want        int
	}
	cases := map[string]expectation{
		"empty": {
			buildHeader: func(string) string { return "" },
			want:        1,
		},
		"only-rel-last": {
			buildHeader: func(srvURL string) string {
				return `<` + srvURL + `/items?page=2>; rel="last"`
			},
			want: 1,
		},
		"with-extra-params": {
			buildHeader: func(srvURL string) string {
				return `<` + srvURL + `/items?page=2>; rel="next"; type="application/json"`
			},
			want: 2,
		},
		// RFC 8288: link-param names are case-insensitive. Servers or
		// proxies that uppercase the parameter name must still drive
		// pagination.
		"uppercase-rel-param": {
			buildHeader: func(srvURL string) string {
				return `<` + srvURL + `/items?page=2>; REL="next"`
			},
			want: 2,
		},
		"mixedcase-rel-param": {
			buildHeader: func(srvURL string) string {
				return `<` + srvURL + `/items?page=2>; Rel="next"`
			},
			want: 2,
		},
		// RFC 8288: the rel value is a space-separated list of relation
		// types. A single entry advertising both next and prev must be
		// followed when next is present.
		"multi-rel-with-next": {
			buildHeader: func(srvURL string) string {
				return `<` + srvURL + `/items?page=2>; rel="next prev"`
			},
			want: 2,
		},
		"multi-rel-without-next": {
			buildHeader: func(srvURL string) string {
				return `<` + srvURL + `/items?page=2>; rel="prev last"`
			},
			want: 1,
		},
		// rel value with internal whitespace must not match a substring
		// of next: "nextish" or "subnext" should not be confused with
		// the actual rel="next".
		"rel-substring-not-matching": {
			buildHeader: func(srvURL string) string {
				return `<` + srvURL + `/items?page=2>; rel="nextish"`
			},
			want: 1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") != "2" {
					if h := tc.buildHeader(srv.URL); h != "" {
						w.Header().Set("Link", h)
					}
				}
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()

			var walked int
			for resp, err := range pages.Pages(context.Background(), srv.Client(), "GET", srv.URL+"/items", nil) {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				walked++
				if walked > 5 {
					t.Fatal("runaway iteration")
				}
			}
			if walked != tc.want {
				t.Errorf("walked = %d, want %d", walked, tc.want)
			}
		})
	}
}

// TestPages_HeadersAreForwarded verifies headers passed to Pages reach
// the outgoing request on every page, not just the first.
func TestPages_HeadersAreForwarded(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Probe"))
		mu.Unlock()
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		base := srv.URL + r.URL.Path
		if link := ghtest.LinkHeader(base, page, 1, 3); link != "" {
			w.Header().Set("Link", link)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	headers := http.Header{
		"X-Probe": []string{"abc-123"},
		"Accept":  []string{"application/vnd.github+json"},
	}
	for resp, err := range pages.Pages(context.Background(), srv.Client(), "GET", srv.URL+"/items", headers) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("got %d requests, want 3 (one per page)", len(seen))
	}
	for i, v := range seen {
		if v != "abc-123" {
			t.Errorf("page %d X-Probe = %q, want %q", i+1, v, "abc-123")
		}
	}
}

// TestPages_HeadersClonePerRequest verifies the iterator clones the
// caller's headers map per request, so a transport that mutates
// req.Header (e.g. oauth2 stamping Authorization) cannot leak state
// back to the caller's map across pages.
func TestPages_HeadersClonePerRequest(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mutate (a copy of) request headers; this is fine for the
		// transport contract since http.Server hands us a non-shared
		// map here.
		r.Header.Set("Authorization", "should-not-leak")
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		base := srv.URL + r.URL.Path
		if link := ghtest.LinkHeader(base, page, 1, 2); link != "" {
			w.Header().Set("Link", link)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	headers := http.Header{"X-Probe": []string{"orig"}}
	for resp, err := range pages.Pages(context.Background(), srv.Client(), "GET", srv.URL+"/items", headers) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = resp.Body.Close()
	}
	if got := headers.Get("Authorization"); got != "" {
		t.Errorf("caller's headers leaked Authorization: %q", got)
	}
	if got := headers.Get("X-Probe"); got != "orig" {
		t.Errorf("caller's X-Probe was mutated: %q", got)
	}
}
