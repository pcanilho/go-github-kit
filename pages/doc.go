// Package pages walks paginated GitHub REST responses by following the
// RFC 8288 Link header, exposing the iteration as a Go 1.23
// range-over-func iterator.
//
// The iterator runs on a caller-supplied *http.Client, so the configured
// transport stack (RateLimit, Throttle, Retry, oauth2, ETag) applies per
// page with no extra wiring. Use ghkit.HTTPClient or any *http.Client
// the caller already has.
//
// Two entry points:
//
//   - Pages: yields *http.Response per page. The caller decodes and
//     closes each body. Use this when the caller wants to handle the
//     response shape directly or is decoding into types other than the
//     element-per-page model.
//   - As[T]: decodes each page into []T and yields one element at a
//     time. The iterator owns the response body and closes it after
//     decoding, including on caller break or context cancellation.
//
// Errors are surfaced through the iterator's error slot and stop
// iteration after one yield. ErrInvalidLinkHeader is returned when the
// response Link header is structurally malformed; a header with no
// rel="next" is treated as a clean end of pagination, not an error.
package pages
