# Migrating from in-tree GitHub transports

If your repo already has a hand-rolled stack of `oauth2.Transport`, `go-github-ratelimit`, a custom ETag transport, and a manual `x/time/rate` limiter, this page maps the most common shapes to ghkit's options API. The call sites shrink; the dependency footprint stays the same; the underlying libraries (`gofri/go-github-ratelimit`, `golang.org/x/oauth2`, `x/time/rate`, `hashicorp/golang-lru`) are unchanged.

## What the swap preserves

- **ETag hash algorithm**: ghkit's `etag` package implements the same SHA256-over-Vary-headers algorithm derived from [bored-engineer/github-conditional-http-transport](https://github.com/bored-engineer/github-conditional-http-transport). If you ported that algorithm into your repo, the hash bytes match.
- **Rate-limit error types**: the underlying `*github_primary_ratelimit.RateLimitReachedError` propagates through ghkit's transport chain, so an `errors.As(err, &primary)` in your controller continues to work.
- **DisableCompression on the base transport**: ghkit's default base is a clone of `http.DefaultTransport` with `DisableCompression=true`. You no longer need to clone and disable it yourself.
- **Multi-tenant cache safety**: pass `etag.WithCache(shared)` plus `etag.WithKeyScope(perTenantString)` to share one cache across identities without cross-tenant leaks; ghkit refuses to start if you pass a shared cache without a scope (`ErrKeyScopeRequired`).

## What the swap changes

- **Drift safety hatch is automatic, not a flag**. If your repo today exposes a runtime flag like `--enable-precomputed-etag=false` to fall back to passive mode under drift, you can retire it. ghkit detects drift on every cacheable 200 and silently switches to passive mode after 10 mismatches in a 60-second window; after a 1-hour cooldown it probes back to precompute and recovers if the algorithm is working again. Wire `etag.WithEventCallback(...)` and filter on `KindDriftDetected`/`KindDriftRecovered` for transition alerts; poll `(*etag.Transport).Stats()` for live state on `/healthz`. As of v1.6.0, `Stats` also carries `TotalHits`/`TotalMisses`/`TotalStores`/`TotalBypasses` so a polling adapter can publish per-Outcome counters without scraping DEBUG-level slog records.
- **Stricter cache-policy gate**. ghkit's etag transport refuses to cache responses carrying `Cache-Control: no-store` or `Vary: *` (RFC 9111 alignment). GitHub does not currently emit either, so the practical effect is zero, but the behavior is stricter than a typical in-tree port.
- **Log/metric label fidelity**. ghkit's etag transport emits a small built-in slog event set with a generic path-template allowlist. Your repo's bespoke route table (`/repos/{o}/{r}/commits/{sha}/branches-where-head` and similar) collapses to a coarse fallback label. If your dashboards key on per-route labels, keep emitting metrics from your own RoundTripper above ghkit's transport; do not rely on ghkit's slog event labels for app-specific path cardinality.
- **Upstream features ghkit does not curate**. The named `ratelimit.With*` options cover the common callbacks; for upstream features ghkit does not expose (the abort callback on `WithTotalSleepLimit`, custom limit providers, before-request hooks), use `ratelimit.WithUpstreamOptions(opts ...any)` to forward raw `gofri/go-github-ratelimit/v2` options.

## Recipe 1: Kubernetes operator with rotating PAT

Shape: a long-lived process holds one `*http.Client`. Each reconcile reads a fresh token from disk and clones a `*github.Client` on top of the shared transport with `(*github.Client).WithAuthToken(tok)`. The transport keeps the ETag cache and rate-limit bucket warm across reconciles.

### Before

```go
// Process startup -- builds the long-lived transport stack:
cloned := http.DefaultTransport.(*http.Transport).Clone()
cloned.DisableCompression = true
var base http.RoundTripper = cloned
if flags.Controller.EnableETagCache {
    base = github.NewETagTransport(base, installationID, flags.Controller.ETagCacheSize)
}
hc := github_ratelimit.NewClient(base,
    github_primary_ratelimit.WithLimitDetectedCallback(func(c *github_primary_ratelimit.CallbackContext) {
        logger.Error(nil, "GitHub primary rate limit reached", "category", c.Category, "reset_time", c.ResetTime)
    }),
    github_primary_ratelimit.WithLimitResetCallback(func(c *github_primary_ratelimit.CallbackContext) {
        logger.Info("GitHub primary rate limit reset", "category", c.Category)
    }),
    github_secondary_ratelimit.WithLimitDetectedCallback(func(c *github_secondary_ratelimit.CallbackContext) {
        logger.Error(nil, "GitHub secondary rate limit reached", "reset_time", c.ResetTime)
    }),
    github_secondary_ratelimit.WithTotalSleepLimit(time.Hour, nil),
)

// Per reconcile -- new client, same transport:
gh := github.NewClient(hc).WithAuthToken(readGitHubToken())
```

