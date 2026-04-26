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

	"github.com/google/go-github/v85/github"
	ghkit "github.com/pcanilho/go-github-kit"
)

func main() {
	token := os.Getenv("GITHUB_ENTERPRISE_TOKEN")

	var factoryErr error
	gh, err := ghkit.New(func(hc *http.Client) *github.Client {
		c, ghErr := github.NewClient(hc).WithEnterpriseURLs(
			"https://github.example.com/api/v3/",
			"https://github.example.com/api/uploads/",
		)
		if ghErr != nil {
			factoryErr = ghErr
			return nil
		}
		c.UserAgent = "my-app/1.0"
		return c
	}, ghkit.WithToken(token))
	if err != nil {
		log.Fatalf("ghkit.New: %v", err)
	}
	if factoryErr != nil {
		log.Fatalf("WithEnterpriseURLs: %v", factoryErr)
	}
	if gh == nil {
		log.Fatal("enterprise client construction failed (see factory error)")
	}

	repo, _, err := gh.Repositories.Get(context.Background(), "internal", "service-x")
	if err != nil {
		log.Fatalf("Repositories.Get: %v", err)
	}
	fmt.Println(repo.GetFullName())
}
