// Package ratelimit is a thin facade over github.com/gofri/go-github-ratelimit/v2.
//
// It wires the primary + secondary rate limiters with sensible defaults
// (sanitised slog logging, 1 hour secondary sleep cap) and exposes the
// upstream CallbackContext types directly as type aliases so advanced
// callers can use them without importing the upstream package. The wrapper
// adds no behaviour beyond the defaults; import the upstream directly if
// you need full control.
package ratelimit
