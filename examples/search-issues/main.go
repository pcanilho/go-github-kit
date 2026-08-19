// Search GitHub issues with envelope-aware pagination, surfacing
// IncompleteResults and the 1000-result cap.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/google/go-github/v90/github"
	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/search"
)

func main() {
	q := os.Getenv("Q")
	if q == "" {
		q = "is:open is:issue label:good-first-issue"
	}

	hc, err := ghkit.HTTPClient(
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithETagCache(),
		ghkit.WithUserAgent("ghkit-search-issues/1.0"),
	)
	if err != nil {
		log.Fatalf("ghkit.HTTPClient: %v", err)
	}

	var (
		seen    int
		capHit  bool
		anyPart bool
		lastTC  int
	)
	for r, err := range search.Issues[*github.Issue](
		context.Background(), hc, q,
		search.WithPerPage(100),
		search.WithSort("updated"),
		search.WithOrder("desc"),
	) {
		if err != nil {
			if errors.Is(err, search.ErrResultCapHit) {
				capHit = true
				break
			}
			log.Fatalf("search: %v", err)
		}
		seen++
		lastTC = r.TotalCount
		if r.IncompleteResults {
			anyPart = true
		}
		if seen <= 5 {
			fmt.Printf("[%d/%d] %s\n", seen, r.TotalCount, r.Item.GetHTMLURL())
		}
		if seen >= 25 {
			break
		}
	}
	fmt.Printf("walked %d items of total %d (cap_hit=%v incomplete=%v)\n",
		seen, lastTC, capHit, anyPart)
}
