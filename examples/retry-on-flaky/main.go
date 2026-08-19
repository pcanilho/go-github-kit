// Retry on transient failures: 5xx, EOF, dial errors. Default policy is
// idempotent-only; this example also shows opting POST in via
// Idempotency-Key.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/go-github/v90/github"
	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/retry"
)

func main() {
	gh, err := ghkit.NewE(func(hc *http.Client) (*github.Client, error) {
		return github.NewClient(github.WithHTTPClient(hc))
	},
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithRetry(
			retry.WithMaxAttempts(5),
			retry.WithBackoff(500*time.Millisecond, 10*time.Second),
			retry.WithRetryOn(func(req *http.Request, resp *http.Response, err error) bool {
				if req.Header.Get("Idempotency-Key") != "" {
					if err != nil {
						return retry.IsTransientNetErr(err)
					}
					return resp != nil && retry.IsRetryable5xx(resp.StatusCode)
				}
				if !retry.IsIdempotent(req.Method) {
					return false
				}
				if err != nil {
					return retry.IsTransientNetErr(err)
				}
				return resp != nil && retry.IsRetryable5xx(resp.StatusCode)
			}),
		),
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
