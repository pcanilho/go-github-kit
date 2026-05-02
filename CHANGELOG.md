# Changelog

All notable changes to **go-github-kit** are documented in this file.

The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.5.0] - 2026-05-02

Adds three consumer-utility sub-packages that ride on top of the
assembled `*http.Client` and compose with the existing transport
stack: `polling` (range-over-func iterator over an HTTP endpoint on
an interval), `search` (envelope iterator for `/search/*` endpoints
that `pages.As[T]` cannot serve), and `cond` (visible 304 / change-
vs-unchanged signal building on the etag layer). Hardens `retry`'s
`Retry-After` parser as part of the same release.

### Fixed

- `retry`: `Retry-After` parser saturates extremely large delta-seconds
  values (above `math.MaxInt64 / int64(time.Second)`) instead of wrapping
  int64 nanoseconds. Clamped values route through the existing
  `ErrRetryAfterExceedsMax` abort path.
- `retry`: whitespace around `Retry-After` values (`" 5"`, `"5 "`) is now
  trimmed before parsing.

### Added

- `polling.Poll` and `polling.As[T]`: Go 1.23 range-over-func iterator
  that re-issues a request on a caller-tunable interval, reusing the
  supplied `*http.Client` so retry, etag, ratelimit, throttle, and
  oauth2 apply per attempt. Options: `WithDone` (header/status
  predicate), `WithDoneT[T]` (typed predicate on the decoded value),
  `WithDecode[T]` (custom decoder), `WithChangeOnly` (skip yields on
  cache hit), `WithMaxAttempts`, `WithMaxWallClock`, `WithJitter`
  (deterministic mid-point), `WithHonorRetryAfter`, `WithLogger`,
  plus `WithSleepFunc` / `WithNowFunc` test seams. Sentinels
  `ErrMaxAttemptsExceeded`, `ErrMaxWallClockExceeded` (wraps
  `context.DeadlineExceeded`), `ErrInvalidInterval`,
  `ErrInvalidOption`, `ErrPredicatePanic`. Boundary yields surface
  `(lastResp, sentinel)` so the caller can inspect what triggered
  the limit.
- `search.Issues`, `search.Code`, `search.Repos`, `search.Users`:
  typed iterators over GitHub's `/search/*` envelope endpoints.
  Yields `Result[T]{Item, TotalCount, IncompleteResults}` per item;
  surfaces the post-page-10 1000-result hard cap as
  `ErrResultCapHit`. Reuses `pages.Pages` for Link-header walking.
  Functional options (`WithBaseURL`, `WithPerPage`, `WithSort`,
  `WithOrder`, `WithHeaders`) match the convention used by the rest
  of ghkit; `WithBaseURL` overrides the GitHub default for GHES or
  test fixtures.
- `cond.Status`, `cond.StatusOf`, `cond.Fetch[T]`: surfaces the
  change-vs-unchanged signal the etag layer already computes.
  `cond.HeaderCacheStatus` is the canonical `"X-Ghkit-Cache"` header
  the etag layer sets on synth-200 (`"hit"`) and wire-200 store
  (`"miss"`). `StatusOf` is nil-safe and maps absent to `Updated`.
  Sentinel errors `cond.ErrNilClient`, `cond.ErrNilContext`,
  `cond.ErrNilRequest` for invalid `Fetch` arguments. Decode errors
  wrap as `cond: decode: %w` for parity with pages, polling, search.
- `etag/transport.go`: sets `cond.HeaderCacheStatus` on synth-200
  (after header merge) and wire-200 (after `cache.Add` returns) so
  the cache does not ingest the key. Drift detector and
  `ComputeExpectedETag` are unaffected (the header is response-only
  and not in the hash domain).
- `retry.RetryAfter(resp) (time.Duration, bool)`: exported wrapper
  around `parseRetryAfter`. Returns `(0, false)` for absent,
  unparseable, AND negative-numeric values. Used by `polling` to
  honor server hints without duplicating the parser.
