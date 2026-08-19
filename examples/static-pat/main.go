// Static PAT + default precompute-mode ETag cache.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/google/go-github/v90/github"
	ghkit "github.com/pcanilho/go-github-kit"
)

func main() {
	gh, err := ghkit.NewE(func(hc *http.Client) (*github.Client, error) {
		return github.NewClient(github.WithHTTPClient(hc))
	},
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithETagCache(),
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
