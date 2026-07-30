// Wait for a GitHub Actions workflow run to reach status="completed",
// then print its conclusion. Demonstrates polling.As[*github.WorkflowRun]
// with WithDoneT (typed predicate on the decoded value), WithMaxWallClock
// (wall-clock budget that wraps context.DeadlineExceeded), and WithJitter
// (deterministic mid-point clamp).
//
// Reads OWNER, REPO, RUN_ID from the environment. Pair with
// WithETagCache to skip wire round-trips on unchanged poll responses;
// add polling.WithChangeOnly to silently skip yields when the cache
// signals the run hasn't changed since last tick.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/go-github/v85/github"
	ghkit "github.com/pcanilho/go-github-kit"
	"github.com/pcanilho/go-github-kit/polling"
	"github.com/pcanilho/go-github-kit/retry"
)

func main() {
	owner := os.Getenv("OWNER")
	repo := os.Getenv("REPO")
	runID, err := strconv.ParseInt(os.Getenv("RUN_ID"), 10, 64)
	if err != nil || owner == "" || repo == "" {
		log.Fatal("set OWNER, REPO, RUN_ID")
	}

	// retry.WithMaxAttempts(1) so retry does not compound polling's
	// outer loop on transient flakes; polling owns the loop here.
	hc, err := ghkit.HTTPClient(
		ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
		ghkit.WithRetry(retry.WithMaxAttempts(1)),
		ghkit.WithETagCache(),
		ghkit.WithUserAgent("ghkit-poll-workflow-run/1.0"),
	)
	if err != nil {
		log.Fatalf("ghkit.HTTPClient: %v", err)
	}

	run, err := waitForRun(hc, owner, repo, runID)
	if err != nil {
		if errors.Is(err, polling.ErrMaxWallClockExceeded) {
			fmt.Fprintln(os.Stderr, "workflow did not finish within budget")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "poll: %v\n", err)
		os.Exit(1)
	}
	if run == nil {
		log.Fatal("no responses observed")
	}
	fmt.Printf("run %d: status=%s conclusion=%s\n",
		run.GetID(), run.GetStatus(), run.GetConclusion())
}

// waitForRun polls the run until it reports status="completed", the
// wall-clock budget expires, or the request fails. The context lives in
// this function so its cancel runs on return, ahead of any os.Exit in
// main.
func waitForRun(hc *http.Client, owner, repo string, runID int64) (*github.WorkflowRun, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs/%d",
		owner, repo, runID)
	headers := http.Header{
		"Accept":               []string{"application/vnd.github+json"},
		"X-GitHub-Api-Version": []string{"2022-11-28"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()

	seq := polling.As[*github.WorkflowRun](
		ctx, hc, http.MethodGet, url, headers, nil,
		15*time.Second,
		polling.WithDoneT(func(r *github.WorkflowRun) bool {
			return r.GetStatus() == "completed"
		}),
		polling.WithMaxWallClock(30*time.Minute),
		polling.WithJitter(0.2),
	)

	var run *github.WorkflowRun
	for r, err := range seq {
		if err != nil {
			return nil, err
		}
		run = r
	}
	return run, nil
}
