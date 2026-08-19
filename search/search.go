package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pcanilho/go-github-kit/pages"
)

// ErrResultCapHit signals that GitHub's 1000-result hard cap was
// reached. The cap surfaces as 422 with a documented body substring
// after page 10 (10 * per_page=100 = 1000 items).
var ErrResultCapHit = errors.New("search: GitHub 1000-result cap reached")

// ErrNilClient is returned by the iterators when the supplied
// *http.Client is nil.
var ErrNilClient = errors.New("search: nil *http.Client")

// ErrEmptyQuery is returned when the q parameter is empty.
var ErrEmptyQuery = errors.New("search: q is required")

// capSubstring is undocumented in GitHub's status table. Pinned
// in tests so drift surfaces.
const capSubstring = "Only the first 1000 search results are available"

// DefaultBaseURL is the GitHub API root used when WithBaseURL is not
// supplied. Override via WithBaseURL to target GHES or a fixture
// server.
const DefaultBaseURL = "https://api.github.com"

// Result wraps each yielded item with the envelope's TotalCount and
// IncompleteResults flags. IncompleteResults is set per-page when
// GitHub's server timed out for that page; consumers OR across pages.
type Result[T any] struct {
	Item              T
	TotalCount        int
	IncompleteResults bool
}

// Option configures a search iterator. Mirrors the functional-options
// convention used across ghkit (etag, retry, ratelimit, throttle,
// polling).
type Option func(*config)

type config struct {
	baseURL string
	perPage int
	sort    string
	order   string
	headers http.Header
}

// WithBaseURL overrides the default GitHub API root. Use for GHES or
// fixture servers.
func WithBaseURL(u string) Option { return func(c *config) { c.baseURL = u } }

// WithPerPage sets the per-page count. Range [1, 100]; values above
// 100 are clamped. Zero or negative values are ignored (GitHub
// default applies).
func WithPerPage(n int) Option { return func(c *config) { c.perPage = n } }

// WithSort sets the sort field (endpoint-specific; e.g. "updated"
// for issues, "stars" for repositories).
func WithSort(s string) Option { return func(c *config) { c.sort = s } }

// WithOrder sets the sort order ("asc" or "desc").
func WithOrder(o string) Option { return func(c *config) { c.order = o } }

// WithHeaders sets the request headers (cloned per page by the
// underlying pages.Pages iterator).
func WithHeaders(h http.Header) Option { return func(c *config) { c.headers = h } }

func newConfig(opts []Option) *config {
	cfg := &config{
		baseURL: DefaultBaseURL,
		perPage: 100,
	}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// Issues iterates `/search/issues` (issues + pull requests).
func Issues[T any](ctx context.Context, c *http.Client, q string, opts ...Option) iter.Seq2[Result[T], error] {
	return iterate[T](ctx, c, "/search/issues", q, opts)
}

// Code iterates `/search/code`. Has a separate `code_search` rate
// budget on the gofri side; gofri handles routing transparently.
func Code[T any](ctx context.Context, c *http.Client, q string, opts ...Option) iter.Seq2[Result[T], error] {
	return iterate[T](ctx, c, "/search/code", q, opts)
}

// Repos iterates `/search/repositories`.
func Repos[T any](ctx context.Context, c *http.Client, q string, opts ...Option) iter.Seq2[Result[T], error] {
	return iterate[T](ctx, c, "/search/repositories", q, opts)
}

// Users iterates `/search/users`.
func Users[T any](ctx context.Context, c *http.Client, q string, opts ...Option) iter.Seq2[Result[T], error] {
	return iterate[T](ctx, c, "/search/users", q, opts)
}

// envelope is the shape every /search/* endpoint returns.
type envelope[T any] struct {
	TotalCount        int  `json:"total_count"`
	IncompleteResults bool `json:"incomplete_results"`
	Items             []T  `json:"items"`
}

func iterate[T any](ctx context.Context, c *http.Client, path, q string, opts []Option) iter.Seq2[Result[T], error] {
	return func(yield func(Result[T], error) bool) {
		var zero Result[T]
		if c == nil {
			yield(zero, ErrNilClient)
			return
		}
		cfg := newConfig(opts)
		base := strings.TrimRight(cfg.baseURL, "/")

		startURL, err := buildURL(base+path, q, cfg)
		if err != nil {
			yield(zero, err)
			return
		}

		for resp, perr := range pages.Pages(ctx, c, http.MethodGet, startURL, cfg.headers) { //nolint:bodyclose // body consumed below
			if perr != nil {
				yield(zero, perr)
				return
			}
			env, err := decodeEnvelope[T](resp)
			if err != nil {
				yield(zero, err)
				return
			}
			for _, item := range env.Items {
				r := Result[T]{
					Item:              item,
					TotalCount:        env.TotalCount,
					IncompleteResults: env.IncompleteResults,
				}
				if !yield(r, nil) {
					return
				}
			}
		}
	}
}

func decodeEnvelope[T any](resp *http.Response) (envelope[T], error) {
	defer resp.Body.Close() //nolint:errcheck // best-effort close; the decode result is the primary signal

	if resp.StatusCode == http.StatusUnprocessableEntity {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Drain the rest so the connection can return to the keep-alive
		// pool on a failure-loop.
		_, _ = io.Copy(io.Discard, resp.Body)
		if strings.Contains(string(body), capSubstring) {
			return envelope[T]{}, ErrResultCapHit
		}
		return envelope[T]{}, fmt.Errorf("search: 422: %s", strings.TrimSpace(string(body)))
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_, _ = io.Copy(io.Discard, resp.Body)
		return envelope[T]{}, fmt.Errorf("search: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var env envelope[T]
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return envelope[T]{}, fmt.Errorf("search: decode: %w", err)
	}
	return env, nil
}

func buildURL(base, q string, cfg *config) (string, error) {
	if q == "" {
		return "", ErrEmptyQuery
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("search: parse base: %w", err)
	}
	v := u.Query()
	v.Set("q", q)
	perPage := 100
	if cfg.perPage > 0 {
		perPage = min(cfg.perPage, 100)
	}
	v.Set("per_page", strconv.Itoa(perPage))
	if cfg.sort != "" {
		v.Set("sort", cfg.sort)
	}
	if cfg.order != "" {
		v.Set("order", cfg.order)
	}
	u.RawQuery = v.Encode()
	return u.String(), nil
}
