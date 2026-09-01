package pages_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
	"github.com/pcanilho/go-github-kit/ghtest"
	"github.com/pcanilho/go-github-kit/pages"
)

// benchPaginatedServer returns a server that serves N pages of M items
// each, with a deterministic body so an ETag layer wrapped around the
// client can negotiate 304s on a second walk.
func benchPaginatedServer(b *testing.B, totalPages, perPage int) *httptest.Server {
	b.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		base := srv.URL + r.URL.Path
		if link := ghtest.LinkHeader(base, page, perPage, totalPages); link != "" {
			w.Header().Set("Link", link)
		}
		body := make([]byte, 0, 64)
		body = append(body, '[')
		for i := range perPage {
			if i > 0 {
				body = append(body, ',')
			}
			body = append(body, []byte(`{"id":`)...)
			body = strconv.AppendInt(body, int64((page-1)*perPage+i+1), 10)
			body = append(body, '}')
		}
		body = append(body, ']')
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"`+etag.ComputeExpectedETag(r.Header, nil, body)+`"`)
		if got := r.Header.Get("If-None-Match"); got != "" && etag.NormaliseETag(got) == etag.ComputeExpectedETag(r.Header, nil, body) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(body)
	}))
	b.Cleanup(srv.Close)
	return srv
}

func drainPages(b *testing.B, hc *http.Client, url string) {
	b.Helper()
	for resp, err := range pages.Pages(context.Background(), hc, "GET", url, nil) {
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// BenchmarkPages_ColdNoETag walks 5 pages with no caching, isolating
// the iterator and Link parsing cost from the network and ETag layers.
func BenchmarkPages_ColdNoETag(b *testing.B) {
	srv := benchPaginatedServer(b, 5, 30)
	hc := srv.Client()

	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		drainPages(b, hc, srv.URL+"/items")
	}
}

// BenchmarkPages_WarmETag walks the same 5 pages through a configured
// ghkit.HTTPClient with ETag caching enabled. After the warm-up walk
// every iteration of b.N hits 304 per page.
func BenchmarkPages_WarmETag(b *testing.B) {
	srv := benchPaginatedServer(b, 5, 30)
	hc, err := ghkit.HTTPClient(ghkit.WithETagCache())
	if err != nil {
		b.Fatal(err)
	}
	// Warm the cache: first walk primes ETag entries for all pages.
	drainPages(b, hc, srv.URL+"/items")

	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		drainPages(b, hc, srv.URL+"/items")
	}
}

// BenchmarkPages_AsTyped measures the As[T] decode path on top of the
// iterator: 5 pages of 30 items each, decoded into a struct slice.
func BenchmarkPages_AsTyped(b *testing.B) {
	srv := benchPaginatedServer(b, 5, 30)
	hc := srv.Client()

	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		var n int
		for _, err := range pages.As[item](context.Background(), hc, "GET", srv.URL+"/items", nil) {
			if err != nil {
				b.Fatal(err)
			}
			n++
		}
		if n != 5*30 {
			b.Fatalf("element count = %d, want %d", n, 5*30)
		}
	}
}

// BenchmarkPages_HandlerHits records the per-walk handler hit rate so
// changes to caching behaviour show up here, not just in the latency
// numbers above.
func BenchmarkPages_HandlerHits(b *testing.B) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			page, _ = strconv.Atoi(p)
		}
		base := srv.URL + r.URL.Path
		if link := ghtest.LinkHeader(base, page, 30, 5); link != "" {
			w.Header().Set("Link", link)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]item{{ID: page}})
	}))
	defer srv.Close()
	hc := srv.Client()

	b.ResetTimer()
	for range b.N {
		drainPages(b, hc, srv.URL+"/items")
	}
	b.ReportMetric(float64(hits.Load())/float64(b.N), "hits/walk")
}
