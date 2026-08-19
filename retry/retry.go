package retry

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrInvalidMaxAttempts is returned when WithMaxAttempts is out of range.
var ErrInvalidMaxAttempts = errors.New("retry: maxAttempts must be in [1, 100]")

// ErrInvalidBackoff is returned when WithBackoff bounds are invalid.
var ErrInvalidBackoff = errors.New("retry: backoff requires 1ms <= minDelay <= maxDelay <= 1h")

// ErrBodyNotRewindable is joined with the prior error when a retry
// attempt is needed for a body-bearing request with no GetBody.
var ErrBodyNotRewindable = errors.New("retry: cannot retry request with non-nil Body and nil GetBody")

// ErrRetryAfterExceedsMax is returned when a server's Retry-After header
// exceeds the operator-configured maxDelay. When this error is returned,
// resp is nil; the transport drains and closes the prior response before
// returning, satisfying the http.RoundTripper invariant that a non-nil
// error implies the caller has nothing to close.
var ErrRetryAfterExceedsMax = errors.New("retry: server Retry-After exceeds operator max delay")

const (
	defaultMaxAttempts = 3
	defaultMinDelay    = 200 * time.Millisecond
	defaultMaxDelay    = 2 * time.Second

	maxAttemptsCeiling = 100
	minDelayFloor      = time.Millisecond
	maxDelayCeiling    = time.Hour

	drainCap = 128 << 10
)

// Option configures a Transport.
type Option interface{ apply(*config) }

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

type config struct {
	maxAttempts   int
	minDelay      time.Duration
	maxDelay      time.Duration
	userPredicate func(*http.Request, *http.Response, error) bool
	logger        *slog.Logger
}

// WithMaxAttempts sets the total number of attempts including the initial
// request. n=1 disables retries (single attempt only). n must be in [1, 100];
// values outside that range return ErrInvalidMaxAttempts at construction.
// The 100 ceiling guards against misconfigured env vars (e.g.
// MAX_RETRIES=10000 instead of 10) blocking goroutines for hours.
func WithMaxAttempts(n int) Option {
	return optionFunc(func(c *config) { c.maxAttempts = n })
}

// WithBackoff sets the decorrelated-jitter window. Defaults: 200ms / 2s.
// Constraints: 1ms <= minDelay <= maxDelay <= 1h. The 1ms floor prevents
// sub-millisecond intervals that would hammer the server; the 1h ceiling
// keeps prev*3 well within int64 in computeJitter.
func WithBackoff(minDelay, maxDelay time.Duration) Option {
	return optionFunc(func(c *config) {
		c.minDelay = minDelay
		c.maxDelay = maxDelay
	})
}

// WithRetryOn replaces the default retry predicate. The replacement takes
// ownership of method-safety: the default's idempotency check is bypassed.
// The 429 hard-exclusion in RoundTrip still applies regardless of this
// predicate. Caller-context cancellation (req.Context().Err() != nil) always
// stops retries before the predicate is consulted.
//
// Panics in the predicate are recovered: the call is treated as "do not
// retry" and a retry_predicate_panic event is logged.
func WithRetryOn(predicate func(*http.Request, *http.Response, error) bool) Option {
	return optionFunc(func(c *config) { c.userPredicate = predicate })
}

// WithLogger supplies the slog.Logger for diagnostic events. Pass nil
// (or omit the option) to silence the package; a nil logger is replaced
// with slog.New(slog.DiscardHandler) at construction.
func WithLogger(l *slog.Logger) Option {
	return optionFunc(func(c *config) { c.logger = l })
}

