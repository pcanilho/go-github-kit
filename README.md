# `ghkit`

A small Go toolkit that wraps [`github.com/google/go-github`](https://github.com/google/go-github) (REST), [`github.com/shurcooL/githubv4`](https://github.com/shurcooL/githubv4) (GraphQL), or any `func(*http.Client) T` client factory with ETag caching, reactive rate limiting, and a client-side token bucket. Opt into what you need; compose the rest yourself.

[![CI](https://github.com/pcanilho/go-github-kit/actions/workflows/ci.yml/badge.svg)](https://github.com/pcanilho/go-github-kit/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/pcanilho/go-github-kit.svg)](https://pkg.go.dev/github.com/pcanilho/go-github-kit)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## Why?

Most projects that talk to the GitHub API eventually reimplement the same three things: a conditional-request cache so repeated reads stop burning rate-limit quota, the well-known reactive rate limiter from `go-github-ratelimit`, and a client-side throttle for jobs that want a hard cap. This kit packages those behind one options-pattern constructor so you can pick them up together, or import the sub-packages a la carte if you already have one and just want the others.

The headline feature is the ETag layer. GitHub's server-side ETag hash includes the `Authorization` header, which means a passive store-and-forward cache falls apart the moment your token rotates. That happens on a fixed 60-minute cadence under GitHub App installation tokens, and on whatever schedule you set for fine-grained PATs. The kit reproduces GitHub's hash client-side so cached entries keep working across rotations and your 304 hit rate stays high.

## Install or Update

```sh
go get -u github.com/pcanilho/go-github-kit
```

## Quick start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/google/go-github/v85/github"
    ghkit "github.com/pcanilho/go-github-kit"
)

func main() {
    gh, err := ghkit.New(github.NewClient,
        ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
        ghkit.WithETagCache(),
    )
    if err != nil {
        log.Fatal(err)
    }
    repo, _, err := gh.Repositories.Get(context.Background(), "google", "go-github")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(repo.GetFullName())
}
```

`ghkit.New` is generic over the returned type; passing `github.NewClient` lets type inference pick up `*github.Client`. ghkit itself has zero dependency on `go-github`. It isn't in `go.mod`, isn't imported, and won't end up in your compiled binary unless you pull it in yourself. Pass whichever go-github major (or any other `func(*http.Client) T` factory) you want.

For runnable starter programs, see [`examples/`](examples/): `static-pat`, `installation-token`, `graphql-v4`, `backfill`, `github-enterprise`, and `retry-on-flaky` are each a complete `main()` you can copy-paste.

## How?

```
http.Client
 Throttle              (x/time/rate proactive)       [WithRequestsPerSecond]
  RateLimit            (go-github-ratelimit v2)      [default ON]
   Retry               (5xx + transient net errors)  [WithRetry]
    oauth2.Transport   (clones req, sets Auth)       [WithToken/WithTokenSource]
     ETag              (hashes auth'd clone)         [WithETagCache]
      Base             (*http.Transport,
                        DisableCompression=true)     [WithBaseTransport]
```

Each layer is optional. The stack is opt-in: `ghkit.HTTPClient(...)` only includes the layers you asked for. The order is load-bearing, though. ETag sits below the oauth2 layer so it hashes the request with the current Authorization header. Rate limiting sits above so it sees every outgoing call including ETag-triggered conditional GETs. Retry sits below RateLimit so 429s are deferred to the rate-limit layer; sits above oauth2 so retried requests get the latest token via oauth2's per-call `Source.Token()`. The proactive throttle sits outermost so it caps issued RPS regardless of cache replays.

The rate-limit layer's named options (`WithPrimaryLimitDetected`, `WithSecondaryLimitDetected`, `WithTotalSleepLimit`, `WithLogger`) cover the common callbacks. For upstream features ghkit does not curate, `ratelimit.WithUpstreamOptions(opts ...any)` forwards raw options to `gofri/go-github-ratelimit/v2`.

## Recipes

<details open>
<summary><b>Recommended setup for a long-lived service</b></summary>

```go
gh, err := ghkit.New(github.NewClient,
    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
    ghkit.WithETagCache(),
    ghkit.WithRetry(),
    ghkit.WithUserAgent("my-app/1.0"),
    ghkit.WithTimeout(30*time.Second),
)
```

Defaults are tuned for steady-state operators: rate-limit on, retry 3 attempts with decorrelated jitter on idempotent 5xx + transient net errors, etag at 4096 entries / 256 MiB. Tune downward via the per-layer options if your workload differs. There is no `RecommendedDefaults()` API on purpose: the constructor defaults are the single source of truth.
</details>

<details>
<summary><b>Static PAT with ETag caching</b></summary>

```go
gh, err := ghkit.New(github.NewClient,
    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
    ghkit.WithETagCache(),
)
```

The default cache is a 4096-entry in-process LRU with a 256 MiB byte budget; safe to run in a long-lived process without watching it grow.
</details>

<details>
<summary><b>GraphQL with <code>shurcooL/githubv4</code></b></summary>

```go
import (
    "github.com/shurcooL/githubv4"
    ghkit "github.com/pcanilho/go-github-kit"
)

v4, err := ghkit.New(githubv4.NewClient,
    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
    ghkit.WithRetry(),
)
```

`ghkit.New` is generic; `githubv4.NewClient` satisfies `func(*http.Client) *githubv4.Client` and gets oauth2 + retry + ratelimit + throttle + UA from the transport stack. ETag caching is REST-only by design (the etag layer no-ops on POST), so `WithETagCache` is a no-op for v4 traffic; leave it off unless you also issue REST GETs through the same client.

A runnable version lives at [`examples/graphql-v4/`](examples/graphql-v4/main.go).
</details>

<details>
<summary><b>GitHub App installation tokens (JIT auth, shared cache)</b></summary>

```go
import (
    ghkit "github.com/pcanilho/go-github-kit"
    "github.com/pcanilho/go-github-kit/etag"
)

// In production, build this with ghinstallation (plain local-key JWT
// signing) or ghait (KMS-backed signing via AWS/GCP/Azure/Vault, selected
// via build tags so you only pull in the SDK you actually use). Both vend
// a fresh installation token on each Token() call; the transport picks up
// the new value per request.
var source oauth2.TokenSource // = ghinstallation.New(...) or wrap ghait.NewToken

// One cache shared across all installations in this process.
cache := etag.NewLRUCache(8192)

hc, err := ghkit.HTTPClient(
    ghkit.WithTokenSource(source),
    ghkit.WithETagCache(
        etag.WithCache(cache),
        etag.WithKeyScope(fmt.Sprintf("installation-%d", installationID)),
    ),
    ghkit.WithTimeout(5 * time.Second),
)
if err != nil { return err }
gh := github.NewClient(hc)
```

`WithKeyScope` is required whenever you supply a `Cache` yourself. It namespaces entries so two installations hitting the same URL never read each other's bodies.

Signer options for the `oauth2.TokenSource`:
- [`bradleyfalzon/ghinstallation`](https://github.com/bradleyfalzon/ghinstallation) for local-key JWT signing (the common default).
- [`isometry/ghait`](https://github.com/isometry/ghait) for KMS-backed signing (AWS, GCP, Azure, Vault, or a local file). Each KMS provider is behind a build tag so you only pull the SDK you use.

<details>
<summary><i>ghait adapter</i></summary>

`ghait.NewGHAIT` returns a factory whose `NewToken(ctx)` mints an `*InstallationToken`. Adapt it to `oauth2.TokenSource` and wrap with `oauth2.ReuseTokenSource` so you mint one token per hour, not per request:

```go
type ghaitSource struct {
    ctx     context.Context
    factory ghait.TokenFactory
}

func (s *ghaitSource) Token() (*oauth2.Token, error) {
    t, err := s.factory.NewToken(s.ctx)
    if err != nil {
        return nil, err
    }
    return &oauth2.Token{AccessToken: t.GetToken(), Expiry: t.GetExpiresAt().Time}, nil
}

// Build with -tags=aws|gcp|azure|vault|file to pull only the SDK you use.
factory, _ := ghait.NewGHAIT(ctx, ghait.NewConfig(appID, installationID, "aws", keyRef))
source := oauth2.ReuseTokenSource(nil, &ghaitSource{ctx: ctx, factory: factory})
```

Pass `source` to `ghkit.WithTokenSource` as in the recipe above.
</details>
</details>

<details>
<summary><b>Multi-tenant single client (one Transport, many installations)</b></summary>

```go
import (
    "github.com/pcanilho/go-github-kit/etag"
)

type tenantKey struct{}

cache := etag.NewLRUCache(8192)

hc, err := ghkit.HTTPClient(
    ghkit.WithTokenSource(perTenantTokenSource), // resolves token per req.Context()
    ghkit.WithETagCache(
        etag.WithCache(cache),
        etag.WithAutoKeyScope(func(req *http.Request) (string, error) {
            id, ok := req.Context().Value(tenantKey{}).(string)
            if !ok || id == "" {
                return "", fmt.Errorf("tenant id missing from request context")
            }
            return id, nil
        }),
    ),
)
```

Use `WithAutoKeyScope` instead of `WithKeyScope` when one `*http.Client` serves N tenants (typical for warm Lambda pools fronting many GitHub App installations). The fn is invoked per request and must return either a non-empty scope or a non-nil error; an empty string with a nil error surfaces as `etag.ErrEmptyScope`. `WithKeyScope` and `WithAutoKeyScope` are mutually exclusive.
</details>

<details>
<summary><b>Backfill shape with a proactive RPS cap</b></summary>

```go
gh, err := ghkit.New(github.NewClient,
    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
    ghkit.WithETagCache(etag.WithCache(etag.NewLRUCache(8192))),
    ghkit.WithRequestsPerSecond(1.3, 1),
)
```

`WithRequestsPerSecond` is a standard `x/time/rate` token bucket. It adds a client-side cap on top of the reactive limiter, which is useful for batch jobs that want predictable pacing under sustained load.
</details>

<details>
<summary><b>GitHub Enterprise Server</b></summary>

```go
gh, err := ghkit.New(func(hc *http.Client) *github.Client {
    c, ghErr := github.NewClient(hc).WithEnterpriseURLs(
        "https://github.example.com/api/v3/",
        "https://github.example.com/api/uploads/",
    )
    if ghErr != nil {
        return github.NewClient(hc) // fall back to github.com on a bad URL
    }
    c.UserAgent = "my-app/1.0"
    return c
}, ghkit.WithToken(os.Getenv("GITHUB_ENTERPRISE_TOKEN")))
```

`WithEnterpriseURLs` requires both URLs to end with a trailing slash and returns an error otherwise. `UserAgent` can also be set at the transport level via `ghkit.WithUserAgent("my-app/1.0")`, which applies to every outbound request regardless of which SDK you wrap around `HTTPClient()`.
</details>

<details>
<summary><b>Retry on transient failures (5xx, network errors)</b></summary>

```go
gh, err := ghkit.New(github.NewClient,
    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
    ghkit.WithRetry(), // 3 attempts, 200ms..2s decorrelated jitter, idempotent methods only
)
```

Tuned policy with POST opt-in via `Idempotency-Key`:

```go
import "github.com/pcanilho/go-github-kit/retry"

gh, err := ghkit.New(github.NewClient,
    ghkit.WithToken(token),
    ghkit.WithRetry(
        retry.WithMaxAttempts(5),
        retry.WithBackoff(500*time.Millisecond, 10*time.Second),
        retry.WithRetryOn(func(req *http.Request, resp *http.Response, err error) bool {
            // Retry POST/PATCH when the caller asserted idempotency.
            if req.Header.Get("Idempotency-Key") != "" {
                if err != nil { return retry.IsTransientNetErr(err) }
                return resp != nil && retry.IsRetryable5xx(resp.StatusCode)
            }
            // Otherwise the default behaviour.
            if !retry.IsIdempotent(req.Method) { return false }
            if err != nil { return retry.IsTransientNetErr(err) }
            return resp != nil && retry.IsRetryable5xx(resp.StatusCode)
        }),
    ),
)
```

POST/PATCH retries with a body require `req.GetBody`; `http.NewRequest` only sets it for `*bytes.Buffer`, `*bytes.Reader`, and `*strings.Reader`. For other readers, set it manually so the retry layer can rewind on attempt 2+.

429 is hard-excluded regardless of the predicate so `ratelimit` (the layer above) owns it. `Retry-After` is honored when present; if it exceeds `maxDelay` the call returns `(nil, retry.ErrRetryAfterExceedsMax)` and the transport has already drained and closed the prior response.
</details>

<details>
<summary><b>Use only the etag sub-package in a hand-built stack</b></summary>

```go
import "github.com/pcanilho/go-github-kit/etag"

rt, err := etag.NewTransport(nil, // nil = default base with DisableCompression=true
    etag.WithCache(etag.NewLRUCache(1024)),
    etag.WithKeyScope("tenant-42"),
)
if err != nil { return err }
hc := &http.Client{Transport: rt}
gh := github.NewClient(hc)
```
</details>

## Testing your code

The `ghtest` sub-package provides two helpers for the GitHub-specific
traps in writing tests: secondary-rate-limit classification and the
bored-engineer ETag hash domain. See [`TESTING.md`](TESTING.md) for the
full recipe set.

## Migrating from an in-tree GitHub transport

If your repo already has a hand-rolled `oauth2.Transport` + `go-github-ratelimit` + custom ETag transport stack, [`MIGRATION.md`](MIGRATION.md) maps the most common shapes (Kubernetes operator, multi-installation webhook processor, backfill job) to ghkit's options API with concrete before/after snippets and notes on behavioral differences worth checking before the swap.

## How the ETag layer works

GitHub's server-side ETag hash includes the `Authorization` header. Store the server's ETag and send it back on the next request, and you get near-zero hit rate the moment the token rotates: the server-side hash has moved, the cached ETag no longer matches, and every request goes through as a full 200. That's the default state for anyone running GitHub App installation tokens.

The precompute trick, reverse-engineered by [bored-engineer](https://github.com/bored-engineer/github-conditional-http-transport), is to reproduce that hash client-side at request time using the current Authorization header. The cached body stays valid across rotations; `If-None-Match` is recomputed on the fly. Hit rate stays high, quota savings become durable, and GitHub Apps actually benefit from caching instead of fighting it.

The algorithm walkthrough lives at <https://www.bored-engineer.com/posts/github-etag-algorithm/>.

**What happens when GitHub changes the algorithm.** Every cacheable 200 is validated: the transport recomputes the expected ETag and compares it to the server's. After 10 mismatches inside a 60-second window, the transport silently switches to sending the server's stored ETag as `If-None-Match` -- 304s resume on stable bodies, you pay at most one extra miss per URL when the algorithm changes. After a 1-hour cooldown, the transport probes back to precompute on a small fraction of requests; consecutive successes restore precompute mode automatically, so a transient drift blip doesn't permanently degrade a long-running process. Wire `etag.WithDriftDetected(...)` for an alert hook on each state transition; call `(*etag.Transport).Stats()` for `/healthz` or dashboard polling. The fallback itself is unconditional and has no public knob -- this is by design.

What this kit adds on top of the original idea:

- A bounded in-process `Cache` (LRU) as the default backend.
- Multi-tenant safety via `etag.WithKeyScope(...)` so one cache can be shared across installations without cross-tenant leaks.
- A live drift probe against `api.github.com` in CI, so the day GitHub changes the algorithm, we know within one CI run.
- Sanitised structured logging with a strict field allowlist (no header values, no hash prefixes, no auth lengths).

## BYO storage

`etag.Cache` is a three-method interface that takes a context on every call so network-backed backends (Redis, S3, etc.) can honour deadlines and cancellation:

```go
type Cache interface {
    Get(ctx context.Context, key string) (Entry, bool, error)
    Add(ctx context.Context, key string, e Entry) error
    Remove(ctx context.Context, key string) error
}
```

The kit ships `etag.NewLRUCache(size)` as the only built-in (in-process, memory-bounded, ignores the context because there's no network I/O to cancel). Swap it for Redis, bbolt, S3, or anything else by implementing the interface. bored-engineer's repo has backend examples (`memory`, `bbolt`, `pebble`, `redis`, `s3`) you can adapt. This kit deliberately doesn't ship those itself so you don't pay for five dependency trees on day one. Open an issue when a backend shape becomes common enough to standardise.

## Things worth knowing

Gzip has to be disabled on the underlying transport, otherwise the hash domain diverges from what GitHub signed. The default base is a clone of `http.DefaultTransport` with `DisableCompression=true`; if you pass your own base via `WithBaseTransport` and it isn't an `*http.Transport`, construction fails with an explicit error rather than silently miscomputing every hash.

`etag.WithKeyScope` is required the moment you share a cache across identities, static PAT or JIT alike. Two callers hitting the same URL under different auth without a scope would race their bodies into the same key, and the library refuses to guess which one wins. Use the installation ID, a per-app scope, or any opaque string.

Each retry attempt is a real HTTP call from the throttle layer's perspective. `WithRetry(retry.WithMaxAttempts(5))` combined with `WithRequestsPerSecond(2, 1)` means a worst-case failing request can briefly use 5x your nominal RPS budget. Size accordingly or leave `maxAttempts` at the default (3).

## Using a different go-github version

The kit has no compile-time pin on `go-github`. Its main `go.mod` does not require `github.com/google/go-github`, so you choose the major. Two equally valid shapes:

**Generic factory** (when you want type inference to pick up `*github.Client`):

```go
import githubX "github.com/google/go-github/vX/github"

gh, err := ghkit.New(githubX.NewClient,
    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
    ghkit.WithETagCache(),
)
```

**Library-agnostic** (when you want the `*http.Client` and will wire your own client library):

```go
import githubX "github.com/google/go-github/vX/github"

hc, err := ghkit.HTTPClient(
    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
    ghkit.WithETagCache(),
)
gh := githubX.NewClient(hc)
```

The runnable demos under [`examples/`](examples/) live in their own sub-module and pin a specific go-github version (currently `v85`) so the kit's main `go.mod` stays clean across go-github upgrades.

## Development

```sh
make test       # go test -race ./...
make test-unit  # short tests only
make test-live  # the live ETag drift probe (needs GITHUB_TOKEN)
make test-fuzz  # fuzz the ETag hash for 30s
make lint       # golangci-lint v2
make vuln       # govulncheck on the module
make bench      # write benchmarks to dist/bench-current.txt
```
