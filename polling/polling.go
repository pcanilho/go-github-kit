package polling

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/pcanilho/go-github-kit/cond"
	"github.com/pcanilho/go-github-kit/retry"
)

// ErrMaxAttemptsExceeded is the boundary sentinel for WithMaxAttempts.
var ErrMaxAttemptsExceeded = errors.New("polling: max attempts exceeded")

// ErrMaxWallClockExceeded is the boundary sentinel for WithMaxWallClock;
// wraps context.DeadlineExceeded.
var ErrMaxWallClockExceeded = fmt.Errorf("polling: max wall-clock exceeded: %w", context.DeadlineExceeded)

// ErrInvalidInterval is returned when interval <= 0.
var ErrInvalidInterval = errors.New("polling: interval must be > 0")

// ErrInvalidOption flags malformed Option values (nil seam, n < 1,
// generic T mismatch).
var ErrInvalidOption = errors.New("polling: invalid option")

// ErrPredicatePanic is yielded when WithDone or WithDoneT panics.
var ErrPredicatePanic = errors.New("polling: predicate panicked")

// ErrNilClient is returned by Poll and As when the supplied
// *http.Client is nil.
var ErrNilClient = errors.New("polling: nil *http.Client")

// Option configures a polling iterator.
type Option func(*config)

type config struct {
	doneResp    func(*http.Response) bool
	doneTyped   any // boxed func(T) bool; unboxed in As[T]
	decodeAny   any // boxed func(*http.Response) (T, error); unboxed in As[T]
	maxAttempts int
	maxWall     time.Duration
	jitter      float64
	fullJitter  bool
	honorRA     bool
	changeOnly  bool
	logger      *slog.Logger
	sleep       func(context.Context, time.Duration) error
	now         func() time.Time
	cfgErr      error
}

// WithDone stops the iterator when the predicate returns true. Runs
// after the caller's range body, so by then the body is consumed and
// closed; the predicate must inspect status and headers only. For
// body-aware stop logic, use As[T] with WithDoneT.
func WithDone(predicate func(*http.Response) bool) Option {
	return func(c *config) { c.doneResp = predicate }
}

// WithDoneT stops As[T] when the predicate returns true. T must match
// the type parameter on As[T]; mismatches surface as ErrInvalidOption
// at the first iteration.
func WithDoneT[T any](predicate func(T) bool) Option {
	return func(c *config) { c.doneTyped = predicate }
}

// WithDecode overrides the default JSON decoder on As[T]. T must
// match the type parameter on As[T]; mismatches surface as
// ErrInvalidOption at the first iteration.
func WithDecode[T any](decode func(*http.Response) (T, error)) Option {
	return func(c *config) { c.decodeAny = decode }
}

// WithChangeOnly skips iterations where the etag layer signals a
// cache hit. No-op when the etag layer is not in the chain.
func WithChangeOnly() Option {
	return func(c *config) { c.changeOnly = true }
}

// WithMaxAttempts caps total attempts (including the first). n must
// be >= 1; n=1 means "one attempt only". Omitting the option leaves
// attempts unbounded; pair with WithMaxWallClock plus WithDoneT or
// WithDone for safety on unbounded runs. Zero or negative n surfaces
// as ErrInvalidOption at construction.
//
// Validation aligns with retry.WithMaxAttempts (both reject n < 1);
// the sentinel error type differs (polling.ErrInvalidOption vs
// retry.ErrInvalidMaxAttempts).
func WithMaxAttempts(n int) Option {
	return func(c *config) {
		if n < 1 {
			c.cfgErr = fmt.Errorf("%w: WithMaxAttempts(%d) must be >= 1", ErrInvalidOption, n)
			return
		}
		c.maxAttempts = n
	}
}

// WithMaxWallClock derives a child context deadline. Zero is unbounded.
// On expiry the iterator yields (lastResp, ErrMaxWallClockExceeded),
// which wraps context.DeadlineExceeded.
func WithMaxWallClock(d time.Duration) Option {
	return func(c *config) { c.maxWall = d }
}

