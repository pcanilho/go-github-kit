# Changelog

All notable changes to **go-github-kit** are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.2.0] - 2026-04-26

Adds a `ghtest` sub-package with two helpers that hide the non-discoverable
GitHub-specific traps in testing ghkit-using code: secondary-rate-limit
classification (the `documentation_url` suffix `gofri/go-github-ratelimit`
pattern-matches on) and the bored-engineer ETag hash domain (which includes
the request `Authorization` header). Plus a `TESTING.md` with self-contained
recipes for the surrounding stdlib bits. No new runtime dependencies.

### Added

#### `ghtest` sub-package

- `ghtest.WriteSecondaryLimit(w, retryAfter)` writes a 403 with `Retry-After`
  (whole seconds; negative durations clamp to zero) and a JSON body whose
  `documentation_url` ends in `#secondary-rate-limits`. That suffix is what
  `gofri/go-github-ratelimit` pattern-matches on to classify the response as
  an `AbuseRateLimitError`, so consumer retry paths actually trigger in
  tests instead of silently falling through.
- `ghtest.Write304IfMatch(w, r, body) bool` computes the expected ETag using
  the bored-engineer algorithm (hashes Authorization, Accept, and Cookie
  request headers along with the body), normalises every tag in
  `If-None-Match` (split on commas, trim whitespace, strip `W/` weak prefix
  and surrounding quotes), and on any match sets a quoted RFC 7232 `ETag`
  response header, writes 304 Not Modified with empty body, and returns
  true. Returns false and writes nothing on miss.
- Both helpers compose with stdlib `httptest.NewServer`; consumers bring
  their own handler. Tests cover quoted/unquoted/weak-only matches, multi-tag
  comma splitting, no-match leaves response untouched, no-header
  short-circuit, and different-body misses.

#### Documentation

- New `TESTING.md` with self-contained recipes: routing a ghkit-built client
  at a test server (the `gh.BaseURL = url.Parse(srv.URL+"/")` pattern),
  ETag 304 replay handling, rate-limit header emission (no helper, recipe
  only), Link-header pagination (no helper, recipe only), secondary-rate-
  limit testing, plus a "See also" pointer to `migueleliasweb/go-github-mock`
  for the SDK-layer mocking case.
- README "Testing your code" section, three lines linking to `TESTING.md`.

### Dependencies

No changes. Same as 1.1.0.

## [1.1.0] - 2026-04-26

Adds a `retry` middleware for transient failures (5xx, network errors,
transport-level deadline exceeded), filling the gap between the rate-limit
layer (which owns 429s) and the underlying transport. Composes with the
existing stack without new runtime dependencies.

### Added

#### Retry middleware (`retry/`)

- New `retry.NewTransport(base, opts...)` chainable `http.RoundTripper`.
  Defaults: 3 attempts (1 initial + 2 retries), 200ms..2s decorrelated jitter
  (per AWS guidance), idempotent methods only (GET/HEAD/OPTIONS/PUT/DELETE),
  default predicate retries {500, 502, 503, 504} or `*net.OpError`/`io.EOF`/
  `io.ErrUnexpectedEOF`/`context.DeadlineExceeded`.
- Options: `WithMaxAttempts(n)` (clamped to [1, 100]), `WithBackoff(min, max)`
  (clamped to [1ms, 1h] with min<=max), `WithRetryOn(predicate)` (replaces
  the default predicate; takes ownership of method-safety),
  `WithLogger(*slog.Logger)`.
- Top-level `ghkit.WithRetry(opts ...retry.Option)` slots the layer between
  RateLimit and oauth2 in the chain - 429s never reach retry, and retried
  requests get the latest token via oauth2's per-call `Source.Token()`.
- 429 hard-exclusion lives outside the predicate so a user-supplied
  `WithRetryOn` cannot accidentally fight `ratelimit`.
- `Retry-After` honored (delta-seconds and HTTP-date formats); when it
  exceeds the operator's `maxDelay` the call returns
  `(prior_resp, retry.ErrRetryAfterExceedsMax)` and the caller owns
  drain+close on the response.
- Caller-context cancellation is terminal: `req.Context().Err() != nil`
  always stops retries before any predicate is consulted, and during
  backoff sleep via `time.NewTimer` + `Stop` (no leaked timers on long
  `Retry-After` values).
