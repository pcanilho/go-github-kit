// Package search iterates GitHub's /search/* envelope endpoints
// (`{total_count, incomplete_results, items[]}`). The iterator
// reuses pages.Pages for Link-header walking, so the configured
// transport stack (RateLimit, Throttle, Retry, oauth2, ETag) applies
// per page.
//
// Each Result[T] carries the per-page TotalCount and IncompleteResults
// flag alongside the typed Item. ErrResultCapHit signals GitHub's
// 1000-result hard cap (a 422 after page 10).
//
// Four endpoints are exposed: Issues, Code, Repos, Users. They share
// one private iteration core; the typed parameter T lets callers pick
// the decode target (e.g. *github.Issue, *github.Repository) without
// pulling go-github into ghkit.
//
// Per-resource rate limit: /search/* uses a separate `search` budget;
// /search/code uses `code_search`. gofri's ratelimit handles the
// routing transparently (no search-specific ghkit option needed).
//
// Options use the functional pattern (WithBaseURL, WithPerPage,
// WithSort, WithOrder, WithHeaders) for parity with the rest of
// ghkit. Each call materializes its own config; sharing options
// across goroutines is safe by construction.
//
// Body lifecycle: the iterator owns each per-page response body and
// closes it via defer in decodeEnvelope. Non-2xx error bodies are
// read (capped at 4 KiB on 422, 1 KiB otherwise) and the remainder
// drained so connections return cleanly to the keep-alive pool.
//
// Error messages on 4xx/422 echo bounded upstream bytes; operators
// piping errors into structured logs should escape if downstream
// handlers are sensitive to control characters.
package search
