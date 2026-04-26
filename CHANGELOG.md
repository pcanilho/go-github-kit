# Changelog

All notable changes to **go-github-kit** are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - 2026-04-25

Initial public release.

`go-github-kit` packages three things most projects re-implement on top of
[`google/go-github`](https://github.com/google/go-github) - a conditional-request
ETag cache, the well-known reactive rate limiter from `gofri/go-github-ratelimit`,
and a client-side token-bucket throttle - behind one options-pattern constructor.
You can adopt the whole stack in a few lines, or import the sub-packages a la
carte if you already have one of these and just want the others.

The headline feature is the ETag layer: GitHub's server-side ETag includes the
`Authorization` header, so a passive store-and-forward cache loses its hit rate
the moment your token rotates. ghkit reproduces that hash client-side so cached
entries keep working across rotations - durable quota savings for GitHub Apps
and rotating PATs alike.

### Added

#### ETag caching that survives token rotation (`etag/`)

- Client-side precompute of GitHub's auth-inclusive ETag hash, so 304s keep
  flowing across GitHub App installation-token rotations and rotating PATs.
  Algorithm originally reverse-engineered by
  [bored-engineer](https://github.com/bored-engineer/github-conditional-http-transport).
- Bounded in-process LRU as the default backend (`etag.NewLRUCache`) - defaults
  to 4096 entries and a 256 MiB byte budget, both tunable via
  `etag.WithMaxBodyBytes` and `etag.WithMaxCacheBytes`.
- Pluggable `etag.Cache` interface (`Get` / `Add` / `Remove`, all context-aware)
  so you can drop in Redis, S3, bbolt, Pebble, or any other backend without
  forking the kit.
- Multi-tenant safety via `etag.WithKeyScope(...)` - required whenever a cache
  is shared across identities, so two installations hitting the same URL can
  never read each other's bodies.
- Automatic drift detection: every cacheable 200 is verified, and after 10
  precompute mismatches inside a 60-second window the transport silently
  falls back to passively echoing the server's ETag. After a one-hour
  cooldown it probes a small fraction of requests; consecutive successes
  restore precompute mode automatically. Wire `etag.WithDriftDetected(...)`
  for an alert hook on each transition; call `(*etag.Transport).Stats()` for
  `/healthz` or dashboard polling.
- Explicit construction-time error if the supplied base transport isn't an
  `*http.Transport`, instead of silently miscomputing every hash. Default
  base disables gzip so the hash domain matches what GitHub signed.
- Sanitised structured logging on a strict allowlist - no header values, no
  hash prefixes, no auth lengths. Records are emitted via slog's `*Context`
  variants (`DebugContext`, `WarnContext`, `InfoContext`, `ErrorContext`)
  so a context-aware `slog.Handler` can stamp request IDs onto every line.
  The upstream `X-GitHub-Request-Id` response header is included as
  `github_request_id` for cross-referencing with GitHub-side debugging.

#### Reactive rate limiting (`ratelimit/`)

- Thin facade over [`gofri/go-github-ratelimit/v2`](https://github.com/gofri/go-github-ratelimit)
  with sensible defaults for both GitHub primary and secondary limits.
- Curated callbacks for the common observability hooks:
  `WithPrimaryLimitDetected`, `WithPrimaryLimitReset`,
  `WithSecondaryLimitDetected`, `WithTotalSleepLimit`, `WithLogger`.
- Escape hatch via `ratelimit.WithUpstreamOptions(opts ...any)` for any
  upstream feature the kit hasn't curated yet.

#### Proactive client-side throttling (`throttle/`)

- Token-bucket throttle (built on `golang.org/x/time/rate`) that caps RPS
  before GitHub ever sees the request - useful for backfill and batch jobs
  that would otherwise burst into secondary limits.
- Standalone `throttle.NewTransport(base, rps, opts...)` for hand-built
  stacks, or `ghkit.WithRequestsPerSecond(rps, burst)` from the top level.

#### Composable client construction (top-level `ghkit` package)

- `ghkit.New(...)` - generic factory wrapper that returns whatever client
  type your factory produces (`*github.Client`, `githubX.NewClient`, or any
  `func(*http.Client) T`). ghkit itself has zero compile-time dependency
  on `go-github`, so you can pin any major version you like.
- `ghkit.HTTPClient(...)` - assemble just the transport stack and hand the
  resulting `*http.Client` to whichever SDK you prefer.
- Options:
  - `WithToken(pat)` and `WithTokenSource(src oauth2.TokenSource)` for static
    PATs and JIT auth respectively (works cleanly with `ghinstallation` for
    local-key JWT signing or `isometry/ghait` for KMS-backed signing).
  - `WithETagCache(opts ...etag.Option)` to plug in the ETag layer, with
    the full sub-package option surface forwarded.
  - `WithRateLimit(opts ...ratelimit.Option)` (default ON) and
    `WithRateLimitDisabled()` for the rare cases you don't want it.
  - `WithRequestsPerSecond(rps, burst)` for the proactive throttle.
  - `WithBaseTransport(rt)`, `WithTimeout(d)`, `WithUserAgent(ua)`,
    `WithLogger(l)` for the usual transport-shape knobs.
- Layer order is load-bearing and documented: throttle, then rate-limit,
  then oauth2, then etag, then the base transport. The kit only assembles
  the layers you opt into.
- GitHub Enterprise Server supported by passing a custom factory that calls
  `(*github.Client).WithEnterpriseURLs(...)`.

#### Documentation

- `README.md` with quick-start, the full transport-stack diagram, recipes for
  static PAT, GitHub App installation tokens (with `ghinstallation` and
  `ghait` adapters), backfill jobs, GitHub Enterprise Server, and using
  the `etag` sub-package on its own.
- `MIGRATION.md` with three before/after recipes - Kubernetes operator with
  rotating PAT, multi-installation webhook processor, backfill/batch job,
  plus a verification checklist and notes on behavioural differences worth
  spotting before the swap.
- Package-level `doc.go`, runnable `example_test.go`, and pkg.go.dev-rendered
  reference for every exported symbol.

#### Tooling and CI

- `Makefile` targets: `test`, `test-unit`, `test-live` (live ETag drift
  probe against `api.github.com`), `test-fuzz` (30s fuzz over the ETag
  hash), `bench`, `bench-update`, `lint` (golangci-lint v2),
  `vuln` (govulncheck), `tidy`.
- GitHub Actions CI running lint, race-enabled tests, fuzzing, and the live
  drift probe - so the day GitHub changes its ETag algorithm we know within
  one CI run instead of when users start filing issues.
- MIT license.

### Dependencies

- Go 1.26.2
- `github.com/google/go-github/v85` v85.0.0
- `github.com/gofri/go-github-ratelimit/v2` v2.0.2
- `github.com/hashicorp/golang-lru/v2` v2.0.7
- `golang.org/x/oauth2` v0.36.0
- `golang.org/x/time` v0.15.0

[1.0.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.0.0
