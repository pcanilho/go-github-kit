// GitHub App installation-token via oauth2.TokenSource, with a shared ETag
// cache scoped per installation. In production, replace the StaticTokenSource
// with ghinstallation (or similar) so it refreshes the installation token.
package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/google/go-github/v85/github"
	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
	"golang.org/x/oauth2"
)

func main() {
	// Stand-in: replace with ghinstallation.NewAppsTransport or similar.
	source := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "example"})
	var installationID int64 = 12345

	sharedCache := etag.NewLRUCache(8192)
	hc, err := ghkit.HTTPClient(
		ghkit.WithTokenSource(source),
		ghkit.WithETagCache(
			etag.WithCache(sharedCache),
			etag.WithKeyScope(strconv.FormatInt(installationID, 10)),
		),
		ghkit.WithTimeout(5*time.Second),
	)
	if err != nil {
		log.Fatalf("ghkit.HTTPClient: %v", err)
	}

	gh := github.NewClient(hc)
	repo, _, err := gh.Repositories.Get(context.Background(), "google", "go-github")
	if err != nil {
		log.Fatalf("Repositories.Get: %v", err)
	}
	fmt.Println(repo.GetFullName())
}
