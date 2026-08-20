package ghkit

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pcanilho/go-github-kit/etag"
	"github.com/pcanilho/go-github-kit/ratelimit"
	"github.com/pcanilho/go-github-kit/retry"
	"github.com/pcanilho/go-github-kit/throttle"
	"golang.org/x/oauth2"
)

// Sentinel errors for config validation. Callers can use errors.Is to
// distinguish specific failure modes in tests or runtime handling.
var (
	ErrConflictingAuth       = errors.New("ghkit: WithToken and WithTokenSource are mutually exclusive")
	ErrConflictingRateLimit  = errors.New("ghkit: WithRateLimit and WithRateLimitDisabled are mutually exclusive")
	ErrPreAuthedBaseWithAuth = errors.New("ghkit: WithBaseTransport with a non-*http.Transport base cannot be combined with WithToken or WithTokenSource")
	ErrNonPositiveRPS        = errors.New("ghkit: WithRequestsPerSecond requires rps > 0 and burst >= 1")
	ErrNilFactory            = errors.New("ghkit: New requires a non-nil factory function")
	ErrETagTransportType     = errors.New("ghkit: WithETagTransport: constructed transport is not an *etag.Transport")
)

// HTTPClient builds an *http.Client with the configured transport stack.
// Returns an error when the option combination is invalid; see the sentinel
// errors above.
func HTTPClient(opts ...Option) (*http.Client, error) {
	cfg := newConfig()
	for _, o := range opts {
		o.apply(cfg)
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	// Silent by default: no logger means no log output. Sub-packages also
	// receive this (possibly discarding) logger, but per-sub-package
	// WithLogger options can still override since user opts apply after
	// our defaulted one (see prepend pattern below).
	cfg.logger = cmp.Or(cfg.logger, slog.New(slog.DiscardHandler))

	// Build inside-out: base -> etag -> oauth2 -> retry -> throttle -> ratelimit -> userAgent.
	// ETag below oauth2: hash domain sees the cloned Authorization header.
	// RateLimit above throttle: secondary cooldown parks new arrivals before
	// they consume throttle tokens. UserAgent outermost: overrides any
	// SDK-level value.
	rt := cfg.baseTransport

	if cfg.etagEnabled {
		etagOpts := append([]etag.Option{etag.WithLogger(cfg.logger)}, cfg.etagOpts...)
		inner, err := etag.NewTransport(rt, etagOpts...)
		if err != nil {
			return nil, fmt.Errorf("ghkit: etag: %w", err)
		}
		// NewTransport returns http.RoundTripper; comma-ok keeps this
		// correct if that ever changes.
		if len(cfg.etagTransportFns) > 0 {
			et, ok := inner.(*etag.Transport)
			if !ok {
				return nil, fmt.Errorf("%w: got %T", ErrETagTransportType, inner)
			}
			for _, fn := range cfg.etagTransportFns {
				fn(et)
			}
		}
		rt = inner
	}

	if cfg.token != "" || cfg.tokenSource != nil {
		src := cfg.tokenSource
		if cfg.token != "" {
			src = oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.token})
		}
		rt = &oauth2.Transport{Source: src, Base: rt}
	} else if rt == nil {
		rt = http.DefaultTransport
	}

	if cfg.retryEnabled {
		retryOpts := append([]retry.Option{retry.WithLogger(cfg.logger)}, cfg.retryOpts...)
		inner, err := retry.NewTransport(rt, retryOpts...)
		if err != nil {
			return nil, fmt.Errorf("ghkit: retry: %w", err)
		}
		rt = inner
	}

	if cfg.throttleRPS > 0 {
		thr, err := throttle.NewTransport(rt, cfg.throttleRPS, throttle.WithBurst(cfg.throttleBurst))
		if err != nil {
			return nil, fmt.Errorf("ghkit: throttle: %w", err)
		}
		rt = thr
	}

	if cfg.rateLimitEnabled {
		rlOpts := append([]ratelimit.Option{ratelimit.WithLogger(cfg.logger)}, cfg.rateLimitOpts...)
		rt = ratelimit.NewTransport(rt, rlOpts...)
	}

	if cfg.userAgent != "" {
		rt = &userAgentTransport{base: rt, ua: cfg.userAgent}
	}

	hc := &http.Client{Transport: rt}
	if cfg.timeout > 0 {
		hc.Timeout = cfg.timeout
	}
	return hc, nil
}

// userAgentTransport sets a fixed User-Agent header on every request. It
// clones the request before mutating (the http.RoundTripper contract
// forbids modifying the caller's request).
type userAgentTransport struct {
	base http.RoundTripper
	ua   string
}

func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("User-Agent", t.ua)
	return t.base.RoundTrip(cloned)
}

