package ghtest_test

import (
	"bytes"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/pcanilho/go-github-kit/etag"
	"github.com/pcanilho/go-github-kit/ghtest"
)

func TestETagServer_HitMiss304(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	srv, reqs := ghtest.ETagServer(t, body)

	c := srv.Client()

	// First GET: cold cache, server emits 200 + ETag.
	resp1, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d want 200", resp1.StatusCode)
	}
	if !bytes.Equal(got1, body) {
		t.Fatalf("first body=%q want %q", got1, body)
	}
	tag := resp1.Header.Get("ETag")
	if tag == "" {
		t.Fatal("first response missing ETag")
	}

	// Second GET with matching If-None-Match: server emits 304.
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("If-None-Match", tag)
	resp2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotModified {
		t.Fatalf("second status=%d want 304", resp2.StatusCode)
	}

	if got := atomic.LoadInt64(reqs); got != 2 {
		t.Fatalf("reqs=%d want 2", got)
	}

	// Sanity: the precomputed ETag matches our algorithm's output.
	expected := etag.ComputeExpectedETag(http.Header{}, nil, body)
	if etag.NormaliseETag(tag) != expected {
		t.Fatalf("ETag header %q does not match ComputeExpectedETag %q", tag, expected)
	}
}

func TestETagServer_NoMatch_HitsWire(t *testing.T) {
	body := []byte(`{"k":"v"}`)
	srv, reqs := ghtest.ETagServer(t, body)
	c := srv.Client()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("If-None-Match", `"does-not-match"`)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d want 200", resp.StatusCode)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body=%q want %q", got, body)
	}
	if atomic.LoadInt64(reqs) != 1 {
		t.Fatalf("reqs=%d want 1", *reqs)
	}
}
