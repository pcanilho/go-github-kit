// GraphQL/v4 via shurcooL/githubv4 over a ghkit-built *http.Client.
// The etag layer no-ops on POST so v4 traffic flows through oauth2 +
// retry + ratelimit + throttle + UA without ETag caching.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/shurcooL/githubv4"
)

func main() {
	v4, err := ghkit.New(githubv4.NewClient,
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithRetry(),
		ghkit.WithUserAgent("ghkit-graphql-example/1.0"),
	)
	if err != nil {
		log.Fatalf("ghkit.New: %v", err)
	}

	var query struct {
		Viewer struct {
			Login githubv4.String
		}
	}
	if err := v4.Query(context.Background(), &query, nil); err != nil {
		log.Fatalf("v4.Query: %v", err)
	}
	fmt.Println(query.Viewer.Login)
}