- `ghtest.ETagServer(t, body) (*httptest.Server, *int64)`: lifted
  from `etag/transport_test.go`. Used by polling, cond, and any
  future test that exercises etag composition.
- `retry`: `retry_retry_after_unparseable` slog event at Warn when
  the header is present but matches neither delta-seconds nor
  HTTP-date. Carries a length-bounded, ASCII-sanitised `raw`
  attribute (32 bytes max).
- `retry`: `retry_sleep` event's `source` attribute gains a
  `"malformed"` value, distinct from `"jitter"` and `"retry_after"`.
- `retry`: `FuzzParseRetryAfter` fuzz test pinning the parser as
  total.

### Documentation

- New `polling/doc.go`, `search/doc.go`, `cond/doc.go` documenting
  contracts and sharp edges (retry compounding, throttle backpressure,
  etag synth-200 sameness, gofri ctx-cancel observed on next
  round-trip, header set ordering for the cond signal).
- `retry/doc.go` documents the unparseable arm and the
  `source="malformed"` label.
- `pages/doc.go`: corrected the stale chain-order note (predates the
  v1.4.0 ratelimit/throttle inversion).
- `README.md`: Go Report Card badge added; coverage badge skipped
  (peer cohort split: 3 of 4 transport-utility libraries do not
  carry one).

### Examples

- New `examples/poll-workflow-run/`: waits for a workflow run to
  reach `status="completed"` via `polling.As[*github.WorkflowRun]`
  with `WithDoneT`, `WithMaxWallClock`, `WithJitter`. Pairs polling
  with `WithETagCache` so unchanged ticks short-circuit at the etag
  layer.
- New `examples/search-issues/`: walks `/search/issues` with
  `search.Issues[*github.Issue]`, surfaces `incomplete_results` and
  the 1000-result cap.
- New `examples/conditional-fetch/`: visible 304 via
  `cond.Fetch[*github.Repository]`; the second call returns
  `cond.Unchanged` so downstream work can be skipped.

### Tooling

- `polling`, `search`, and `cond` introduce no new runtime
  dependencies in the root module. The `ghtest.ETagServer` lift adds
  no module surface; `cond` is stdlib-only; `polling` imports `cond`
  and `retry`; `search` imports `pages`. The import graph remains
  acyclic (`etag -> cond`, `polling -> cond, retry`, `search ->
  pages`).

## [1.4.0] - 2026-05-02

Adds a `pages` sub-package: a Go 1.23 range-over-func iterator for
Link-header pagination, plus a typed wrapper that decodes each page
into elements of any type. The iterator runs on a caller-supplied
`*http.Client`, so the existing RateLimit, Throttle, Retry, oauth2,
and ETag layers apply per page with no extra wiring. Also extends
`ghtest` with a `LinkHeader` fixture builder so tests can construct
RFC 8288 Link headers without rolling their own. No new runtime
dependencies in the root module.

### Added

- `pages.Pages(ctx, client, method, url, headers)`: range-over-func
  iterator over paginated REST responses. Walks `Link: rel="next"`
  and yields each `*http.Response`. Caller drains and closes each
  body. Errors stop iteration after one yield; a clean end of
  pagination (no `rel="next"`) stops silently.
- `pages.As[T any](ctx, client, method, url, headers)`: typed wrapper
  that decodes each page into `[]T` with the standard library JSON
  decoder and yields elements one at a time. Iterator owns each
  response body and closes it after decoding, including on caller
  break and on context cancellation. `T` has no constraint, so
  `*github.Repository` and similar SDK types work without
  implementing `json.Unmarshaler`.
- `pages.ErrInvalidLinkHeader`: exported sentinel surfaced when a
  response Link header is structurally malformed. A missing
  `rel="next"` is treated as a clean end of pagination, not an error.
