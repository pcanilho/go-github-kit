// Backfill / batch-job shape: ETag cache + a client-side requests-per-second cap.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/google/go-github/v90/github"
	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
)

func main() {
	gh, err := ghkit.NewE(func(hc *http.Client) (*github.Client, error) {
		return github.NewClient(github.WithHTTPClient(hc))
	},
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithETagCache(etag.WithCache(etag.NewLRUCache(8192))),
		ghkit.WithRequestsPerSecond(1.3, 1),
	)
	if err != nil {
		log.Fatalf("ghkit.NewE: %v", err)
	}

	repo, _, err := gh.Repositories.Get(context.Background(), "google", "go-github")
	if err != nil {
		log.Fatalf("Repositories.Get: %v", err)
	}
	fmt.Println(repo.GetFullName())
}