// WithJitter applies a deterministic mid-point jitter:
// interval + (interval * frac / 2), clamped to [interval/2, 3*interval/2].
// Frac is clamped to [0, 1]. Not applied when honoring Retry-After.
//
// Clears WithFullJitter, so the last of the two applied wins.
func WithJitter(frac float64) Option {
	return func(c *config) {
		c.jitter = frac
		c.fullJitter = false
	}
}

// WithFullJitter applies uniform jitter over
// [interval - span/2, interval + span/2] where span = interval * frac, so
// concurrent pollers de-correlate. Frac is clamped to [0, 1]; at frac=1 the
// range is [interval/2, 3*interval/2]. Not applied when honoring
// Retry-After.
//
// Prefer this over WithJitter, which adds a fixed offset and leaves pollers
// started together in step. The last of the two applied wins.
func WithFullJitter(frac float64) Option {
	return func(c *config) {
		c.jitter = frac
		c.fullJitter = true
	}
}

// WithHonorRetryAfter (default true) honors the upstream Retry-After
// header. The honored value is clamped to [interval, max(interval,
// MaxWallClock)]; with MaxWallClock=0 the upper bound collapses to
// interval, ignoring saturated upstream values.
func WithHonorRetryAfter(b bool) Option {
	return func(c *config) { c.honorRA = b }
}

// WithLogger receives the polling_predicate_panic event. Default discards.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// WithSleepFunc is a test seam. Nil errors with ErrInvalidOption at
// construction. Production uses time.NewTimer + select-on-ctx.
func WithSleepFunc(f func(time.Duration)) Option {
	return func(c *config) {
		if f == nil {
			c.cfgErr = fmt.Errorf("%w: WithSleepFunc nil", ErrInvalidOption)
			return
		}
		c.sleep = func(_ context.Context, d time.Duration) error {
			f(d)
			return nil
		}
	}
}

// WithNowFunc is a test seam. Nil errors with ErrInvalidOption at
// construction.
func WithNowFunc(f func() time.Time) Option {
	return func(c *config) {
		if f == nil {
			c.cfgErr = fmt.Errorf("%w: WithNowFunc nil", ErrInvalidOption)
			return
		}
		c.now = f
	}
}

// withClearAsOptions strips the As[T]-only options (WithDoneT,
// WithDecode) and the Poll-only WithDone before delegating to Poll
// from As[T]. As[T] has already captured the typed predicate and
// decoder into its own scope; Poll's guards would otherwise reject
// the delegation.
func withClearAsOptions() Option {
	return func(c *config) {
		c.doneResp = nil
		c.doneTyped = nil
		c.decodeAny = nil
	}
}

func newConfig(opts []Option) (*config, error) {
	cfg := &config{
		honorRA: true,
		now:     time.Now,
		sleep:   ctxSleep,
		logger:  slog.New(slog.DiscardHandler),
	}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.cfgErr != nil {
		return nil, cfg.cfgErr
	}
	if cfg.jitter < 0 {
		cfg.jitter = 0
	}
	if cfg.jitter > 1 {
		cfg.jitter = 1
	}
	return cfg, nil
}

// yieldDoErr surfaces a c.Do error, remapping a wall-clock-driven
// DeadlineExceeded to ErrMaxWallClockExceeded.
func yieldDoErr(yield func(*http.Response, error) bool, cfg *config, lastResp *http.Response, derr error) {
	if cfg.maxWall > 0 && errors.Is(derr, context.DeadlineExceeded) {
		yield(lastResp, ErrMaxWallClockExceeded)
		return
	}
	yield(nil, derr)
}

// ctxSleep blocks for d or until ctx is done, mirroring
// retry/retry.go's timer pattern.
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return ctx.Err()
	}
}