### After

```go
import (
    ghkit "github.com/pcanilho/go-github-kit"
    "github.com/pcanilho/go-github-kit/etag"
    "github.com/pcanilho/go-github-kit/ratelimit"
)

// Process startup:
hc, err := ghkit.HTTPClient(
    ghkit.WithETagCache(
        etag.WithCache(etag.NewLRUCache(flags.Controller.ETagCacheSize)),
        etag.WithKeyScope(installationID),
    ),
    ghkit.WithRateLimit(
        ratelimit.WithPrimaryLimitDetected(func(c *ratelimit.PrimaryEvent) {
            logger.Error(nil, "GitHub primary rate limit reached", "category", c.Category, "reset_time", c.ResetTime)
        }),
        ratelimit.WithPrimaryLimitReset(func(c *ratelimit.PrimaryEvent) {
            logger.Info("GitHub primary rate limit reset", "category", c.Category)
        }),
        ratelimit.WithSecondaryLimitDetected(func(c *ratelimit.SecondaryEvent) {
            logger.Error(nil, "GitHub secondary rate limit reached", "reset_time", c.ResetTime)
        }),
        ratelimit.WithTotalSleepLimit(time.Hour),
    ),
)
if err != nil { return err }

// Per reconcile -- new client, same transport:
gh := github.NewClient(hc).WithAuthToken(readGitHubToken())
```

What moves: the bespoke `etag_transport.go` and the manual `github_ratelimit.NewClient(...)` call collapse into options. Compression handling is implicit. The reconcile-loop call site is unchanged. `PrimaryEvent` / `SecondaryEvent` are type aliases of the upstream `gofri` types, so callback bodies do not need to change.

If you are pinned to `go-github/v84` and ghkit is on `v85`, keep your major: ghkit does not import `go-github` from `HTTPClient()` -- it returns `*http.Client`, which you hand to your own go-github version.

## Recipe 2: Multi-installation webhook processor

Shape: a webhook handler builds a per-installation `*github.Client` on demand. A `tokenManager` mints JIT tokens (e.g. via `ghait` + KMS). One `etag.Cache` is shared across installations; per-installation `keyScope` keeps tenants isolated.

### Before

```go
func (p *Processor) getGitHubClient(ctx context.Context, installationID int64) (*github.Client, error) {
    token, err := p.tokenManager.GetToken(ctx, installationID)
    if err != nil { return nil, err }

    ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})

    var base http.RoundTripper = http.DefaultTransport
    if p.etagCache != nil {
        clonedBase := http.DefaultTransport.(*http.Transport).Clone()
        clonedBase.DisableCompression = true
        base = etag.NewWithCache(clonedBase, p.etagCache,
            strconv.FormatInt(installationID, 10), p.etagPrecomputed)
    }
    oauthTransport := &oauth2.Transport{Source: ts, Base: base}
    rateLimitTransport := github_ratelimit.New(oauthTransport)
    httpClient := &http.Client{Timeout: 5 * time.Second, Transport: rateLimitTransport}
    return github.NewClient(httpClient), nil
}
```

### After

```go
import (
    ghkit "github.com/pcanilho/go-github-kit"
    "github.com/pcanilho/go-github-kit/etag"
    "golang.org/x/oauth2"
)

// At startup, build the shared cache once:
//   p.etagCache = etag.NewLRUCache(8192)

func (p *Processor) getGitHubClient(ctx context.Context, installationID int64) (*github.Client, error) {
    token, err := p.tokenManager.GetToken(ctx, installationID)
    if err != nil { return nil, err }

    source := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})

    hc, err := ghkit.HTTPClient(
        ghkit.WithTokenSource(source),
        ghkit.WithETagCache(
            etag.WithCache(p.etagCache),
            etag.WithKeyScope(strconv.FormatInt(installationID, 10)),
        ),
        ghkit.WithTimeout(5 * time.Second),
    )
    if err != nil { return nil, err }
    return github.NewClient(hc), nil
}
```