- `ghtest.LinkHeader(baseURL, page, perPage, lastPage)`: builds an
  RFC 8288 Link header value for fixture servers. Returns "" when
  `lastPage <= 1` so handlers can branch on a single string. Pairs
  with `ghtest.Write304IfMatch` and `ghtest.WriteSecondaryLimit`.

### Documentation

- `doc.go` adds a paragraph naming the `pages` sub-package alongside
  the existing `etag`, `ratelimit`, and `throttle` notes.
- `README.md` adds an "Iterating over paginated results" recipe under
  the Recipes section, with a runnable snippet using `pages.As` plus
  `WithETagCache`.
- `TESTING.md` replaces the manual Link-header pagination recipe with
  one that uses `ghtest.LinkHeader`, mirroring how the
  secondary-rate-limit recipe references `ghtest.WriteSecondaryLimit`.
- `examples/README.md` adds the `list-all-repos/` row to the table.

### Examples

- New `examples/list-all-repos/`: uses `pages.As[*github.Repository]`
  to walk `/user/repos` against `api.github.com` end to end. Shows
  the iterator composing with `WithETagCache` and `WithUserAgent`.

### Dependencies

No changes in the root module. The `examples/` module is unchanged.

### Tooling

- `.golangci.yaml`: `bodyclose` added to the test-file linter
  exclusion list. The iter.Seq2 yield-then-close pattern in
  `pages_test.go` produces false positives the linter cannot trace
  through; the suppression is scoped to `_test.go` paths so production
  code still gets full bodyclose coverage. The two `//nolint:bodyclose`
  directives in `pages/pages.go` remain (caller-closes contract on
  yielded responses; close-via-decodePage on the typed wrapper).

## [1.3.1] - 2026-05-01

Inverts the throttle/ratelimit chain so RateLimit wraps Throttle.
Pre-1.4 ordering let throttle keep admitting new requests during a
gofri secondary cooldown; at cooldown end the parked requests
stampeded the server simultaneously, re-tripping the abuse detector.
Inverted ordering parks new arrivals inside gofri's `waitForRateLimit`
before they consume throttle tokens, so post-cooldown release is
bounded by the throttle burst.

### Changed

- Transport stack: `ratelimit` now wraps `throttle` instead of the
  inverse. Callers using only one of the two layers see no behavior
  change. Logger event ordering is inverted as a side effect: throttle
  events now emit inside ratelimit's RoundTrip span rather than
  wrapping it; observability dashboards keying on the legacy ordering
  need adjustment.

## [1.3.0] - 2026-04-30

Adds `etag.WithAutoKeyScope` so a single `*http.Client` can serve
multiple tenants without provisioning N transports. Elevates GraphQL/v4
to first-class billing in the docs: the generic factory already worked
with `shurcooL/githubv4`, but the lead documentation framed the kit as
a `google/go-github` (REST) wrapper. No new runtime dependencies in
the root module.

### Added

- `etag.WithAutoKeyScope(fn func(*http.Request) (string, error))`: derives
  the cache-key scope per request. Mutually exclusive with
  `etag.WithKeyScope`; combining both yields `etag.ErrConflictingScope`.
  Either option satisfies the caller-supplied-`Cache` scope requirement.
- `etag.ErrEmptyScope`: sentinel wrapped and returned from `RoundTrip`
  when the fn returns an empty string with a nil error. The contract is
  "non-empty scope or non-nil error"; the empty + nil combination is a
  programming error, not a bypass signal.
- `etag.ErrConflictingScope`: sentinel surfaced at construction when both
  `WithKeyScope` and `WithAutoKeyScope` are set.

### Documentation

- `doc.go` lead paragraph rewritten to name both `google/go-github` (REST)
  and `shurcooL/githubv4` (GraphQL) as canonical factories. New
  paragraphs document GraphQL/v4 compatibility (etag layer no-ops on
  POST; v4 traffic flows through oauth2 + retry + ratelimit + throttle
  + UA without ETag caching) and custom cache backends via the
  three-method `etag.Cache` interface.
