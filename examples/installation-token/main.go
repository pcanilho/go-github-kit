// GitHub App installation-token via oauth2.TokenSource, with a shared
// ETag cache scoped per installation. Reads APP_ID, INSTALLATION_ID, and
// the path to the App private key PEM from the environment.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v85/github"
	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/etag"
	"golang.org/x/oauth2"
)

// Adapter from ghinstallation.Transport to oauth2.TokenSource. ghkit does
// not ship this bridge so the root module stays at four runtime deps;
// callers copy these few lines (or substitute ghait for KMS-backed signing).
type ghInstallationTokenSource struct {
	t   *ghinstallation.Transport
	ctx context.Context
}

func (s *ghInstallationTokenSource) Token() (*oauth2.Token, error) {
	tok, err := s.t.Token(s.ctx)
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{AccessToken: tok}, nil
}

func main() {
	appID, err := strconv.ParseInt(os.Getenv("APP_ID"), 10, 64)
	if err != nil {
		log.Fatalf("APP_ID: %v", err)
	}
	installationID, err := strconv.ParseInt(os.Getenv("INSTALLATION_ID"), 10, 64)
	if err != nil {
		log.Fatalf("INSTALLATION_ID: %v", err)
	}
	keyPath := os.Getenv("APP_PRIVATE_KEY_PATH")

	gt, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, appID, installationID, keyPath)
	if err != nil {
		log.Fatalf("ghinstallation: %v", err)
	}

	source := &ghInstallationTokenSource{t: gt, ctx: context.Background()}
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
