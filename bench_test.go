package ghkit_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
)

const benchPayloadSize = 4 * 1024

func benchPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return b
}

// benchPlainServer always returns 200 with the same body. No ETag negotiation.
func benchPlainServer(b *testing.B) (*httptest.Server, []byte) {
	b.Helper()
	body := benchPayload(benchPayloadSize)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	b.Cleanup(s.Close)
	return s, body
}

// benchETagServer reproduces ghkit's ETag algorithm server-side and returns
// 304 when the client's If-None-Match matches.
func benchETagServer(b *testing.B) (*httptest.Server, []byte) {
	b.Helper()
	body := benchPayload(benchPayloadSize)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		expected := etag.ComputeExpectedETag(r.Header, nil, body)
		quoted := `"` + expected + `"`
		w.Header().Set("ETag", quoted)
		if got := r.Header.Get("If-None-Match"); got != "" && etag.NormaliseETag(got) == expected {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	b.Cleanup(s.Close)
	return s, body
}

func drain(b *testing.B, resp *http.Response) {
	b.Helper()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		b.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkHTTPClient_NoLayers is the baseline. Anything ghkit's stack adds
// to BenchmarkHTTPClient_FullStack relative to this is per-request overhead.
func BenchmarkHTTPClient_NoLayers(b *testing.B) {
	s, body := benchPlainServer(b)
	hc := &http.Client{}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for range b.N {
		resp, err := hc.Get(s.URL)
		if err != nil {
			b.Fatal(err)
		}
		drain(b, resp)
	}
}

// BenchmarkHTTPClient_FullStack measures every layer in steady state: the
// ETag cache is pre-warmed, so each iteration is a 304 synthesised back to
// 200 from the cached body. This is the headline value path for long-lived
// services.
func BenchmarkHTTPClient_FullStack(b *testing.B) {
	s, body := benchETagServer(b)

	hc, err := ghkit.HTTPClient(
		ghkit.WithETagCache(),
		ghkit.WithRetry(),
		ghkit.WithRequestsPerSecond(1e9, 1e9),
		ghkit.WithUserAgent("bench/1.0"),
	)
	if err != nil {
		b.Fatal(err)
	}

	// Warm the cache: first hit is a 200, subsequent are 304.
	resp, err := hc.Get(s.URL)
	if err != nil {
		b.Fatal(err)
	}
	drain(b, resp)

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for range b.N {
		resp, err := hc.Get(s.URL)
		if err != nil {
			b.Fatal(err)
		}
		drain(b, resp)
	}
}

// BenchmarkHTTPClient_ColdMiss measures the cold-cache 200 path: read body,
// compute SHA256 over the cached representation, store. Each iteration uses
// a unique URL so the cache always misses.
func BenchmarkHTTPClient_ColdMiss(b *testing.B) {
	s, body := benchETagServer(b)

	hc, err := ghkit.HTTPClient(
		ghkit.WithETagCache(),
	)
	if err != nil {
		b.Fatal(err)
	}

	urls := make([]string, b.N)
	for i := range urls {
		urls[i] = s.URL + "/p/" + strconv.Itoa(i)
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := range b.N {
		resp, err := hc.Get(urls[i])
		if err != nil {
			b.Fatal(err)
		}
		drain(b, resp)
	}
}
