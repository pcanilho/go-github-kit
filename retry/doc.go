// Package retry wraps an http.RoundTripper with retries on transient
// failures (5xx responses, network errors, transport-level deadline
// exceeded).
//
// The default predicate retries idempotent methods (GET, HEAD, OPTIONS, PUT,
// DELETE) on {500, 502, 503, 504} or recognised transient network errors.
// Known-permanent failures (DNS NXDOMAIN, ECONNREFUSED, x509 validation
// errors) are explicitly NOT retried even when wrapped in a *net.OpError,
// so a misconfigured URL or expired cert fails fast instead of burning the
// retry budget.
//
// 429 Too Many Requests is hard-excluded so callers can compose retry with
// the ratelimit package without the two layers fighting.
//
// Backoff uses decorrelated jitter with caller-supplied min/max bounds.
// Server-supplied Retry-After (delta-seconds or HTTP-date) overrides the
// jitter; if it exceeds the operator's maxDelay the call returns
// ErrRetryAfterExceedsMax with the prior response (which the caller must
// Close).
//
// Caller-context cancellation is terminal: req.Context().Err() != nil stops
// retries before any predicate is consulted, including a user-supplied
// WithRetryOn.
//
// Body-bearing requests must set req.GetBody so the body can be rewound
// per attempt; a nil GetBody on a retry attempt yields
// errors.Join(ErrBodyNotRewindable, prior_err).
//
// Throttle interaction: each retry attempt is a real HTTP call from the
// throttle layer's perspective, so a worst-case failing request can briefly
// use maxAttempts times the nominal RPS budget.
package retry
