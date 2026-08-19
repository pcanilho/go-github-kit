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

// TestPages_Live_Commits walks a public repository's commits against
// api.github.com, pinning the iterator against real Link headers so a
// server-side shape change surfaces here.
//
// The endpoint must be readable by CI's App installation token. /user/repos
// and /repos/{o}/{r}/stargazers both 403 with "not accessible by
// integration"; commits work, as the etag probe's single-commit URL shows.
//
// Hard-fatals on missing GITHUB_TOKEN. For local development:
//
//	GITHUB_TOKEN=$(gh auth token) go test -tags=live -run TestPages_Live ./pages/...
func TestPages_Live_Commits(t *testing.T) {
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
	for resp, err := range pages.Pages(ctx, hc, "GET", "https://api.github.com/repos/octocat/Spoon-Knife/commits?per_page=1", headers) {
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
		// Cap the walk; Spoon-Knife has many commits.
		if pagesWalked >= 5 {
			break
		}
	}

	if pagesWalked == 0 {
		t.Fatal("no pages walked; live endpoint may be down")
	}
	t.Logf("walked %d pages, %d items via Link header", pagesWalked, totalItems)
}