// IsIdempotent reports whether a request method can be retried by default.
// Per RFC 7231 these are GET, HEAD, OPTIONS, PUT, DELETE.
func IsIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// IsRetryable5xx reports whether a 5xx code is transient (worth retrying).
// 501 Not Implemented and 505 HTTP Version Not Supported are permanent.
func IsRetryable5xx(code int) bool {
	switch code {
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// IsTransientNetErr reports whether a transport-level error is worth
// retrying. Known-permanent failures (NXDOMAIN, ECONNREFUSED, x509
// validation errors) short-circuit to false so we don't waste budget on
// them. Otherwise *net.OpError, io.EOF/io.ErrUnexpectedEOF, and
// context.DeadlineExceeded from the inner transport are treated as
// transient.
func IsTransientNetErr(err error) bool {
	if isPermanentNetErr(err) {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// IsNotFound was filtered out above; remaining DNS errors (server
		// failure, timeout) are transient.
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}

// isPermanentNetErr identifies failures that retrying cannot resolve within
// the lifetime of a single request: DNS NXDOMAIN, TCP refusal from a closed
// port, and x509 validation failures.
func isPermanentNetErr(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return true
	}
	var hostnameErr *x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return true
	}
	var certInvalid x509.CertificateInvalidError
	return errors.As(err, &certInvalid)
}

// Transport is a chainable http.RoundTripper that retries transient failures
// from the inner transport. Safe for concurrent use.
type Transport struct {
	base          http.RoundTripper
	maxAttempts   int
	minDelay      time.Duration
	maxDelay      time.Duration
	userPredicate func(*http.Request, *http.Response, error) bool
	logger        *slog.Logger
}

// NewTransport returns a retry middleware wrapping base. When base is nil,
// http.DefaultTransport is used.
//
// Returns ErrInvalidMaxAttempts or ErrInvalidBackoff at construction when
// the option combination is out of bounds.
func NewTransport(base http.RoundTripper, opts ...Option) (http.RoundTripper, error) {
	cfg := &config{
		maxAttempts: defaultMaxAttempts,
		minDelay:    defaultMinDelay,
		maxDelay:    defaultMaxDelay,
	}
	for _, o := range opts {
		o.apply(cfg)
	}
	if cfg.maxAttempts < 1 || cfg.maxAttempts > maxAttemptsCeiling {
		return nil, ErrInvalidMaxAttempts
	}
	if cfg.minDelay < minDelayFloor || cfg.maxDelay > maxDelayCeiling || cfg.minDelay > cfg.maxDelay {
		return nil, ErrInvalidBackoff
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{
		base:          base,
		maxAttempts:   cfg.maxAttempts,
		minDelay:      cfg.minDelay,
		maxDelay:      cfg.maxDelay,
		userPredicate: cfg.userPredicate,
		logger:        cmp.Or(cfg.logger, slog.New(slog.DiscardHandler)),
	}, nil
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var (
		prevSleep = t.minDelay
		resp      *http.Response
		err       error
	)

	for attempt := 0; attempt < t.maxAttempts; attempt++ {
		if attempt > 0 {
			raOverride, outcome := parseRetryAfter(resp)

			if outcome == outcomeUnparseable {
				raw := ""
				if resp != nil {
					raw = previewRawHeader(resp.Header.Get("Retry-After"))
				}
				t.logEvent(ctx, slog.LevelWarn, "retry_retry_after_unparseable",
					"attempt", attempt,
					"raw", raw)
			}

			if raOverride > t.maxDelay {
				if resp != nil && resp.Body != nil {
					_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
					_ = resp.Body.Close()
				}
				t.logEvent(ctx, slog.LevelWarn, "retry_abort",
					"reason", "retry_after_exceeds_max",
					"retry_after_ms", raOverride.Milliseconds(),
					"max_delay_ms", t.maxDelay.Milliseconds())
				return nil, ErrRetryAfterExceedsMax
			}

			computed := t.computeJitter(prevSleep)
			sleep := computed
			if raOverride > 0 {
				sleep = raOverride
			}

			// Body can be nil when a hand-rolled base transport supplied
			// via WithBaseTransport returns a bare &http.Response.
			if resp != nil {
				if resp.Body != nil {
					_, _ = io.CopyN(io.Discard, resp.Body, drainCap)
					_ = resp.Body.Close()
				}
				resp = nil
			}

			t.logEvent(ctx, slog.LevelDebug, "retry_sleep",
				"attempt", attempt,
				"sleep_ms", sleep.Milliseconds(),
				"source", sourceLabel(raOverride, outcome))

			timer := time.NewTimer(sleep)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil, ctx.Err()
			}

			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}

			if req.Body != nil {
				if req.GetBody == nil {
					t.logEvent(ctx, slog.LevelWarn, "retry_body_unrewindable",
						"attempt", attempt,
						"method", req.Method,
						"prior_err_type", typeOf(err))
					return nil, errors.Join(ErrBodyNotRewindable, err)
				}
				newBody, gerr := req.GetBody()
				if gerr != nil {
					return nil, fmt.Errorf("retry: GetBody after attempt %d: %w", attempt, gerr)
				}
				reqCopy := req.Clone(ctx)
				reqCopy.Body = newBody
				req = reqCopy
			}

			if raOverride <= 0 {
				prevSleep = computed
			}
		}

		resp, err = t.base.RoundTrip(req)

		if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
			t.logEvent(ctx, slog.LevelDebug, "retry_decision",
				"attempt", attempt, "decision", "stop", "reason", "429_handoff")
			return resp, err
		}

		if !t.shouldRetry(req, resp, err) {
			if attempt != 0 || err != nil || resp == nil || resp.StatusCode >= 500 {
				t.logEvent(ctx, slog.LevelDebug, "retry_decision",
					"attempt", attempt, "decision", "stop",
					"reason", classifyStopReason(req, resp, err))
			}
			return resp, err
		}
		t.logEvent(ctx, slog.LevelDebug, "retry_decision",
			"attempt", attempt, "decision", "retry",
			"reason", classifyRetryReason(resp, err))
	}

	attrs := []any{"attempts", t.maxAttempts, "last_err_type", typeOf(err)}
	if s := statusOf(resp); s != "" {
		attrs = append(attrs, "last_status", s)
	}
	t.logEvent(ctx, slog.LevelWarn, "retry_exhausted", attrs...)
	return resp, err
}