What moves: `etag.NewWithCache(...)` becomes `etag.WithCache(...) + etag.WithKeyScope(...)`. The shared cache type changes from your in-tree `*expirable.LRU[string, Entry]` to `etag.Cache` (`etag.NewLRUCache(n)` returns a default LRU that satisfies the interface; substitute Redis or another backend by implementing the three-method interface). The `p.etagPrecomputed` flag has no equivalent: ghkit always runs precompute mode.

If your `tokenManager` already returns a cached token with a TTL, prefer mint-on-demand (`oauth2.StaticTokenSource` rebuilt per call) so the JIT layer stays in your code. If you'd rather have ghkit pull tokens directly, hand it an `oauth2.ReuseTokenSource` that wraps your factory; either pattern is supported.

To collapse this further into a single shared transport across all installations, swap `etag.WithKeyScope(...)` for `etag.WithAutoKeyScope(fn)` and let `fn` resolve the installation id from `req.Context()`. See the README's "Multi-tenant single client" recipe and `examples/installation-token/` for the canonical `ghinstallation.Transport` to `oauth2.TokenSource` bridge.

## Recipe 3: Backfill / batch job with proactive RPS cap

Shape: a one-off CLI that walks GitHub APIs at a deliberately low rate (1.3 req/s) on top of the reactive limiter, with a static PAT.

### Before

```go
type Client struct {
    client  *github.Client
    limiter *rate.Limiter
}

func NewClient(token string, opts ...ETagOptions) (*Client, error) {
    ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})

    var base http.RoundTripper = http.DefaultTransport
    if len(opts) > 0 && opts[0].EnableETagCache {
        clonedBase := http.DefaultTransport.(*http.Transport).Clone()
        clonedBase.DisableCompression = true
        base = etag.New(clonedBase, "backfill", opts[0].ETagCacheSize, opts[0].EnablePrecomputedETag)
    }
    oauthTransport := &oauth2.Transport{Source: ts, Base: base}
    rateLimitTransport := github_ratelimit.New(oauthTransport)
    httpClient := &http.Client{Transport: rateLimitTransport}

    return &Client{
        client:  github.NewClient(httpClient),
        limiter: rate.NewLimiter(rate.Limit(1.3), 1),
    }, nil
}

// Callers must do:  c.limiter.Wait(ctx); c.client.Repositories.Get(...)
```

### After

```go
import (
    ghkit "github.com/pcanilho/go-github-kit"
    "github.com/pcanilho/go-github-kit/etag"
)

func NewClient(token string, etagSize int) (*github.Client, error) {
    return ghkit.New(github.NewClient,
        ghkit.WithToken(token),
        ghkit.WithETagCache(etag.WithCache(etag.NewLRUCache(etagSize))),
        ghkit.WithRequestsPerSecond(1.3, 1),
    )
}

// Callers just do:  client.Repositories.Get(...)
```

What moves: the manual `c.limiter.Wait(ctx)` calls disappear because `WithRequestsPerSecond` runs as a transport layer on every outbound HTTP request. This is functionally equivalent for a backfill workload (the rate of HTTP requests is what GitHub cares about) but it does mean any code path that wrapped `Wait()` errors needs to be rechecked: a transport-level limiter cancellation surfaces as a context error from the API call, not from a separate Wait.

## Verification checklist

After swapping, before merging:

- `go test ./...` and `go vet ./...` in your repo: tests that mocked the in-tree etag transport will need to mock against ghkit's `etag.NewTransport` or against a higher-level `*http.Client`.
- Confirm `errors.As(err, &primaryRateLimit)` still unwraps the `*github_primary_ratelimit.RateLimitReachedError` your controller relies on (it does, ghkit re-uses gofri unchanged).
- If you keep your own metric emitter, wrap it as a RoundTripper above the `*http.Client` returned by `ghkit.HTTPClient(...)` rather than relying on ghkit's slog events for per-route labels.

## Recipe 4 (1.5): consumer utilities round (`polling`, `search`, `cond`)

Three consumer-facing iterators ship in v1.5.0. Each composes with the
existing transport stack via the shared `*http.Client` and adds no new
runtime dependencies in the root module.

### `polling`: long-running operations

Replace ad-hoc `time.Ticker` loops with a range-over-func iterator:

```go
seq := polling.As[*github.WorkflowRun](ctx, hc, http.MethodGet, runURL, headers, nil,
    15*time.Second,
    polling.WithDoneT(func(r *github.WorkflowRun) bool { return r.GetStatus() == "completed" }),
    polling.WithMaxWallClock(30*time.Minute),
)
for run, err := range seq { /* ... */ }
```

