// List every repository the authenticated user can see, walking the
// Link header via the pages sub-package. Demonstrates how the iterator
// composes with WithETagCache: a second run over the same account
// returns 304s on every page, so total bytes drop to near zero while
// the iterator still yields every repository.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/google/go-github/v90/github"
	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/pages"
)

func main() {
	hc, err := ghkit.HTTPClient(
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithETagCache(),
		ghkit.WithUserAgent("ghkit-list-all-repos/1.0"),
	)
	if err != nil {
		log.Fatalf("ghkit.HTTPClient: %v", err)
	}

	headers := http.Header{
		"Accept":               []string{"application/vnd.github+json"},
		"X-GitHub-Api-Version": []string{"2022-11-28"},
	}

	var n int
	var sample []string
	for repo, err := range pages.As[*github.Repository](
		context.Background(), hc, "GET",
		"https://api.github.com/user/repos?per_page=100",
		headers,
	) {
		if err != nil {
			log.Fatalf("pages.As: %v", err)
		}
		n++
		if len(sample) < 3 {
			sample = append(sample, repo.GetFullName())
		}
	}

	fmt.Printf("listed %d repositories\n", n)
	for _, name := range sample {
		fmt.Println("  -", name)
	}
}
