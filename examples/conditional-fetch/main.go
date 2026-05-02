// Conditional GET with visible 304: the second call returns
// cond.Unchanged so downstream work (parse, diff, DB write,
// notification fan-out) can be skipped.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/google/go-github/v85/github"
	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/cond"
)

func main() {
	hc, err := ghkit.HTTPClient(
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithETagCache(),
		ghkit.WithUserAgent("ghkit-conditional-fetch/1.0"),
	)
	if err != nil {
		log.Fatalf("ghkit.HTTPClient: %v", err)
	}

	url := "https://api.github.com/repos/google/go-github"
	decode := func(r io.Reader) (*github.Repository, error) {
		var v github.Repository
		err := json.NewDecoder(r).Decode(&v)
		return &v, err
	}

	for i := 1; i <= 2; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")

		repo, status, err := cond.Fetch(context.Background(), hc, req, decode)
		if err != nil {
			log.Fatalf("call %d: %v", i, err)
		}
		fmt.Printf("call %d: %s status=%s\n", i, repo.GetFullName(), status)
		if status == cond.Unchanged {
			fmt.Println("  -> body unchanged; skipping downstream work")
		}
	}
}
