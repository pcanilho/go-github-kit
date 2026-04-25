package ghkit_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/go-github/v85/github"
	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
	"golang.org/x/oauth2"
)

// Example_staticPAT is the simplest shape: a Personal Access Token plus the
// default precompute-mode ETag cache. ghkit.New is generic over the returned
// type; passing github.NewClient lets type inference pick up *github.Client
// without ghkit itself depending on any specific go-github major.
func Example_staticPAT() {
	gh, err := ghkit.New(github.NewClient,
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithETagCache(),
	)
	if err != nil {
		fmt.Println("construct:", err)
		return
	}
	if _, _, err = gh.Repositories.Get(context.Background(), "google", "go-github"); err != nil {
		fmt.Println("get:", err)
	}
}

// Example_installationToken shows a GitHub App installation-token setup.
// In production, build the oauth2.TokenSource with ghinstallation (or
// similar) so it returns a fresh installation token on each Token() call.
// The precompute ETag cache stays useful across the hourly token refresh
// because we recompute GitHub's server-side hash client-side with the
// current Authorization header.
func Example_installationToken() {
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
		fmt.Println("construct:", err)
		return
	}
	gh := github.NewClient(hc)
	if _, _, err := gh.Repositories.Get(context.Background(), "google", "go-github"); err != nil {
		fmt.Println("get:", err)
	}
}

// Example_backfill shows the shape used for batch or backfill jobs that want
// a proactive client-side rate cap on top of the reactive limiter.
func Example_backfill() {
	gh, err := ghkit.New(github.NewClient,
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithETagCache(etag.WithCache(etag.NewLRUCache(8192))),
		ghkit.WithRequestsPerSecond(1.3, 1),
	)
	if err != nil {
		fmt.Println("construct:", err)
		return
	}
	if _, _, err := gh.Repositories.Get(context.Background(), "google", "go-github"); err != nil {
		fmt.Println("get:", err)
	}
}

// Example_etagOnly uses only the etag sub-package inside a hand-built
// transport chain.
func Example_etagOnly() {
	rt, err := etag.NewTransport(nil,
		etag.WithCache(etag.NewLRUCache(1024)),
		etag.WithKeyScope("tenant-42"),
	)
	if err != nil {
		fmt.Println("construct:", err)
		return
	}
	hc := &http.Client{Transport: rt}
	resp, err := hc.Get("https://api.github.com/meta")
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	if err := resp.Body.Close(); err != nil {
		fmt.Println("close:", err)
	}
}

// Example_githubEnterprise targets GitHub Enterprise Server. The base and
// upload URLs MUST end with a trailing slash; WithEnterpriseURLs returns
// an error otherwise. We surface the error via a nil client rather than
// silently falling back to github.com, since sending an Enterprise token
// to public github.com is a credential-leak pattern.
func Example_githubEnterprise() {
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
		fmt.Println("ghkit.New:", err)
		return
	}
	if factoryErr != nil {
		fmt.Println("enterprise URLs:", factoryErr)
		return
	}
	if gh == nil {
		fmt.Println("enterprise client construction failed (see factory error)")
		return
	}
	if _, _, err := gh.Repositories.Get(context.Background(), "internal", "service-x"); err != nil {
		fmt.Println("get:", err)
	}
}
