package cond_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/cond"
	"github.com/pcanilho/go-github-kit/ghtest"
)

func TestStatusOf_NilSafe(t *testing.T) {
	if got := cond.StatusOf(nil); got != cond.Updated {
		t.Fatalf("StatusOf(nil)=%v want Updated", got)
	}
}

func TestStatusOf_HeaderMissing(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if got := cond.StatusOf(resp); got != cond.Updated {
		t.Fatalf("absent header => %v want Updated", got)
	}
}

func TestStatusOf_Hit(t *testing.T) {
	resp := &http.Response{Header: http.Header{cond.HeaderCacheStatus: []string{"hit"}}}
	if got := cond.StatusOf(resp); got != cond.Unchanged {
		t.Fatalf("hit => %v want Unchanged", got)
	}
}

func TestStatusOf_Miss(t *testing.T) {
	resp := &http.Response{Header: http.Header{cond.HeaderCacheStatus: []string{"miss"}}}
	if got := cond.StatusOf(resp); got != cond.Updated {
		t.Fatalf("miss => %v want Updated", got)
	}
}

func TestStatusOf_Unknown(t *testing.T) {
	resp := &http.Response{Header: http.Header{cond.HeaderCacheStatus: []string{"strange"}}}
	if got := cond.StatusOf(resp); got != cond.Updated {
		t.Fatalf("unknown => %v want Updated (default-safe)", got)
	}
}

func TestFetch_HappyPath_DecodesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"v":42}`)
	}))
	t.Cleanup(srv.Close)

	type payload struct {
		V int `json:"v"`
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	v, status, err := cond.Fetch(t.Context(), srv.Client(), req, func(r io.Reader) (payload, error) {
		var p payload
		err := json.NewDecoder(r).Decode(&p)
		return p, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.V != 42 {
		t.Fatalf("v=%d want 42", v.V)
	}
	if status != cond.Updated {
		t.Fatalf("status=%v want Updated (no etag layer)", status)
	}
}

func TestFetch_NilClient(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example", nil)
	_, _, err := cond.Fetch(context.Background(), nil, req, func(r io.Reader) (int, error) { return 0, nil })
	if !errors.Is(err, cond.ErrNilClient) {
		t.Fatalf("err=%v want ErrNilClient", err)
	}
}

func TestFetch_NilContext(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example", nil)
	_, _, err := cond.Fetch(nil, &http.Client{}, req, func(r io.Reader) (int, error) { return 0, nil }) //nolint:staticcheck // SA1012: nil ctx is the test subject
	if !errors.Is(err, cond.ErrNilContext) {
		t.Fatalf("err=%v want ErrNilContext", err)
	}
}

func TestFetch_NilRequest(t *testing.T) {
	_, _, err := cond.Fetch(context.Background(), &http.Client{}, nil, func(r io.Reader) (int, error) { return 0, nil })
	if !errors.Is(err, cond.ErrNilRequest) {
		t.Fatalf("err=%v want ErrNilRequest", err)
	}
}

func TestFetch_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{not json`)
	}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	type p struct {
		X int `json:"x"`
	}
	_, status, err := cond.Fetch(t.Context(), srv.Client(), req, func(r io.Reader) (p, error) {
		var v p
		err := json.NewDecoder(r).Decode(&v)
		return v, err
	})
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "cond: decode:") {
		t.Fatalf("err=%v want cond: decode: prefix for cross-package parity", err)
	}
	// Decode-error path still surfaces the cache status (Updated here
	// since no etag layer is in the chain).
	if status != cond.Updated {
		t.Fatalf("status=%v want Updated even on decode error", status)
	}
}

// TestFetch_WithETagCache_HitOnSecondRequest exercises the full
// integration: the first Fetch through a real ghkit transport stack
// stores in the cache; the second returns Unchanged because the etag
// layer's synth-200 carries cond.HeaderCacheStatus="hit".
func TestFetch_WithETagCache_HitOnSecondRequest(t *testing.T) {
	body := []byte(`{"v":7}`)
	srv, reqs := ghtest.ETagServer(t, body)

	hc, err := ghkit.HTTPClient(ghkit.WithETagCache())
	if err != nil {
		t.Fatal(err)
	}
	type payload struct {
		V int `json:"v"`
	}
	decode := func(r io.Reader) (payload, error) {
		var p payload
		err := json.NewDecoder(r).Decode(&p)
		return p, err
	}

	// First Fetch: cold cache.
	req1, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	v1, s1, err := cond.Fetch(t.Context(), hc, req1, decode)
	if err != nil {
		t.Fatal(err)
	}
	if s1 != cond.Updated {
		t.Fatalf("first status=%v want Updated", s1)
	}
	if v1.V != 7 {
		t.Fatalf("v1=%d want 7", v1.V)
	}

	// Second Fetch: cache hit, synth-200 carries Unchanged.
	req2, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	v2, s2, err := cond.Fetch(t.Context(), hc, req2, decode)
	if err != nil {
		t.Fatal(err)
	}
	if s2 != cond.Unchanged {
		t.Fatalf("second status=%v want Unchanged", s2)
	}
	if v2.V != 7 {
		t.Fatalf("v2=%d want 7 (decoded from cached body)", v2.V)
	}

	if got := atomic.LoadInt64(reqs); got != 2 {
		t.Fatalf("server saw %d reqs; want 2 (both attempts hit the wire even on cache hit)", got)
	}
}
