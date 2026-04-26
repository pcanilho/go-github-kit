package ratelimit

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gofri/go-github-ratelimit/v2/github_ratelimit"
	"github.com/gofri/go-github-ratelimit/v2/github_ratelimit/github_primary_ratelimit"
	"github.com/gofri/go-github-ratelimit/v2/github_ratelimit/github_secondary_ratelimit"
)

// PrimaryEvent is an alias for the upstream primary rate-limit callback
// context. Exposed so callers can type callbacks without importing the
// upstream package.
type PrimaryEvent = github_primary_ratelimit.CallbackContext

// SecondaryEvent is an alias for the upstream secondary rate-limit callback
// context.
type SecondaryEvent = github_secondary_ratelimit.CallbackContext

// Option configures a Transport.
type Option interface{ apply(*config) }

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

type config struct {
	primaryDetected   func(*PrimaryEvent)
	primaryReset      func(*PrimaryEvent)
	secondaryDetected func(*SecondaryEvent)
	totalSleepLimit   time.Duration
	logger            *slog.Logger
	upstream          []any // raw options forwarded to github_ratelimit.New
}

const defaultTotalSleepLimit = time.Hour

// WithPrimaryLimitDetected registers a callback that fires when the primary
// rate limit is reached.
func WithPrimaryLimitDetected(cb func(*PrimaryEvent)) Option {
	return optionFunc(func(c *config) { c.primaryDetected = cb })
}

// WithPrimaryLimitReset registers a callback that fires when the primary
// rate limit resets.
func WithPrimaryLimitReset(cb func(*PrimaryEvent)) Option {
	return optionFunc(func(c *config) { c.primaryReset = cb })
}

// WithSecondaryLimitDetected registers a callback that fires when the
// secondary rate limit is detected.
func WithSecondaryLimitDetected(cb func(*SecondaryEvent)) Option {
	return optionFunc(func(c *config) { c.secondaryDetected = cb })
}

// WithTotalSleepLimit caps the cumulative sleep the secondary limiter will
// incur. Default: 1 hour.
func WithTotalSleepLimit(d time.Duration) Option {
	return optionFunc(func(c *config) {
		if d > 0 {
			c.totalSleepLimit = d
		}
	})
}

// WithLogger supplies the slog.Logger for default event logging. Default:
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return optionFunc(func(c *config) {
		if l != nil {
			c.logger = l
		}
	})
}

// WithUpstreamOptions appends raw upstream options from
// github.com/gofri/go-github-ratelimit/v2 to the constructed transport.
// Use this for upstream features ghkit does not expose as named options
// (the secondary abort callback, custom limit providers, before-request
// hooks). Applied after ghkit's named options, so they can override
// callbacks ghkit installed by default.
func WithUpstreamOptions(opts ...any) Option {
	return optionFunc(func(c *config) { c.upstream = append(c.upstream, opts...) })
}

// NewTransport wraps base with primary + secondary GitHub rate limiting.
// When callbacks are not supplied, default handlers log sanitised events:
//
//   - primary detected: logger.Error "ratelimit_primary_detected" kind=primary limit_type=<category>
//   - primary reset:    logger.Info  "ratelimit_primary_reset"    kind=primary limit_type=<category>
//   - secondary detected: logger.Error "ratelimit_secondary_detected" kind=secondary sleep_duration=<d>
//
// The raw CallbackContext is never logged; we read only safe scalar fields.
func NewTransport(base http.RoundTripper, opts ...Option) http.RoundTripper {
	cfg := &config{
		totalSleepLimit: defaultTotalSleepLimit,
	}
	for _, o := range opts {
		o.apply(cfg)
	}
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}

	primaryDetected := cfg.primaryDetected
	if primaryDetected == nil {
		primaryDetected = func(ev *PrimaryEvent) {
			cfg.logger.Error("ratelimit_primary_detected",
				"kind", "primary",
				"limit_type", string(ev.Category),
			)
		}
	}
	primaryReset := cfg.primaryReset
	if primaryReset == nil {
		primaryReset = func(ev *PrimaryEvent) {
			cfg.logger.Info("ratelimit_primary_reset",
				"kind", "primary",
				"limit_type", string(ev.Category),
			)
		}
	}
	secondaryDetected := cfg.secondaryDetected
	if secondaryDetected == nil {
		secondaryDetected = func(ev *SecondaryEvent) {
			var sleep time.Duration
			if ev.TotalSleepTime != nil {
				sleep = *ev.TotalSleepTime
			}
			cfg.logger.Error("ratelimit_secondary_detected",
				"kind", "secondary",
				"sleep_duration", sleep.String(),
			)
		}
	}

	all := make([]any, 0, 4+len(cfg.upstream))
	all = append(all,
		github_primary_ratelimit.WithLimitDetectedCallback(primaryDetected),
		github_primary_ratelimit.WithLimitResetCallback(primaryReset),
		github_secondary_ratelimit.WithLimitDetectedCallback(secondaryDetected),
		github_secondary_ratelimit.WithTotalSleepLimit(cfg.totalSleepLimit, nil),
	)
	all = append(all, cfg.upstream...)
	return github_ratelimit.New(base, all...)
}