- Body-bearing retries require `req.GetBody`; missing GetBody on a retry
  attempt yields `errors.Join(retry.ErrBodyNotRewindable, prior_err)` so
  callers see both causes.
- Exported predicate primitives - `retry.IsIdempotent(method)`,
  `retry.IsRetryable5xx(code)`, `retry.IsTransientNetErr(err)` - so callers
  composing their own `WithRetryOn` don't have to reimplement the defaults.
- Panic recovery around user predicates: a panicking `WithRetryOn` is
  treated as "do not retry" and emits a `retry_predicate_panic` event
  rather than crashing the transport.
- Sanitised structured logging via `slog.Logger` with explicit per-event
  levels: `retry_sleep`, `retry_decision` (Debug; silent-success first
  attempts skipped), `retry_abort`, `retry_body_unrewindable`,
  `retry_exhausted`, `retry_predicate_panic` (Warn). `last_err_type` walks
  joined/wrapped error chains so operators see meaningful types instead of
  `*errors.joinError`.
- DoS protection: prior-response drain capped at 128 KiB before close,
  bounding the time we hold a connection on hostile/oversize bodies.

#### Documentation

- README transport-stack diagram updated to show retry between RateLimit
  and oauth2.
- New retry recipe with default and tuned-policy examples (including
  `Idempotency-Key`-based POST opt-in).
- New "Things worth knowing" note on retry/throttle interaction (each retry
  attempt consumes a throttle token).
- Package `doc.go` for `retry/` and updated top-level `doc.go` with the new
  chain layout.

#### Tooling and CI

- Live integration test (`retry/live_check_test.go`, build-tag `live`,
  function `TestRetry_Live`) that exercises retry against `api.github.com`.
- CI `live-drift` job renamed to `Live drift and retry probe` and extended
  to run both `TestETag_Live` and `TestRetry_Live`.

### Changed

- **Silent by default.** `ghkit`, `etag`, `ratelimit`, and `retry` no longer
  default-initialise a `slog.Default()` logger. Without an explicit
  `WithLogger(...)` call, the library emits no log records. This is a
  behaviour change from 1.0.0; users who relied on default stderr output
  must now opt in via `ghkit.WithLogger(slog.Default())` (or any logger).
- Per-sub-package `WithLogger` options inside `WithRetry`, `WithETagCache`,
  and `WithRateLimit` now correctly override the top-level `WithLogger`
  instead of being silently shadowed by it. Previously the chain assembly
  appended ghkit's logger after user-supplied sub-package options, so
  user values lost; the prepend pattern lets user values win.
- `etag.WithLogger(nil)` and `ratelimit.WithLogger(nil)` now mean
  "explicitly silent" instead of being a no-op. Combined with silent-by-
  default, this lets callers compose `WithLogger(real)` then
  `WithLogger(nil)` to silence on a per-construction basis.
- `retry.IsTransientNetErr` now short-circuits known-permanent failures to
  `false`: DNS NXDOMAIN (`*net.DNSError.IsNotFound`), `syscall.ECONNREFUSED`
  (TCP RST on connect to a closed port), and x509 cert-validation errors
  (`x509.UnknownAuthorityError`, `*x509.HostnameError`,
  `x509.CertificateInvalidError`). Misconfigured URLs and expired certs now
  fail fast instead of burning the retry budget. Other DNS errors (server
  failure, timeout) remain transient.
- `ghkit.WithRateLimit` and `ghkit.WithRateLimitDisabled` are now mutually
  exclusive. Combining them returns `ErrConflictingRateLimit` at
  construction. Previously the kit logged a warning and silently dropped
  the registered callbacks; with silent-by-default that warning would have
  been invisible. The hard error matches the existing
  `ErrConflictingAuth` precedent for `WithToken` + `WithTokenSource`.

### Added (continued)

- New runnable example at `examples/retry-on-flaky/` demonstrating
  `WithRetry` with a tuned backoff and a custom predicate that opts POST in
  via `Idempotency-Key`.

### Dependencies

No changes. Same as 1.0.0.

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

[1.2.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.2.0
[1.1.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.1.0
[1.0.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.0.0
