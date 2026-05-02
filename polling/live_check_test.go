//go:build live

package polling_test

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/polling"
)

// TestPoll_Live_RateLimit polls /rate_limit on api.github.com via the
// real ghkit transport stack. The endpoint is read-only and stable
// (never bit-rots, never 404s), so the test exercises the iterator's
// transport composition, request-header plumbing, and stop-on-done
// path without needing maintainer-curated fixture state.
//
// Hard-fatals on missing GITHUB_TOKEN so the live gate cannot be
// silently skipped in CI. For local runs:
//
//	GITHUB_TOKEN=$(gh auth token) go test -tags=live -run TestPoll_Live ./polling/...
func TestPoll_Live_RateLimit(t *testing.T) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		t.Fatal("GITHUB_TOKEN is required for the live polling check; this gate is intentionally non-skippable.\n" +
			"For local runs:\n" +
			"    GITHUB_TOKEN=$(gh auth token) go test -tags=live -run TestPoll_Live ./polling/...\n")
	}

	hc, err := ghkit.HTTPClient(
		ghkit.WithToken(tok),
		ghkit.WithUserAgent("go-github-kit-live-polling-check"),
		ghkit.WithTimeout(15*time.Second),
	)
	if err != nil {
		t.Fatalf("ghkit.HTTPClient: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	headers := http.Header{
		"Accept":               []string{"application/vnd.github+json"},
		"X-GitHub-Api-Version": []string{"2022-11-28"},
	}

	// Stop on the first successful response: /rate_limit always
	// returns 200 immediately, so the iterator yields once and the
	// predicate fires.
	done := func(resp *http.Response) bool { return resp.StatusCode == 200 }

	var n int
	for resp, err := range polling.Poll(ctx, hc, http.MethodGet,
		"https://api.github.com/rate_limit", headers, nil,
		2*time.Second,
		polling.WithDone(done),
		polling.WithMaxAttempts(2),
	) {
		if err != nil {
			t.Fatalf("polling error on attempt %d: %v", n+1, err)
		}
		_ = resp.Body.Close()
		n++
	}
	if n == 0 {
		t.Fatal("no responses observed; live endpoint may be down")
	}
	if n > 1 {
		t.Fatalf("expected 1 yield (done predicate fires on attempt 1); got %d", n)
	}
	t.Logf("polled rate_limit endpoint %d time(s)", n)
}