Each `c.Do` benefits from retry, etag, ratelimit, throttle, oauth2.
Recommend `retry.WithMaxAttempts(1)` on the client when polling owns
the outer loop, otherwise retry's per-attempt jitter compounds with
the polling interval.

### `search`: envelope-shaped pagination

`pages.As[T]` cannot serve `/search/*` (envelope, not array). Use
`search.Issues[T]` etc. and surface `IncompleteResults` /
`ErrResultCapHit` per page:

```go
for r, err := range search.Issues[*github.Issue](ctx, hc, q, search.WithPerPage(100)) {
    if errors.Is(err, search.ErrResultCapHit) { /* refine query */ break }
    /* ... */
}
```

### `cond`: visible 304

The etag layer used to erase the change-vs-unchanged signal before the
caller saw it. v1.5.0 surfaces it via `cond.HeaderCacheStatus`
(`"X-Ghkit-Cache"`) on the response, mapped to `cond.Status` by
`cond.StatusOf(resp)`. Use `cond.Fetch[T]` for one-shot conditional
GETs, or `polling.WithChangeOnly` to silently skip polling iterations
on cache hits.

No API breaks. The new `X-Ghkit-Cache` response header is additive;
existing consumers ignore it. The exported `retry.RetryAfter` is a
new symbol consumed internally by `polling`; consumers can use it for
their own Retry-After parsing without depending on the unexported
`parseOutcome` enum.

## v1.6: per-call event attribution

`etag.WithDriftDetected` is removed in v1.6.0. The unified
`etag.WithEventCallback` hook delivers every cache decision and
validation event with the request URL, normalised path template, and
Kind-specific fields. Use it when you need to attribute outcomes to a
specific GitHub URI, repository, or consumer-side context (e.g., the
webhook event type that triggered the API call).

### Before (v1.5)

```go
tr, err := etag.NewTransport(base,
    etag.WithCache(cache),
    etag.WithDriftDetected(func(evt etag.DriftEvent) {
        if evt.Recovered {
            metrics.RecordDriftRecovered(evt.DetectedAt)
        } else {
            metrics.RecordDriftDetected(evt.DetectedAt)
        }
    }),
)
```

### After (v1.6)

```go
type webhookCtxKey struct{}

func handleWebhook(w http.ResponseWriter, r *http.Request) {
    evtType := r.Header.Get("X-GitHub-Event")
    ctx := context.WithValue(r.Context(), webhookCtxKey{}, evtType)
    // ... call gh.Repositories.Get(ctx, owner, repo) etc.
}

func emitMetric(ctx context.Context, evt etag.Event) {
    webhookType, _ := ctx.Value(webhookCtxKey{}).(string)
    repo := ""
    if evt.URL != nil { // nil on KindDriftDetected / KindDriftRecovered
        parts := strings.Split(strings.TrimPrefix(evt.URL.Path, "/"), "/")
        if len(parts) >= 3 && parts[0] == "repos" {
            repo = parts[1] + "/" + parts[2]
        }
    }
    switch evt.Kind {
    case etag.KindDriftDetected:
        metrics.RecordDriftDetected(evt.DriftEvent.DetectedAt)
    case etag.KindDriftRecovered:
        metrics.RecordDriftRecovered(evt.DriftEvent.DetectedAt)
    default:
        metrics.Inc("github_etag", "kind", string(evt.Kind),
            "webhook_type", webhookType, "repo", repo,
            "path_template", evt.PathTemplate)
    }
}

tr, _ := etag.NewTransport(base,
    etag.WithCache(cache),
    etag.WithEventCallback(emitMetric),
)
```

The drift-transition payload is unchanged: `evt.DriftEvent.DetectedAt`
and `evt.DriftEvent.Recovered` carry the same values as the v1.5
callback argument. New consumers pick up cache hit/miss/store/mismatch
metrics for free by handling additional `Kind` values.

The callback runs synchronously inside `RoundTrip`. Panics propagate up;
the library does not catch them. For dashboards that only need totals,
prefer `Stats()` polling: its per-Outcome counters are read lock-free
and require no per-event allocation.

**Pages composition.** With the `pages` package (`pages.As[T]`,
`pages.Pages`), every page is a separate `RoundTrip`, so the callback
fires once per page in a paginated traversal, not once per logical
fetch. A 30-page list issuing 30 conditional GETs produces 30 callback
invocations. Aggregate in your handler if your metric backend wants
one event per logical fetch.
