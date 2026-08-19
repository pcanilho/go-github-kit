//go:build live

package etag

import (
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestETag_Live_DriftCheck hits api.github.com on a handful of public endpoints
// and asserts our precompute hash matches the server-issued ETag byte-for-byte.
// If this test starts failing, GitHub's reverse-engineered algorithm has
// drifted and the library needs an update.
//
// Hard-fatals on missing GITHUB_TOKEN so the drift gate can never be silently
// skipped in CI. For local development, run:
//
//	GITHUB_TOKEN=$(gh auth token) go test -tags=live -run TestETag_Live ./etag/...
//
// The test uses a bare *http.Transport clone with DisableCompression=true so
// we see the same pre-compression bytes GitHub hashes.
func TestETag_Live_DriftCheck(t *testing.T) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		t.Fatal("GITHUB_TOKEN is required for the live drift check; this gate is intentionally non-skippable.\n" +
			"CI sets this automatically. For local runs:\n" +
			"    GITHUB_TOKEN=$(gh auth token) go test -tags=live -run TestETag_Live ./etag/...\n")
	}

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DisableCompression = true
	// Cap per-request time so a slow api.github.com does not hang the suite
	// for Go's default 10-minute test timeout.
	client := &http.Client{Transport: base, Timeout: 15 * time.Second}

	probes := []string{
		"https://api.github.com/users/octocat",
		"https://api.github.com/orgs/github",
		"https://api.github.com/meta",
		"https://api.github.com/repos/octocat/Spoon-Knife/commits/bb4cc8d3b2e14b3af5df699876dd4ff3acd00b7f",
	}

	for _, url := range probes {
		t.Run(url, func(t *testing.T) {
			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept", "application/vnd.github.v3+json")
			req.Header.Set(headerAuthorization, "token "+tok)
			req.Header.Set("User-Agent", "go-github-kit-drift-check")

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("non-200 (%d); drift check needs a live public endpoint", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Encoding"); got == "gzip" {
				t.Fatal("response was gzip-compressed; DisableCompression not honoured; hash domain would be wrong")
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			serverETag := resp.Header.Get("ETag")
			if serverETag == "" {
				t.Fatal("server returned no ETag header")
			}
			theirs := NormaliseETag(serverETag)
			ours := ComputeExpectedETag(req.Header, ParseVary(resp.Header), body)
			if ours != theirs {
				t.Fatalf("ETag drift detected! ours=%q theirs=%q (body_len=%d, vary=%v)",
					ours, theirs, len(body), ParseVary(resp.Header))
			}
		})
	}
}