// Poll iterates an HTTP endpoint on interval, reusing the supplied
// *http.Client so the transport stack applies per attempt. The caller
// drains and closes each yielded body. See package doc for stop
// conditions and sharp edges.
func Poll(
	ctx context.Context,
	c *http.Client,
	method, url string,
	headers http.Header,
	body []byte,
	interval time.Duration,
	opts ...Option,
) iter.Seq2[*http.Response, error] {
	return func(yield func(*http.Response, error) bool) {
		if c == nil {
			yield(nil, ErrNilClient)
			return
		}
		if interval <= 0 {
			yield(nil, ErrInvalidInterval)
			return
		}
		cfg, err := newConfig(opts)
		if err != nil {
			yield(nil, err)
			return
		}
		if cfg.doneTyped != nil {
			yield(nil, fmt.Errorf("%w: WithDoneT requires As[T]", ErrInvalidOption))
			return
		}
		if cfg.decodeAny != nil {
			yield(nil, fmt.Errorf("%w: WithDecode requires As[T]", ErrInvalidOption))
			return
		}

		runCtx := ctx
		if cfg.maxWall > 0 {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithDeadline(ctx, cfg.now().Add(cfg.maxWall))
			defer cancel()
		}

		attempt := 0
		var lastResp *http.Response
		for {
			if cerr := runCtx.Err(); cerr != nil {
				if cfg.maxWall > 0 && errors.Is(cerr, context.DeadlineExceeded) {
					yield(lastResp, ErrMaxWallClockExceeded)
				}
				return
			}

			req, rerr := buildRequest(runCtx, method, url, headers, body)
			if rerr != nil {
				yield(nil, rerr)
				return
			}

			resp, derr := c.Do(req) //nolint:bodyclose // yielded to caller; caller closes per Poll contract
			attempt++
			if derr != nil {
				yieldDoErr(yield, cfg, lastResp, derr)
				return
			}

			if cfg.changeOnly && cond.StatusOf(resp) == cond.Unchanged {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if cfg.maxAttempts > 0 && attempt >= cfg.maxAttempts {
					yield(lastResp, ErrMaxAttemptsExceeded)
					return
				}
				if serr := cfg.sleep(runCtx, nextSleep(cfg, nil, interval)); serr != nil {
					if cfg.maxWall > 0 && errors.Is(serr, context.DeadlineExceeded) {
						yield(lastResp, ErrMaxWallClockExceeded)
					}
					return
				}
				continue
			}

			lastResp = resp
			if !yield(resp, nil) {
				return
			}

			if cfg.doneResp != nil {
				stop, panicked := callPredicateResp(cfg, resp)
				if panicked {
					yield(resp, ErrPredicatePanic)
					return
				}
				if stop {
					return
				}
			}

			if cfg.maxAttempts > 0 && attempt >= cfg.maxAttempts {
				yield(resp, ErrMaxAttemptsExceeded)
				return
			}

			if serr := cfg.sleep(runCtx, nextSleep(cfg, resp, interval)); serr != nil {
				if cfg.maxWall > 0 && errors.Is(serr, context.DeadlineExceeded) {
					yield(resp, ErrMaxWallClockExceeded)
				}
				return
			}
		}
	}
}

