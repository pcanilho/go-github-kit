// Package polling iterates an HTTP endpoint on an interval, reusing
// the supplied *http.Client so the configured transport stack
// (RateLimit, Throttle, Retry, oauth2, ETag) applies per attempt.
//
// Two entry points:
//
//   - Poll yields *http.Response per attempt. The caller drains and
//     closes each body. WithDone is header/status-only; the body is
//     consumed by the time the predicate runs.
//   - As[T] decodes each response into T (default JSON) and yields
//     the typed value. The iterator owns and closes each body. Use
//     WithDoneT to stop on the decoded value; use WithDecode to
//     swap the decoder.
//
// Stop conditions: ctx cancel, predicate match, WithMaxAttempts hit,
// WithMaxWallClock expiry, transport error, predicate panic. On
// MaxAttempts and MaxWallClock the iterator yields (lastResp,
// sentinel) so the caller can inspect headers/status of what
// triggered the limit; lastResp.Body has already been drained and
// closed by the caller's prior normal-yield iteration (per Poll's
// body-ownership contract). lastResp may be nil if WithChangeOnly
// suppressed every yield. ErrMaxWallClockExceeded wraps
// context.DeadlineExceeded.
//
// Sharp edges:
//
//   - Each c.Do may itself loop through retry.Transport (default 3
//     attempts, 200ms-2s jitter). When polling owns the outer loop,
//     pass retry.WithMaxAttempts(1) on the client.
//   - throttle.WithRequestsPerSecond below 1/interval dominates
//     cadence; effective_interval = max(interval, 1/rps).
//   - With WithETagCache and an unchanged resource, As[T] decodes
//     identical bytes per tick. WithChangeOnly silently skips those.
//   - gofri ctx-cancel during a secondary cooldown is observed on the
//     next inner round-trip, not synchronously.
//
// Determinism: production uses time.NewTimer + select-on-ctx. Tests
// inject WithSleepFunc / WithNowFunc. Jitter is a deterministic
// mid-point clamp; not applied when honoring Retry-After.
//
// Body argument: the body []byte passed to Poll/As is not deep-copied;
// the iterator constructs a fresh bytes.NewReader(body) per attempt
// that aliases the slice. Do not mutate the slice while polling.
//
// Retry-After interaction with WithMaxWallClock: an honored Retry-After
// exceeding the remaining wall-clock budget is truncated by the ctx
// deadline and surfaces as ErrMaxWallClockExceeded.
//
// WithNowFunc and ctx.Deadline: WithNowFunc affects the value passed
// to context.WithDeadline at construction (now() + MaxWallClock); the
// resulting deadline still fires on real wall-clock. A frozen-past
// nowFunc therefore cancels immediately.
//
// Slog field allowlist (polling_predicate_panic):
//   - event:      "polling_predicate_panic"
//   - panic_type: fmt.Sprintf("%T", recovered) (type name, no payload)
//
// No request/response headers are emitted to slog; the panic event
// echoes only the recovered value's Go type name.
package polling
