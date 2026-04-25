package etag

import (
	"net/http"
	"strings"
	"testing"
)

// FuzzETag_ComputeExpectedETag asserts the hash function is total
// (does not panic) and deterministic over arbitrary header / vary / body
// inputs. Byte equality is validated separately against the live server.
func FuzzETag_ComputeExpectedETag(f *testing.F) {
	// Seeds: (acceptVals, authVals, cookieVals, varyCSV, body)
	f.Add("application/json", "token abc", "", "", []byte("hello"))
	f.Add("application/vnd.github.v3+json", "Bearer xyz", "s=1", "Accept, Authorization, Cookie", []byte(`{"k":"v"}`))
	f.Add("", "", "", "", []byte{})
	f.Add("a", "b", "c", "Accept", []byte("payload"))
	f.Add("a1\na2", "b1\nb2", "", "Accept, Cookie", []byte("multi-header"))
	f.Add("x", "y", "z", "*", []byte("vary-star"))

	f.Fuzz(func(t *testing.T, accept, auth, cookie, varyCSV string, body []byte) {
		h := http.Header{}
		if accept != "" {
			h["Accept"] = strings.Split(accept, "\n")
		}
		if auth != "" {
			h["Authorization"] = strings.Split(auth, "\n")
		}
		if cookie != "" {
			h["Cookie"] = strings.Split(cookie, "\n")
		}
		var vary []string
		if varyCSV != "" {
			for _, p := range strings.Split(varyCSV, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					vary = append(vary, p)
				}
			}
		}

		// Must not panic.
		a := ComputeExpectedETag(h, vary, body)
		b := ComputeExpectedETag(h, vary, body)
		if a != b {
			t.Fatalf("non-deterministic: %q vs %q", a, b)
		}
		if len(a) != 64 {
			t.Fatalf("expected 64-char hex, got %d", len(a))
		}
	})
}
