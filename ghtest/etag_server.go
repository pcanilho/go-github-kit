package ghtest

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pcanilho/go-github-kit/etag"
)

// ETagServer simulates GitHub's server-side ETag behaviour: a request
// whose If-None-Match matches the body hash gets a 304, otherwise a 200
// with the precomputed ETag. The *int64 counts requests so callers can
// assert wire hits. Registered with t.Cleanup.
func ETagServer(t *testing.T, body []byte) (*httptest.Server, *int64) {
	t.Helper()
	var reqs int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&reqs, 1)
		w.Header().Set("Content-Type", "application/json")
		expected := etag.ComputeExpectedETag(r.Header, nil, body)
		quoted := `"` + expected + `"`
		if got := r.Header.Get("If-None-Match"); got != "" && etag.NormaliseETag(got) == expected {
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