- `README.md` lead paragraph elevates GraphQL/v4 to first-class billing.
  New recipes: "Recommended setup for a long-lived service",
  "GraphQL with shurcooL/githubv4", "Multi-tenant single client (one
  Transport, many installations)".
- `MIGRATION.md` Recipe 2 cross-references `WithAutoKeyScope` for the
  multi-installation single-transport pattern.

### Examples

- `examples/installation-token/main.go` replaces the
  `oauth2.StaticTokenSource` stand-in with an inline
  `bradleyfalzon/ghinstallation/v2` to `oauth2.TokenSource` adapter
  (the canonical local-key JWT signing path).
- New `examples/graphql-v4/` runs a minimal `Viewer.Login` query through
  `shurcooL/githubv4` over a ghkit-built `*http.Client`. Pins compile-time
  compatibility with the v4 SDK in the examples CI lane.

### Dependencies

No changes in the root module. The `examples/` module gains
`bradleyfalzon/ghinstallation/v2`, `golang-jwt/jwt/v4` (transitive),
`shurcooL/githubv4`, and `shurcooL/graphql` (transitive).

## [1.2.1] - 2026-04-28

Three bug fixes in transport behaviour and a small set of godoc clarifications.
No API surface changes, no new dependencies.

### Fixed

- `retry.Transport`: when `Retry-After` exceeds the configured `maxDelay`,
  the prior 5xx response is now drained and closed inside the transport and
  the call returns `(nil, ErrRetryAfterExceedsMax)`. Previously the
  transport returned `(resp, err)` with `resp.Body` open, which violated
  the `http.RoundTripper` contract that a non-nil error implies the caller
  has nothing to close. When wrapped by `http.Client` (the standard
  `ghkit.New` path), the response was dropped unclosed, leaking the body
  and preventing connection reuse on every aborted retry.
- `etag.Transport` drift recovery: the post-cooldown probe predicate
  shifted from `n%driftProbeEveryN == 0` to `n%driftProbeEveryN == 1`.
  Probes now fire on calls 1, 51, 101 after `driftCooldown` elapses (was
  50, 100, 150), shifting the probe burst to the start of the post-cooldown
  window and cutting recovery latency from 150 to 101 cacheable requests.
- `etag.NewLRUCache`: `byteTotal` accounting now decrements on count-cap
  evictions via a registered eviction callback. Previously, when the LRU's
  internal slot cap (default 4096) overflowed and the eldest entry was
  evicted, `byteTotal` was not adjusted; on workloads with more than 4096
  distinct cached entries combined with `WithMaxCacheBytes`, the
  over-counted total made the byte-budget loop evict real entries to
  compensate for phantom bytes, shrinking effective cache capacity.

### Documentation

- `retry.ErrRetryAfterExceedsMax` godoc records the new contract: on this
  error `resp` is `nil` and the transport has already drained and closed
  the prior response.
- `etag.WithKeyScope` godoc clarifies that an empty scope is treated
  identically to omitting the option, and that combining `WithCache` with
  no scope fails construction with `ErrKeyScopeRequired`.
- `ratelimit.WithUpstreamOptions` godoc warns that type-mismatched values
  are silently dropped by the upstream constructor.
- `etag` package doc records the drift detector's tuning assumption: a
  transport handling fewer than roughly 100 cacheable requests after the
  cooldown window elapses will not complete recovery under the current
  private thresholds.
- `ghkit.HTTPClient` composition-order comment now lists the `retry` layer
  between `oauth2` and `ratelimit`, matching the actual code.
- README retry recipe updated to match the new abort contract and to note
  that POST/PATCH retries with a body require `req.GetBody`.

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

[1.5.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.5.0
[1.4.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.4.0
[1.3.1]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.3.1
[1.3.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.3.0
[1.2.1]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.2.1
[1.2.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.2.0
[1.1.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.1.0
[1.0.0]: https://github.com/pcanilho/go-github-kit/releases/tag/v1.0.0
