// Static PAT + default precompute-mode ETag cache.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/google/go-github/v85/github"
	ghkit "github.com/pcanilho/go-github-kit"
)

func main() {
	gh, err := ghkit.New(github.NewClient,
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithETagCache(),
	)
	if err != nil {
		log.Fatalf("ghkit.New: %v", err)
	}

	repo, _, err := gh.Repositories.Get(context.Background(), "google", "go-github")
	if err != nil {
		log.Fatalf("Repositories.Get: %v", err)
	}
	fmt.Println(repo.GetFullName())
}
