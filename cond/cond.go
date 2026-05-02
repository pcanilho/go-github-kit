package cond

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// HeaderCacheStatus is the response header the etag layer sets to
// signal cache hit ("hit"), wire store ("miss"), or absent (no etag
// layer in chain). etag and polling import this directly.
const HeaderCacheStatus = "X-Ghkit-Cache"

const (
	valueHit  = "hit"
	valueMiss = "miss"
)

// Status reports the change-vs-unchanged signal for a response.
type Status uint8

const (
	// Updated is the default: a wire 200, or no etag layer in the
	// transport chain. The body is fresh from upstream.
	Updated Status = iota
	// Unchanged is a synth 200 produced by the etag layer from a
	// 304 cache hit. The body is byte-for-byte identical to the last
	// time this resource was fetched.
	Unchanged
)

// String returns the lower-case label used in slog events.
func (s Status) String() string {
	switch s {
	case Updated:
		return "updated"
	case Unchanged:
		return "unchanged"
	}
	return "updated"
}

// ErrNilClient is returned by Fetch when the supplied *http.Client is nil.
var ErrNilClient = errors.New("cond: nil *http.Client")

// ErrNilContext is returned by Fetch when the supplied context is nil.
var ErrNilContext = errors.New("cond: nil context")

// ErrNilRequest is returned by Fetch when the supplied *http.Request is nil.
var ErrNilRequest = errors.New("cond: nil *http.Request")

// StatusOf inspects the response's HeaderCacheStatus and reports the
// change-vs-unchanged signal. Nil-safe: returns Updated.
func StatusOf(resp *http.Response) Status {
	if resp == nil {
		return Updated
	}
	switch resp.Header.Get(HeaderCacheStatus) {
	case valueHit:
		return Unchanged
	case valueMiss, "":
		return Updated
	}
	return Updated
}

// Fetch issues req via c, decodes the response body via decode, and
// returns the typed value plus the change status. The body is closed
// via defer regardless of decode success. On any error (transport,
// decode, ctx), the zero value of T and Updated are returned.
func Fetch[T any](ctx context.Context, c *http.Client, req *http.Request, decode func(io.Reader) (T, error)) (T, Status, error) {
	var zero T
	if ctx == nil {
		return zero, Updated, ErrNilContext
	}
	if c == nil {
		return zero, Updated, ErrNilClient
	}
	if req == nil {
		return zero, Updated, ErrNilRequest
	}
	if req.Context() != ctx {
		req = req.Clone(ctx)
	}
	resp, err := c.Do(req) //nolint:gosec // G104/G704: caller-controlled URL is the API contract; ghkit is an HTTP client library
	if err != nil {
		return zero, Updated, err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	val, decErr := decode(resp.Body)
	if decErr != nil {
		return zero, StatusOf(resp), fmt.Errorf("cond: decode: %w", decErr)
	}
	return val, StatusOf(resp), nil
}
