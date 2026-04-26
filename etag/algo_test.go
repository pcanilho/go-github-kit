package etag

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestETag_Hash_Deterministic(t *testing.T) {
	h := http.Header{"Accept": {"application/vnd.github.v3+json"}, "Authorization": {"token abc"}}
	a := ComputeExpectedETag(h, nil, []byte("hello"))
	b := ComputeExpectedETag(h, nil, []byte("hello"))
	if a != b {
		t.Fatalf("want deterministic hash; got %q vs %q", a, b)
	}
}

func TestETag_Hash_VaryFallbackOnEmpty(t *testing.T) {
	h := http.Header{"Accept": {"a"}, "Authorization": {"b"}, "Cookie": {"c"}}
	withNil := ComputeExpectedETag(h, nil, []byte("body"))
	withAll := ComputeExpectedETag(h, []string{"Accept", "Authorization", "Cookie"}, []byte("body"))
	if withNil != withAll {
		t.Fatalf("nil vary should equal explicit full list; %q vs %q", withNil, withAll)
	}
}

func TestETag_Hash_DifferentAuthProducesDifferentHash(t *testing.T) {
	h1 := http.Header{"Authorization": {"token-1"}}
	h2 := http.Header{"Authorization": {"token-2"}}
	if ComputeExpectedETag(h1, nil, []byte("x")) == ComputeExpectedETag(h2, nil, []byte("x")) {
		t.Fatal("different auth must produce different hash")
	}
}

func TestETag_Hash_UnknownVaryHeaderWritesEmptyValue(t *testing.T) {
	h := http.Header{"X-Custom": {"value"}}
	// Vary naming "X-Custom" but the canonical list doesn't include it;
	// hash should be stable regardless of X-Custom presence.
	a := ComputeExpectedETag(h, []string{"X-Custom"}, []byte("b"))
	b := ComputeExpectedETag(http.Header{}, []string{"X-Custom"}, []byte("b"))
	if a != b {
		t.Fatalf("unknown Vary names must not affect hash: %q vs %q", a, b)
	}
}

func TestETag_NormaliseETag_StripsWeakAndQuotes(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"abc"`, "abc"},
		{`W/"abc"`, "abc"},
		{`abc`, "abc"},
		{` W/"abc"`, "abc"}, // leading whitespace must be trimmed
	}
	for _, c := range cases {
		if got := NormaliseETag(c.in); got != c.want {
			t.Errorf("NormaliseETag(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestETag_ParseVary_OrdersAndDedups(t *testing.T) {
	h := http.Header{}
	h.Add("Vary", "Accept, Authorization")
	h.Add("Vary", "Cookie")
	got := ParseVary(h)
	want := []string{"Accept", "Authorization", "Cookie"}
	if len(got) != len(want) {
		t.Fatalf("ParseVary len = %d; want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ParseVary[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

func TestETag_ParseVary_SkipsStarAndEmpty(t *testing.T) {
	h := http.Header{"Vary": {"*"}}
	if got := ParseVary(h); got != nil {
		t.Fatalf("Vary: * should produce nil; got %v", got)
	}
	h = http.Header{"Vary": {","}}
	if got := ParseVary(h); got != nil {
		t.Fatalf("Vary: (comma only) should produce nil; got %v", got)
	}
}

func TestETag_Cacheable(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		header http.Header
		want   bool
	}{
		{"GET cacheable", "GET", "/users/octocat", nil, true},
		{"HEAD cacheable", "HEAD", "/users/octocat", nil, true},
		{"POST not cacheable", "POST", "/users/octocat", nil, false},
		{"PUT not cacheable", "PUT", "/x", nil, false},
		{"Range request not cacheable", "GET", "/x", http.Header{"Range": {"bytes=0-100"}}, false},
		{"/rate_limit not cacheable", "GET", "/rate_limit", nil, false},
		{"/api/v3/rate_limit not cacheable", "GET", "/api/v3/rate_limit", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "https://api.github.com"+c.path, nil)
			for k, vs := range c.header {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			if got := cacheable(req); got != c.want {
				t.Errorf("cacheable = %v; want %v", got, c.want)
			}
		})
	}
}

func TestETag_Cacheable_RespectsNoStore(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Cache-Control": {"no-store"}}}
	if cacheableResponse(resp) {
		t.Fatal("no-store must not be cacheable")
	}
	resp = &http.Response{Header: http.Header{"Cache-Control": {"public, no-store, max-age=60"}}}
	if cacheableResponse(resp) {
		t.Fatal("no-store in multi-value Cache-Control must not be cacheable")
	}
	resp = &http.Response{Header: http.Header{"Cache-Control": {"public, max-age=60"}}}
	if !cacheableResponse(resp) {
		t.Fatal("cache-control without no-store should be cacheable")
	}
}

func TestETag_Cacheable_RespectsVaryStar(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Vary": {"*"}}}
	if cacheableResponse(resp) {
		t.Fatal("Vary: * must not be cacheable")
	}
	resp = &http.Response{Header: http.Header{"Vary": {"Accept, *"}}}
	if cacheableResponse(resp) {
		t.Fatal("Vary: * in multi-value header must not be cacheable")
	}
}

func TestETag_VaryHeaders_ReturnsCopy(t *testing.T) {
	a := VaryHeaders()
	a[0] = "MUTATED"
	b := VaryHeaders()
	if b[0] == "MUTATED" {
		t.Fatal("VaryHeaders must return an immutable snapshot")
	}
}

func TestETag_NormalisePath(t *testing.T) {
	cases := map[string]string{
		"/repos/google/go-github":                        "/repos/{o}/{r}",
		"/repos/google/go-github/commits/abc1234567":     "/repos/{o}/{r}/commits/{sha}",
		"/repos/google/go-github/compare/main...feature": "/repos/{o}/{r}/compare/{base...head}",
		"/users/octocat":                                 "/users/{u}",
		"/orgs/github":                                   "/orgs/{o}",
		"/app/installations/12345":                       "/app/installations/{id}",
		"/meta":                                          "/meta",
		"/gists/1234":                                    "/gists/_", // unknown-route fallback
		"/unmapped":                                      "unknown",
	}
	for in, want := range cases {
		if got := normalisePath(in); got != want {
			t.Errorf("normalisePath(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestETag_ComputeExpectedETag_GoldenShape pins the structural property
// that ComputeExpectedETag returns 64-char hex (SHA256). Byte equality
// against the real server is gated by the -tags=live tests.
func TestETag_ComputeExpectedETag_GoldenShape(t *testing.T) {
	got := ComputeExpectedETag(http.Header{"Accept": {"application/json"}}, nil, []byte("hello"))
	if len(got) != 64 {
		t.Fatalf("expected 64-char hex, got %d: %q", len(got), got)
	}
	// Check it's lowercase hex.
	if strings.ToLower(got) != got {
		t.Fatalf("expected lowercase hex, got %q", got)
	}
}
