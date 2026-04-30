// Package ghkit bundles ETag caching, rate limiting, retry on transient
// failures, and a proactive token bucket behind a single options-pattern
// API. New is generic over the returned client type, so ghkit has no
// compile-time dependency on any specific GitHub SDK; pass any
// func(*http.Client) T factory at the call site:
// github.com/google/go-github's NewClient for REST,
// github.com/shurcooL/githubv4's NewClient for GraphQL, or any other
// client constructor that takes an *http.Client.
//
// Transport stack (outer -> inner, each layer optional):
//
//	http.Client
//	 UserAgent             (overwrites User-Agent)       [WithUserAgent]
//	  Throttle             (x/time/rate proactive)       [WithRequestsPerSecond]
//	   RateLimit           (go-github-ratelimit v2)      [default ON]
//	    Retry              (5xx + transient net errors)  [WithRetry]
//	     oauth2.Transport  (clones req, sets Auth)       [WithToken/WithTokenSource]
//	      ETag             (hashes auth'd clone)         [WithETagCache]
//	       Base            (*http.Transport,
//	                        DisableCompression=true)     [WithBaseTransport]
//
// Retry sits below RateLimit so 429s are deferred to the rate-limit layer;
// sits above oauth2 so retried requests get the latest token via oauth2's
// per-call Source.Token().
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
//     call (e.g. go-github's (*Client).WithAuthToken, which clones the
//     go-github Client above ghkit's shared transport). The ETag LRU and
//     rate-limit bucket persist across token rotation. This is the
//     canonical pattern for Kubernetes operators that reconcile with a
//     per-reconcile installation token.
//
// Sub-packages (etag, ratelimit, throttle) are independently importable
// for callers composing their own stack.
//
// # GraphQL / v4 compatibility
//
// HTTPClient returns an *http.Client usable with any GraphQL v4 library
// (e.g. github.com/shurcooL/githubv4). The etag layer no-ops on POST, so
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
