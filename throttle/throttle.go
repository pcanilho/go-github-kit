package throttle

import (
	"errors"
	"net/http"

	"golang.org/x/time/rate"
)

// ErrInvalidRate is returned when NewTransport is called with a non-positive
// rps or a burst < 1. Keeping this as an error (instead of panicking or
// silently hanging callers) lets the kit validate at construction time.
var ErrInvalidRate = errors.New("throttle: rps must be > 0 and burst must be >= 1")

// Option configures a Transport.
type Option interface{ apply(*config) }

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

type config struct {
	burst int
}

// WithBurst sets the burst capacity of the token bucket. Default: 1
// (requests are strictly paced at 1/rps seconds apart). Higher burst values
// allow short bursts of activity followed by the sustained rps rate.
// Values < 1 are not silently clamped; they surface as ErrInvalidRate from
// NewTransport.
func WithBurst(n int) Option {
	return optionFunc(func(c *config) { c.burst = n })
}

// NewTransport returns an http.RoundTripper that admits at most rps
// requests per second on average, with the configured burst allowance.
// When base is nil, http.DefaultTransport is used.
//
// Returns ErrInvalidRate when rps <= 0 or burst < 1 so callers see the
// misconfiguration at construction rather than a hanging request later.
//
// The returned RoundTripper honours req.Context(): when the context is
// cancelled before the limiter admits the request, RoundTrip returns the
// context error and does not call the underlying transport.
func NewTransport(base http.RoundTripper, rps float64, opts ...Option) (http.RoundTripper, error) {
	cfg := &config{burst: 1}
	for _, o := range opts {
		o.apply(cfg)
	}
	if rps <= 0 || cfg.burst < 1 {
		return nil, ErrInvalidRate
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{
		base:    base,
		limiter: rate.NewLimiter(rate.Limit(rps), cfg.burst),
	}, nil
}

type transport struct {
	base    http.RoundTripper
	limiter *rate.Limiter
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.limiter.Wait(req.Context()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}
