package pages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"slices"
	"strings"
)

// ErrInvalidLinkHeader signals a structurally malformed Link header.
// A clean end of pagination (a Link with no rel="next" entry, or a
// missing Link altogether) stops iteration silently and is not an
// error.
var ErrInvalidLinkHeader = errors.New("pages: malformed Link header")

// ErrNilClient is returned by Pages and As when the supplied
// *http.Client is nil.
var ErrNilClient = errors.New("pages: nil *http.Client")

// Pages iterates paginated HTTP responses by following the
// Link: rel="next" header. The configured *http.Client carries the full
// transport stack (RateLimit, Throttle, Retry, oauth2, ETag in
// outermost-to-innermost order), so each page goes through it. The
// caller drains and closes each response body.
//
// On error (request build, transport, context cancellation, malformed
// Link header), the iterator yields (nil, err) once and stops. A
// response without a rel="next" Link is treated as a clean end of
// pagination, not an error.
//
// The iterator follows whatever absolute URL the server returns in the
// rel="next" entry, including across hosts. GitHub Enterprise and
// hostname migration scenarios depend on this; callers concerned about
// SSRF in untrusted-server environments should validate the URL before
// looping.
func Pages(
	ctx context.Context,
	client *http.Client,
	method, url string,
	headers http.Header,
) iter.Seq2[*http.Response, error] {
	return func(yield func(*http.Response, error) bool) {
		if client == nil {
			yield(nil, ErrNilClient)
			return
		}
		next := url
		for next != "" {
			req, err := http.NewRequestWithContext(ctx, method, next, nil)
			if err != nil {
				yield(nil, fmt.Errorf("pages: build request: %w", err))
				return
			}
			if headers != nil {
				req.Header = headers.Clone()
			}
			resp, err := client.Do(req) //nolint:bodyclose // yielded to caller; the caller closes per Pages contract
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(resp, nil) {
				return
			}
			link := resp.Header.Get("Link")
			next, err = parseNextLink(link)
			if err != nil {
				yield(nil, err)
				return
			}
		}
	}
}

// As decodes each paginated response into elements of type T and yields
// one element per iteration. The iterator owns the response body and
// closes it after decoding, including on caller break, on context
// cancellation, and on a panic from json.Decode. T has no constraint:
// types from go-github such as *github.Repository decode via the
// standard reflection path.
//
// A page that decodes to an empty slice is skipped silently; iteration
// continues to the next page. A decode error stops iteration after
// yielding (zero, err) once.
func As[T any](
	ctx context.Context,
	client *http.Client,
	method, url string,
	headers http.Header,
) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		for resp, err := range Pages(ctx, client, method, url, headers) { //nolint:bodyclose // body closed by decodePage
			if err != nil {
				yield(zero, err)
				return
			}
			items, decErr := decodePage[T](resp)
			if decErr != nil {
				yield(zero, fmt.Errorf("pages: decode: %w", decErr))
				return
			}
			for _, item := range items {
				if !yield(item, nil) {
					return
				}
			}
		}
	}
}

// decodePage decodes a JSON array body into []T. The body is closed via
// defer, so a panic from json.Decode (e.g. a custom UnmarshalJSON) does
// not leak the response body.
func decodePage[T any](resp *http.Response) ([]T, error) {
	defer func() { _ = resp.Body.Close() }()
	var items []T
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	return items, nil
}

// parseNextLink scans a Link header value and returns the URI tagged
// rel="next", or "" if none is present. Structurally malformed entries
// return ErrInvalidLinkHeader.
//
// Format per RFC 8288:
//
//	<uri1>; rel="next", <uri2>; rel="last"
//
// Per RFC 8288, link-param names are case-insensitive and the rel value
// is a space-separated list of relation types. GitHub never embeds a
// comma inside <uri>, so a top-level comma split is safe in practice.
func parseNextLink(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", nil
	}
	for entry := range strings.SplitSeq(header, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		uri, params, ok := strings.Cut(entry, ">")
		if !ok || !strings.HasPrefix(uri, "<") {
			return "", fmt.Errorf("%w: %q", ErrInvalidLinkHeader, header)
		}
		uri = uri[1:]
		for p := range strings.SplitSeq(params, ";") {
			name, val, ok := strings.Cut(strings.TrimSpace(p), "=")
			if !ok || !strings.EqualFold(name, "rel") {
				continue
			}
			rels := strings.Fields(strings.Trim(val, `"`))
			if slices.Contains(rels, "next") {
				return uri, nil
			}
		}
	}
	return "", nil
}
