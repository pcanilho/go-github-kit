// GitHub Enterprise Server via WithEnterpriseURLs and a custom user agent.
// Surfaces an enterprise-URL error rather than falling back to github.com:
// sending an Enterprise token to public github.com is a credential-leak path.
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
	token := os.Getenv("GITHUB_ENTERPRISE_TOKEN")

	// A closure rather than ghkit.Adapt: Adapt passes only the HTTP client,
	// and this client needs go-github options of its own.
	//
	// NewE propagates the constructor error, so a bad enterprise URL stops
	// us rather than yielding a github.com client.
	gh, err := ghkit.NewE(func(hc *http.Client) (*github.Client, error) {
		return github.NewClient(
			github.WithHTTPClient(hc),
			github.WithEnterpriseURLs(
				"https://github.example.com/api/v3/",
				"https://github.example.com/api/uploads/",
			),
			github.WithUserAgent("my-app/1.0"),
		)
	}, ghkit.WithToken(token))
	if err != nil {
		log.Fatalf("ghkit.NewE: %v", err)
	}

	repo, _, err := gh.Repositories.Get(context.Background(), "internal", "service-x")
	if err != nil {
		log.Fatalf("Repositories.Get: %v", err)
	}
	fmt.Println(repo.GetFullName())
}
