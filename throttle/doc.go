// Package throttle wraps an http.RoundTripper with a client-side
// token-bucket rate limiter backed by golang.org/x/time/rate.
//
// RoundTrip blocks until the limiter admits the request or the request's
// context is cancelled, whichever comes first.
package throttle
