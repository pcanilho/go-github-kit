//go:build live

package pages_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/pages"
)

// TestPages_Live_UserRepos walks /user/repos against api.github.com via
// the real ghkit transport stack. It pins the iterator's behaviour
// against actual GitHub Link headers so a server-side change in the
// response shape (header ordering, rel quoting, additional params)
// surfaces here rather than as a silent regression in production.
//
// Hard-fatals on missing GITHUB_TOKEN so the live gate can never be
// silently skipped in CI. For local development:
//
//	GITHUB_TOKEN=$(gh auth token) go test -tags=live -run TestPages_Live ./pages/...
func TestPages_Live_UserRepos(t *testing.T) {
	tok := os.Getenv("GITHUB_TOKEN")
	if tok == "" {
		t.Fatal("GITHUB_TOKEN is required for the live pagination check; this gate is intentionally non-skippable.\n" +
			"For local runs:\n" +
			"    GITHUB_TOKEN=$(gh auth token) go test -tags=live -run TestPages_Live ./pages/...\n")
	}

	hc, err := ghkit.HTTPClient(
		ghkit.WithToken(tok),
		ghkit.WithETagCache(),
		ghkit.WithUserAgent("go-github-kit-live-pages-check"),
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

	var pagesWalked int
	var totalItems int
	for resp, err := range pages.Pages(ctx, hc, "GET", "https://api.github.com/user/repos?per_page=1", headers) {
		if err != nil {
			t.Fatalf("walk error on page %d: %v", pagesWalked+1, err)
		}
		if resp.StatusCode != 200 && resp.StatusCode != 304 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			t.Fatalf("page %d: status %d body=%q", pagesWalked+1, resp.StatusCode, body)
		}
		var arr []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
			_ = resp.Body.Close()
			t.Fatalf("page %d decode: %v", pagesWalked+1, err)
		}
		_ = resp.Body.Close()
		totalItems += len(arr)
		pagesWalked++
		// Cap the walk at 5 pages so the test does not hammer api.github.com
		// for accounts with thousands of repos.
		if pagesWalked >= 5 {
			break
		}
	}

	if pagesWalked == 0 {
		t.Fatal("no pages walked; live endpoint may be down or the account has no repos")
	}
	t.Logf("walked %d pages, %d items via Link header", pagesWalked, totalItems)
}
