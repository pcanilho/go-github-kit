# go-github-kit examples

Runnable demo programs for [`github.com/pcanilho/go-github-kit`](https://github.com/pcanilho/go-github-kit).

This directory is a **separate Go module** (its own `go.mod`). The kit itself has no compile-time dependency on `github.com/google/go-github` (`ghkit.New`, `ghkit.NewE` and `ghkit.Adapt` are generic over the returned client type), so the kit's main `go.mod` does not pin `go-github`. The examples here do, because they show concrete usage with `github.NewClient`. Keeping this module separate means a breaking `go-github` major bumps the examples without touching the kit's own dependency surface, and without forcing anything on consumers. These examples currently pin **v90**.

## Examples are copy-paste templates, not directly installable

The committed `replace github.com/pcanilho/go-github-kit => ../` directive points at the local sibling main module so the in-repo CI job builds the examples against the just-changed kit code. **Do not** try to `go install github.com/pcanilho/go-github-kit/examples/<name>@latest`. Go would try to resolve `../` from an unpredictable cache path and the build would fail.

The intended workflow:

```sh
git clone https://github.com/pcanilho/go-github-kit
cd go-github-kit/examples/<name>
go run .
```

When **copying an example into your own project**: drop the `replace` directive from your `go.mod`, and `go get github.com/pcanilho/go-github-kit@latest` (and `go-github` at whichever major you want).

## Examples

| Directory                 | Demonstrates                                                                  |
| ------------------------- | ----------------------------------------------------------------------------- |
| `static-pat/`             | The simplest setup: a static Personal Access Token + the precompute ETag cache. |
| `installation-token/`     | A GitHub App installation token via `oauth2.TokenSource`, with a shared ETag cache scoped per installation. |
| `backfill/`               | Backfill / batch jobs: ETag cache + a client-side requests-per-second cap.    |
| `github-enterprise/`      | Targeting GitHub Enterprise Server via `WithEnterpriseURLs` and a custom user agent. |
| `retry-on-flaky/`         | `WithRetry` with a tuned backoff and a custom predicate that opts POST in via `Idempotency-Key`. |
| `list-all-repos/`         | Walks `/user/repos` with `pages.As[*github.Repository]` over Link headers; demonstrates per-page ETag composition. |
| `poll-workflow-run/`      | Waits for a workflow run to reach `status="completed"` with `polling.As[*github.WorkflowRun]` + `WithDoneT` + `WithMaxWallClock` + `WithJitter`. |
| `search-issues/`          | Walks `/search/issues` with `search.Issues[*github.Issue]`; surfaces `incomplete_results` and the 1000-result cap as `ErrResultCapHit`. |
| `conditional-fetch/`      | Visible 304: `cond.Fetch[*github.Repository]` returns `cond.Unchanged` on the second call so downstream work can be skipped. |
| `graphql-v4/`             | GraphQL v4 via `shurcooL/githubv4`, whose `NewClient` takes the `*http.Client` alone, so it binds directly to `ghkit.New`. |

Each example reads its credentials from environment variables (e.g. `GITHUB_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`) and exercises one or more REST endpoints. They will fail at the API call without valid credentials. That's expected; they're starting templates, not standalone tools.