func (t *Transport) shouldRetry(req *http.Request, resp *http.Response, err error) bool {
	if cerr := req.Context().Err(); cerr != nil {
		return false
	}
	if t.userPredicate != nil {
		return t.callUserPredicate(req, resp, err)
	}
	if !IsIdempotent(req.Method) {
		return false
	}
	if err != nil {
		return IsTransientNetErr(err)
	}
	return resp != nil && IsRetryable5xx(resp.StatusCode)
}

func (t *Transport) callUserPredicate(req *http.Request, resp *http.Response, err error) (out bool) {
	ctx := req.Context()
	defer func() {
		if r := recover(); r != nil {
			t.logEvent(ctx, slog.LevelWarn, "retry_predicate_panic",
				"panic_type", fmt.Sprintf("%T", r))
			out = false
		}
	}()
	return t.userPredicate(req, resp, err)
}

func (t *Transport) logEvent(ctx context.Context, level slog.Level, event string, kvs ...any) {
	attrs := append([]any{"event", event}, kvs...)
	t.logger.Log(ctx, level, "retry_event", attrs...)
}

// computeJitter implements decorrelated jitter (AWS).
// Precondition (NewTransport-validated): 0 < minDelay <= maxDelay, prev >= minDelay.
func (t *Transport) computeJitter(prev time.Duration) time.Duration {
	span := int64(prev*3 - t.minDelay + 1)
	//nolint:gosec // G404: math/rand/v2 is intentional for backoff jitter; not a crypto context.
	n := time.Duration(rand.Int64N(span))
	return min(t.maxDelay, t.minDelay+n)
}

