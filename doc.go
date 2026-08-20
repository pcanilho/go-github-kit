// Package ghkit bundles ETag caching, rate limiting, retry on transient
// failures, and a proactive token bucket behind a single options-pattern
// API. New, NewE and Adapt are generic over the returned client type, so
// ghkit has no compile-time dependency on any specific GitHub SDK, and no
// ghkit release ever forces an SDK version on you.
//
// For github.com/google/go-github v87 and later, Adapt with NewE is the
// plainest option:
//
//	gh, err := ghkit.NewE(
//	    ghkit.Adapt(github.NewClient, github.WithHTTPClient),
//	    ghkit.WithToken(tok),
//	)
//
// HTTPClient returns the *http.Client on its own if you would rather
// construct the SDK client yourself. That form fits every SDK, and it is
// what you want when the constructor needs its own options:
//
//	hc, err := ghkit.HTTPClient(ghkit.WithToken(tok))
//	gh, err := github.NewClient(github.WithHTTPClient(hc))
//
// New takes a func(*http.Client) T factory, which fits
// github.com/shurcooL/githubv4's NewClient. NewE takes a
// func(*http.Client) (T, error) factory, for constructors that can fail.
//
// Transport stack (outer -> inner, each layer optional):
//
//	http.Client
//	 UserAgent             (overwrites User-Agent)       [WithUserAgent]
//	  RateLimit            (go-github-ratelimit v2)      [default ON]
//	   Throttle            (x/time/rate proactive)       [WithRequestsPerSecond]
//	    Retry              (5xx + transient net errors)  [WithRetry]
//	     oauth2.Transport  (clones req, sets Auth)       [WithToken/WithTokenSource]
//	      ETag             (hashes auth'd clone)         [WithETagCache]
//	       Base            (*http.Transport,
//	                        DisableCompression=true)     [WithBaseTransport]
//
// RateLimit above Throttle: a secondary cooldown parks new arrivals at
// gofri's waitForRateLimit before they consume throttle tokens. Parked
// requests release through Throttle at cooldown end, bounded by burst.
// Pre-1.4 the order was inverted; parked requests stampeded at cooldown
// end.
//
// Retry below both rate-limit layers: 429s are deferred to the reactive
// limiter. Above oauth2: retried requests get the latest token via
// oauth2's per-call Source.Token().
//
// The ETag precompute algorithm is the reason to use this kit. GitHub's
// server-side ETag hash includes the Authorization header, so a passive
// store-and-forward cache falls over under rotating auth (GitHub App
// installation tokens refresh hourly). The etag sub-package reproduces
// that hash client-side so cached entries stay useful across rotations.
// Algorithm credit: https://github.com/bored-engineer/github-conditional-http-transport
//
// # Auth patterns
//
// ghkit offers two auth paths. Pick one; do not combine them.
//
//  1. ghkit owns auth: pass WithToken or WithTokenSource. ghkit inserts an
//     oauth2.Transport into the stack and injects Authorization on every
//     outbound request. Works for static PATs and for oauth2.TokenSource
//     implementations (e.g. ghinstallation for GitHub App installation
//     tokens).
//
//  2. ghkit is auth-free; the SDK owns auth via per-call cloning. Omit
//     WithToken/WithTokenSource. Build one ghkit HTTPClient at startup,
//     hand it to your SDK, and let the SDK inject the current token per
//     call (e.g. go-github's WithAuthToken option, which sets auth above
//     ghkit's shared transport). The ETag LRU and
//     rate-limit bucket persist across token rotation. This is the
//     canonical pattern for Kubernetes operators that reconcile with a
//     per-reconcile installation token.
//
// Sub-packages (etag, ratelimit, retry, throttle) are independently
// importable for callers composing their own stack. The pages
// sub-package adds a Go 1.23 range-over-func iterator over Link-header
// pagination; polling iterates an HTTP endpoint on a caller-tunable
// interval (workflow run / check run completion); search wraps
// `/search/*` envelope endpoints with cap and incomplete-results
// awareness; cond surfaces the change-vs-unchanged signal computed
// by the etag layer. All four run on any *http.Client, so the
// configured transport stack applies per attempt.
//
// # GraphQL / v4 compatibility
//
// HTTPClient returns an *http.Client usable with any GraphQL v4 library
// (e.g. github.com/shurcooL/githubv4). The etag layer no-ops on anything but GET, so
// v4 traffic flows through oauth2 + retry + ratelimit + throttle + UA
// without ETag caching. Use WithETagCache only when you also issue REST
// GETs through the same client.
//
// # Custom cache backends
//
// The etag.Cache interface (Get/Add/Remove) is the seam for Redis,
// DynamoDB, or any other store. Implement the three methods and pass
// the result via etag.WithCache; ghkit never holds a binary dependency
// on a backend.
package ghkit
