package ghkit

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/pcanilho/go-github-kit/etag"
	"github.com/pcanilho/go-github-kit/ratelimit"
	"golang.org/x/oauth2"
)

// Option configures a Transport. The interface form (rather than a bare
// `func(*config)`) lets us evolve the API without breaking callers.
type Option interface{ apply(*config) }

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

type config struct {
	// Auth
	token       string
	tokenSource oauth2.TokenSource

	// Base transport. nil = use default (http.DefaultTransport clone with
	// DisableCompression).
	baseTransport http.RoundTripper

	// http.Client timeout.
	timeout time.Duration

	// ETag settings.
	etagEnabled bool
	etagOpts    []etag.Option

	// Reactive rate limiter (go-github-ratelimit).
	rateLimitEnabled bool // set to false by WithRateLimitDisabled
	rateLimitOpts    []ratelimit.Option

	// Proactive throttle (x/time/rate).
	throttleRPS   float64
	throttleBurst int

	// Diagnostics / identity.
	logger    *slog.Logger
	userAgent string
}

func newConfig() *config {
	return &config{
		rateLimitEnabled: true, // default ON
	}
}

// WithToken configures static Personal Access Token authentication.
// Exactly one of WithToken or WithTokenSource may be set.
func WithToken(pat string) Option {
	return optionFunc(func(c *config) { c.token = pat })
}

// WithTokenSource configures auth via an oauth2.TokenSource. Use this for
// GitHub App installation tokens (via ghinstallation or similar) and any
// other rotating-token setup. Exactly one of WithToken or WithTokenSource
// may be set.
func WithTokenSource(src oauth2.TokenSource) Option {
	return optionFunc(func(c *config) { c.tokenSource = src })
}

// WithBaseTransport supplies the bottom of the transport stack. When
// omitted, a cloned http.DefaultTransport with DisableCompression=true is
// used. Passing a non-nil RoundTripper that is not an *http.Transport is
// rejected when ETag caching is enabled (the gzip invariant cannot be
// enforced on an arbitrary wrapper). Passing nil is equivalent to omitting
// the option.
//
// DO NOT combine WithBaseTransport with WithToken or WithTokenSource when
// the supplied transport is not a bare *http.Transport; two auth sources
// produce undefined winner.
func WithBaseTransport(rt http.RoundTripper) Option {
	return optionFunc(func(c *config) { c.baseTransport = rt })
}

// WithTimeout sets http.Client.Timeout on the returned client.
func WithTimeout(d time.Duration) Option {
	return optionFunc(func(c *config) { c.timeout = d })
}

// WithETagCache enables the precompute-mode ETag cache. Sub-options
// (etag.WithCache, etag.WithKeyScope, etc.) configure the cache backend
// and scope.
func WithETagCache(opts ...etag.Option) Option {
	return optionFunc(func(c *config) {
		c.etagEnabled = true
		c.etagOpts = append(c.etagOpts, opts...)
	})
}

// WithRateLimit configures the reactive rate limiter (go-github-ratelimit).
// The rate limiter is ENABLED by default; call this only to register
// callbacks or tune sleep limits.
func WithRateLimit(opts ...ratelimit.Option) Option {
	return optionFunc(func(c *config) {
		c.rateLimitEnabled = true
		c.rateLimitOpts = append(c.rateLimitOpts, opts...)
	})
}

// WithRateLimitDisabled turns off the reactive rate limiter.
func WithRateLimitDisabled() Option {
	return optionFunc(func(c *config) { c.rateLimitEnabled = false })
}

// WithRequestsPerSecond enables the proactive token-bucket throttle.
// rps <= 0 or burst < 1 returns an error at construction time.
func WithRequestsPerSecond(rps float64, burst int) Option {
	return optionFunc(func(c *config) {
		c.throttleRPS = rps
		c.throttleBurst = burst
	})
}

// WithLogger supplies the slog.Logger used for diagnostic events. Default:
// slog.Default(). Forwarded to etag and ratelimit sub-packages.
func WithLogger(l *slog.Logger) Option {
	return optionFunc(func(c *config) { c.logger = l })
}

// WithUserAgent sets the User-Agent header on every outbound request at
// the transport level. Applied after any SDK sets its own User-Agent, so
// the caller's value wins. User-Agent is not in GitHub's server-side ETag
// hash domain, so setting this does not interfere with the ETag cache.
//
// An empty string is a no-op: the middleware is not inserted. To suppress
// User-Agent entirely, supply a base RoundTripper that sets an empty
// header.
func WithUserAgent(ua string) Option {
	return optionFunc(func(c *config) { c.userAgent = ua })
}