// New builds an *http.Client via HTTPClient and plumbs it into the
// caller-supplied factory. Generic over the returned type so ghkit has no
// compile-time dependency on any specific GitHub SDK; pass whichever
// constructor you use at the call site.
//
// Use New for constructors that cannot fail, which take the *http.Client
// as their only argument:
//
//	import "github.com/shurcooL/githubv4"
//
//	v4, err := ghkit.New(githubv4.NewClient,
//	    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
//	)
//
// go-github v87 changed NewClient to
// `NewClient(opts ...ClientOptionsFunc) (*Client, error)`, so it no longer
// binds here. Use Adapt with NewE instead.
//
// When factory is nil, New returns the zero value of T and ErrNilFactory.
// When HTTPClient returns an error (invalid option combination), New
// proxies the error.
func New[T any](factory func(*http.Client) T, opts ...Option) (T, error) {
	var zero T
	if factory == nil {
		return zero, ErrNilFactory
	}
	hc, err := HTTPClient(opts...)
	if err != nil {
		return zero, err
	}
	return factory(hc), nil
}

// NewE is New for SDK constructors that return an error. For go-github,
// pair it with Adapt:
//
//	gh, err := ghkit.NewE(
//	    ghkit.Adapt(github.NewClient, github.WithHTTPClient),
//	    ghkit.WithToken(tok),
//	)
//
// Pass a closure instead when the constructor needs its own options:
//
//	gh, err := ghkit.NewE(func(hc *http.Client) (*github.Client, error) {
//	    return github.NewClient(
//	        github.WithHTTPClient(hc),
//	        github.WithEnterpriseURLs(baseURL, uploadURL),
//	    )
//	}, ghkit.WithToken(tok))
//
// Use New for constructors that cannot fail, such as githubv4.NewClient.
//
// Factory errors are wrapped as "ghkit: factory: %w". A nil factory returns
// the zero value of T and ErrNilFactory.
func NewE[T any](factory func(*http.Client) (T, error), opts ...Option) (T, error) {
	var zero T
	if factory == nil {
		return zero, ErrNilFactory
	}
	hc, err := HTTPClient(opts...)
	if err != nil {
		return zero, err
	}
	v, err := factory(hc)
	if err != nil {
		return zero, fmt.Errorf("ghkit: factory: %w", err)
	}
	return v, nil
}

// Adapt converts an SDK constructor that is variadic over its own options
// into the func(*http.Client) (T, error) shape NewE takes. go-github v87
// changed NewClient to `NewClient(opts ...ClientOptionsFunc) (*Client, error)`,
// which no longer binds to New or NewE on its own:
//
//	import "github.com/google/go-github/v90/github"
//
//	gh, err := ghkit.NewE(
//	    ghkit.Adapt(github.NewClient, github.WithHTTPClient),
//	    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
//	)
//
// Adapt pairs with NewE, not New: it returns an error-returning factory, so
// ghkit.New(ghkit.Adapt(...)) does not compile.
//
// It fits constructors shaped func(...O) (T, error) only. SDKs taking a
// leading positional argument or a context, such as ghinstallation or
// google.golang.org/api, do not fit; build those on HTTPClient directly.
//
// httpOption must be the SDK option constructor that accepts the
// *http.Client, which for go-github is the only such option. Options that
// would replace the transport stack (github.WithTransport) or read it back
// (github.WithEnvProxy) take other types and will not compile here.
//
// Adapt passes no further options. When the constructor needs its own, pass
// a closure to NewE instead, as shown on NewE.
//
// A nil factory or httpOption yields a nil result, so New and NewE report
// ErrNilFactory.
func Adapt[T, O any](factory func(...O) (T, error), httpOption func(*http.Client) O) func(*http.Client) (T, error) {
	if factory == nil || httpOption == nil {
		return nil
	}
	return func(hc *http.Client) (T, error) {
		return factory(httpOption(hc))
	}
}

func validateConfig(c *config) error {
	if c.token != "" && c.tokenSource != nil {
		return ErrConflictingAuth
	}

	// WithRateLimit and WithRateLimitDisabled together is a contradiction:
	// the user registered callbacks while explicitly turning the layer off.
	if c.rateLimitDisabledByUser && len(c.rateLimitOpts) > 0 {
		return ErrConflictingRateLimit
	}

	// Wrap with the concrete base type so the error tells the caller what
	// they actually passed.
	if c.baseTransport != nil && (c.token != "" || c.tokenSource != nil) {
		if _, ok := c.baseTransport.(*http.Transport); !ok {
			return fmt.Errorf("%w: WithBaseTransport was %T", ErrPreAuthedBaseWithAuth, c.baseTransport)
		}
	}

	// Both fields at zero means WithRequestsPerSecond was never called; only
	// validate when the caller opted in.
	if c.throttleRPS != 0 || c.throttleBurst != 0 {
		if c.throttleRPS <= 0 || c.throttleBurst < 1 {
			return ErrNonPositiveRPS
		}
	}

	return nil
}
