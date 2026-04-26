//go:build live

package retry

import (
	"net/http"
	"os"
	"testing"
	"time"
)

// TestRetry_Live exercises the retry transport against api.github.com on a
// public endpoint. The default predicate retries 5xx and transient network
// errors only; a successful 200 should produce a single attempt.
//
// For local runs:
//
//	GITHUB_TOKEN=$(gh auth token) go test -tags=live -run TestRetry_Live ./retry/...
func TestRetry_Live(t *testing.T) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		t.Fatal("GITHUB_TOKEN is required for the live retry check; this gate is intentionally non-skippable.\n" +
			"CI sets this automatically. For local runs:\n" +
			"    GITHUB_TOKEN=$(gh auth token) go test -tags=live -run TestRetry_Live ./retry/...\n")
	}

	rt, err := NewTransport(http.DefaultTransport,
		WithBackoff(200*time.Millisecond, 2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Transport: rt, Timeout: 15 * time.Second}

	req, err := http.NewRequest("GET", "https://api.github.com/users/octocat", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("User-Agent", "go-github-kit-retry-check")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
}