// RetryAfter parses a response's Retry-After header per RFC 9110 §10.2.3
// and reports whether it should be honored as a sleep override. Returns
// (0, false) for absent, unparseable, OR negative-numeric values; returns
// the duration plus true otherwise. Used by sibling packages that want to
// honor server hints without duplicating the parser.
func RetryAfter(resp *http.Response) (time.Duration, bool) {
	d, outcome := parseRetryAfter(resp)
	if outcome != outcomeNumeric && outcome != outcomeDate {
		return 0, false
	}
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// parseOutcome lets the retry_sleep source label split "no override" from
// "upstream sent an off-spec value". Both yield duration 0 today.
type parseOutcome int

const (
	outcomeAbsent parseOutcome = iota
	outcomeNumeric
	outcomeDate
	outcomeUnparseable
)

// parseRetryAfter follows RFC 9110 §10.2.3 (delta-seconds or HTTP-date).
// RFC 3339 / ISO 8601 is not in the spec and is not accepted.
func parseRetryAfter(resp *http.Response) (time.Duration, parseOutcome) {
	if resp == nil {
		return 0, outcomeAbsent
	}
	h := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if h == "" {
		return 0, outcomeAbsent
	}
	if secs, err := strconv.Atoi(h); err == nil {
		switch {
		case secs < 0:
			return 0, outcomeNumeric
		case secs == 0:
			return time.Nanosecond, outcomeNumeric
		case int64(secs) > int64(math.MaxInt64)/int64(time.Second):
			// Saturate above maxDelayCeiling so the abort path trips
			// instead of int64-ns multiplication wrapping silently.
			return maxDelayCeiling + time.Hour, outcomeNumeric
		}
		return time.Duration(secs) * time.Second, outcomeNumeric
	}
	if target, err := http.ParseTime(h); err == nil {
		switch d := time.Until(target); {
		case d < 0:
			return 0, outcomeDate
		case d == 0:
			return time.Nanosecond, outcomeDate
		default:
			return d, outcomeDate
		}
	}
	return 0, outcomeUnparseable
}

// sourceJitter labels a sleep whose duration came from computeJitter
// rather than an upstream Retry-After.
const sourceJitter = "jitter"

func sourceLabel(raOverride time.Duration, outcome parseOutcome) string {
	switch {
	case raOverride > 0:
		return "retry_after"
	case outcome == outcomeUnparseable:
		return "malformed"
	default:
		return sourceJitter
	}
}

// previewRawHeader bounds slog payload size and prevents log-line injection
// from a hostile upstream Retry-After value.
func previewRawHeader(s string) string {
	const maxBytes = 32
	if len(s) > maxBytes {
		s = s[:maxBytes]
	}
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c >= 0x7f {
			b[i] = '?'
		}
	}
	return string(b)
}

func classifyStopReason(req *http.Request, resp *http.Response, err error) string {
	if req.Context().Err() != nil {
		return "context_cancelled"
	}
	if !IsIdempotent(req.Method) {
		return "non_idempotent"
	}
	if err == nil && resp != nil && resp.StatusCode < 500 {
		return "success"
	}
	return "predicate_false"
}

func classifyRetryReason(resp *http.Response, err error) string {
	if err != nil {
		return "net_error"
	}
	_ = resp
	return "5xx"
}

func statusOf(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	return strconv.Itoa(resp.StatusCode)
}

// typeOf walks the error chain. "/" separates wrap-chain depth; "|" separates
// Join siblings. Capped at depth 5; "..." marks truncation.
func typeOf(err error) string {
	if err == nil {
		return ""
	}
	return strings.Join(walkErrTypes(err, 0), "/")
}

func walkErrTypes(err error, depth int) []string {
	if err == nil {
		return nil
	}
	if depth >= 5 {
		return []string{"..."}
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		var parts []string
		for _, e := range u.Unwrap() {
			parts = append(parts, strings.Join(walkErrTypes(e, depth+1), "/"))
		}
		return []string{strings.Join(parts, "|")}
	}
	out := []string{fmt.Sprintf("%T", err)}
	if next := errors.Unwrap(err); next != nil {
		out = append(out, walkErrTypes(next, depth+1)...)
	}
	return out
}
