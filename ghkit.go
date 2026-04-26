package ghkit

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pcanilho/go-github-kit/etag"
	"github.com/pcanilho/go-github-kit/ratelimit"
	"github.com/pcanilho/go-github-kit/throttle"
	"golang.org/x/oauth2"
)

// Sentinel errors for config validation. Callers can use errors.Is to
// distinguish specific failure modes in tests or runtime handling.
var (
	ErrConflictingAuth       = errors.New("ghkit: WithToken and WithTokenSource are mutually exclusive")
	ErrPreAuthedBaseWithAuth = errors.New("ghkit: WithBaseTransport with a non-*http.Transport base cannot be combined with WithToken or WithTokenSource")
	ErrNonPositiveRPS        = errors.New("ghkit: WithRequestsPerSecond requires rps > 0 and burst >= 1")
	ErrNilFactory            = errors.New("ghkit: New requires a non-nil factory function")
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
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}

	// Build inside-out: base -> etag -> oauth2 -> ratelimit -> throttle -> userAgent.
	// ETag must sit below oauth2 so its hash domain sees the cloned
	// Authorization header. UserAgent sits outermost so it overrides any
	// SDK-level value.
	rt := cfg.baseTransport

	if cfg.etagEnabled {
		etagOpts := cfg.etagOpts
		etagOpts = append(etagOpts, etag.WithLogger(cfg.logger))
		inner, err := etag.NewTransport(rt, etagOpts...)
		if err != nil {
			return nil, fmt.Errorf("ghkit: etag: %w", err)
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

	if cfg.rateLimitEnabled {
		rlOpts := cfg.rateLimitOpts
		rlOpts = append(rlOpts, ratelimit.WithLogger(cfg.logger))
		rt = ratelimit.NewTransport(rt, rlOpts...)
	} else if len(cfg.rateLimitOpts) > 0 {
		cfg.logger.Warn("ghkit: WithRateLimit callbacks were registered but WithRateLimitDisabled was also set; callbacks will be ignored")
	}

	if cfg.throttleRPS > 0 {
		thr, err := throttle.NewTransport(rt, cfg.throttleRPS, throttle.WithBurst(cfg.throttleBurst))
		if err != nil {
			return nil, fmt.Errorf("ghkit: throttle: %w", err)
		}
		rt = thr
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
// Canonical usage:
//
//	import "github.com/google/go-github/v85/github"
//
//	gh, err := ghkit.New(github.NewClient,
//	    ghkit.WithToken(os.Getenv("GITHUB_TOKEN")),
//	    ghkit.WithETagCache(),
//	)
//
// Custom construction (UserAgent, GitHub Enterprise BaseURL, any other
// post-construction tweaks) goes inside a factory closure:
//
//	gh, err := ghkit.New(func(hc *http.Client) *github.Client {
//	    c := github.NewClient(hc)
//	    c.UserAgent = "my-app/1.0"
//	    return c
//	}, opts...)
//
// For GitHub Enterprise, call github.NewClient(hc).WithEnterpriseURLs(base, upload)
// inside the closure. The base URL must end with a trailing slash; go-github
// returns an error if it does not.
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

func validateConfig(c *config) error {
	if c.token != "" && c.tokenSource != nil {
		return ErrConflictingAuth
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