// As decodes each polling response into T (default: encoding/json).
// The iterator owns and closes each body via defer; decode errors
// yield once and stop, decoder panics propagate with the body closed.
// Use WithDoneT to stop on the decoded value.
func As[T any](
	ctx context.Context,
	c *http.Client,
	method, url string,
	headers http.Header,
	body []byte,
	interval time.Duration,
	opts ...Option,
) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		cfg, err := newConfig(opts)
		if err != nil {
			yield(zero, err)
			return
		}
		if cfg.doneResp != nil {
			yield(zero, fmt.Errorf("%w: WithDone is for Poll; use WithDoneT on As[T]", ErrInvalidOption))
			return
		}

		var typedDone func(T) bool
		if cfg.doneTyped != nil {
			td, ok := cfg.doneTyped.(func(T) bool)
			if !ok {
				yield(zero, fmt.Errorf("%w: WithDoneT[T] T mismatch with As[T]", ErrInvalidOption))
				return
			}
			typedDone = td
		}

		decode := jsonDecode[T]
		if cfg.decodeAny != nil {
			d, ok := cfg.decodeAny.(func(*http.Response) (T, error))
			if !ok {
				yield(zero, fmt.Errorf("%w: WithDecode[T] T mismatch with As[T]", ErrInvalidOption))
				return
			}
			decode = d
		}

		// Delegate per-attempt iteration to Poll. Clear the typed
		// predicate so Poll's response-side path does not also try to
		// inspect it; we run typedDone here after decode.
		pollOpts := append([]Option{}, opts...)
		pollOpts = append(pollOpts, withClearAsOptions())

		for resp, perr := range Poll(ctx, c, method, url, headers, body, interval, pollOpts...) { //nolint:bodyclose // body closed by decodeAndClose
			if perr != nil {
				// Setup error (resp == nil) or boundary yield from Poll
				// (e.g., ErrMaxAttemptsExceeded with lastResp whose body
				// was decoded and closed on the prior iteration). Either
				// way, do not attempt to decode; surface the error.
				yield(zero, perr)
				return
			}
			val, decErr := decodeAndClose(resp, decode)
			if decErr != nil {
				yield(zero, fmt.Errorf("polling: decode: %w", decErr))
				return
			}
			if !yield(val, nil) {
				return
			}
			if typedDone != nil {
				stop, panicked := callPredicateTyped(cfg, typedDone, val)
				if panicked {
					yield(zero, ErrPredicatePanic)
					return
				}
				if stop {
					return
				}
			}
		}
	}
}

func buildRequest(ctx context.Context, method, url string, headers http.Header, body []byte) (*http.Request, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("polling: build request: %w", err)
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	return req, nil
}

func decodeAndClose[T any](resp *http.Response, decode func(*http.Response) (T, error)) (T, error) {
	defer resp.Body.Close() //nolint:errcheck // best-effort close; decode error is the primary signal
	return decode(resp)
}

func jsonDecode[T any](resp *http.Response) (T, error) {
	var v T
	err := json.NewDecoder(resp.Body).Decode(&v)
	return v, err
}

func nextSleep(cfg *config, resp *http.Response, interval time.Duration) time.Duration {
	if cfg.honorRA && resp != nil {
		if d, ok := retry.RetryAfter(resp); ok {
			lo := interval
			hi := max(cfg.maxWall, interval)
			if d < lo {
				d = lo
			}
			if d > hi {
				d = hi
			}
			return d
		}
	}
	d := interval
	if cfg.jitter > 0 {
		// Clamp in float space. Out-of-range float64->int64 is
		// implementation-defined: amd64 yields MinInt64, arm64 saturates to
		// MaxInt64, so no post-conversion check catches both.
		var span time.Duration
		if f := float64(interval) * cfg.jitter; f >= 1 && f < float64(math.MaxInt64) {
			span = time.Duration(f)
		}
		if cfg.fullJitter {
			// Uniform across the span, centred on interval. Unlike retry's
			// decorrelated backoff this must not drift upward.
			//nolint:gosec // G404: math/rand/v2 is intentional for jitter; not a crypto context.
			d = interval - span/2 + time.Duration(rand.Int64N(int64(span)+1))
		} else {
			d = interval + span/2
		}
		if d < interval/2 {
			d = interval / 2
		}
		if d > 3*interval/2 {
			d = 3 * interval / 2
		}
	}
	return d
}

func callPredicateResp(cfg *config, resp *http.Response) (stop, panicked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			cfg.logger.Warn("polling_event",
				"event", "polling_predicate_panic",
				"panic_type", fmt.Sprintf("%T", rec),
			)
			panicked = true
		}
	}()
	return cfg.doneResp(resp), false
}

func callPredicateTyped[T any](cfg *config, p func(T) bool, v T) (stop, panicked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			cfg.logger.Warn("polling_event",
				"event", "polling_predicate_panic",
				"panic_type", fmt.Sprintf("%T", rec),
			)
			panicked = true
		}
	}()
	return p(v), false
}
